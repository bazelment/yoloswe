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
// Cancelling the context passed to StartSession closes the underlying connection
// and drains the Events() channel (all pending events are delivered, then channel closes).
// Close() is idempotent and also drains Events().
type Session interface {
	SendAudio(data []byte) error
	Events() <-chan Event
	Close() error
}
