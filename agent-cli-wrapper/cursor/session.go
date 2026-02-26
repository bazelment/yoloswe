package cursor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// SessionInfo contains session metadata from the system init message.
type SessionInfo struct {
	SessionID string
	Model     string
	CWD       string
}

// QueryResult contains the result of a one-shot query.
type QueryResult struct {
	SessionID  string
	Text       string
	DurationMs int64
	Success    bool
}

// Session manages a one-shot interaction with the Cursor Agent CLI.
type Session struct {
	events  chan Event
	process *processManager
	info    *SessionInfo
	done    chan struct{}
	config  SessionConfig
	prompt  string
	mu      sync.RWMutex
	started bool
	stopped bool
}

// NewSession creates a new Cursor session with the given prompt and options.
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

// Start spawns the CLI process and begins reading events.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()

	if s.started {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}

	s.process = newProcessManager(s.prompt, s.config)
	if err := s.process.Start(ctx); err != nil {
		s.mu.Unlock()
		return err
	}

	go s.readLoop(ctx)

	if s.config.StderrHandler != nil {
		go s.stderrLoop()
	}

	s.started = true
	s.mu.Unlock()

	return nil
}

// Events returns a read-only channel for receiving events.
func (s *Session) Events() <-chan Event {
	return s.events
}

// Info returns session information (available after ReadyEvent).
func (s *Session) Info() *SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// Stop gracefully shuts down the session.
func (s *Session) Stop() error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()

	close(s.done)

	if s.process != nil {
		s.process.Stop()
	}

	close(s.events)
	return nil
}

// readLoop reads NDJSON lines from the CLI and dispatches events.
func (s *Session) readLoop(ctx context.Context) {
	var textBuilder strings.Builder

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		default:
			line, err := s.process.ReadLine()
			if err != nil {
				if err == io.EOF {
					return
				}
				if !s.isStopped() {
					s.emit(ErrorEvent{
						Error:   err,
						Context: "read_line",
					})
				}
				return
			}

			s.handleLine(line, &textBuilder)
		}
	}
}

// stderrLoop reads and handles stderr from the CLI.
func (s *Session) stderrLoop() {
	stderr := s.process.Stderr()
	if stderr == nil {
		return
	}

	buf := make([]byte, 4096)
	for {
		select {
		case <-s.done:
			return
		default:
			n, err := stderr.Read(buf)
			if err != nil {
				return
			}
			if n > 0 && s.config.StderrHandler != nil {
				s.config.StderrHandler(buf[:n])
			}
		}
	}
}

// handleLine processes a single NDJSON line.
func (s *Session) handleLine(line []byte, textBuilder *strings.Builder) {
	msg, err := ParseMessage(line)
	if err != nil {
		s.emit(ErrorEvent{
			Error: &ProtocolError{
				Message: "failed to parse message",
				Line:    string(line),
				Cause:   err,
			},
			Context: "parse_message",
		})
		return
	}

	switch m := msg.(type) {
	case *SystemInitMessage:
		s.handleSystemInit(m)
	case *AssistantMessage:
		s.handleAssistant(m, textBuilder)
	case *ToolCallMessage:
		s.handleToolCall(m)
	case *ResultMessage:
		s.handleResult(m)
	}
}

func (s *Session) handleSystemInit(msg *SystemInitMessage) {
	s.mu.Lock()
	s.info = &SessionInfo{
		SessionID: msg.SessionID,
		Model:     msg.Model,
		CWD:       msg.CWD,
	}
	s.mu.Unlock()

	s.emit(ReadyEvent{
		SessionID: msg.SessionID,
		Model:     msg.Model,
	})
}

func (s *Session) handleAssistant(msg *AssistantMessage, textBuilder *strings.Builder) {
	for _, block := range msg.Message.Content {
		if block.Type == "text" && block.Text != "" {
			textBuilder.WriteString(block.Text)
			s.emit(TextEvent{
				Text:     block.Text,
				FullText: textBuilder.String(),
			})
		}
	}
}

func (s *Session) handleToolCall(msg *ToolCallMessage) {
	detail, err := ParseToolCallDetail(msg)
	if err != nil {
		s.emit(ErrorEvent{
			Error:   &ProtocolError{Message: "failed to parse tool call detail", Cause: err},
			Context: "parse_tool_call",
		})
		return
	}

	switch msg.Subtype {
	case "started":
		s.emit(ToolStartEvent{
			ID:    msg.CallID,
			Name:  detail.Name,
			Input: detail.Args,
		})
	case "completed":
		s.emit(ToolCompleteEvent{
			ID:      msg.CallID,
			Name:    detail.Name,
			Input:   detail.Args,
			Result:  detail.Result,
			IsError: false,
		})
	}
}

func (s *Session) handleResult(msg *ResultMessage) {
	var resultErr error
	if msg.IsError {
		resultErr = fmt.Errorf("%s", msg.Result)
	}

	s.emit(TurnCompleteEvent{
		Success:       !msg.IsError,
		DurationMs:    msg.DurationMs,
		DurationAPIMs: msg.DurationAPIMs,
		Error:         resultErr,
	})
}

// emit sends an event to the events channel.
func (s *Session) emit(event Event) {
	select {
	case <-s.done:
		return
	default:
	}

	select {
	case s.events <- event:
	case <-s.done:
	default:
		// Channel full, drop event
	}
}

// isStopped returns whether the session has been stopped.
func (s *Session) isStopped() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stopped
}
