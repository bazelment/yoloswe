// Package speak provides the "bramble speak" subcommand for synthesizing
// and playing text from stdin via TTS.
package speak

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/voice/playback"
	"github.com/bazelment/yoloswe/voice/tts"
	"github.com/bazelment/yoloswe/voice/tts/elevenlabs"
)

var (
	elevenLabsAPIKey string
	ttsVoice         string
	speed            float64
)

// Cmd is the cobra command for "bramble speak".
var Cmd = &cobra.Command{
	Use:   "speak",
	Short: "Synthesize and speak text from stdin",
	Long: `Reads text from stdin, synthesizes speech via TTS, and plays it through
the system speaker using an available audio player (ffplay or afplay).

Useful for remote playback of bramble voice reports:
  ssh remote "cat ~/.bramble/voice-reports/voice-report-latest.txt" | bramble speak`,
	RunE: runSpeak,
}

func init() {
	Cmd.Flags().StringVar(&elevenLabsAPIKey, "elevenlabs-api-key", "", "ElevenLabs API key (or set ELEVENLABS_API_KEY env var)")
	Cmd.Flags().StringVar(&ttsVoice, "tts-voice", "", "ElevenLabs voice ID for TTS synthesis")
	Cmd.Flags().Float64Var(&speed, "speed", 1.0, "Speech rate multiplier (1.0 = normal)")
}

func runSpeak(cmd *cobra.Command, args []string) error {
	// Read all text from stdin.
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil // nothing to speak
	}

	// Create ElevenLabs provider.
	provider, err := elevenlabs.NewProvider(elevenLabsAPIKey)
	if err != nil {
		return fmt.Errorf("create TTS provider: %w", err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	// Synthesize.
	audio, err := provider.Synthesize(ctx, text, tts.SynthOpts{
		Voice: ttsVoice,
		Speed: speed,
	})
	if err != nil {
		return fmt.Errorf("synthesize: %w", err)
	}

	// Play.
	player := playback.NewPlayer()
	defer player.Close()

	if err := player.PlayMP3(ctx, audio.Data); err != nil {
		return fmt.Errorf("playback: %w", err)
	}

	return nil
}
