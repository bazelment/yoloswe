package agy

import (
	"context"
	"strings"
	"sync"
	"time"
)

// QueryResult contains the result of a one-shot agy query.
type QueryResult struct {
	Text       string
	DurationMs int64
	Success    bool
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
	out, _, err := s.process.Start(ctx)
	duration := time.Since(start).Milliseconds()
	text := strings.TrimRight(string(out), "\r\n")
	if text != "" {
		s.emit(TextEvent{Text: text})
	}
	if err != nil {
		s.emit(ErrorEvent{Error: err, Context: "process"})
		s.emit(TurnCompleteEvent{Error: err, DurationMs: duration, Success: false})
		return
	}
	s.emit(TurnCompleteEvent{DurationMs: duration, Success: true})
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
			return &result, e.Error
		case ErrorEvent:
			return nil, e.Error
		}
	}
	return nil, ErrNotStarted
}
