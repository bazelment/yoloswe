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

// defaultBgTaskSafetyTimeout is the maximum time to wait for a continuation
// ResultMessage after suppressing a turn due to background tasks. If no
// continuation arrives within this duration, the turn completes with
// accumulated data to prevent indefinite blocking.
const defaultBgTaskSafetyTimeout = 90 * time.Second

// bgSuppressionState groups all background-task turn-suppression state.
// When the CLI runs background tasks (run_in_background: true), the SDK
// suppresses the intermediate ResultMessage and waits for continuation
// task-notifications. This struct holds the state for that mechanism.
// Use reset() to clear all fields at turn boundaries.
type bgSuppressionState struct {
	timer            *time.Timer // fires if no continuation arrives; see completeSuppressedTurn
	accumulatedUsage TurnUsage   // token/cost totals from suppressed intermediate results
	active           bool        // true while waiting for continuation; cleared by timer or normal path
	timerFired       bool        // set by completeSuppressedTurn; cleared at start of next turn
}

// reset clears all suppression state and stops any pending timer.
func (b *bgSuppressionState) reset() {
	b.active = false
	b.timerFired = false
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.accumulatedUsage = TurnUsage{}
}

// SessionInfo contains session metadata.
type SessionInfo struct {
	SessionID      string
	Model          string
	WorkDir        string
	PermissionMode PermissionMode
	Tools          []string
}

// Session manages interaction with the Claude CLI.
//
// Field ordering follows the fieldalignment rule: pointer/interface fields
// first (minimises GC pointer bitmap), then value types, then small scalars.
type Session struct {
	// Pointer / interface / channel fields (all 8-16 bytes, contain pointers).
	ctx                     context.Context
	pendingControlResponses map[string]chan protocol.ControlResponsePayload
	accumulator             *streamAccumulator
	turnManager             *turnManager
	permissionManager       *permissionManager
	state                   *sessionState
	process                 *processManager
	info                    *SessionInfo
	done                    chan struct{}
	events                  chan Event
	recorder                *sessionRecorder
	cancel                  context.CancelFunc

	// Value / struct fields.
	config            SessionConfig
	bgState           bgSuppressionState // background-task turn-suppression state; protected by mu
	cumulativeCostUSD float64

	// Scalar and sync fields.
	mu        sync.RWMutex
	pendingMu sync.Mutex
	started   bool
	stopping  bool
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
	// Clear any stale background-task suppression state from the previous turn.
	// A safety timer for Turn N must not fire after Turn N+1 has started.
	s.bgState.reset()

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
	// Clear any stale background-task suppression state from the previous turn.
	s.bgState.reset()

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
	s.bgState.reset()
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
	// Check for task notifications (background task completed).
	// These arrive as string content, not blocks.
	if _, ok := msg.Message.Content.AsString(); ok {
		return // String content has no blocks to process
	}

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

// shouldSuppressForBgTasks inspects the current turn's ContentBlocks to decide
// whether the ResultMessage should be suppressed (waiting for background task
// continuations). Returns true only when ALL non-cancelled tool_use blocks in
// the turn had run_in_background: true — meaning the ResultMessage arrived
// because bg tools returned immediately, and the real work is still in progress.
//
// When non-bg tools are present, the ResultMessage represents the completion of
// synchronous work and must not be suppressed.
func (s *Session) shouldSuppressForBgTasks(turn *turnState) bool {
	if turn == nil {
		return false
	}

	// Build set of cancelled (errored) tool IDs from tool_result blocks.
	// Cancelled tools never launched a background task, so they should not
	// influence the suppression decision.
	cancelled := make(map[string]bool)
	for _, block := range turn.ContentBlocks {
		if block.Type == ContentBlockTypeToolResult && block.IsError {
			cancelled[block.ToolUseID] = true
		}
	}

	// Check tool_use blocks. Non-bg tools always prevent suppression (even if
	// they errored), because their presence means the ResultMessage represents
	// completion of synchronous work. Cancelled bg tools are skipped — they
	// never actually launched a background task.
	hasBgTool := false
	for _, block := range turn.ContentBlocks {
		if block.Type != ContentBlockTypeToolUse {
			continue
		}
		isBg, _ := block.ToolInput["run_in_background"].(bool)
		if !isBg {
			return false // non-bg tool exists → don't suppress
		}
		// It's a bg tool — only count it if not cancelled.
		if !cancelled[block.ToolUseID] {
			hasBgTool = true
		}
	}
	return hasBgTool
}

func (s *Session) handleResult(msg protocol.ResultMessage) {
	// Cancel any pending background-task safety timer — a normal continuation
	// ResultMessage has arrived, so the safety path is no longer needed.
	// If bgTimerFired is set, the safety timer already completed this turn;
	// return early to prevent a duplicate TurnCompleteEvent.
	s.mu.Lock()
	if s.bgState.timer != nil {
		s.bgState.timer.Stop()
		s.bgState.timer = nil
	}
	timerAlreadyFired := s.bgState.timerFired
	s.bgState.timerFired = false // clear; also reset by SendMessage at next turn start
	s.bgState.active = false
	if timerAlreadyFired {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	turnNumber := s.turnManager.CurrentTurnNumber()
	turn := s.turnManager.CurrentTurn()

	durationMs := msg.DurationMs
	if turn != nil && durationMs == 0 {
		durationMs = time.Since(turn.StartTime).Milliseconds()
	}

	// Collect any accumulated usage from suppressed intermediate results
	// (background task continuation turns) to report the full logical-turn cost.
	s.mu.Lock()
	accUsage := s.bgState.accumulatedUsage
	s.bgState.accumulatedUsage = TurnUsage{}
	s.mu.Unlock()

	result := TurnResult{
		TurnNumber: turnNumber,
		Success:    !msg.IsError,
		DurationMs: durationMs,
		Usage: TurnUsage{
			InputTokens:         msg.Usage.InputTokens + accUsage.InputTokens,
			OutputTokens:        msg.Usage.OutputTokens + accUsage.OutputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens + accUsage.CacheCreationTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens + accUsage.CacheReadTokens,
			CostUSD:             msg.TotalCostUSD + accUsage.CostUSD,
		},
	}

	// Extract context window size from per-model usage if available.
	for _, mu := range msg.ModelUsage {
		if mu.ContextWindow > 0 {
			result.Usage.ContextWindow = mu.ContextWindow
			break
		}
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

	// Check if ALL non-cancelled tools in this turn were background tasks.
	// If so, the CLI will auto-continue after the tasks complete (delivering
	// task-notification messages and starting a new assistant turn). Suppress
	// turn completion so callers of Ask()/WaitForTurn()/CollectResponse()
	// block until the truly-final turn.
	//
	// When non-bg tools are present (mixed turn), the ResultMessage represents
	// completion of synchronous work and must not be suppressed.
	shouldSuppress := s.shouldSuppressForBgTasks(turn)

	// costAccounted tracks whether cumulativeCostUSD has already been updated
	// for this result (the background-task branch does it early to enable budget
	// checks mid-turn; the normal path must not double-count).
	costAccounted := false

	if shouldSuppress {
		// If the intermediate result carries an error, propagate it now rather
		// than silently dropping it — the background task continuation will not
		// arrive, so the session would otherwise stay stuck in StateProcessing.
		// result.Error was already set above from msg.IsError; just fall through.
		if msg.IsError {
			// Fall through to the normal completion path below.
		} else {
			// Accumulate cost and token usage from this intermediate result so
			// the final TurnResult reports the true total for the logical turn.
			// Also re-add accUsage (usage from earlier suppressed results that was
			// snapshotted at the top of this call) so multi-background-task turns
			// accumulate correctly across all suppressed intermediate results.
			s.mu.Lock()
			s.cumulativeCostUSD += msg.TotalCostUSD
			totalCostSoFar := s.cumulativeCostUSD
			s.bgState.accumulatedUsage.InputTokens += msg.Usage.InputTokens + accUsage.InputTokens
			s.bgState.accumulatedUsage.OutputTokens += msg.Usage.OutputTokens + accUsage.OutputTokens
			s.bgState.accumulatedUsage.CacheCreationTokens += msg.Usage.CacheCreationInputTokens + accUsage.CacheCreationTokens
			s.bgState.accumulatedUsage.CacheReadTokens += msg.Usage.CacheReadInputTokens + accUsage.CacheReadTokens
			s.bgState.accumulatedUsage.CostUSD += msg.TotalCostUSD + accUsage.CostUSD
			s.mu.Unlock()
			costAccounted = true

			// Enforce budget limit: if the intermediate result already pushed us
			// over budget, surface ErrBudgetExceeded now rather than letting the
			// continuation turn run up additional cost. Clear accumulated usage so
			// it does not leak into the next turn's TurnResult.
			//
			// Note: the background task is still running in the CLI process and will
			// send a task-notification when it finishes, producing an unattended
			// assistant continuation turn. This is an acceptable edge case (budget
			// exceeded mid-background-turn) and matches existing budget enforcement,
			// which also does not cancel the CLI process. Callers should call Stop()
			// after receiving ErrBudgetExceeded if they want to halt fully.
			if s.config.MaxBudgetUSD > 0 && totalCostSoFar >= s.config.MaxBudgetUSD {
				result.Error = ErrBudgetExceeded
				s.mu.Lock()
				s.bgState.accumulatedUsage = TurnUsage{}
				s.mu.Unlock()
				// Fall through to normal completion path to emit TurnCompleteEvent.
			} else {
				// Reset accumulator so the continuation turn's streaming events
				// are processed cleanly.
				s.accumulator.Reset()

				// Start a safety timer: if no continuation ResultMessage arrives
				// within the timeout, complete the turn with accumulated data
				// to prevent indefinite blocking.
				timeout := defaultBgTaskSafetyTimeout
				if s.config.BgTaskSafetyTimeout > 0 {
					timeout = s.config.BgTaskSafetyTimeout
				}
				safetyResult := result
				s.mu.Lock()
				s.bgState.active = true
				s.bgState.timerFired = false // reset for this turn's timer
				if s.bgState.timer != nil {
					s.bgState.timer.Stop()
				}
				s.bgState.timer = time.AfterFunc(timeout, func() {
					s.completeSuppressedTurn(safetyResult)
				})
				s.mu.Unlock()
				return
			}
		}
	}

	// Update cumulative cost and check SDK-level limits.
	// SDK limit errors are only set if there is no existing error from the CLI,
	// to avoid silently overwriting the real error.
	// Skip if the background-task branch already accounted for this result's cost.
	s.mu.Lock()
	if !costAccounted {
		s.cumulativeCostUSD += msg.TotalCostUSD
	}
	totalCost := s.cumulativeCostUSD
	s.mu.Unlock()

	if result.Error == nil {
		if s.config.MaxTurns > 0 && turnNumber >= s.config.MaxTurns {
			result.Error = ErrMaxTurnsExceeded
		} else if s.config.MaxBudgetUSD > 0 && totalCost >= s.config.MaxBudgetUSD {
			result.Error = ErrBudgetExceeded
		}
	}
	// Keep Success consistent with Error: SDK-set errors (budget, max-turns)
	// do not set msg.IsError, so Success must be updated here.
	if result.Error != nil {
		result.Success = false
	}

	s.finalizeTurn(result)
}

// finalizeTurn records, emits, and completes the turn. Called from both the
// normal handleResult path and the safety-timer path (completeSuppressedTurn).
func (s *Session) finalizeTurn(result TurnResult) {
	if s.recorder != nil {
		s.recorder.CompleteTurn(result.TurnNumber, result)
	}
	_ = s.state.Transition(TransitionResultReceived)
	s.emit(TurnCompleteEvent{
		TurnNumber: result.TurnNumber,
		Success:    result.Success,
		DurationMs: result.DurationMs,
		Usage:      result.Usage,
		Error:      result.Error,
	})
	s.turnManager.CompleteTurn(result)
}

// completeSuppressedTurn is called by the background-task safety timer when
// no continuation ResultMessage arrives within the timeout. It completes the
// turn with whatever data was accumulated, preventing indefinite blocking.
func (s *Session) completeSuppressedTurn(result TurnResult) {
	s.mu.Lock()
	if !s.bgState.active {
		s.mu.Unlock()
		return // Already completed by normal path
	}
	s.bgState.active = false
	s.bgState.timerFired = true
	s.bgState.timer = nil
	s.bgState.accumulatedUsage = TurnUsage{}
	s.mu.Unlock()

	// Incorporate streaming updates from the correct turn only. Guard with
	// TurnNumber to avoid cross-contaminating with a new turn that may have
	// started between the lock release above and this read.
	turn := s.turnManager.CurrentTurn()
	if turn != nil && turn.Number == result.TurnNumber {
		result.Text = turn.FullText
		result.Thinking = turn.FullThinking
		result.ContentBlocks = turn.ContentBlocks
	}

	s.finalizeTurn(result)
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
