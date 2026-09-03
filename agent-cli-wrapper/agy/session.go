package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// QueryResult contains the result of a one-shot agy query.
//
// Model is the id that actually reached the CLI; see TurnCompleteEvent.Model
// for why it can differ from the configured one.
type QueryResult struct {
	Text           string
	ConversationID string
	Model          string
	DurationMs     int64
	Usage          Usage
	Success        bool
}

// resultPayload mirrors agy's --output-format json result object.
type resultPayload struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
	Usage          Usage  `json:"usage"`
}

// parseResultPayload parses agy's JSON stdout. Empty stdout yields the zero
// payload because a process error already describes the root cause.
func parseResultPayload(raw []byte) (resultPayload, error) {
	raw = bytes.TrimSpace(raw)
	var payload resultPayload
	if len(raw) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return resultPayload{}, fmt.Errorf("agy: failed to parse --output-format json result: %w (raw output: %s)", err, raw)
	}
	return payload, nil
}

// Session manages one agy print-mode invocation.
type Session struct {
	process *processManager
	events  chan Event
	done    chan struct{}
	prompt  string
	config  SessionConfig
	mu      sync.RWMutex
	started bool
	stopped bool
}

// NewSession creates a new agy session.
func NewSession(prompt string, opts ...SessionOption) *Session {
	config := defaultConfig()
	for _, opt := range opts {
		opt(&config)
	}
	return &Session{
		prompt: prompt,
		config: config,
		events: make(chan Event, config.EventBufferSize),
		done:   make(chan struct{}),
	}
}

// Start spawns agy, waits for print-mode completion, and emits result events.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}
	s.process = newProcessManager(s.prompt, s.config)
	s.started = true
	s.mu.Unlock()

	go s.run(ctx)
	return nil
}

// Events returns a read-only event channel.
func (s *Session) Events() <-chan Event {
	return s.events
}

// Stop terminates the subprocess if it is still running.
func (s *Session) Stop() error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	close(s.done)
	process := s.process
	s.mu.Unlock()

	if process != nil {
		return process.Stop()
	}
	return nil
}

func (s *Session) run(ctx context.Context) {
	defer close(s.events)

	start := time.Now()
	out, _, procErr := s.process.Start(ctx)
	duration := time.Since(start).Milliseconds()

	payload, parseErr := parseResultPayload(out)

	if payload.Response != "" {
		s.emit(TextEvent{Text: strings.TrimRight(payload.Response, "\r\n")})
	}

	turn := TurnCompleteEvent{
		ConversationID: payload.ConversationID,
		Model:          s.process.EffectiveModel(),
		DurationMs:     duration,
		Usage:          payload.Usage,
	}
	switch {
	case procErr != nil:
		s.emit(ErrorEvent{Error: procErr, Context: "process"})
		turn.Error = procErr
	case parseErr != nil:
		s.emit(ErrorEvent{Error: parseErr, Context: "parse"})
		turn.Error = parseErr
	case payload.Status != "SUCCESS":
		turnErr := fmt.Errorf("agy: turn failed with status %q: %s", payload.Status, payload.Error)
		s.emit(ErrorEvent{Error: turnErr, Context: "agy"})
		turn.Error = turnErr
	default:
		turn.Success = true
	}
	s.emit(turn)
}

func (s *Session) emit(evt Event) {
	select {
	case <-s.done:
		return
	case s.events <- evt:
	}
}

// Query runs a one-shot agy prompt and returns the result.
func Query(ctx context.Context, prompt string, opts ...SessionOption) (*QueryResult, error) {
	session := NewSession(prompt, opts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	var result QueryResult
	for evt := range session.Events() {
		switch e := evt.(type) {
		case TextEvent:
			result.Text += e.Text
		case TurnCompleteEvent:
			result.DurationMs = e.DurationMs
			result.Success = e.Success
			result.ConversationID = e.ConversationID
			result.Model = e.Model
			result.Usage = e.Usage
			return &result, e.Error
		case ErrorEvent:
			return nil, e.Error
		}
	}
	return nil, ErrNotStarted
}
