// Package logging provides JSONL event logging for STT sessions.
//
// Each transcription session logs events for vendor comparison, latency
// measurement, and debugging. One JSON object per line.
package logging

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/voice/stt"
)

// Entry is a single JSONL log entry.
type Entry struct {
	Raw        *json.RawMessage `json:"raw,omitempty"`
	Timestamp  time.Time        `json:"timestamp"`
	Provider   string           `json:"provider"`
	EventType  string           `json:"event_type"`
	Text       string           `json:"text,omitempty"`
	Error      string           `json:"error,omitempty"`
	Confidence float64          `json:"confidence,omitempty"`
	LatencyMs  int64            `json:"latency_ms"`
	IsFinal    bool             `json:"is_final,omitempty"`
}

// Logger writes JSONL entries to a writer. Safe for concurrent use.
type Logger struct {
	w     io.Writer
	enc   *json.Encoder
	start time.Time
	mu    sync.Mutex
}

// New creates a Logger that writes to w. The start time is used to compute
// latency_ms for each event.
func New(w io.Writer, start time.Time) *Logger {
	return &Logger{
		w:     w,
		enc:   json.NewEncoder(w),
		start: start,
	}
}

// LogEvent writes a JSONL entry for the given event.
func (l *Logger) LogEvent(provider string, evt stt.Event) {
	entry := Entry{
		Timestamp:  evt.Timestamp,
		Provider:   provider,
		EventType:  evt.Type.String(),
		Text:       evt.Text,
		Confidence: evt.Confidence,
		IsFinal:    evt.IsFinal,
		LatencyMs:  evt.Timestamp.Sub(l.start).Milliseconds(),
	}
	if evt.Error != nil {
		entry.Error = evt.Error.Error()
	}
	if len(evt.Raw) > 0 {
		raw := json.RawMessage(evt.Raw)
		entry.Raw = &raw
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(entry) //nolint:errcheck // best-effort logging
}

// LogSessionStart writes a session-start marker.
func (l *Logger) LogSessionStart(provider string) {
	entry := Entry{
		Timestamp: time.Now(),
		Provider:  provider,
		EventType: "session-start",
		LatencyMs: 0,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(entry) //nolint:errcheck
}

// LogSessionEnd writes a session-end marker.
func (l *Logger) LogSessionEnd(provider string) {
	entry := Entry{
		Timestamp: time.Now(),
		Provider:  provider,
		EventType: "session-end",
		LatencyMs: time.Since(l.start).Milliseconds(),
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(entry) //nolint:errcheck
}
