package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
// When the CLI runs background tasks (run_in_background: true or tools
// in backgroundTools such as Monitor), the SDK suppresses the
// intermediate ResultMessage and waits for either a continuation
// ResultMessage (auto-continued bg-Bash path) or all registered tasks to
// reach a terminal state via task_updated/task_notification (Monitor path).
// Use reset() to clear all fields at turn boundaries.
type bgSuppressionState struct {
	timer            *time.Timer // fires if no release signal arrives; see completeSuppressedTurn
	heldResult       *TurnResult // captured intermediate result, finalized by the release path
	accumulatedUsage TurnUsage   // token/cost totals from suppressed intermediate results
	active           bool        // true while waiting for release; cleared by timer, task terminal, or continuation result
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
	b.heldResult = nil
}

// wakeupSuppressionState tracks cross-turn suppression for ScheduleWakeup.
// When the agent calls ScheduleWakeup, the CLI will inject a continuation
// user message after the specified delay, starting a new assistant turn.
// The wrapper suppresses the current turn's completion and waits for the
// continuation turn. If the continuation also calls ScheduleWakeup, it is
// suppressed again, chaining until a turn completes without ScheduleWakeup.
type wakeupSuppressionState struct {
	timer            *time.Timer // safety timer; fires if no continuation arrives
	accumulatedUsage TurnUsage   // token/cost totals from suppressed turns
	active           bool        // true while waiting for the continuation turn
	timerFired       bool        // set by the safety timer
	// suppressedTurnNumber is the original turn number that waiters are
	// blocked on. The continuation turn has a higher number, but we
	// complete it under the original number so WaitForTurn callers unblock.
	suppressedTurnNumber int
}

// reset clears wakeup suppression state and stops any pending timer.
func (w *wakeupSuppressionState) reset() {
	w.active = false
	w.timerFired = false
	w.suppressedTurnNumber = 0
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.accumulatedUsage = TurnUsage{}
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
	bgState           bgSuppressionState     // background-task turn-suppression state; protected by mu
	wakeupState       wakeupSuppressionState // ScheduleWakeup turn-suppression state; protected by mu
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
		switch resp.Subtype {
		case "error":
			return resp, fmt.Errorf("control request error: %s", resp.Error)
		case controlResponseCancelledSubtype:
			return resp, ErrControlRequestCancelled
		}
		return resp, nil
	case <-timeoutCtx.Done():
		return protocol.ControlResponsePayload{}, fmt.Errorf("control request timed out")
	case <-s.done:
		return protocol.ControlResponsePayload{}, fmt.Errorf("session stopped")
	}
}

// controlResponseCancelledSubtype is an internal sentinel used to mark a
// pending control response that was cancelled by a control_cancel_request
// from the CLI. It is not part of the wire protocol.
const controlResponseCancelledSubtype = "__cancelled__"

// ErrControlRequestCancelled is returned by sendControlRequestLocked when the
// CLI cancels an in-flight control request via control_cancel_request before a
// real response arrives.
var ErrControlRequestCancelled = fmt.Errorf("control request cancelled by CLI")

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
	// Clear any stale suppression state from the previous turn. A safety
	// timer for Turn N must not fire after Turn N+1 has started, and the
	// suppressed turn number must not bleed across user-initiated turns.
	s.bgState.reset()
	s.wakeupState.reset()

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
	// Clear any stale suppression state from the previous turn.
	s.bgState.reset()
	s.wakeupState.reset()

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
					TurnNumber:            tc.TurnNumber,
					Success:               tc.Success,
					DurationMs:            tc.DurationMs,
					Usage:                 tc.Usage,
					Error:                 tc.Error,
					HasLiveBackgroundWork: tc.HasLiveBackgroundWork,
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
	s.wakeupState.reset()
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
	case protocol.KeepAliveMessage:
		// Heartbeat; no-op but consumed so it isn't logged as unknown.
	case protocol.ToolProgressMessage:
		s.emit(ToolExecutionProgressEvent{
			ParentToolUseID:    m.ParentToolUseID,
			TaskID:             m.TaskID,
			ToolUseID:          m.ToolUseID,
			ToolName:           m.ToolName,
			ElapsedTimeSeconds: m.ElapsedTimeSeconds,
			TurnNumber:         s.turnManager.CurrentTurnNumber(),
		})
	case protocol.ToolUseSummaryMessage:
		slog.Debug("tool_use_summary", "summary", m.Summary, "preceding", m.PrecedingToolUseIDs)
	case protocol.AuthStatusMessage:
		s.emit(AuthStatusEvent{
			Error:            m.Error,
			Output:           m.Output,
			IsAuthenticating: m.IsAuthenticating,
			TurnNumber:       s.turnManager.CurrentTurnNumber(),
		})
	case protocol.RateLimitEventMessage:
		info := m.RateLimitInfo
		s.emit(RateLimitEvent{
			ResetsAt:              info.ResetsAt,
			Utilization:           info.Utilization,
			OverageResetsAt:       info.OverageResetsAt,
			OverageDisabledReason: info.OverageDisabledReason,
			SurpassedThreshold:    info.SurpassedThreshold,
			Status:                info.Status,
			RateLimitType:         info.RateLimitType,
			IsUsingOverage:        info.IsUsingOverage,
			IsOverageActive:       info.OverageStatus == "active",
			TurnNumber:            s.turnManager.CurrentTurnNumber(),
		})
	case protocol.PromptSuggestionMessage, protocol.StreamlinedTextMessage, protocol.StreamlinedToolUseSummaryMessage:
		// Internal CLI messages — consumed silently.
	case protocol.ControlCancelRequest:
		// Unblock the waiting goroutine by sending a sentinel cancelled
		// payload on the buffered channel. We must NOT close the channel —
		// a racing handleControlResponse send would panic, and a receive
		// from a closed channel would yield (zero, nil) which the waiter
		// would mistake for success.
		s.pendingMu.Lock()
		if ch, ok := s.pendingControlResponses[m.RequestID]; ok {
			select {
			case ch <- protocol.ControlResponsePayload{
				RequestID: m.RequestID,
				Subtype:   controlResponseCancelledSubtype,
			}:
			default:
			}
			delete(s.pendingControlResponses, m.RequestID)
		}
		s.pendingMu.Unlock()
		slog.Debug("control_cancel_request received", "request_id", m.RequestID)
	case protocol.UpdateEnvironmentVariablesMessage:
		slog.Warn("unexpected update_environment_variables from CLI (SDK->CLI only)")
	case protocol.UnknownMessage:
		rawStr := string(m.Raw)
		if len(rawStr) > 200 {
			rawStr = rawStr[:200]
		}
		slog.Warn("unknown top-level message type", "type", m.Type, "raw", rawStr)
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
		return
	}

	turnNum := s.turnManager.CurrentTurnNumber()
	switch protocol.SystemSubtype(msg.Subtype) {
	case protocol.SystemSubtypeCompactBoundary:
		if p, ok := msg.AsCompactBoundary(); ok {
			s.emit(CompactBoundaryEvent{
				PreservedSegment: p.CompactMetadata.PreservedSegment,
				Trigger:          p.CompactMetadata.Trigger,
				PreTokens:        p.CompactMetadata.PreTokens,
				TurnNumber:       turnNum,
			})
		} else {
			slog.Warn("failed to decode compact_boundary payload")
		}
	case protocol.SystemSubtypePostTurnSummary:
		if p, ok := msg.AsPostTurnSummary(); ok {
			s.emit(PostTurnSummaryEvent{
				ArtifactURLs:   p.ArtifactURLs,
				SummarizesUUID: p.SummarizesUUID,
				StatusCategory: p.StatusCategory,
				StatusDetail:   p.StatusDetail,
				Title:          p.Title,
				Description:    p.Description,
				RecentAction:   p.RecentAction,
				IsNoteworthy:   p.IsNoteworthy,
				NeedsAction:    p.NeedsAction,
				TurnNumber:     turnNum,
			})
		} else {
			slog.Warn("failed to decode post_turn_summary payload")
		}
	case protocol.SystemSubtypeAPIRetry:
		if p, ok := msg.AsAPIRetry(); ok {
			s.emit(APIRetryEvent{
				ErrorStatus:  p.ErrorStatus,
				ErrorType:    p.Error,
				Attempt:      p.Attempt,
				MaxRetries:   p.MaxRetries,
				RetryDelayMs: p.RetryDelayMs,
				TurnNumber:   turnNum,
			})
		} else {
			slog.Warn("failed to decode api_retry payload")
		}
	case protocol.SystemSubtypeLocalCommandOutput:
		if p, ok := msg.AsLocalCommandOutput(); ok {
			s.emit(LocalCommandOutputEvent{Content: p.Content, TurnNumber: turnNum})
		} else {
			slog.Warn("failed to decode local_command_output payload")
		}
	case protocol.SystemSubtypeHookStarted:
		if p, ok := msg.AsHookStarted(); ok {
			s.emit(HookLifecycleEvent{
				Phase:         HookPhaseStarted,
				HookID:        p.HookID,
				HookName:      p.HookName,
				HookEventName: p.HookEvent,
				TurnNumber:    turnNum,
			})
		} else {
			slog.Warn("failed to decode hook_started payload")
		}
	case protocol.SystemSubtypeHookProgress:
		if p, ok := msg.AsHookProgress(); ok {
			s.emit(HookLifecycleEvent{
				Phase:         HookPhaseProgress,
				HookID:        p.HookID,
				HookName:      p.HookName,
				HookEventName: p.HookEvent,
				Stdout:        p.Stdout,
				Stderr:        p.Stderr,
				Output:        p.Output,
				TurnNumber:    turnNum,
			})
		} else {
			slog.Warn("failed to decode hook_progress payload")
		}
	case protocol.SystemSubtypeHookResponse:
		if p, ok := msg.AsHookResponse(); ok {
			s.emit(HookLifecycleEvent{
				ExitCode:      p.ExitCode,
				Phase:         HookPhaseResponse,
				HookID:        p.HookID,
				HookName:      p.HookName,
				HookEventName: p.HookEvent,
				Stdout:        p.Stdout,
				Stderr:        p.Stderr,
				Output:        p.Output,
				Outcome:       p.Outcome,
				TurnNumber:    turnNum,
			})
		} else {
			slog.Warn("failed to decode hook_response payload")
		}
	case protocol.SystemSubtypeTaskStarted:
		if p, ok := msg.AsTaskStarted(); ok {
			s.turnManager.TrackTask(p.TaskID)
			s.emit(TaskStartedEvent{
				ToolUseID:    p.ToolUseID,
				WorkflowName: p.WorkflowName,
				TaskID:       p.TaskID,
				Description:  p.Description,
				TaskType:     p.TaskType,
				Prompt:       p.Prompt,
				TurnNumber:   turnNum,
			})
		} else {
			slog.Warn("failed to decode task_started payload")
		}
	case protocol.SystemSubtypeTaskUpdated:
		if p, ok := msg.AsTaskUpdated(); ok {
			s.emit(TaskUpdatedEvent{
				Status:         p.Patch.Status,
				Description:    p.Patch.Description,
				EndTime:        p.Patch.EndTime,
				TotalPausedMs:  p.Patch.TotalPausedMs,
				Error:          p.Patch.Error,
				IsBackgrounded: p.Patch.IsBackgrounded,
				TaskID:         p.TaskID,
				TurnNumber:     turnNum,
			})
			if p.Patch.Status != nil {
				switch *p.Patch.Status {
				case "completed", "failed", "killed":
					s.turnManager.UntrackTask(p.TaskID)
					s.maybeReleaseSuppression("task_updated:" + *p.Patch.Status)
				}
			}
		} else {
			slog.Warn("failed to decode task_updated payload")
		}
	case protocol.SystemSubtypeTaskProgress:
		if p, ok := msg.AsTaskProgress(); ok {
			s.emit(TaskProgressEvent{
				ToolUseID:    p.ToolUseID,
				TaskID:       p.TaskID,
				Description:  p.Description,
				LastToolName: p.LastToolName,
				Summary:      p.Summary,
				Usage:        p.Usage,
				TurnNumber:   turnNum,
			})
		} else {
			slog.Warn("failed to decode task_progress payload")
		}
	case protocol.SystemSubtypeTaskNotification:
		if p, ok := msg.AsTaskNotification(); ok {
			s.emit(TaskNotificationEvent{
				ToolUseID:  p.ToolUseID,
				TaskID:     p.TaskID,
				Status:     p.Status,
				OutputFile: p.OutputFile,
				Summary:    p.Summary,
				Usage:      p.Usage,
				TurnNumber: turnNum,
			})
			// Belt-and-suspenders: task_notification may fire without a
			// preceding terminal task_updated (e.g. if the bg process writes
			// stdout on exit). Drain the live set and attempt release.
			s.turnManager.UntrackTask(p.TaskID)
			s.maybeReleaseSuppression("task_notification")
		} else {
			slog.Warn("failed to decode task_notification payload")
		}
	case protocol.SystemSubtypeSessionStateChanged:
		if p, ok := msg.AsSessionStateChanged(); ok {
			s.emit(CLISessionStateChangedEvent{State: p.State, TurnNumber: turnNum})
		} else {
			slog.Warn("failed to decode session_state_changed payload")
		}
	case protocol.SystemSubtypeFilesPersisted:
		if p, ok := msg.AsFilesPersisted(); ok {
			s.emit(FilesPersistedEvent{
				Files:       p.Files,
				Failed:      p.Failed,
				ProcessedAt: p.ProcessedAt,
				TurnNumber:  turnNum,
			})
		} else {
			slog.Warn("failed to decode files_persisted payload")
		}
	case protocol.SystemSubtypeElicitationComplete:
		slog.Debug("system.elicitation_complete", "subtype", msg.Subtype)
	case protocol.SystemSubtypeStatus:
		slog.Debug("system.status", "subtype", msg.Subtype)
	default:
		slog.Debug("unhandled system subtype", "subtype", msg.Subtype)
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

func (s *Session) handleResult(msg protocol.ResultMessage) {
	// Cancel any pending background-task safety timer — a normal continuation
	// ResultMessage has arrived, so the safety path is no longer needed.
	// If bgState.timerFired is set, the safety timer already completed this turn;
	// return early to prevent a duplicate TurnCompleteEvent.
	s.mu.Lock()
	if s.bgState.timer != nil {
		s.bgState.timer.Stop()
		s.bgState.timer = nil
	}
	timerAlreadyFired := s.bgState.timerFired
	s.bgState.timerFired = false // clear; also reset by SendMessage at next turn start
	// wasSuppressed is true when a prior ResultMessage triggered suppression and
	// this ResultMessage is the bg-Bash continuation that releases it. In that
	// case shouldSuppressForBgTasks would still return true (the bg-Bash tool_use
	// remains in ContentBlocks), but we must NOT re-suppress — this IS the final
	// result. We detect it by checking whether suppression was active when we
	// arrive: if active was true, we just cleared it, so this is the continuation.
	wasSuppressed := s.bgState.active
	s.bgState.active = false
	s.bgState.heldResult = nil

	// Cancel any pending wakeup-suppression safety timer.
	// If the wakeup timer already fired, a duplicate TurnCompleteEvent must
	// not be emitted — return early.
	if s.wakeupState.timer != nil {
		s.wakeupState.timer.Stop()
		s.wakeupState.timer = nil
	}
	wakeupTimerFired := s.wakeupState.timerFired
	s.wakeupState.timerFired = false
	wakeupSuppressed := s.wakeupState.active
	// Clear wakeupState.active under the same lock where timerFired is read.
	// Leaving it true while mu is released would race with the safety-timer
	// callback (completeWakeupSuppressedTurn), which would see active=true
	// and emit a duplicate TurnCompleteEvent. If this turn needs to be
	// re-suppressed (chained ScheduleWakeup), the shouldSuppressWakeup
	// branch below re-arms it.
	s.wakeupState.active = false

	if timerAlreadyFired || wakeupTimerFired {
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
	// (background task continuation turns and wakeup-suppressed turns)
	// to report the full logical-turn cost.
	s.mu.Lock()
	accUsage := s.bgState.accumulatedUsage
	s.bgState.accumulatedUsage = TurnUsage{}
	accUsage.Add(s.wakeupState.accumulatedUsage)
	s.wakeupState.accumulatedUsage = TurnUsage{}
	s.mu.Unlock()

	// When wakeup suppression was active, waiters are blocked on the original
	// suppressed turn number. Use that number so CompleteTurn unblocks them.
	// (wakeupState.active was already cleared above under the initial lock.)
	resultTurnNumber := turnNumber
	if wakeupSuppressed {
		s.mu.Lock()
		resultTurnNumber = s.wakeupState.suppressedTurnNumber
		s.mu.Unlock()
	}

	result := TurnResult{
		TurnNumber: resultTurnNumber,
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
		// Per the protocol, error subtypes populate Errors, not Result.
		// Fall back to Result/subtype if Errors is empty so we never surface
		// a blank error string.
		switch {
		case len(msg.Errors) > 0:
			result.Error = fmt.Errorf("%s", strings.Join(msg.Errors, "; "))
		case msg.Result != "":
			result.Error = fmt.Errorf("%s", msg.Result)
		default:
			result.Error = fmt.Errorf("turn failed: %s", msg.Subtype)
		}
	}

	// Check if ALL non-cancelled tools in this turn were background tasks.
	// If so, the CLI will auto-continue after the tasks complete (delivering
	// task-notification messages and starting a new assistant turn). Suppress
	// turn completion so callers of Ask()/WaitForTurn()/CollectResponse()
	// block until the truly-final turn.
	//
	// When non-bg tools are present (mixed turn), the ResultMessage represents
	// completion of synchronous work and must not be suppressed.
	//
	// When wasSuppressed is true, this ResultMessage is the bg-Bash continuation
	// that releases prior suppression. The turn's ContentBlocks still contain the
	// bg-Bash tool_use (so shouldSuppressForBgTasks would return true), but
	// suppression must not be re-armed — this IS the final result for the turn.
	shouldSuppress := !wasSuppressed && turn.shouldSuppressForBgTasks()

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
			// Fall through to the normal completion path below. Mark the
			// result as having live bg work so downstream retry loops do
			// not interrupt the parked tasks — defer session.Stop() on a
			// retry would orphan them.
			result.HasLiveBackgroundWork = true
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
				// The background task is still running in the CLI process. Mark
				// so retry loops do not interrupt it (same guard as the error
				// and safety-timer paths).
				result.HasLiveBackgroundWork = true
				s.mu.Lock()
				s.bgState.accumulatedUsage = TurnUsage{}
				s.mu.Unlock()
				// Fall through to normal completion path to emit TurnCompleteEvent.
			} else {
				// Reset accumulator so the continuation turn's streaming events
				// are processed cleanly.
				s.accumulator.Reset()

				// Start a safety timer: if no release signal arrives within the
				// timeout, complete the turn with accumulated data to prevent
				// indefinite blocking.
				//
				// Two release paths exist:
				//   1. A continuation ResultMessage (auto-continued bg-Bash path).
				//      The CLI auto-starts a new assistant turn when bg work
				//      finishes. handleResult clears bgState.active at the top,
				//      then the new turn has no bg tools so shouldSuppressForBgTasks
				//      returns false and the turn completes normally.
				//   2. All registered tasks reach a terminal state via
				//      task_updated/task_notification (Monitor path —
				//      maybeReleaseSuppression fires from handleSystem; or the
				//      AllTasksCompleted fast path fires if tasks completed before
				//      this ResultMessage arrived).
				//
				// The timeout is max(configured, longest bg tool timeout_ms,
				// 1h upper clamp) so it never releases before the agent's own
				// deadline (Monitor lets callers pass timeout_ms up to 1h).
				timeout := defaultBgTaskSafetyTimeout
				if s.config.BgTaskSafetyTimeout > 0 {
					timeout = s.config.BgTaskSafetyTimeout
				}
				if toolMs := turn.longestBackgroundToolTimeoutMs(); toolMs > 0 {
					toolTimeout := time.Duration(toolMs) * time.Millisecond
					if toolTimeout > timeout {
						timeout = toolTimeout
					}
				}
				if maxTimeout := time.Hour; timeout > maxTimeout {
					timeout = maxTimeout
				}
				safetyResult := result
				s.mu.Lock()
				// Fast path: all bg tasks already completed before this
				// ResultMessage arrived (task_updated/task_notification arrived
				// first). In that case liveTasks is already empty and no future
				// task event will call maybeReleaseSuppression, so we must
				// finalize now rather than hanging until the safety timer fires.
				//
				// Only applies when tasks were actually registered (Monitor
				// path); bg-Bash turns never register tasks and use the
				// continuation-ResultMessage release path instead.
				if s.turnManager.AllTasksCompleted() {
					s.bgState.accumulatedUsage = TurnUsage{}
					s.mu.Unlock()
					s.finalizeTurn(safetyResult)
					return
				}
				s.bgState.active = true
				s.bgState.timerFired = false // reset for this turn's timer
				s.bgState.heldResult = &safetyResult
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

	// Check if the turn ends with a ScheduleWakeup tool call. When present,
	// the CLI will auto-inject a continuation user message after the specified
	// delay, starting a new assistant turn. Suppress turn completion so callers
	// of Ask()/WaitForTurn() block until the continuation turn completes.
	//
	// Skip suppression when:
	//   - the result carries an error (propagate immediately)
	//   - this is already a wakeup continuation (wakeupSuppressed=true) and
	//     the continuation turn does NOT have another ScheduleWakeup
	shouldSuppressWakeup := !msg.IsError && turn.hasScheduleWakeup()
	if shouldSuppressWakeup {
		s.mu.Lock()
		s.cumulativeCostUSD += msg.TotalCostUSD
		s.wakeupState.accumulatedUsage.Add(TurnUsage{
			InputTokens:         msg.Usage.InputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
			CostUSD:             msg.TotalCostUSD,
		})
		// Re-add usage drained from prior suppressed turns (snapshotted into
		// accUsage at the top of this call). Without this, chained wakeups
		// would silently drop earlier turns' token/cost totals.
		s.wakeupState.accumulatedUsage.Add(accUsage)

		// On first suppression, capture the original turn number so chained
		// continuation turns can be completed under the same number.
		if !wakeupSuppressed {
			s.wakeupState.suppressedTurnNumber = turnNumber
		}

		totalCostSoFar := s.cumulativeCostUSD
		if s.config.MaxBudgetUSD > 0 && totalCostSoFar >= s.config.MaxBudgetUSD {
			s.wakeupState.accumulatedUsage = TurnUsage{}
			s.wakeupState.active = false
			s.mu.Unlock()
			// Fall through to normal completion with budget error.
		} else {
			s.wakeupState.active = true

			// Safety timer: delay + 60s buffer. ScheduleWakeup delays are
			// clamped to [60, 3600] by the runtime, so worst case is ~61 min.
			delaySec := turn.scheduleWakeupDelaySeconds()
			if delaySec < 60 {
				delaySec = 60
			}
			timeout := time.Duration(delaySec+60) * time.Second

			safetyResult := result
			safetyResult.TurnNumber = resultTurnNumber
			s.wakeupState.timer = time.AfterFunc(timeout, func() {
				s.completeWakeupSuppressedTurn(safetyResult)
			})
			s.mu.Unlock()

			s.accumulator.Reset()
			return
		}
	}

	// Update cumulative cost and check SDK-level limits.
	// SDK limit errors are only set if there is no existing error from the CLI,
	// to avoid silently overwriting the real error.
	// Skip if the background-task branch already accounted for this result's cost.
	s.mu.Lock()
	if !costAccounted && !shouldSuppressWakeup {
		s.cumulativeCostUSD += msg.TotalCostUSD
	}
	totalCost := s.cumulativeCostUSD
	s.mu.Unlock()

	if result.Error == nil {
		// MaxTurns is enforced against the actual turn count, not
		// resultTurnNumber. Under wakeup suppression resultTurnNumber is
		// the original (lower) turn number used to unblock waiters, but
		// the real assistant turn count has advanced — enforcing against
		// the stale number would let chained wakeups exceed the limit.
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
		TurnNumber:            result.TurnNumber,
		Success:               result.Success,
		DurationMs:            result.DurationMs,
		Usage:                 result.Usage,
		Error:                 result.Error,
		HasLiveBackgroundWork: result.HasLiveBackgroundWork,
	})
	s.turnManager.CompleteTurn(result)
}

// completeSuppressedTurn is called by the background-task safety timer when
// no release signal arrives within the timeout. It completes the turn with
// whatever data was accumulated, preventing indefinite blocking.
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
	s.bgState.heldResult = nil
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
	// The safety timer fired without any release signal. The bg work is
	// still live from the session's perspective — mark so retry loops do
	// not interrupt it.
	result.HasLiveBackgroundWork = true

	s.finalizeTurn(result)
}

// completeWakeupSuppressedTurn is the safety-timer equivalent for
// ScheduleWakeup suppression. If no continuation turn arrives within
// the expected delay + buffer, complete the turn to prevent hanging.
func (s *Session) completeWakeupSuppressedTurn(result TurnResult) {
	s.mu.Lock()
	if !s.wakeupState.active {
		s.mu.Unlock()
		return // Already completed by the normal continuation path
	}
	s.wakeupState.active = false
	s.wakeupState.timerFired = true
	s.wakeupState.timer = nil
	s.wakeupState.accumulatedUsage = TurnUsage{}
	s.mu.Unlock()

	// Refresh content from the latest turn snapshot. For chained wakeups
	// the current turn number has advanced beyond result.TurnNumber (which
	// is the original suppressed turn used to unblock waiters), so we take
	// whatever the most recent streamed assistant state is rather than
	// finalizing with the stale snapshot captured when the timer was armed.
	if turn := s.turnManager.CurrentTurn(); turn != nil {
		result.Text = turn.FullText
		result.Thinking = turn.FullThinking
		result.ContentBlocks = turn.ContentBlocks
	}

	s.finalizeTurn(result)
}

// maybeReleaseSuppression releases a suppressed turn when all live tasks
// have reached a terminal state. Called from task_updated and
// task_notification handling after a terminal state is observed. No-op if
// suppression is not active, if the live set is still non-empty, or if the
// held result no longer matches the current turn (stale release from a
// prior turn's leftover task).
//
// Unlike completeSuppressedTurn (which is timer-driven and finalizes with a
// captured copy), this uses bgState.heldResult so both paths surface the
// same TurnResult.
func (s *Session) maybeReleaseSuppression(reason string) {
	s.mu.Lock()
	if !s.bgState.active || s.bgState.heldResult == nil {
		s.mu.Unlock()
		return
	}
	// AllTasksCompleted returns false when liveTasks is non-empty (Monitor task
	// still running), when no tasks were ever registered (bg-Bash only turns
	// that use the continuation-ResultMessage release path), or when uncancelled
	// bg-Bash tools are present (mixed Monitor+bg-Bash must defer to the
	// continuation-ResultMessage path).
	if !s.turnManager.AllTasksCompleted() {
		s.mu.Unlock()
		return
	}
	result := *s.bgState.heldResult
	currentTurnNum := s.turnManager.CurrentTurnNumber()
	if result.TurnNumber != currentTurnNum {
		s.mu.Unlock()
		return
	}
	s.bgState.active = false
	s.bgState.heldResult = nil
	if s.bgState.timer != nil {
		s.bgState.timer.Stop()
		s.bgState.timer = nil
	}
	// Leave accumulatedUsage alone — the held result already includes
	// accumulated usage folded into result.Usage at handleResult time.
	s.bgState.accumulatedUsage = TurnUsage{}
	s.mu.Unlock()

	slog.Debug("releasing suppressed turn", "reason", reason, "turn", result.TurnNumber)

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
	// Hold pendingMu across the send to serialize with the cancel path,
	// which deletes the entry while sending its own sentinel. This prevents
	// a panic from sending on a channel that the cancel path might have
	// otherwise closed, and ensures at most one writer per request_id.
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	ch, ok := s.pendingControlResponses[requestID]
	if !ok {
		return
	}
	select {
	case ch <- msg.Response:
	default:
	}
}

func (s *Session) handleControlRequest(msg protocol.ControlRequest) {
	// Use the session context so handlers (hook callbacks, elicitation,
	// interactive tools, permission requests) are cancelled when Stop() runs
	// and cannot outlive the session.
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	// First check if this is an MCP message (SDK MCP server traffic)
	reqData, parseErr := protocol.ParseControlRequest(msg.Request)
	if parseErr != nil {
		// A malformed control_request frame must still receive a response —
		// otherwise the CLI's pending future never resolves and the session
		// stalls. Send a deny so the peer can surface the error.
		slog.Warn("failed to parse control_request", "error", parseErr, "request_id", msg.RequestID)
		resp := buildDenyResponse(msg.RequestID, fmt.Sprintf("malformed control_request: %v", parseErr), false)
		if err := s.process.WriteMessage(resp); err != nil {
			s.emitError(err, "send_control_response")
		}
		if s.recorder != nil {
			s.recorder.RecordSent(resp)
		}
		return
	}
	// toolReq is populated from the already-parsed reqData when the subtype is
	// can_use_tool, avoiding a redundant ParseControlRequest call.
	var toolReq *protocol.ToolUseRequest
	switch r := reqData.(type) {
	case protocol.MCPMessageRequest:
		s.handleMCPMessage(msg.RequestID, r)
		return
	case protocol.HookCallbackRequest:
		s.handleHookCallbackControl(ctx, msg.RequestID, r)
		return
	case protocol.ElicitationRequest:
		s.handleElicitationControl(ctx, msg.RequestID, r)
		return
	case protocol.UnknownControlRequest:
		// ParseControlRequest already logged the unknown subtype at warn.
		// Reply with an explicit empty-object body so the wire shape always
		// includes the inner `response` field, matching the hook_callback /
		// elicitation paths and avoiding any ambiguity for the CLI.
		_ = r // bound for the type switch; the body itself is not needed.
		s.sendControlSuccess(msg.RequestID, map[string]any{})
		return
	case protocol.CanUseToolRequest:
		// Use the already-parsed result directly to avoid a second JSON decode.
		toolReq = &protocol.ToolUseRequest{
			RequestID:   msg.RequestID,
			ToolName:    r.ToolName,
			Input:       r.Input,
			BlockedPath: r.BlockedPath,
		}
	}

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

	// Delegate to permission manager for non-interactive tools.
	// Use HandleToolRequest when we already have the parsed ToolUseRequest to
	// avoid a redundant JSON decode inside HandleRequest/ExtractPermissionRequest.
	var resp *protocol.ControlResponse
	var err error
	if toolReq != nil {
		resp, err = s.permissionManager.HandleToolRequest(ctx, toolReq)
	} else {
		resp, err = s.permissionManager.HandleRequest(ctx, msg)
	}
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

// sendControlSuccess sends a success control response with an optional
// response body. Used for hook_callback, elicitation, and unknown-subtype
// fallbacks to keep the CLI unblocked.
func (s *Session) sendControlSuccess(requestID string, body interface{}) {
	resp := protocol.ControlResponse{
		Type: protocol.MessageTypeControlResponse,
		Response: protocol.ControlResponsePayload{
			Subtype:   protocol.ControlResponseSubtypeSuccess,
			RequestID: requestID,
			Response:  body,
		},
	}
	if err := s.process.WriteMessage(&resp); err != nil {
		s.emitError(err, "send_control_response")
		return
	}
	if s.recorder != nil {
		s.recorder.RecordSent(&resp)
	}
}

// handleHookCallbackControl dispatches a hook_callback control request to
// the configured handler, or responds with an empty success body if none.
func (s *Session) handleHookCallbackControl(ctx context.Context, requestID string, req protocol.HookCallbackRequest) {
	var body map[string]any
	if s.config.HookCallbackHandler != nil {
		out, err := s.config.HookCallbackHandler(ctx, req)
		if err != nil {
			s.emitError(err, "hook_callback_handler")
			// Fall through to empty-body success so CLI is not blocked.
		} else {
			body = out
		}
	}
	if body == nil {
		body = map[string]any{}
	}
	s.sendControlSuccess(requestID, body)
}

// handleElicitationControl dispatches an elicitation control request to
// the configured handler, or responds with Action="cancel" if none.
func (s *Session) handleElicitationControl(ctx context.Context, requestID string, req protocol.ElicitationRequest) {
	var resp protocol.ElicitationResponse
	if s.config.ElicitationHandler != nil {
		out, err := s.config.ElicitationHandler(ctx, req)
		if err != nil {
			s.emitError(err, "elicitation_handler")
			resp = protocol.ElicitationResponse{Action: "cancel"}
		} else {
			resp = out
		}
	} else {
		resp = protocol.ElicitationResponse{Action: "cancel"}
	}
	s.sendControlSuccess(requestID, resp)
}

// generateRequestID generates a unique request ID.
func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("req_%d_%s", time.Now().UnixNano(), hex.EncodeToString(b))
}
