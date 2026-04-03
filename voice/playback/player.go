// Package playback provides audio playback utilities.
package playback

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Player plays MP3 audio using available system audio players.
type Player struct{}

// NewPlayer creates a new Player.
func NewPlayer() *Player {
	return &Player{}
}

// PlayMP3 writes MP3 data to a temp file and plays it using the first
// available system audio player (ffplay, afplay).
// Blocks until playback completes or the context is cancelled.
func (p *Player) PlayMP3(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty audio data")
	}

	// Write to temp file for the player.
	tmpFile, err := os.CreateTemp("", "bramble-speak-*.mp3")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write audio: %w", err)
	}
	tmpFile.Close()

	// Try players in order of preference.
	players := []struct {
		name string
		args []string
	}{
		{"ffplay", []string{"-nodisp", "-autoexit", "-loglevel", "quiet", tmpFile.Name()}},
		{"afplay", []string{tmpFile.Name()}},
	}

	for _, player := range players {
		path, err := exec.LookPath(player.name)
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, path, player.args...)
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		return nil
	}

	return fmt.Errorf("no audio player found (tried ffplay, afplay)")
}

// Close is a no-op for the external-command-based player.
func (p *Player) Close() error {
	return nil
}
