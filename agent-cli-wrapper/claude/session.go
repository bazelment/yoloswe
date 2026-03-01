package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	ctx                     context.Context
	events                  chan Event
	recorder                *sessionRecorder
	accumulator             *streamAccumulator
	turnManager             *turnManager
	permissionManager       *permissionManager
	state                   *sessionState
	process                 *processManager
	info                    *SessionInfo
	done                    chan struct{}
	pendingControlResponses map[string]chan protocol.ControlResponsePayload
	cancel                  context.CancelFunc
	config                  SessionConfig
	cumulativeCostUSD       float64
	mu                      sync.RWMutex
	pendingMu               sync.Mutex
	started                 bool
	stopping                bool
}

// NewSession creates a new Claude session with options.
func NewSession(opts ...SessionOption) *Session {
	config := defaultConfig()
	for _, opt := range opts {
		opt(&config)
	}

	s := &Session{
		config:                  config,
		events:                  make(chan Event, config.EventBufferSize),
		done:                    make(chan struct{}),
		pendingControlResponses: make(map[string]chan protocol.ControlResponsePayload),
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

	if s.started {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}

	// Create a context for tool handler goroutines, cancelled on Stop().
	s.ctx, s.cancel = context.WithCancel(context.Background())

	s.process = newProcessManager(s.config)
	if err := s.process.Start(ctx); err != nil {
		s.cancel()
		s.mu.Unlock()
		return err
	}

	// Transition state
	if err := s.state.Transition(TransitionStarted); err != nil {
		s.process.Stop()
		s.mu.Unlock()
		return err
	}

	// Start message handling goroutine
	go s.readLoop(ctx)

	// Start stderr handling if configured
	if s.config.StderrHandler != nil {
		go s.stderrLoop()
	}

	s.started = true
	s.mu.Unlock()

	// Send SDK initialize handshake (matching Python SDK behavior).
	// This MUST happen:
	//   1. After readLoop is started — so we can receive the CLI's response.
	//   2. After s.mu is released — because during the initialize handshake,
	//      the CLI interleaves MCP setup requests (initialize, notifications/initialized,
	//      tools/list) as control_request messages. The readLoop calls handleControlRequest
	//      which calls handleMCPMessage, and if s.mu were held, it would deadlock.
	//
	// Protocol flow during initialization:
	//   SDK → CLI: control_request {subtype: "initialize"}
	//   CLI → SDK: control_request {mcp_message: {method: "initialize"}}        (for each SDK server)
	//   SDK → CLI: control_response {mcp_response: {result: initializeResult}}
	//   CLI → SDK: control_request {mcp_message: {method: "notifications/initialized"}}
	//   SDK → CLI: control_response {mcp_response: {result: {}}}
	//   CLI → SDK: control_request {mcp_message: {method: "tools/list"}}
	//   SDK → CLI: control_response {mcp_response: {result: toolsListResult}}
	//   CLI → SDK: control_response {request_id: <init_req_id>}                 (initialize done)
	//   CLI → SDK: system {subtype: "init", session_id: ..., tools: [...]}
	if err := s.sendInitialize(ctx); err != nil {
		s.Stop()
		return fmt.Errorf("SDK initialize handshake failed: %w", err)
	}

	return nil
}

// sendInitialize sends the SDK initialize control request and waits for the response.
// This is required by the Claude CLI control protocol before any user messages.
func (s *Session) sendInitialize(ctx context.Context) error {
	initReq := map[string]interface{}{
		"subtype": "initialize",
	}

	_, err := s.sendControlRequestLocked(ctx, initReq, 60*time.Second)
	return err
}

// sendControlRequestLocked sends a control request and waits for the response.
// Despite the name, this does NOT require s.mu to be held — it uses its own
// pendingMu for synchronizing the response channel map.
func (s *Session) sendControlRequestLocked(ctx context.Context, request interface{}, timeout time.Duration) (protocol.ControlResponsePayload, error) {
	requestID := generateRequestID()

	// Register pending response channel
	ch := make(chan protocol.ControlResponsePayload, 1)
	s.pendingMu.Lock()
	s.pendingControlResponses[requestID] = ch
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pendingControlResponses, requestID)
		s.pendingMu.Unlock()
	}()

	// Build and send the control request
	msg := protocol.ControlRequestToSend{
		Type:      "control_request",
		RequestID: requestID,
		Request:   request,
	}

	if err := s.process.WriteMessage(msg); err != nil {
		return protocol.ControlResponsePayload{}, err
	}

	if s.recorder != nil {
		s.recorder.RecordSent(msg)
	}

	// Wait for response with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case resp := <-ch:
		if resp.Subtype == "error" {
			return resp, fmt.Errorf("control request error: %s", resp.Error)
		}
		return resp, nil
	case <-timeoutCtx.Done():
		return protocol.ControlResponsePayload{}, fmt.Errorf("control request timed out")
	case <-s.done:
		return protocol.ControlResponsePayload{}, fmt.Errorf("session stopped")
	}
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

	msg := protocol.NewUserTextMessage(content)

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

// CollectResponse loops on Events() until a TurnCompleteEvent, returning
// the accumulated TurnResult and all events received during the turn.
func (s *Session) CollectResponse(ctx context.Context) (*TurnResult, []Event, error) {
	var events []Event
	for {
		select {
		case <-ctx.Done():
			return nil, events, ctx.Err()
		case evt, ok := <-s.events:
			if !ok {
				return nil, events, ErrSessionClosed
			}
			events = append(events, evt)
			if tc, ok := evt.(TurnCompleteEvent); ok {
				result := &TurnResult{
					TurnNumber: tc.TurnNumber,
					Success:    tc.Success,
					DurationMs: tc.DurationMs,
					Usage:      tc.Usage,
					Error:      tc.Error,
				}
				// Populate text/blocks from turn state
				turn := s.turnManager.GetTurnByNumber(tc.TurnNumber)
				if turn != nil {
					result.Text = turn.FullText
					result.Thinking = turn.FullThinking
					result.ContentBlocks = turn.ContentBlocks
				}
				return result, events, nil
			}
		}
	}
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
	req := protocol.NewSetPermissionMode(generateRequestID(), string(mode))

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

	req := protocol.NewInterrupt(generateRequestID())

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
	req := protocol.NewSetModel(generateRequestID(), model)

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

	// Cancel context for tool handler goroutines
	if s.cancel != nil {
		s.cancel()
	}

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
	// Record raw line before parsing — preserves unknown message types.
	if s.recorder != nil {
		s.recorder.RecordReceived(line)
	}

	msg, err := protocol.ParseMessage(line)
	if err != nil {
		s.emitError(&ProtocolError{
			Message: "failed to parse message",
			Line:    string(line),
			Cause:   err,
		}, "parse_message")
		return
	}

	if msg == nil {
		return // unknown type — already recorded above
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
	case protocol.ControlResponse:
		s.handleControlResponse(m)
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

	// Accumulate structured content blocks
	for _, block := range blocks {
		switch b := block.(type) {
		case protocol.TextBlock:
			s.turnManager.AppendContentBlock(ContentBlock{
				Type: ContentBlockTypeText,
				Text: b.Text,
			})
		case protocol.ThinkingBlock:
			s.turnManager.AppendContentBlock(ContentBlock{
				Type:     ContentBlockTypeThinking,
				Thinking: b.Thinking,
			})
		case protocol.ToolUseBlock:
			s.turnManager.AppendContentBlock(ContentBlock{
				Type:      ContentBlockTypeToolUse,
				ToolUseID: b.ID,
				ToolName:  b.Name,
				ToolInput: b.Input,
			})
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

	// Accumulate tool result content blocks
	for _, block := range blocks {
		if resultBlock, ok := block.(protocol.ToolResultBlock); ok {
			isError := false
			if resultBlock.IsError != nil {
				isError = *resultBlock.IsError
			}
			s.turnManager.AppendContentBlock(ContentBlock{
				Type:       ContentBlockTypeToolResult,
				ToolUseID:  resultBlock.ToolUseID,
				ToolResult: resultBlock.Content,
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

	// Populate text fields and content blocks from turn state
	if turn != nil {
		result.Text = turn.FullText
		result.Thinking = turn.FullThinking
		result.ContentBlocks = turn.ContentBlocks
	}

	if msg.IsError {
		result.Error = fmt.Errorf("%s", msg.Result)
	}

	// Update cumulative cost and check SDK-level limits.
	// SDK limit errors are only set if there is no existing error from the CLI,
	// to avoid silently overwriting the real error.
	s.mu.Lock()
	s.cumulativeCostUSD += msg.TotalCostUSD
	totalCost := s.cumulativeCostUSD
	s.mu.Unlock()

	if result.Error == nil {
		if s.config.MaxTurns > 0 && turnNumber >= s.config.MaxTurns {
			result.Error = ErrMaxTurnsExceeded
		} else if s.config.MaxBudgetUSD > 0 && totalCost >= s.config.MaxBudgetUSD {
			result.Error = ErrBudgetExceeded
		}
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

// handleControlResponse routes incoming control_response messages to the goroutine
// that sent the corresponding control_request (matched by request_id).
// This is used during the SDK initialize handshake — the CLI sends back a
// control_response after completing MCP server setup.
func (s *Session) handleControlResponse(msg protocol.ControlResponse) {
	requestID := msg.Response.RequestID
	s.pendingMu.Lock()
	ch, ok := s.pendingControlResponses[requestID]
	s.pendingMu.Unlock()

	if ok {
		// Send response to waiting goroutine (non-blocking since channel is buffered)
		select {
		case ch <- msg.Response:
		default:
		}
	}
}

func (s *Session) handleControlRequest(msg protocol.ControlRequest) {
	ctx := context.Background()

	// First check if this is an MCP message (SDK MCP server traffic)
	reqData, parseErr := protocol.ParseControlRequest(msg.Request)
	if parseErr == nil {
		if mcpReq, ok := reqData.(protocol.MCPMessageRequest); ok {
			s.handleMCPMessage(msg.RequestID, mcpReq)
			return
		}
	}

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
	resp := protocol.NewPermissionDeny(requestID, message, interrupt)
	return &resp
}

// buildAllowResponseWithAnswers builds an allow response with answers embedded in updatedInput.
func buildAllowResponseWithAnswers(requestID string, originalInput map[string]interface{}, answers map[string]string) *protocol.ControlResponse {
	updatedInput := make(map[string]interface{})
	for k, v := range originalInput {
		updatedInput[k] = v
	}
	updatedInput["answers"] = answers
	resp := protocol.NewPermissionAllow(requestID, updatedInput, nil)
	return &resp
}

// buildAllowResponseWithFeedback builds an allow response with feedback embedded in updatedInput.
func buildAllowResponseWithFeedback(requestID string, originalInput map[string]interface{}, feedback string) *protocol.ControlResponse {
	updatedInput := make(map[string]interface{})
	for k, v := range originalInput {
		updatedInput[k] = v
	}
	updatedInput["feedback"] = feedback
	resp := protocol.NewPermissionAllow(requestID, updatedInput, nil)
	return &resp
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

// handleMCPMessage dispatches an MCP JSON-RPC message to the appropriate SDK handler.
// The CLI wraps MCP JSON-RPC requests in control_request messages with a special
// "mcp_message" subtype, routing them to the SDK server identified by server_name.
//
// Expected methods during session lifecycle:
//   - "initialize": Sent once per server during CLI startup. Must respond with protocol version and capabilities.
//   - "notifications/initialized": Acknowledgement after initialize. Respond with empty object.
//   - "tools/list": Sent after initialization. Must respond with the list of available tools.
//   - "tools/call": Sent when Claude wants to invoke a tool. Handled async since tools may block.
func (s *Session) handleMCPMessage(requestID string, mcpReq protocol.MCPMessageRequest) {
	// Look up SDK handler
	var handler SDKToolHandler
	if s.config.MCPConfig != nil {
		handlers := s.config.MCPConfig.SDKHandlers()
		if handlers != nil {
			handler = handlers[mcpReq.ServerName]
		}
	}
	if handler == nil {
		s.sendMCPErrorResponse(requestID, nil, -32603, fmt.Sprintf("no SDK handler for server %q", mcpReq.ServerName))
		return
	}

	// Parse the JSON-RPC request from the message
	var rpcReq protocol.JSONRPCRequest
	if err := json.Unmarshal(mcpReq.Message, &rpcReq); err != nil {
		s.sendMCPErrorResponse(requestID, nil, -32700, "failed to parse JSON-RPC request")
		return
	}

	switch rpcReq.Method {
	case "initialize":
		result := buildInitializeResult(mcpReq.ServerName)
		s.sendMCPResponse(requestID, rpcReq.ID, result)

	case "notifications/initialized":
		// Notification acknowledgement — send empty success response
		s.sendMCPResponse(requestID, rpcReq.ID, map[string]interface{}{})

	case "tools/list":
		result := buildToolsListResult(handler)
		s.sendMCPResponse(requestID, rpcReq.ID, result)

	case "tools/call":
		// tools/call MUST be handled async — tool handlers can block for minutes.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.sendMCPErrorResponse(requestID, rpcReq.ID, -32603,
						fmt.Sprintf("tool handler panic: %v", r))
					s.emitError(fmt.Errorf("SDK tool handler panic: %v", r), "mcp_tool_call")
				}
			}()

			var params protocol.MCPToolsCallParams
			if err := json.Unmarshal(rpcReq.Params, &params); err != nil {
				s.sendMCPErrorResponse(requestID, rpcReq.ID, -32602, "invalid tools/call params")
				return
			}

			result, err := handler.HandleToolCall(s.ctx, params.Name, params.Arguments)
			if err != nil {
				// Return error as tool result (not JSON-RPC error) so Claude sees it
				result = &protocol.MCPToolCallResult{
					Content: []protocol.MCPContentItem{
						{Type: "text", Text: fmt.Sprintf("Tool error: %v", err)},
					},
					IsError: true,
				}
			}

			s.sendMCPResponse(requestID, rpcReq.ID, result)
		}()

	default:
		s.sendMCPErrorResponse(requestID, rpcReq.ID, -32601, fmt.Sprintf("method not found: %s", rpcReq.Method))
	}
}

// sendMCPResponse sends a successful MCP control response.
func (s *Session) sendMCPResponse(requestID string, rpcID interface{}, result interface{}) {
	rpcResp := protocol.JSONRPCResponse{JSONRPC: "2.0", ID: rpcID, Result: result}
	resp := protocol.NewMCPResponse(requestID, rpcResp)
	if err := s.process.WriteMessage(&resp); err != nil {
		s.emitError(err, "send_mcp_response")
	}
	if s.recorder != nil {
		s.recorder.RecordSent(&resp)
	}
}

// sendMCPErrorResponse sends a JSON-RPC error as an MCP control response.
func (s *Session) sendMCPErrorResponse(requestID string, rpcID interface{}, code int, message string) {
	rpcResp := protocol.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      rpcID,
		Error:   &protocol.JSONRPCError{Code: code, Message: message},
	}
	resp := protocol.NewMCPResponse(requestID, rpcResp)
	if err := s.process.WriteMessage(&resp); err != nil {
		s.emitError(err, "send_mcp_error_response")
	}
	if s.recorder != nil {
		s.recorder.RecordSent(&resp)
	}
}

// generateRequestID generates a unique request ID.
func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("req_%d_%s", time.Now().UnixNano(), hex.EncodeToString(b))
}
