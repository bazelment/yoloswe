// Package tts provides text-to-speech interfaces and implementations.
//
// It defines a provider-agnostic interface for speech synthesis, mirroring
// the structure of the stt package. Implementations handle provider-specific
// API calls while consumers work with the common TextToSpeech interface.
package tts

import "context"

// TextToSpeech synthesizes speech from text.
type TextToSpeech interface {
	Synthesize(ctx context.Context, text string, opts SynthOpts) (*Audio, error)
}

// SynthOpts configures a synthesis request.
type SynthOpts struct {
	// Voice is the provider-specific voice identifier.
	Voice string
	// Language is the BCP-47 language code (e.g. "en-US"). Empty means provider default.
	Language string
	// Speed is a multiplier for speech rate (1.0 = normal). Zero means provider default.
	Speed float64
}
