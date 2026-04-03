package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// PlaybackMode identifies how audio should be played.
type PlaybackMode string

const (
	// PlaybackModeAuto auto-detects the best playback method.
	PlaybackModeAuto PlaybackMode = "auto"
	// PlaybackModeLocal plays audio using a system audio player.
	PlaybackModeLocal PlaybackMode = "local"
	// PlaybackModeFile saves audio to disk and returns the path.
	PlaybackModeFile PlaybackMode = "file"
)

// PlaybackResult contains the result of a playback attempt.
type PlaybackResult struct {
	// FilePath is set when the audio was saved to a file (file mode).
	FilePath string
}

// PlaybackHandler plays audio data.
type PlaybackHandler interface {
	Play(ctx context.Context, data []byte, format string) (*PlaybackResult, error)
}

// LocalPlayback plays audio using a system command (ffplay, paplay, afplay).
type LocalPlayback struct{}

// Play attempts to play audio using available system players.
func (lp *LocalPlayback) Play(ctx context.Context, data []byte, format string) (*PlaybackResult, error) {
	// Write to temp file for the player.
	tmpFile, err := os.CreateTemp("", "bramble-voice-*."+format)
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("write audio: %w", err)
	}
	tmpFile.Close()

	// Try players in order of preference.
	players := []struct {
		name string
		args []string
	}{
		{"ffplay", []string{"-nodisp", "-autoexit", "-loglevel", "quiet", tmpFile.Name()}},
		{"paplay", []string{tmpFile.Name()}},
		{"afplay", []string{tmpFile.Name()}},
		{"aplay", []string{tmpFile.Name()}},
	}

	for _, player := range players {
		path, err := exec.LookPath(player.name)
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, path, player.args...)
		if err := cmd.Run(); err != nil {
			continue
		}
		return &PlaybackResult{}, nil
	}

	return nil, fmt.Errorf("no audio player found (tried ffplay, paplay, afplay, aplay)")
}

// FilePlayback saves audio to a directory on disk.
type FilePlayback struct {
	Dir string
}

// Play saves the audio data to a file and returns the path.
func (fp *FilePlayback) Play(_ context.Context, data []byte, format string) (*PlaybackResult, error) {
	if err := os.MkdirAll(fp.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create voice reports dir: %w", err)
	}

	filename := fmt.Sprintf("voice-report-%s.%s", time.Now().Format("20060102-150405"), format)
	path := filepath.Join(fp.Dir, filename)

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("write audio file: %w", err)
	}

	return &PlaybackResult{FilePath: path}, nil
}

// DetectPlaybackMode determines the best playback mode for the current
// environment. Returns PlaybackModeLocal if a TTY and audio player are
// available, otherwise PlaybackModeFile.
func DetectPlaybackMode() PlaybackMode {
	// If running over SSH, prefer file mode.
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" {
		return PlaybackModeFile
	}

	// Check if any audio player is available.
	players := []string{"ffplay", "paplay", "afplay", "aplay"}
	for _, player := range players {
		if _, err := exec.LookPath(player); err == nil {
			return PlaybackModeLocal
		}
	}

	return PlaybackModeFile
}

// NewPlaybackHandler creates a PlaybackHandler for the given mode.
// For PlaybackModeAuto, it auto-detects the best mode.
// saveDir is the directory for file-mode playback (defaults to ~/.bramble/voice-reports/).
func NewPlaybackHandler(mode PlaybackMode, saveDir string) PlaybackHandler {
	if mode == PlaybackModeAuto {
		mode = DetectPlaybackMode()
	}

	switch mode {
	case PlaybackModeLocal:
		return &LocalPlayback{}
	case PlaybackModeFile:
		if saveDir == "" {
			home, _ := os.UserHomeDir()
			saveDir = filepath.Join(home, ".bramble", "voice-reports")
		}
		return &FilePlayback{Dir: saveDir}
	default:
		return &FilePlayback{Dir: saveDir}
	}
}
