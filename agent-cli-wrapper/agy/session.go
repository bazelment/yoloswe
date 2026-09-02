package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// QueryResult contains the result of a one-shot agy query.
type QueryResult struct {
	Text           string
	ConversationID string
	DurationMs     int64
	Usage          Usage
	Success        bool
}

// resultPayload mirrors agy's --output-format json result object, e.g.:
//
//	{"conversation_id":"...","status":"SUCCESS","response":"...",
//	 "duration_seconds":1.04,"num_turns":1,
//	 "usage":{"input_tokens":13888,"output_tokens":2,"thinking_tokens":0,
//	          "cache_read_tokens":0,"total_tokens":13890}}
//
// On error, status is "ERROR", response is empty, and error carries the
// message (see agy --output-format json --print "" for a live example).
type resultPayload struct {
	ConversationID string       `json:"conversation_id"`
	Status         string       `json:"status"`
	Response       string       `json:"response"`
	Error          string       `json:"error"`
	Usage          usagePayload `json:"usage"`
}

type usagePayload struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

func (u usagePayload) toUsage() Usage {
	return Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		ThinkingTokens:  u.ThinkingTokens,
		CacheReadTokens: u.CacheReadTokens,
		TotalTokens:     u.TotalTokens,
	}
}

// parseResultPayload parses agy's --output-format json stdout blob. An empty
// (whitespace-only) raw string returns the zero payload with no error — the
// caller treats that as "the process produced no result", not a parse
// failure, since a nonzero process error already explains it.
func parseResultPayload(raw string) (resultPayload, error) {
	raw = strings.TrimSpace(raw)
	var payload resultPayload
	if raw == "" {
		return payload, nil
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
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

	payload, parseErr := parseResultPayload(string(out))

	if payload.Response != "" {
		s.emit(TextEvent{Text: strings.TrimRight(payload.Response, "\r\n")})
	}

	switch {
	case procErr != nil:
		s.emit(ErrorEvent{Error: procErr, Context: "process"})
		s.emit(TurnCompleteEvent{
			Error:          procErr,
			ConversationID: payload.ConversationID,
			DurationMs:     duration,
			Usage:          payload.Usage.toUsage(),
			Success:        false,
		})
	case parseErr != nil:
		s.emit(ErrorEvent{Error: parseErr, Context: "parse"})
		s.emit(TurnCompleteEvent{Error: parseErr, DurationMs: duration, Success: false})
	case payload.Status != "SUCCESS":
		turnErr := fmt.Errorf("agy: turn failed with status %q: %s", payload.Status, payload.Error)
		s.emit(ErrorEvent{Error: turnErr, Context: "agy"})
		s.emit(TurnCompleteEvent{
			Error:          turnErr,
			ConversationID: payload.ConversationID,
			DurationMs:     duration,
			Usage:          payload.Usage.toUsage(),
			Success:        false,
		})
	default:
		s.emit(TurnCompleteEvent{
			ConversationID: payload.ConversationID,
			DurationMs:     duration,
			Usage:          payload.Usage.toUsage(),
			Success:        true,
		})
	}
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
			result.Usage = e.Usage
			return &result, e.Error
		case ErrorEvent:
			return nil, e.Error
		}
	}
	return nil, ErrNotStarted
}
