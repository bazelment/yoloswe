package app

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/voice/playback"
)

// PlaybackMode identifies how audio should be played.
type PlaybackMode string

const (
	// PlaybackModeAuto auto-detects the best playback method.
	PlaybackModeAuto PlaybackMode = "auto"
	// PlaybackModeDirect plays audio using Go-native PCM output.
	PlaybackModeDirect PlaybackMode = "direct"
	// PlaybackModeLocal is a deprecated alias for PlaybackModeDirect.
	PlaybackModeLocal PlaybackMode = "local"
	// PlaybackModeFile saves audio to disk and returns the path.
	PlaybackModeFile PlaybackMode = "file"
	// PlaybackModeRedirect writes summary text to a file for remote consumption.
	PlaybackModeRedirect PlaybackMode = "redirect"
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

// DirectPlayback plays audio using a system audio player (ffplay, afplay).
type DirectPlayback struct {
	player *playback.Player
}

// NewDirectPlayback creates a DirectPlayback handler.
func NewDirectPlayback() *DirectPlayback {
	return &DirectPlayback{player: playback.NewPlayer()}
}

// Play plays MP3 audio through an available system audio player.
func (dp *DirectPlayback) Play(ctx context.Context, data []byte, format string) (*PlaybackResult, error) {
	if format != "mp3" {
		return nil, fmt.Errorf("direct playback only supports mp3, got %s", format)
	}
	if err := dp.player.PlayMP3(ctx, data); err != nil {
		return nil, fmt.Errorf("direct playback: %w", err)
	}
	return &PlaybackResult{}, nil
}

// Close shuts down the underlying player.
func (dp *DirectPlayback) Close() error {
	return dp.player.Close()
}

// FilePlayback saves audio to a directory on disk.
type FilePlayback struct {
	Dir string
}

// Play saves the audio data to a file and returns the path.
func (fp *FilePlayback) Play(_ context.Context, data []byte, format string) (*PlaybackResult, error) {
	if err := os.MkdirAll(fp.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create voice reports dir: %w", err)
	}

	// Use a random suffix to avoid collisions when two reports land in the same second.
	suffix := rand.Int63() //nolint:gosec // non-crypto random is fine for uniqueness
	filename := fmt.Sprintf("voice-report-%s-%x.%s", time.Now().Format("20060102-150405"), suffix, format)
	path := filepath.Join(fp.Dir, filename)

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("write audio file: %w", err)
	}

	return &PlaybackResult{FilePath: path}, nil
}

// RedirectTextWriter writes voice report text to files for remote consumption.
// It writes the latest report to voice-report-latest.txt (overwrite) and appends
// timestamped entries to voice-report-log.txt for history.
type RedirectTextWriter struct {
	Dir string
}

// WriteText writes summary text to the redirect directory.
// On failure (disk full, permissions), logs a warning and discards — never blocks.
func (r *RedirectTextWriter) WriteText(text string) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		log.Printf("voice redirect: failed to create dir %s: %v", r.Dir, err)
		return nil //nolint:nilerr // discard on failure per design
	}

	// Write latest (overwrite).
	latestPath := filepath.Join(r.Dir, "voice-report-latest.txt")
	if err := os.WriteFile(latestPath, []byte(text+"\n"), 0o600); err != nil {
		log.Printf("voice redirect: failed to write latest: %v", err)
		return nil //nolint:nilerr // discard on failure per design
	}

	// Append to log with timestamp.
	logPath := filepath.Join(r.Dir, "voice-report-log.txt")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("voice redirect: failed to open log: %v", err)
		return nil //nolint:nilerr // discard on failure per design
	}
	defer f.Close()

	entry := fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), text)
	if _, err := f.WriteString(entry); err != nil {
		log.Printf("voice redirect: failed to append log: %v", err)
	}

	return nil
}

// DetectPlaybackMode determines the best playback mode for the current
// environment. Returns PlaybackModeRedirect if SSH environment variables are
// set, or PlaybackModeDirect for local environments.
func DetectPlaybackMode() PlaybackMode {
	// If running over SSH, prefer redirect mode.
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" {
		return PlaybackModeRedirect
	}

	// Local machine — play via system audio player (ffplay/afplay).
	return PlaybackModeDirect
}

// NewPlaybackHandler creates a PlaybackHandler for the given mode.
// For PlaybackModeAuto, it auto-detects the best mode.
// saveDir is the directory for file-mode playback (defaults to ~/.bramble/voice-reports/).
// Returns nil for PlaybackModeRedirect (handled at a higher level by VoiceReporter).
func NewPlaybackHandler(mode PlaybackMode, saveDir string) PlaybackHandler {
	if mode == PlaybackModeAuto {
		mode = DetectPlaybackMode()
	}

	// Treat "local" as alias for "direct".
	if mode == PlaybackModeLocal {
		mode = PlaybackModeDirect
	}

	// Apply default save directory for any file-based playback.
	if saveDir == "" {
		home, _ := os.UserHomeDir()
		saveDir = filepath.Join(home, ".bramble", "voice-reports")
	}

	switch mode {
	case PlaybackModeDirect:
		return NewDirectPlayback()
	case PlaybackModeRedirect:
		return nil // handled by VoiceReporter
	default:
		// PlaybackModeFile and any unrecognized mode fall through to file playback.
		return &FilePlayback{Dir: saveDir}
	}
}
