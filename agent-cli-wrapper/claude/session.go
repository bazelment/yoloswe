// Package claude provides a Go SDK for interacting with the Claude CLI.
//
// Basic usage:
//
//	session := claude.NewSession(
//	    claude.WithModel("haiku"),
//	    claude.WithPermissionMode(claude.PermissionModeBypass),
//	)
//
//	if err := session.Start(ctx); err != nil {
//	    log.Fatal(err)
//	}
//	defer session.Stop()
//
//	result, err := session.Ask(ctx, "What is 2+2?")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Printf("Success: %v, Cost: $%.6f\n", result.Success, result.Usage.CostUSD)
package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// SessionInfo contains session metadata.
type SessionInfo struct {
	SessionID      string
	Model          string
	WorkDir        string
	PermissionMode PermissionMode
	Tools          []string
}

// Session manages interaction with the Claude CLI.
type Session struct {
	recorder          *sessionRecorder
	process           *processManager
	accumulator       *streamAccumulator
	turnManager       *turnManager
	permissionManager *permissionManager
	state             *sessionState
	events            chan Event
	info              *SessionInfo
	done              chan struct{}
	config            SessionConfig
	mu                sync.RWMutex
	started           bool
	stopping          bool
}

// NewSession creates a new Claude session with options.
func NewSession(opts ...SessionOption) *Session {
	config := defaultConfig()
	for _, opt := range opts {
		opt(&config)
	}

	s := &Session{
		config: config,
		events: make(chan Event, config.EventBufferSize),
		done:   make(chan struct{}),
	}

	s.turnManager = newTurnManager()
	s.state = newSessionState()
	s.accumulator = newStreamAccumulator(s)
	s.permissionManager = newPermissionManager(config.PermissionHandler)

	if config.RecordMessages {
		s.recorder = newSessionRecorder(config.RecordingDir)
	}

	return s
}

// Start spawns the CLI process and begins the session.
func (s *Session) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return ErrAlreadyStarted
	}

	s.process = newProcessManager(s.config)
	if err := s.process.Start(ctx); err != nil {
		return err
	}

	// Transition state
	if err := s.state.Transition(TransitionStarted); err != nil {
		s.process.Stop()
		return err
	}

	// Start message handling goroutine
	go s.readLoop(ctx)

	// Start stderr handling if configured
	if s.config.StderrHandler != nil {
		go s.stderrLoop()
	}

	s.started = true
	return nil
}

// Events returns a read-only channel for receiving events.
func (s *Session) Events() <-chan Event {
	return s.events
}

// SendMessage sends a user message and starts a new turn.
// Returns the turn number.
func (s *Session) SendMessage(ctx context.Context, content string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return 0, ErrNotStarted
	}

	if s.stopping {
		return 0, ErrStopping
	}

	turn := s.turnManager.StartTurn(content)

	// Record turn start
	if s.recorder != nil {
		s.recorder.StartTurn(turn.Number, content)
	}

	msg := protocol.UserMessageToSend{
		Type: "user",
		Message: protocol.UserMessageToSendInner{
			Role:    "user",
			Content: content,
		},
	}

	if err := s.process.WriteMessage(msg); err != nil {
		return 0, err
	}

	if s.recorder != nil {
		s.recorder.RecordSent(msg)
	}

	// Transition to processing state if we're ready
	// Ignore error if already processing (multiple messages in flight)
	_ = s.state.Transition(TransitionUserMessageSent)

	return turn.Number, nil
}

// SendToolResult sends a tool result for a specific tool use.
// This is used when the SDK handles a tool locally (like AskUserQuestion).
// The content is sent as a tool_result content block with the given tool_use_id.
func (s *Session) SendToolResult(ctx context.Context, toolUseID, content string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return 0, ErrNotStarted
	}

	if s.stopping {
		return 0, ErrStopping
	}

	turn := s.turnManager.StartTurn(content)

	// Record turn start
	if s.recorder != nil {
		s.recorder.StartTurn(turn.Number, content)
	}

	msg := protocol.UserMessageToSend{
		Type: "user",
		Message: protocol.UserMessageToSendInner{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
				},
			},
		},
	}

	if err := s.process.WriteMessage(msg); err != nil {
		return 0, err
	}

	if s.recorder != nil {
		s.recorder.RecordSent(msg)
	}

	// Transition to processing state if we're ready
	_ = s.state.Transition(TransitionUserMessageSent)

	return turn.Number, nil
}

// WaitForTurn blocks until the current turn completes.
// If no turn is in progress, it returns immediately with nil.
func (s *Session) WaitForTurn(ctx context.Context) (*TurnResult, error) {
	turnNumber := s.turnManager.CurrentTurnNumber()
	if turnNumber == 0 {
		return nil, nil
	}
	return s.turnManager.WaitForTurn(ctx, turnNumber)
}

// Ask sends a message and waits for turn completion (blocking).
func (s *Session) Ask(ctx context.Context, content string) (*TurnResult, error) {
	_, err := s.SendMessage(ctx, content)
	if err != nil {
		return nil, err
	}
	return s.WaitForTurn(ctx)
}

// AskWithTimeout is a convenience wrapper with timeout context.
func (s *Session) AskWithTimeout(content string, timeout time.Duration) (*TurnResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.Ask(ctx, content)
}

// SetPermissionMode changes the permission mode dynamically.
func (s *Session) SetPermissionMode(ctx context.Context, mode PermissionMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		// Not started - update config for spawn
		s.config.PermissionMode = mode
		return nil
	}

	// Send control request
	req := protocol.ControlRequestToSend{
		Type:      "control_request",
		RequestID: generateRequestID(),
		Request: protocol.SetPermissionModeRequestToSend{
			Subtype: "set_permission_mode",
			Mode:    string(mode),
		},
	}

	if s.recorder != nil {
		s.recorder.RecordSent(req)
	}

	return s.process.WriteMessage(req)
}

// Interrupt sends an interrupt control request.
func (s *Session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return ErrNotStarted
	}

	req := protocol.ControlRequestToSend{
		Type:      "control_request",
		RequestID: generateRequestID(),
		Request: protocol.InterruptRequestToSend{
			Subtype: "interrupt",
		},
	}

	if s.recorder != nil {
		s.recorder.RecordSent(req)
	}

	return s.process.WriteMessage(req)
}

// SetModel sends a set_model control request to switch the model mid-session.
func (s *Session) SetModel(ctx context.Context, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		// Not started - update config for spawn
		s.config.Model = model
		return nil
	}

	// Send control request
	req := protocol.ControlRequestToSend{
		Type:      "control_request",
		RequestID: generateRequestID(),
		Request: protocol.SetModelRequestToSend{
			Subtype: "set_model",
			Model:   model,
		},
	}

	if s.recorder != nil {
		s.recorder.RecordSent(req)
	}

	return s.process.WriteMessage(req)
}

// Stop gracefully shuts down the session.
func (s *Session) Stop() error {
	s.mu.Lock()
	if !s.started || s.stopping {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	s.mu.Unlock()

	// Close done channel to signal goroutines
	close(s.done)

	// Stop the process
	if s.process != nil {
		s.process.Stop()
	}

	// Transition to closed state
	_ = s.state.Transition(TransitionClosed)

	// Close event channel after process stops
	close(s.events)

	return nil
}

// Info returns session information (available after Ready event).
func (s *Session) Info() *SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// CurrentTurnNumber returns the current turn number.
func (s *Session) CurrentTurnNumber() int {
	return s.turnManager.CurrentTurnNumber()
}

// State returns the current session state.
func (s *Session) State() SessionState {
	return s.state.Current()
}

// Recording returns the session recording (if enabled).
func (s *Session) Recording() *SessionRecording {
	if s.recorder == nil {
		return nil
	}
	return s.recorder.GetRecording()
}

// RecordingPath returns the path to recordings (if enabled).
func (s *Session) RecordingPath() string {
	if s.recorder == nil {
		return ""
	}
	return s.recorder.Path()
}

// CLIArgs returns the CLI arguments that will be (or were) used to spawn the CLI.
// This can be called before or after Start() to see the exact flags being used.
func (s *Session) CLIArgs() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Create a temporary process manager to build the args
	pm := newProcessManager(s.config)
	return pm.BuildCLIArgs()
}

// readLoop reads and processes messages from the CLI.
func (s *Session) readLoop(ctx context.Context) {
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
				if !s.stopping {
					s.emitError(err, "read_line")
				}
				return
			}

			s.handleLine(line)
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

// handleLine processes a single JSON line.
func (s *Session) handleLine(line []byte) {
	msg, err := protocol.ParseMessage(line)
	if err != nil {
		s.emitError(&ProtocolError{
			Message: "failed to parse message",
			Line:    string(line),
			Cause:   err,
		}, "parse_message")
		return
	}

	// Record received message
	if s.recorder != nil {
		s.recorder.RecordReceived(msg)
	}

	switch m := msg.(type) {
	case protocol.SystemMessage:
		s.handleSystem(m)
	case protocol.StreamEvent:
		s.accumulator.HandleEvent(m)
	case protocol.AssistantMessage:
		s.handleAssistant(m)
	case protocol.UserMessage:
		s.handleUser(m)
	case protocol.ResultMessage:
		s.handleResult(m)
	case protocol.ControlRequest:
		s.handleControlRequest(m)
	}
}

func (s *Session) handleSystem(msg protocol.SystemMessage) {
	if msg.Subtype == "init" {
		s.mu.Lock()
		s.info = &SessionInfo{
			SessionID:      msg.SessionID,
			Model:          msg.Model,
			WorkDir:        msg.CWD,
			Tools:          msg.Tools,
			PermissionMode: PermissionMode(msg.PermissionMode),
		}
		s.mu.Unlock()

		// Initialize recorder with session info
		if s.recorder != nil {
			s.recorder.Initialize(RecordingMetadata{
				SessionID:         msg.SessionID,
				Model:             msg.Model,
				WorkDir:           msg.CWD,
				Tools:             msg.Tools,
				ClaudeCodeVersion: msg.ClaudeCodeVersion,
				PermissionMode:    msg.PermissionMode,
			})
		}

		// Transition to ready state
		_ = s.state.Transition(TransitionInitReceived)

		// Emit ready event
		s.emit(ReadyEvent{Info: *s.info})
	}
}

func (s *Session) handleAssistant(msg protocol.AssistantMessage) {
	// Get content blocks (if available)
	blocks, ok := msg.Message.Content.AsBlocks()
	if !ok {
		return
	}

	// Extract text from complete message
	for _, block := range blocks {
		if textBlock, ok := block.(protocol.TextBlock); ok {
			turn := s.turnManager.CurrentTurn()
			if turn != nil {
				// Check if we have new text not emitted via streaming
				if len(textBlock.Text) > len(turn.FullText) {
					newText := textBlock.Text[len(turn.FullText):]
					fullText := s.turnManager.AppendText(newText)
					s.emit(TextEvent{
						TurnNumber: s.turnManager.CurrentTurnNumber(),
						Text:       newText,
						FullText:   fullText,
					})
				}
			}
		}
	}

	// Handle tools that weren't seen during streaming
	for _, block := range blocks {
		if toolBlock, ok := block.(protocol.ToolUseBlock); ok {
			tool := s.turnManager.GetTool(toolBlock.ID)
			if tool == nil {
				// Tool not seen during streaming, emit events now
				s.turnManager.GetOrCreateTool(toolBlock.ID, toolBlock.Name)

				s.emit(ToolStartEvent{
					TurnNumber: s.turnManager.CurrentTurnNumber(),
					ID:         toolBlock.ID,
					Name:       toolBlock.Name,
					Timestamp:  time.Now(),
				})

				s.emit(ToolCompleteEvent{
					TurnNumber: s.turnManager.CurrentTurnNumber(),
					ID:         toolBlock.ID,
					Name:       toolBlock.Name,
					Input:      toolBlock.Input,
					Timestamp:  time.Now(),
				})
			} else if tool.Input == nil {
				// Tool was seen but input wasn't set
				tool.Input = toolBlock.Input
			}
		}
	}
}

func (s *Session) handleUser(msg protocol.UserMessage) {
	// Get content blocks (if available)
	blocks, ok := msg.Message.Content.AsBlocks()
	if !ok {
		return
	}

	// Process tool_result blocks from CLI (CLI auto-executed tools)
	for _, block := range blocks {
		if resultBlock, ok := block.(protocol.ToolResultBlock); ok {
			// Find tool name
			toolName := "unknown"
			tool := s.turnManager.FindToolByID(resultBlock.ToolUseID)
			if tool != nil {
				toolName = tool.Name
			}

			isError := false
			if resultBlock.IsError != nil {
				isError = *resultBlock.IsError
			}

			s.emit(CLIToolResultEvent{
				TurnNumber: s.turnManager.CurrentTurnNumber(),
				ToolUseID:  resultBlock.ToolUseID,
				ToolName:   toolName,
				Content:    resultBlock.Content,
				IsError:    isError,
			})
		}
	}
}

func (s *Session) handleResult(msg protocol.ResultMessage) {
	turnNumber := s.turnManager.CurrentTurnNumber()
	turn := s.turnManager.CurrentTurn()

	durationMs := msg.DurationMs
	if turn != nil && durationMs == 0 {
		durationMs = time.Since(turn.StartTime).Milliseconds()
	}

	result := TurnResult{
		TurnNumber: turnNumber,
		Success:    !msg.IsError,
		DurationMs: durationMs,
		Usage: TurnUsage{
			InputTokens:     msg.Usage.InputTokens,
			OutputTokens:    msg.Usage.OutputTokens,
			CacheReadTokens: msg.Usage.CacheReadInputTokens,
			CostUSD:         msg.TotalCostUSD,
		},
	}

	// Populate text fields from turn state
	if turn != nil {
		result.Text = turn.FullText
		result.Thinking = turn.FullThinking
	}

	if msg.IsError {
		result.Error = fmt.Errorf("%s", msg.Result)
	}

	// Record turn completion
	if s.recorder != nil {
		s.recorder.CompleteTurn(turnNumber, result)
	}

	// Transition back to ready state
	_ = s.state.Transition(TransitionResultReceived)

	// Emit turn complete event
	s.emit(TurnCompleteEvent{
		TurnNumber: result.TurnNumber,
		Success:    result.Success,
		DurationMs: result.DurationMs,
		Usage:      result.Usage,
		Error:      result.Error,
	})

	// Complete turn (notifies waiters)
	s.turnManager.CompleteTurn(result)
}

func (s *Session) handleControlRequest(msg protocol.ControlRequest) {
	ctx := context.Background()

	// Parse tool use request from control message
	toolReq := protocol.ParseToolUseRequest(msg)

	// Check if this is an interactive tool with a dedicated handler
	if toolReq != nil && s.config.InteractiveToolHandler != nil {
		var resp *protocol.ControlResponse
		var err error

		switch toolReq.ToolName {
		case "AskUserQuestion":
			resp, err = s.handleAskUserQuestionControl(ctx, msg.RequestID, toolReq)
		case "ExitPlanMode":
			resp, err = s.handleExitPlanModeControl(ctx, msg.RequestID, toolReq)
		}

		if err != nil {
			s.emitError(err, "interactive_tool_handling")
		}

		if resp != nil {
			if err := s.process.WriteMessage(resp); err != nil {
				s.emitError(err, "send_control_response")
			}
			if s.recorder != nil {
				s.recorder.RecordSent(resp)
			}
			return
		}
	}

	// Delegate to permission manager for non-interactive tools
	resp, err := s.permissionManager.HandleRequest(ctx, msg)
	if err != nil {
		s.emitError(err, "permission_handling")
	}

	if resp != nil {
		if err := s.process.WriteMessage(resp); err != nil {
			s.emitError(err, "send_permission_response")
		}

		if s.recorder != nil {
			s.recorder.RecordSent(resp)
		}
	}
}

func (s *Session) handleAskUserQuestionControl(ctx context.Context, requestID string, toolReq *protocol.ToolUseRequest) (*protocol.ControlResponse, error) {
	questions, err := ParseQuestionsFromInput(toolReq.Input)
	if err != nil {
		return buildDenyResponse(requestID, err.Error(), false), nil
	}

	answers, err := s.config.InteractiveToolHandler.HandleAskUserQuestion(ctx, questions)
	if err != nil {
		return buildDenyResponse(requestID, err.Error(), false), nil
	}

	// Embed answers in updatedInput for CLI to use
	return buildAllowResponseWithAnswers(requestID, toolReq.Input, answers), nil
}

func (s *Session) handleExitPlanModeControl(ctx context.Context, requestID string, toolReq *protocol.ToolUseRequest) (*protocol.ControlResponse, error) {
	planInfo, err := ParsePlanInfoFromInput(toolReq.Input)
	if err != nil {
		return buildDenyResponse(requestID, err.Error(), false), nil
	}

	feedback, err := s.config.InteractiveToolHandler.HandleExitPlanMode(ctx, planInfo)
	if err != nil {
		return buildDenyResponse(requestID, err.Error(), false), nil
	}

	// Embed feedback in updatedInput for CLI to use
	return buildAllowResponseWithFeedback(requestID, toolReq.Input, feedback), nil
}

// buildDenyResponse builds a deny control response.
func buildDenyResponse(requestID, message string, interrupt bool) *protocol.ControlResponse {
	return &protocol.ControlResponse{
		Type: protocol.MessageTypeControlResponse,
		Response: protocol.ControlResponsePayload{
			Subtype:   "success",
			RequestID: requestID,
			Response: protocol.PermissionResultDeny{
				Behavior:  protocol.PermissionBehaviorDeny,
				Message:   message,
				Interrupt: interrupt,
			},
		},
	}
}

// buildAllowResponseWithAnswers builds an allow response with answers embedded in updatedInput.
func buildAllowResponseWithAnswers(requestID string, originalInput map[string]interface{}, answers map[string]string) *protocol.ControlResponse {
	updatedInput := make(map[string]interface{})
	for k, v := range originalInput {
		updatedInput[k] = v
	}
	updatedInput["answers"] = answers

	return &protocol.ControlResponse{
		Type: protocol.MessageTypeControlResponse,
		Response: protocol.ControlResponsePayload{
			Subtype:   "success",
			RequestID: requestID,
			Response: protocol.PermissionResultAllow{
				Behavior:     protocol.PermissionBehaviorAllow,
				UpdatedInput: updatedInput,
			},
		},
	}
}

// buildAllowResponseWithFeedback builds an allow response with feedback embedded in updatedInput.
func buildAllowResponseWithFeedback(requestID string, originalInput map[string]interface{}, feedback string) *protocol.ControlResponse {
	updatedInput := make(map[string]interface{})
	for k, v := range originalInput {
		updatedInput[k] = v
	}
	updatedInput["feedback"] = feedback

	return &protocol.ControlResponse{
		Type: protocol.MessageTypeControlResponse,
		Response: protocol.ControlResponsePayload{
			Subtype:   "success",
			RequestID: requestID,
			Response: protocol.PermissionResultAllow{
				Behavior:     protocol.PermissionBehaviorAllow,
				UpdatedInput: updatedInput,
			},
		},
	}
}

// emit sends an event to the events channel.
// Safe to call during/after Stop() - events are dropped if session is stopping.
func (s *Session) emit(event Event) {
	// Check if session is stopping before attempting to send.
	// This prevents "send on closed channel" panic when Stop() closes s.events.
	select {
	case <-s.done:
		// Session is stopping, drop event
		return
	default:
	}

	select {
	case s.events <- event:
	case <-s.done:
		// Session stopped while waiting to send, drop event
	default:
		// Channel full, drop event
		// In production, might want to log this
	}
}

// emitError emits an error event.
func (s *Session) emitError(err error, context string) {
	s.emit(ErrorEvent{
		TurnNumber: s.turnManager.CurrentTurnNumber(),
		Error:      err,
		Context:    context,
	})
}

// generateRequestID generates a unique request ID.
func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("req_%d_%s", time.Now().UnixNano(), hex.EncodeToString(b))
}
