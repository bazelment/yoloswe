package claude

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// waitForTurnComplete drains events from the channel until a TurnCompleteEvent
// is found or the timeout expires.
func waitForTurnComplete(t *testing.T, events <-chan Event, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case event := <-events:
			if _, ok := event.(TurnCompleteEvent); ok {
				return
			}
			// Keep draining other events (CLIToolResultEvent, etc.).
		case <-deadline:
			t.Fatal("turn did not complete — TurnCompleteEvent not emitted within timeout")
		}
	}
}

// makeFlexibleContent marshals v into a FlexibleContent.
func makeFlexibleContent(t *testing.T, v interface{}) protocol.FlexibleContent {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fc protocol.FlexibleContent
	if err := json.Unmarshal(data, &fc); err != nil {
		t.Fatal(err)
	}
	return fc
}

// newTestSession creates a minimal Session suitable for unit-testing
// handleUser/handleResult without a real process. The session's state
// machine and turn manager are initialised so the result-handling code
// path works.
func newTestSession(t *testing.T, opts ...SessionOption) *Session {
	t.Helper()
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	s := &Session{
		config:      cfg,
		events:      make(chan Event, 100),
		turnManager: newTurnManager(),
		state:       newSessionState(),
		accumulator: nil, // set below
		done:        make(chan struct{}),
	}
	s.accumulator = newStreamAccumulator(s)
	// Transition to StateReady so handleResult can transition back via TransitionResultReceived.
	_ = s.state.Transition(TransitionStarted)
	_ = s.state.Transition(TransitionInitReceived)
	return s
}

// simulateAssistantToolUse registers tool_use blocks with the session,
// mirroring what handleAssistant does: tracks tools and appends ContentBlocks.
func simulateAssistantToolUse(s *Session, tools []protocol.ToolUseBlock) {
	// Start a turn and register each tool.
	s.turnManager.StartTurn("test")
	for _, tb := range tools {
		tool := s.turnManager.GetOrCreateTool(tb.ID, tb.Name)
		tool.Input = tb.Input
		s.turnManager.AppendContentBlock(ContentBlock{
			Type:      ContentBlockTypeToolUse,
			ToolUseID: tb.ID,
			ToolName:  tb.Name,
			ToolInput: tb.Input,
		})
	}
}

// simulateUserToolResults calls handleUser with a UserMessage containing
// the given tool_result blocks.
func simulateUserToolResults(t *testing.T, s *Session, results []protocol.ToolResultBlock) {
	t.Helper()
	// Convert to ContentBlock slice for JSON serialization.
	var blocks []interface{}
	for _, r := range results {
		blocks = append(blocks, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": r.ToolUseID,
			"content":     r.Content,
			"is_error":    r.IsError,
		})
	}
	msg := protocol.UserMessage{
		Message: protocol.MessageContent{
			Role:    "user",
			Content: makeFlexibleContent(t, blocks),
		},
	}
	s.handleUser(msg)
}

func TestBgTask_CancelledToolsDoNotSuppressTurn(t *testing.T) {
	s := newTestSession(t)

	// Simulate 3 parallel tool_use blocks: first has no bg, second and third have bg.
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "exit 1"}},
		{ID: "tool-2", Name: "Bash", Input: map[string]interface{}{"command": "echo bg2", "run_in_background": true}},
		{ID: "tool-3", Name: "Bash", Input: map[string]interface{}{"command": "echo bg3", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	// Simulate tool results: tool-1 errors, tool-2 and tool-3 are cancelled (is_error=true).
	isErrTrue := true
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "exit code 1", IsError: &isErrTrue},
		{ToolUseID: "tool-2", Content: "Cancelled: parallel tool call errored", IsError: &isErrTrue},
		{ToolUseID: "tool-3", Content: "Cancelled: parallel tool call errored", IsError: &isErrTrue},
	}
	simulateUserToolResults(t, s, results)

	// shouldSuppressForBgTasks must return false: all tools are cancelled.
	turn := s.turnManager.CurrentTurn()
	if turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return false when all tools are cancelled")
	}

	// Now simulate handleResult — should complete the turn normally, not suppress.
	_ = s.state.Transition(TransitionUserMessageSent)

	resultMsg := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
	}
	s.handleResult(resultMsg)

	// Verify TurnCompleteEvent was emitted (drain other events first).
	waitForTurnComplete(t, s.events, time.Second)
}

func TestBgTask_SuccessfulBgToolSuppressesTurn(t *testing.T) {
	s := newTestSession(t)

	// Simulate a successful bg tool.
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "sleep 2 && echo done", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	// shouldSuppressForBgTasks must return true: only bg tools, none cancelled.
	turn := s.turnManager.CurrentTurn()
	if !turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return true for a single successful bg tool")
	}

	// handleResult should suppress the turn (no TurnCompleteEvent).
	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
	}
	s.handleResult(resultMsg)

	// Should NOT get a TurnCompleteEvent (turn is suppressed).
	select {
	case event := <-s.events:
		if _, ok := event.(TurnCompleteEvent); ok {
			t.Error("TurnCompleteEvent should NOT have been emitted — turn should be suppressed for bg task")
		}
	case <-time.After(100 * time.Millisecond):
		// Good — no event, turn is suppressed as expected.
	}

	// Verify safety timer is active.
	s.mu.RLock()
	timerActive := s.bgState.timer != nil
	suppActive := s.bgState.active
	s.mu.RUnlock()

	if !timerActive {
		t.Error("expected bgState.timer to be active")
	}
	if !suppActive {
		t.Error("expected bgState.active to be true")
	}

	// Clean up: stop the safety timer to avoid leaks.
	s.mu.Lock()
	if s.bgState.timer != nil {
		s.bgState.timer.Stop()
	}
	s.mu.Unlock()
}

func TestBgTask_SafetyTimeoutCompletesTurn(t *testing.T) {
	// Use a very short safety timeout.
	s := newTestSession(t, WithBgTaskSafetyTimeout(200*time.Millisecond))

	// Simulate a successful bg tool.
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "sleep 60", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	// handleResult should suppress the turn.
	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
	}
	s.handleResult(resultMsg)

	// Wait for the safety timer to fire and complete the turn.
	waitForTurnComplete(t, s.events, 2*time.Second)
}

func TestBgTask_SafetyTimerPreventsLateResultDoubleCompletion(t *testing.T) {
	// When the safety timer fires and completes the turn, a late continuation
	// ResultMessage must be ignored — not emit a second TurnCompleteEvent.
	s := newTestSession(t, WithBgTaskSafetyTimeout(100*time.Millisecond))

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "sleep 60", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{Type: "result", IsError: false}
	s.handleResult(resultMsg)

	// Wait for the safety timer to fire.
	waitForTurnComplete(t, s.events, 2*time.Second)

	// Simulate a late continuation result arriving after the timer completed the turn.
	// handleResult must detect bgState.timerFired and return early — no second TurnCompleteEvent.
	s.handleResult(resultMsg)

	// Drain events for a short window; must see at most one TurnCompleteEvent total.
	count := 0
	deadline := time.After(200 * time.Millisecond)
drain:
	for {
		select {
		case event := <-s.events:
			if _, ok := event.(TurnCompleteEvent); ok {
				count++
			}
		case <-deadline:
			break drain
		}
	}
	if count > 0 {
		t.Errorf("expected no additional TurnCompleteEvent after safety timer already completed the turn, got %d", count)
	}
}

func TestBgTask_SafetyTimerDoesNotPoisonNextTurn(t *testing.T) {
	// When the safety timer fires and no late continuation arrives,
	// the NEXT turn's handleResult must still work correctly.
	s := newTestSession(t, WithBgTaskSafetyTimeout(100*time.Millisecond))

	// Turn N: set up a suppressed bg turn.
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "sleep 60", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{Type: "result", IsError: false}
	s.handleResult(resultMsg)

	// Wait for safety timer to fire — Turn N is now complete.
	waitForTurnComplete(t, s.events, 2*time.Second)

	// Simulate Turn N+1 starting (replicates what SendMessage does):
	// clear all stale suppression state so the old timer cannot fire again.
	s.mu.Lock()
	s.bgState.reset()
	s.mu.Unlock()

	// Start a new turn with no bg tools.
	s.turnManager.StartTurn("turn N+1")

	// Turn N+1: transition to processing and deliver a result.
	// This must produce a TurnCompleteEvent — not be silently dropped.
	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(resultMsg)

	waitForTurnComplete(t, s.events, time.Second)
}

func TestBgTask_OldTimerCannotPoisonNextTurnAfterSendMessage(t *testing.T) {
	// Validate that SendMessage correctly stops the old timer and clears
	// bgState.active, so even if the timer fires after SendMessage
	// completes, it cannot set bgState.timerFired and drop the new turn's result.
	//
	// Sequence:
	//   1. Turn N suppressed, timer armed.
	//   2. SendMessage starts Turn N+1 (clears bgState.active, stops timer).
	//   3. Old timer closure fires — completeSuppressedTurn sees bgState.active=false, returns.
	//   4. Turn N+1 handleResult must complete normally.
	s := newTestSession(t, WithBgTaskSafetyTimeout(200*time.Millisecond))

	// Turn N: suppress.
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "sleep 60", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{Type: "result", IsError: false}
	s.handleResult(resultMsg)

	// Verify suppression is active.
	s.mu.RLock()
	suppActive := s.bgState.active
	s.mu.RUnlock()
	if !suppActive {
		t.Fatal("expected bgState.active to be true after suppression started")
	}

	// Simulate SendMessage for Turn N+1: clear all stale state and stop the old timer.
	s.mu.Lock()
	s.bgState.reset()
	s.mu.Unlock()

	// Start a new turn with no bg tools.
	s.turnManager.StartTurn("turn N+1")

	// Wait longer than the timer would have fired (200ms), ensuring no late fire.
	time.Sleep(300 * time.Millisecond)

	// Turn N+1's handleResult: must complete the turn, not be silently dropped.
	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(resultMsg)

	waitForTurnComplete(t, s.events, time.Second)

	// Ensure bgState.timerFired was not set by the old timer closure.
	s.mu.RLock()
	timerFired := s.bgState.timerFired
	s.mu.RUnlock()
	if timerFired {
		t.Error("bgState.timerFired should be false — old timer should have been stopped by bgState.reset()")
	}
}

func TestBgTask_MixedSuccessAndCancelled(t *testing.T) {
	s := newTestSession(t)

	// Simulate 3 bg tools: one succeeds, two are cancelled.
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "echo ok", "run_in_background": true}},
		{ID: "tool-2", Name: "Bash", Input: map[string]interface{}{"command": "echo bg2", "run_in_background": true}},
		{ID: "tool-3", Name: "Bash", Input: map[string]interface{}{"command": "echo bg3", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	isErrTrue := true
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
		{ToolUseID: "tool-2", Content: "Cancelled", IsError: &isErrTrue},
		{ToolUseID: "tool-3", Content: "Cancelled", IsError: &isErrTrue},
	}
	simulateUserToolResults(t, s, results)

	// shouldSuppressForBgTasks must return true: only tool-1 is non-cancelled, and it's bg.
	turn := s.turnManager.CurrentTurn()
	if !turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return true — only non-cancelled tool is bg")
	}
}

func TestBgTask_MixedBgAndNonBgDoesNotSuppressTurn(t *testing.T) {
	// This is the core bug fix test: when a turn has both bg and non-bg tools,
	// the ResultMessage represents completion of synchronous work and must NOT
	// be suppressed. This was the jiradozer stuck-session scenario.
	s := newTestSession(t)

	// Simulate 3 tools: 2 bg + 1 non-bg (the exact jiradozer pattern).
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "sleep 1 && echo BG1", "run_in_background": true}},
		{ID: "tool-2", Name: "Bash", Input: map[string]interface{}{"command": "sleep 1 && echo BG2", "run_in_background": true}},
		{ID: "tool-3", Name: "Bash", Input: map[string]interface{}{"command": "echo blocking", "timeout": 600000}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
		{ToolUseID: "tool-2", Content: "Running in background...", IsError: &isErrFalse},
		{ToolUseID: "tool-3", Content: "blocking output", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	// shouldSuppressForBgTasks must return false: non-bg tool exists.
	turn := s.turnManager.CurrentTurn()
	if turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return false when non-bg tools are present")
	}

	// handleResult should complete the turn normally.
	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
	}
	s.handleResult(resultMsg)

	// TurnCompleteEvent must be emitted — not suppressed.
	waitForTurnComplete(t, s.events, time.Second)

	// Safety timer must NOT be started.
	s.mu.RLock()
	timerActive := s.bgState.timer != nil
	s.mu.RUnlock()
	if timerActive {
		t.Error("safety timer should NOT be started for mixed bg/non-bg turns")
	}
}

func TestBgTask_MultipleBgToolsStillSuppressTurn(t *testing.T) {
	// When ALL tools are bg, the turn should still be suppressed.
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "sleep 5 && echo BG1", "run_in_background": true}},
		{ID: "tool-2", Name: "Bash", Input: map[string]interface{}{"command": "sleep 5 && echo BG2", "run_in_background": true}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
		{ToolUseID: "tool-2", Content: "Running in background...", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	// shouldSuppressForBgTasks must return true.
	turn := s.turnManager.CurrentTurn()
	if !turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return true when all tools are bg")
	}

	// handleResult should suppress the turn.
	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{Type: "result", IsError: false}
	s.handleResult(resultMsg)

	// Should NOT get a TurnCompleteEvent.
	select {
	case event := <-s.events:
		if _, ok := event.(TurnCompleteEvent); ok {
			t.Error("TurnCompleteEvent should NOT have been emitted — all tools are bg")
		}
	case <-time.After(100 * time.Millisecond):
		// Good — suppressed.
	}

	// Safety timer must be started.
	s.mu.RLock()
	timerActive := s.bgState.timer != nil
	suppActive := s.bgState.active
	s.mu.RUnlock()
	if !timerActive {
		t.Error("expected safety timer to be active")
	}
	if !suppActive {
		t.Error("expected bgState.active to be true")
	}

	// Clean up.
	s.mu.Lock()
	s.bgState.reset()
	s.mu.Unlock()
}

func TestBgTask_MixedBgAndNonBgWithNonBgError(t *testing.T) {
	// When a non-bg tool errors alongside a successful bg tool, the turn
	// should NOT be suppressed — the non-bg tool's existence means the
	// ResultMessage is the real completion.
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "echo bg", "run_in_background": true}},
		{ID: "tool-2", Name: "Bash", Input: map[string]interface{}{"command": "exit 1"}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	isErrTrue := true
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Running in background...", IsError: &isErrFalse},
		{ToolUseID: "tool-2", Content: "exit code 1", IsError: &isErrTrue},
	}
	simulateUserToolResults(t, s, results)

	// Non-bg tool exists (even though it errored) → don't suppress.
	turn := s.turnManager.CurrentTurn()
	if turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return false when a non-bg tool exists (even if errored)")
	}
}

func TestBgTask_NoBgToolsNormalCompletion(t *testing.T) {
	// Baseline: no bg tools at all → normal completion.
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "echo hello"}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "hello", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	turn := s.turnManager.CurrentTurn()
	if turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return false when no bg tools exist")
	}
}

// TestScheduleWakeup_SuppressesTurn verifies that a turn ending with a
// ScheduleWakeup tool_use is suppressed (no TurnCompleteEvent emitted)
// and a safety timer is started.
func TestScheduleWakeup_SuppressesTurn(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Bash", Input: map[string]interface{}{"command": "echo work"}},
		{ID: "tool-2", Name: "ScheduleWakeup", Input: map[string]interface{}{
			"delaySeconds": float64(120),
			"prompt":       "check status",
			"reason":       "waiting for reviews",
		}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "work", IsError: &isErrFalse},
		{ToolUseID: "tool-2", Content: "scheduled", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	// hasScheduleWakeup must return true.
	turn := s.turnManager.CurrentTurn()
	if !turn.hasScheduleWakeup() {
		t.Fatal("hasScheduleWakeup should return true")
	}

	// handleResult should suppress the turn.
	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
		Usage: protocol.UsageDetails{
			InputTokens:  100,
			OutputTokens: 50,
		},
		TotalCostUSD: 0.01,
	}
	s.handleResult(resultMsg)

	// Should NOT get a TurnCompleteEvent.
	select {
	case event := <-s.events:
		if _, ok := event.(TurnCompleteEvent); ok {
			t.Error("TurnCompleteEvent should NOT be emitted — turn should be suppressed for ScheduleWakeup")
		}
	case <-time.After(100 * time.Millisecond):
		// Good — suppressed.
	}

	// Verify wakeup state is active.
	s.mu.RLock()
	timerActive := s.wakeupState.timer != nil
	suppActive := s.wakeupState.active
	origTurn := s.wakeupState.suppressedTurnNumber
	s.mu.RUnlock()

	if !timerActive {
		t.Error("expected wakeupState.timer to be active")
	}
	if !suppActive {
		t.Error("expected wakeupState.active to be true")
	}
	if origTurn != 1 {
		t.Errorf("expected suppressedTurnNumber=1, got %d", origTurn)
	}

	// Clean up: stop safety timer.
	s.mu.Lock()
	if s.wakeupState.timer != nil {
		s.wakeupState.timer.Stop()
	}
	s.mu.Unlock()
}

// TestScheduleWakeup_ContinuationCompletesTurn verifies that when a
// continuation turn arrives (without ScheduleWakeup), the suppressed turn
// is completed under the original turn number.
func TestScheduleWakeup_ContinuationCompletesTurn(t *testing.T) {
	s := newTestSession(t)

	// Turn 1: agent calls ScheduleWakeup.
	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "ScheduleWakeup", Input: map[string]interface{}{
			"delaySeconds": float64(60),
			"prompt":       "check again",
			"reason":       "waiting",
		}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "scheduled", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg1 := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
		Usage: protocol.UsageDetails{
			InputTokens:  100,
			OutputTokens: 50,
		},
		TotalCostUSD: 0.01,
	}
	s.handleResult(resultMsg1)

	// Verify suppression is active.
	s.mu.RLock()
	suppActive := s.wakeupState.active
	s.mu.RUnlock()
	if !suppActive {
		t.Fatal("wakeup suppression should be active after ScheduleWakeup turn")
	}

	// Simulate the continuation turn (no ScheduleWakeup this time).
	// The CLI injects a new assistant turn after the wakeup fires.
	// Start a new turn so the ScheduleWakeup block from the prior turn is gone.
	s.turnManager.StartTurn("continuation-turn")
	s.turnManager.AppendContentBlock(ContentBlock{
		Type:      ContentBlockTypeToolUse,
		ToolUseID: "tool-2",
		ToolName:  "Bash",
		ToolInput: map[string]interface{}{"command": "echo done"},
	})
	s.turnManager.AppendText("All checks passed.")

	// Transition state machine back to processing for the continuation result.
	_ = s.state.Transition(TransitionUserMessageSent)

	resultMsg2 := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
		Usage: protocol.UsageDetails{
			InputTokens:  200,
			OutputTokens: 100,
		},
		TotalCostUSD: 0.02,
	}
	s.handleResult(resultMsg2)

	// Now we SHOULD get a TurnCompleteEvent with the original turn number.
	// Drain non-TurnCompleteEvents (e.g. CLIToolResultEvent) before checking.
	var tc TurnCompleteEvent
	deadline := time.After(time.Second)
	for {
		var found bool
		select {
		case event := <-s.events:
			if tce, ok := event.(TurnCompleteEvent); ok {
				tc = tce
				found = true
			}
			// else keep draining
		case <-deadline:
			t.Fatal("expected TurnCompleteEvent for continuation turn")
		}
		if found {
			break
		}
	}
	// Turn number should be the original suppressed turn (1), not the
	// continuation turn's number.
	if tc.TurnNumber != 1 {
		t.Errorf("expected TurnNumber=1 (original suppressed), got %d", tc.TurnNumber)
	}
	if !tc.Success {
		t.Error("expected success=true")
	}
	// Usage should include both the suppressed turn and the continuation.
	expectedInput := int(100 + 200)
	if tc.Usage.InputTokens != expectedInput {
		t.Errorf("expected accumulated InputTokens=%d, got %d", expectedInput, tc.Usage.InputTokens)
	}

	// Wakeup state should be cleared.
	s.mu.RLock()
	suppActiveAfter := s.wakeupState.active
	s.mu.RUnlock()
	if suppActiveAfter {
		t.Error("wakeup suppression should be cleared after continuation")
	}
}

// TestScheduleWakeup_SafetyTimerCompletesTurn verifies that the safety
// timer fires and completes the turn if no continuation arrives.
func TestScheduleWakeup_SafetyTimerCompletesTurn(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "ScheduleWakeup", Input: map[string]interface{}{
			"delaySeconds": float64(60),
			"prompt":       "check",
			"reason":       "waiting",
		}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "scheduled", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	// Override the safety timer to a very short duration for testing.
	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{
		Type:    "result",
		IsError: false,
		Usage: protocol.UsageDetails{
			InputTokens:  100,
			OutputTokens: 50,
		},
		TotalCostUSD: 0.01,
	}
	s.handleResult(resultMsg)

	// Replace the safety timer with a short one.
	s.mu.Lock()
	if s.wakeupState.timer != nil {
		s.wakeupState.timer.Stop()
	}
	safetyResult := TurnResult{TurnNumber: 1, Success: true}
	s.wakeupState.timer = time.AfterFunc(50*time.Millisecond, func() {
		s.completeWakeupSuppressedTurn(safetyResult)
	})
	s.mu.Unlock()

	// Wait for the safety timer to fire, draining non-TurnCompleteEvents.
	var tc TurnCompleteEvent
	deadline := time.After(time.Second)
	for {
		var found bool
		select {
		case event := <-s.events:
			if tce, ok := event.(TurnCompleteEvent); ok {
				tc = tce
				found = true
			}
		case <-deadline:
			t.Fatal("safety timer should have fired and completed the turn")
		}
		if found {
			break
		}
	}
	if tc.TurnNumber != 1 {
		t.Errorf("expected TurnNumber=1, got %d", tc.TurnNumber)
	}
}

// TestScheduleWakeup_ErrorTurnNotSuppressed verifies that an error result
// with ScheduleWakeup is NOT suppressed.
func TestScheduleWakeup_ErrorTurnNotSuppressed(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "ScheduleWakeup", Input: map[string]interface{}{
			"delaySeconds": float64(120),
			"prompt":       "check",
			"reason":       "waiting",
		}},
	}
	simulateAssistantToolUse(s, tools)

	isErrFalse := false
	results := []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "scheduled", IsError: &isErrFalse},
	}
	simulateUserToolResults(t, s, results)

	_ = s.state.Transition(TransitionUserMessageSent)
	resultMsg := protocol.ResultMessage{
		Type:    "result",
		IsError: true,
		Result:  "something went wrong",
	}
	s.handleResult(resultMsg)

	// Error results should NOT be suppressed. Drain non-TurnCompleteEvents.
	var tc TurnCompleteEvent
	deadline := time.After(time.Second)
	for {
		var found bool
		select {
		case event := <-s.events:
			if tce, ok := event.(TurnCompleteEvent); ok {
				tc = tce
				found = true
			}
		case <-deadline:
			t.Fatal("error result with ScheduleWakeup should complete immediately, not suppress")
		}
		if found {
			break
		}
	}
	if tc.Success {
		t.Error("expected success=false for error result")
	}

	// Wakeup state should NOT be active.
	s.mu.RLock()
	suppActive := s.wakeupState.active
	s.mu.RUnlock()
	if suppActive {
		t.Error("wakeup suppression should not be active for error results")
	}
}
