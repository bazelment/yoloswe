package stt

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventType identifies the kind of transcription event.
type EventType int

const (
	EventSpeechStart EventType = iota
	EventPartialText
	EventFinalText
	EventSpeechEnd
	EventError
)

func (t EventType) String() string {
	switch t {
	case EventSpeechStart:
		return "speech-start"
	case EventPartialText:
		return "partial"
	case EventFinalText:
		return "final"
	case EventSpeechEnd:
		return "speech-end"
	case EventError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// Event is a single transcription event from a streaming session.
type Event struct {
	Timestamp  time.Time
	Error      error
	Text       string
	Raw        json.RawMessage
	Confidence float64
	Type       EventType
	IsFinal    bool
}

// AudioConfig specifies the audio format for a session.
type AudioConfig struct {
	SampleRate int           // e.g. 16000
	BitDepth   int           // e.g. 16
	Channels   int           // e.g. 1 (mono)
	ChunkSize  time.Duration // e.g. 20ms
}

// DefaultAudioConfig returns the canonical 16kHz/16-bit/mono config with 20ms chunks.
func DefaultAudioConfig() AudioConfig {
	return AudioConfig{
		SampleRate: 16000,
		BitDepth:   16,
		Channels:   1,
		ChunkSize:  20 * time.Millisecond,
	}
}

// ChunkBytes returns the number of bytes per audio chunk.
func (c AudioConfig) ChunkBytes() int {
	bytesPerSample := c.BitDepth / 8
	samplesPerChunk := int(c.ChunkSize.Seconds() * float64(c.SampleRate))
	return samplesPerChunk * bytesPerSample * c.Channels
}

// StreamOpts configures a streaming transcription session.
type StreamOpts struct {
	// Language is the BCP-47 language code (e.g. "en-US"). Empty means auto-detect.
	Language string
	// Model is the provider-specific model name (e.g. "nova-3" for Deepgram).
	Model string
	// VAD enables voice activity detection if supported by the provider.
	VAD bool
	// Interim enables interim/partial results.
	Interim bool
}

// TranscribeOpts configures a batch transcription request.
type TranscribeOpts struct {
	Language string
	Model    string
}

// Transcript is the result of a batch transcription.
type Transcript struct {
	Text       string
	Raw        json.RawMessage
	Words      []Word
	Duration   time.Duration
	Confidence float64
}

// Word is a single word with timing information.
type Word struct {
	Text       string
	Start      time.Duration
	End        time.Duration
	Confidence float64
}
