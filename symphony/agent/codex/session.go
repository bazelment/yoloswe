package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/bazelment/yoloswe/symphony/agent"
	"github.com/bazelment/yoloswe/symphony/model"
)

// Session manages a Codex app-server session lifecycle:
// handshake, multi-turn on the same thread, turn timeout.
// Implements agent.Agent.
type Session struct {
	process  *Process
	protocol *Protocol
	logger   *slog.Logger
	threadID string
	turnID   string
	config   agent.SessionConfig
}

// NewSession creates and starts a new Codex app-server session.
// Performs the full handshake: initialize → initialized → thread/start.
// Spec Section 10.2.
func NewSession(ctx context.Context, cfg agent.SessionConfig, logger *slog.Logger) (*Session, error) {
	proc, err := StartProcess(ctx, cfg.Command, cfg.WorkDir, logger)
	if err != nil {
		return nil, fmt.Errorf("codex_not_found: %w", err)
	}

	s := &Session{
		process:  proc,
		protocol: NewProtocol(proc, logger),
		config:   cfg,
		logger:   logger,
	}

	if err := s.handshake(ctx); err != nil {
		proc.Stop()
		return nil, err
	}

	return s, nil
}

// handshake performs the initialize → initialized → thread/start sequence.
func (s *Session) handshake(ctx context.Context) error {
	readTimeout := time.Duration(s.config.ReadTimeoutMs) * time.Millisecond

	// 1. Send initialize request and wait for response.
	initCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	type initResult struct {
		resp *Message
		err  error
	}
	ch := make(chan initResult, 1)
	go func() {
		resp, err := s.protocol.Send("initialize", map[string]any{
			"clientInfo":   map[string]any{"name": "symphony", "version": "1.0"},
			"capabilities": map[string]any{},
		})
		ch <- initResult{resp, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("response_timeout: initialize: %w", r.err)
		}
	case <-initCtx.Done():
		return fmt.Errorf("response_timeout: initialize response not received within %dms", s.config.ReadTimeoutMs)
	}

	// 2. Send initialized notification.
	if err := s.protocol.Notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}

	// 3. Send thread/start request.
	threadParams := map[string]any{
		"cwd": s.config.WorkDir,
	}
	if s.config.ApprovalPolicy != "" {
		threadParams["approvalPolicy"] = s.config.ApprovalPolicy
	}
	if s.config.ThreadSandbox != "" {
		threadParams["sandbox"] = s.config.ThreadSandbox
	}

	threadCtx, threadCancel := context.WithTimeout(ctx, readTimeout)
	defer threadCancel()

	threadCh := make(chan initResult, 1)
	go func() {
		resp, err := s.protocol.Send("thread/start", threadParams)
		threadCh <- initResult{resp, err}
	}()

	select {
	case r := <-threadCh:
		if r.err != nil {
			return fmt.Errorf("response_timeout: thread/start: %w", r.err)
		}
		// Extract thread ID from response.
		if r.resp != nil && r.resp.Result != nil {
			var result struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if err := json.Unmarshal(r.resp.Result, &result); err == nil && result.Thread.ID != "" {
				s.threadID = result.Thread.ID
			}
		}
	case <-threadCtx.Done():
		return fmt.Errorf("response_timeout: thread/start response not received within %dms", s.config.ReadTimeoutMs)
	}

	if s.threadID == "" {
		s.threadID = fmt.Sprintf("thread-%d", time.Now().UnixNano())
	}

	return nil
}

// RunTurn starts a turn and streams events until completion.
// Returns the turn result and any events collected.
// Spec Section 10.3.
func (s *Session) RunTurn(ctx context.Context, prompt string, onEvent func(agent.Event)) (agent.TurnResult, error) {
	turnParams := map[string]any{
		"threadId": s.threadID,
		"input":    []map[string]any{{"type": "text", "text": prompt}},
		"cwd":      s.config.WorkDir,
		"title":    fmt.Sprintf("%s: %s", s.config.IssueIdentifier, s.config.IssueTitle),
	}
	if s.config.ApprovalPolicy != "" {
		turnParams["approvalPolicy"] = s.config.ApprovalPolicy
	}
	if s.config.TurnSandboxPolicy != "" {
		turnParams["sandboxPolicy"] = map[string]any{"type": s.config.TurnSandboxPolicy}
	}

	// Send turn/start and get turn ID.
	resp, err := s.protocol.Send("turn/start", turnParams)
	if err != nil {
		return agent.TurnResult{Status: agent.TurnFailed, Error: err}, err
	}

	// Reset turnID before parsing so a missing ID in a later turn doesn't
	// silently reuse the previous turn's value.
	s.turnID = ""
	if resp != nil && resp.Result != nil {
		var result struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(resp.Result, &result); err == nil && result.Turn.ID != "" {
			s.turnID = result.Turn.ID
		}
	}
	if s.turnID == "" {
		s.turnID = fmt.Sprintf("turn-%d", time.Now().UnixNano())
	}

	sessionID := model.ComposeSessionID(s.threadID, s.turnID)
	s.logger.Info("turn started", "session_id", sessionID, "thread_id", s.threadID, "turn_id", s.turnID)

	if onEvent != nil {
		onEvent(agent.Event{Type: agent.EventSessionStarted, SessionID: sessionID, ThreadID: s.threadID, TurnID: s.turnID, PID: s.process.PID()})
	}

	// Stream events until turn completion.
	turnTimeout := time.Duration(s.config.TurnTimeoutMs) * time.Millisecond
	turnCtx, turnCancel := context.WithTimeout(ctx, turnTimeout)
	defer turnCancel()

	return s.streamTurn(turnCtx, onEvent)
}

// readResult holds a message or error from an async ReadMessage call.
type readResult struct {
	msg *Message
	err error
}

// streamTurn reads messages until turn completion.
// Uses a goroutine for ReadMessage so the context can preempt blocking reads.
func (s *Session) streamTurn(ctx context.Context, onEvent func(agent.Event)) (agent.TurnResult, error) {
	msgCh := make(chan readResult, 1)

	// Start async reader.
	readNext := func() {
		msg, err := s.protocol.ReadMessage()
		msgCh <- readResult{msg, err}
	}
	go readNext()

	for {
		select {
		case <-ctx.Done():
			// Distinguish between a context deadline (turn timeout) and an
			// explicit cancellation (e.g. reconcile-driven termination).
			if ctx.Err() == context.DeadlineExceeded {
				return agent.TurnResult{Status: agent.TurnTimedOut, Error: fmt.Errorf("turn_timeout")}, ctx.Err()
			}
			return agent.TurnResult{Status: agent.TurnCancelled, Error: fmt.Errorf("turn_cancelled: %w", ctx.Err())}, ctx.Err()
		case r := <-msgCh:
			if r.err != nil {
				return agent.TurnResult{Status: agent.TurnFailed, Error: fmt.Errorf("port_exit: %w", r.err)}, r.err
			}

			event := ExtractEvent(r.msg)
			if onEvent != nil && event.Type != "" {
				onEvent(event)
			}

			switch r.msg.Method {
			case "turn/completed":
				return agent.TurnResult{Status: agent.TurnCompleted}, nil
			case "turn/failed":
				return agent.TurnResult{Status: agent.TurnFailed, Error: fmt.Errorf("turn_failed")}, nil
			case "turn/cancelled":
				return agent.TurnResult{Status: agent.TurnCancelled, Error: fmt.Errorf("turn_cancelled")}, nil
			}

			// Handle approval requests and tool calls.
			s.handleInteraction(r.msg)

			// Start next read.
			go readNext()
		}
	}
}

// handleInteraction handles approval requests and tool calls.
// Returns true if the message was handled.
func (s *Session) handleInteraction(msg *Message) bool {
	if msg.ID == nil {
		return false
	}

	switch msg.Method {
	case "item/tool/requestApproval":
		HandleApproval(s.protocol, msg, s.logger)
		return true
	case "item/tool/call":
		HandleToolCall(s.protocol, msg, s.logger)
		return true
	case "item/tool/requestUserInput":
		// User input required = hard failure. Spec Section 10.5.
		s.logger.Warn("user input requested, failing run")
		s.protocol.RespondError(msg.ID, -1, "user input not supported in automated mode")
		return true
	}

	return false
}

// ThreadID returns the current thread ID.
func (s *Session) ThreadID() string { return s.threadID }

// SessionID returns the composed session ID.
func (s *Session) SessionID() string {
	return model.ComposeSessionID(s.threadID, s.turnID)
}

// PID returns the process ID of the codex subprocess.
func (s *Session) PID() *int { return s.process.PID() }

// Stop gracefully stops the session.
func (s *Session) Stop() error {
	return s.process.Stop()
}
