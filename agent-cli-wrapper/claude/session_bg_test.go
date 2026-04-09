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

// simulateAssistantToolUse registers tool_use blocks with the session so
// that handleUser can look them up via FindToolByID.
func simulateAssistantToolUse(s *Session, tools []protocol.ToolUseBlock) {
	// Start a turn and register each tool.
	s.turnManager.StartTurn("test")
	for _, tb := range tools {
		tool := s.turnManager.GetOrCreateTool(tb.ID, tb.Name)
		tool.Input = tb.Input
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

	// The counter should be 0: cancelled bg tools should not be counted.
	s.mu.RLock()
	pending := s.bgTasksPendingSinceLastResult
	s.mu.RUnlock()

	if pending != 0 {
		t.Errorf("expected bgTasksPendingSinceLastResult=0, got %d", pending)
	}

	// Now simulate handleResult — should complete the turn normally, not suppress.
	// Transition to processing first.
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

	s.mu.RLock()
	pending := s.bgTasksPendingSinceLastResult
	s.mu.RUnlock()

	if pending != 1 {
		t.Errorf("expected bgTasksPendingSinceLastResult=1, got %d", pending)
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
	timerActive := s.bgSafetyTimer != nil
	suppActive := s.bgTurnSuppressionActive
	s.mu.RUnlock()

	if !timerActive {
		t.Error("expected bgSafetyTimer to be active")
	}
	if !suppActive {
		t.Error("expected bgTurnSuppressionActive to be true")
	}

	// Clean up: stop the safety timer to avoid leaks.
	s.mu.Lock()
	if s.bgSafetyTimer != nil {
		s.bgSafetyTimer.Stop()
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
	// This should be a no-op; no second TurnCompleteEvent should be emitted.
	// The session state is now StateReady, so handleResult should return early.
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

	// Only tool-1 should be counted.
	s.mu.RLock()
	pending := s.bgTasksPendingSinceLastResult
	s.mu.RUnlock()

	if pending != 1 {
		t.Errorf("expected bgTasksPendingSinceLastResult=1 (only non-error bg tool), got %d", pending)
	}

	// Clean up.
	s.mu.Lock()
	s.bgTasksPendingSinceLastResult = 0
	s.mu.Unlock()
}
