package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/voice/tts"
	"github.com/bazelment/yoloswe/voice/tts/elevenlabs"
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

// BuildVoiceReporter creates a VoiceReporter from CLI flags.
// Returns nil if voice reporting is not enabled or cannot be configured.
// apiKey is the ElevenLabs API key; voice is the TTS voice ID; mode is the
// playback mode string (auto/direct/local/file/redirect); saveDir is the
// file-mode save directory (empty for default).
func BuildVoiceReporter(apiKey, voice, mode, saveDir string) *VoiceReporter {
	voiceCfg := &session.VoiceReportingConfig{
		Enabled: true,
		Mode:    mode,
		Voice:   voice,
		SaveDir: saveDir,
	}

	resolvedMode := PlaybackMode(mode)
	if resolvedMode == PlaybackModeAuto {
		resolvedMode = DetectPlaybackMode()
	}
	if resolvedMode == PlaybackModeLocal {
		resolvedMode = PlaybackModeDirect
	}

	switch resolvedMode {
	case PlaybackModeDirect, PlaybackModeFile, PlaybackModeRedirect:
		// valid
	default:
		fmt.Fprintf(os.Stderr, "Warning: unknown voice-report-mode %q, falling back to file\n", mode)
		resolvedMode = PlaybackModeFile
	}

	reporterCfg := VoiceReporterConfig{
		Mode:     resolvedMode,
		VoiceCfg: voiceCfg,
	}

	if resolvedMode == PlaybackModeRedirect {
		if saveDir == "" {
			home, _ := os.UserHomeDir()
			saveDir = filepath.Join(home, ".bramble", "voice-reports")
		}
		reporterCfg.Redirector = &RedirectTextWriter{Dir: saveDir}
	} else {
		provider, err := elevenlabs.NewProvider(apiKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: voice reports disabled: %v\n", err)
			return nil
		}
		reporterCfg.Provider = provider
		reporterCfg.Handler = NewPlaybackHandler(resolvedMode, saveDir)
	}

	if reporterCfg.Provider == nil && reporterCfg.Redirector == nil {
		return nil
	}
	return NewVoiceReporter(reporterCfg)
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
