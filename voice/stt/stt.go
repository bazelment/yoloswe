// Package stt provides speech-to-text interfaces and implementations.
//
// It defines provider-agnostic interfaces for both streaming (live microphone)
// and batch (file) transcription, with typed events including voice activity
// detection boundaries.
package stt

import (
	"context"
	"io"
)

// Transcriber handles batch (file) transcription.
type Transcriber interface {
	Transcribe(ctx context.Context, audio io.Reader, opts TranscribeOpts) (*Transcript, error)
}

// StreamingTranscriber handles live audio input with VAD.
type StreamingTranscriber interface {
	StartSession(ctx context.Context, cfg AudioConfig, opts StreamOpts) (Session, error)
}

// Session represents an active streaming transcription session.
//
// Thread safety: SendAudio is safe to call concurrently with reading Events().
// SendAudio and Close are NOT safe to call concurrently with each other.
// Cancelling the context passed to StartSession closes the underlying connection;
// the Events() channel will close once the provider's close callback fires.
// In-flight events may be dropped after context cancellation.
// Close() is idempotent; calling it causes Events() to close.
type Session interface {
	SendAudio(data []byte) error
	Events() <-chan Event
	Close() error
}
