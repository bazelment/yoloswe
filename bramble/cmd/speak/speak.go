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

type (
	audioPlayer interface {
		PlayMP3(context.Context, []byte) error
		Close() error
	}

	speakConfig struct {
		apiKey  string
		voice   string
		timeout time.Duration
		speed   float64
	}

	speakDeps struct {
		input       io.Reader
		newPlayer   func() audioPlayer
		newProvider func(string) (tts.TextToSpeech, error)
	}
)

func init() {
	Cmd.Flags().StringVar(&elevenLabsAPIKey, "elevenlabs-api-key", "", "ElevenLabs API key (or set ELEVENLABS_API_KEY env var)")
	Cmd.Flags().StringVar(&ttsVoice, "tts-voice", "", "ElevenLabs voice ID for TTS synthesis")
	Cmd.Flags().Float64Var(&speed, "speed", 1.0, "Speech rate multiplier (1.0 = normal)")
}

func runSpeak(cmd *cobra.Command, args []string) error {
	return runSpeakWithDeps(cmd.Context(), defaultSpeakConfig(), defaultSpeakDeps())
}

func defaultSpeakConfig() speakConfig {
	return speakConfig{
		apiKey:  elevenLabsAPIKey,
		voice:   ttsVoice,
		timeout: 60 * time.Second,
		speed:   speed,
	}
}

func defaultSpeakDeps() speakDeps {
	return speakDeps{
		input: os.Stdin,
		newPlayer: func() audioPlayer {
			return playback.NewPlayer()
		},
		newProvider: func(apiKey string) (tts.TextToSpeech, error) {
			return elevenlabs.NewProvider(apiKey)
		},
	}
}

func runSpeakWithDeps(parent context.Context, cfg speakConfig, deps speakDeps) error {
	text, err := readSpeakText(deps.input)
	if err != nil {
		return err
	}
	if text == "" {
		return nil // nothing to speak
	}

	provider, err := deps.newProvider(cfg.apiKey)
	if err != nil {
		return fmt.Errorf("create TTS provider: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()

	audio, err := provider.Synthesize(ctx, text, tts.SynthOpts{
		Voice: cfg.voice,
		Speed: cfg.speed,
	})
	if err != nil {
		return fmt.Errorf("synthesize: %w", err)
	}

	player := deps.newPlayer()
	defer player.Close()

	if err := player.PlayMP3(ctx, audio.Data); err != nil {
		return fmt.Errorf("playback: %w", err)
	}

	return nil
}

func readSpeakText(input io.Reader) (string, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
