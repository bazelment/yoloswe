package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/voice/tts"
)

// SynthesizeAndPlay synthesizes a voice summary for the given session and plays
// it via the provided PlaybackHandler. It is a shared helper used by both the
// TUI model (app.Model.reportSessionVoice) and the delegator CLI.
//
// It blocks until synthesis and playback complete (or the context is cancelled).
// Errors are logged to stderr; the function never returns an error to avoid
// breaking callers that run it in a goroutine.
func SynthesizeAndPlay(ctx context.Context, provider tts.TextToSpeech, handler PlaybackHandler, cfg *session.VoiceReportingConfig, info session.SessionInfo) {
	summaryText := session.GenerateSummary(info)

	var synthVoice string
	if cfg != nil {
		synthVoice = cfg.Voice
	}

	audio, err := provider.Synthesize(ctx, summaryText, tts.SynthOpts{
		Voice: synthVoice,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice report: synthesis failed for session %s: %v\n", info.ID, err)
		return
	}

	result, err := handler.Play(ctx, audio.Data, audio.Format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice report: playback failed for session %s: %v\n", info.ID, err)
		return
	}

	if result != nil && result.FilePath != "" {
		title := info.Title
		if title == "" {
			title = string(info.ID)
		}
		log.Printf("voice report for %s: saved to %s", title, result.FilePath)
	}
}

// synthesisTimeout is the maximum time allowed for TTS synthesis + playback.
const synthesisTimeout = 30 * time.Second
