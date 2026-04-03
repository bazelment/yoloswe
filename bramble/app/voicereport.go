package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/voice/tts"
)

// SynthesisTimeout is the maximum time allowed for TTS synthesis + playback.
const SynthesisTimeout = 30 * time.Second

// VoiceReporterConfig holds the configuration for creating a VoiceReporter.
type VoiceReporterConfig struct { //nolint:govet // fieldalignment: readability over packing
	Mode       PlaybackMode
	Provider   tts.TextToSpeech // nil in redirect mode
	Handler    PlaybackHandler  // nil in redirect mode
	Redirector *RedirectTextWriter
	VoiceCfg   *session.VoiceReportingConfig
}

// VoiceReporter handles voice reporting for completed sessions.
// In redirect mode it writes summary text to a file (skipping TTS).
// In direct/file modes it synthesizes audio and plays it.
type VoiceReporter struct { //nolint:govet // fieldalignment: readability over packing
	mode       PlaybackMode
	provider   tts.TextToSpeech
	handler    PlaybackHandler
	redirector *RedirectTextWriter
	voiceCfg   *session.VoiceReportingConfig
}

// NewVoiceReporter creates a VoiceReporter from the given config.
func NewVoiceReporter(cfg VoiceReporterConfig) *VoiceReporter {
	return &VoiceReporter{
		mode:       cfg.Mode,
		provider:   cfg.Provider,
		handler:    cfg.Handler,
		redirector: cfg.Redirector,
		voiceCfg:   cfg.VoiceCfg,
	}
}

// Report generates a voice report for a completed session.
// In redirect mode, it writes summary text to a file (no TTS).
// In direct/file modes, it synthesizes audio and plays it.
// Errors are logged to stderr; the function never returns an error.
func (vr *VoiceReporter) Report(ctx context.Context, info session.SessionInfo) {
	summaryText := session.GenerateSummary(info)

	if vr.mode == PlaybackModeRedirect {
		if vr.redirector != nil {
			_ = vr.redirector.WriteText(summaryText)
		}
		return
	}

	if vr.provider == nil || vr.handler == nil {
		return
	}

	var synthVoice string
	if vr.voiceCfg != nil {
		synthVoice = vr.voiceCfg.Voice
	}

	audio, err := vr.provider.Synthesize(ctx, summaryText, tts.SynthOpts{
		Voice: synthVoice,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice report: synthesis failed for session %s: %v\n", info.ID, err)
		return
	}

	result, err := vr.handler.Play(ctx, audio.Data, audio.Format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice report: playback failed for session %s: %v\n", info.ID, err)
		return
	}

	if result != nil && result.FilePath != "" {
		title := info.Title
		if title == "" {
			title = string(info.ID)
		}
		fmt.Fprintf(os.Stderr, "voice report for %s: saved to %s\n", title, result.FilePath)
	}
}

// Close cleans up resources. Safe to call on nil receiver.
func (vr *VoiceReporter) Close() error {
	if vr == nil {
		return nil
	}
	if dp, ok := vr.handler.(*DirectPlayback); ok {
		return dp.Close()
	}
	return nil
}

// SynthesizeAndPlay synthesizes a voice summary for the given session and plays
// it via the provided PlaybackHandler. It is a shared helper used by both the
// TUI model (app.Model.reportSessionVoice) and the delegator CLI.
//
// Deprecated: Use VoiceReporter.Report instead. Kept for backward compatibility
// with the delegator CLI.
func SynthesizeAndPlay(ctx context.Context, provider tts.TextToSpeech, handler PlaybackHandler, cfg *session.VoiceReportingConfig, info session.SessionInfo) {
	vr := &VoiceReporter{
		mode:     PlaybackModeDirect,
		provider: provider,
		handler:  handler,
		voiceCfg: cfg,
	}
	vr.Report(ctx, info)
}
