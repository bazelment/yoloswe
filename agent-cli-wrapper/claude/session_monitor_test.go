package claude

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

var notErr = false // reusable *bool for ToolResultBlock.IsError across tests

// makeSystemMessage builds a protocol.SystemMessage from a JSON map by
// round-tripping through UnmarshalJSON so the raw payload is captured for
// DecodePayload to re-decode typed sub-payloads.
func makeSystemMessage(t *testing.T, v map[string]interface{}) protocol.SystemMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var msg protocol.SystemMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

// expectNoTurnComplete asserts no TurnCompleteEvent arrives within the window.
func expectNoTurnComplete(t *testing.T, events <-chan Event, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case event := <-events:
			if _, ok := event.(TurnCompleteEvent); ok {
				t.Fatal("unexpected TurnCompleteEvent — turn should still be suppressed")
			}
		case <-deadline:
			return
		}
	}
}

// TestMonitor_HappyPath verifies the end-to-end Monitor lifecycle:
// a Monitor tool_use → task_started → ResultMessage is suppressed → terminal
// task_updated releases the suppression and fires TurnCompleteEvent.
func TestMonitor_HappyPath(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{
			"command":     "sleep 60",
			"description": "wait for reviews",
			"timeout_ms":  float64(300000),
		}},
	}
	simulateAssistantToolUse(s, tools)

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started (task task-abc, timeout 300000ms).", IsError: &notErr},
	})

	// task_started arrives.
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type":        "system",
		"subtype":     "task_started",
		"session_id":  "sess-1",
		"uuid":        "evt-1",
		"task_id":     "task-abc",
		"tool_use_id": "tool-1",
		"description": "wait for reviews",
		"task_type":   "monitor",
	}))

	// shouldSuppressForBgTasks must be true — Monitor is in backgroundTools.
	turn := s.turnManager.CurrentTurn()
	if !turn.shouldSuppressForBgTasks() {
		t.Error("shouldSuppressForBgTasks should return true for a Monitor tool")
	}
	if !s.turnManager.HasLiveTasks() {
		t.Error("expected HasLiveTasks=true after task_started")
	}

	// ResultMessage arrives — should suppress, not emit TurnCompleteEvent.
	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})

	expectNoTurnComplete(t, s.events, 100*time.Millisecond)

	s.mu.RLock()
	suppActive := s.bgState.active
	heldTurn := 0
	if s.bgState.heldResult != nil {
		heldTurn = s.bgState.heldResult.TurnNumber
	}
	s.mu.RUnlock()
	if !suppActive {
		t.Error("expected suppression to be active after Monitor ResultMessage")
	}
	if heldTurn != turn.Number {
		t.Errorf("heldResult.TurnNumber=%d, want %d", heldTurn, turn.Number)
	}

	// Terminal task_updated releases the suppression.
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type":       "system",
		"subtype":    "task_updated",
		"session_id": "sess-1",
		"uuid":       "evt-2",
		"task_id":    "task-abc",
		"patch": map[string]interface{}{
			"status":   "completed",
			"end_time": float64(1234567890),
		},
	}))

	// TurnCompleteEvent should now fire.
	waitForTurnComplete(t, s.events, time.Second)

	s.mu.RLock()
	stillActive := s.bgState.active
	s.mu.RUnlock()
	if stillActive {
		t.Error("bgState.active should be cleared after release")
	}
	if s.turnManager.HasLiveTasks() {
		t.Error("live tasks should be empty after terminal task_updated")
	}
}

// TestMonitor_FailedTaskReleases verifies that task_updated:failed also
// releases suppression — a failed bg task still signals the turn is done.
func TestMonitor_FailedTaskReleases(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{"command": "false"}},
	}
	simulateAssistantToolUse(s, tools)

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started.", IsError: &notErr},
	})
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u1",
		"task_id": "task-abc", "tool_use_id": "tool-1", "task_type": "monitor",
	}))

	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})
	expectNoTurnComplete(t, s.events, 50*time.Millisecond)

	errStr := "subprocess exited nonzero"
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_updated", "session_id": "s", "uuid": "u2",
		"task_id": "task-abc",
		"patch": map[string]interface{}{
			"status": "failed",
			"error":  errStr,
		},
	}))

	waitForTurnComplete(t, s.events, time.Second)
}

// TestMonitor_TwoTasksReleaseRequiresBoth verifies that when two Monitor tasks
// are live, only the second terminal task_updated releases the suppression.
func TestMonitor_TwoTasksReleaseRequiresBoth(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{"command": "sleep 10"}},
		{ID: "tool-2", Name: "Monitor", Input: map[string]interface{}{"command": "sleep 10"}},
	}
	simulateAssistantToolUse(s, tools)

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started.", IsError: &notErr},
		{ToolUseID: "tool-2", Content: "Monitor started.", IsError: &notErr},
	})
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u1",
		"task_id": "task-A", "tool_use_id": "tool-1", "task_type": "monitor",
	}))
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u2",
		"task_id": "task-B", "tool_use_id": "tool-2", "task_type": "monitor",
	}))

	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})
	expectNoTurnComplete(t, s.events, 50*time.Millisecond)

	// First task completes — still suppressed.
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_updated", "session_id": "s", "uuid": "u3",
		"task_id": "task-A",
		"patch":   map[string]interface{}{"status": "completed"},
	}))
	expectNoTurnComplete(t, s.events, 50*time.Millisecond)
	if !s.turnManager.HasLiveTasks() {
		t.Error("expected one live task remaining after first terminal")
	}

	// Second task completes — now release.
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_updated", "session_id": "s", "uuid": "u4",
		"task_id": "task-B",
		"patch":   map[string]interface{}{"status": "completed"},
	}))
	waitForTurnComplete(t, s.events, time.Second)
}

// TestMonitor_MixedWithNonBgDoesNotSuppress verifies that when a turn has
// both a Monitor tool and a regular non-bg tool, the ResultMessage represents
// real completion and must not be suppressed. task_started for the Monitor
// still registers a live task, but shouldSuppressForBgTasks short-circuits
// on the non-bg tool.
func TestMonitor_MixedWithNonBgDoesNotSuppress(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{"command": "sleep 60"}},
		{ID: "tool-2", Name: "Read", Input: map[string]interface{}{"file_path": "/tmp/x"}},
	}
	simulateAssistantToolUse(s, tools)

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started.", IsError: &notErr},
		{ToolUseID: "tool-2", Content: "file contents", IsError: &notErr},
	})
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u1",
		"task_id": "task-A", "tool_use_id": "tool-1", "task_type": "monitor",
	}))

	turn := s.turnManager.CurrentTurn()
	if turn.shouldSuppressForBgTasks() {
		t.Error("non-bg Read tool present → shouldSuppressForBgTasks must be false")
	}

	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})

	waitForTurnComplete(t, s.events, time.Second)
}

// TestMonitor_HonorsTimeoutMs verifies that Monitor's tool_use.timeout_ms
// extends the safety timer past the configured default.
func TestMonitor_HonorsTimeoutMs(t *testing.T) {
	// Configure a tiny default. Monitor carries a much larger explicit timeout.
	s := newTestSession(t, WithBgTaskSafetyTimeout(10*time.Millisecond))

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{
			"command":    "sleep 60",
			"timeout_ms": float64(600000), // 10 minutes
		}},
	}
	simulateAssistantToolUse(s, tools)

	// longestBackgroundToolTimeoutMs should pick up 600000.
	turn := s.turnManager.CurrentTurn()
	if got := turn.longestBackgroundToolTimeoutMs(); got != 600000 {
		t.Errorf("longestBackgroundToolTimeoutMs=%d, want 600000", got)
	}

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started.", IsError: &notErr},
	})
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u1",
		"task_id": "task-A", "tool_use_id": "tool-1", "task_type": "monitor",
	}))

	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})

	// If the Monitor timeout_ms were ignored, the 10ms default would fire almost
	// instantly and emit TurnCompleteEvent. Observe a window much larger than
	// that to confirm suppression survives.
	expectNoTurnComplete(t, s.events, 150*time.Millisecond)

	// Clean up the armed timer.
	s.mu.Lock()
	s.bgState.reset()
	s.mu.Unlock()
}

// TestMonitor_NonTerminalStatusDoesNotRelease verifies task_updated with
// non-terminal status (pending/running) does not release suppression.
func TestMonitor_NonTerminalStatusDoesNotRelease(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{"command": "sleep 60"}},
	}
	simulateAssistantToolUse(s, tools)

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started.", IsError: &notErr},
	})
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u1",
		"task_id": "task-A", "tool_use_id": "tool-1", "task_type": "monitor",
	}))
	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})

	// Running status patch — must not release.
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_updated", "session_id": "s", "uuid": "u2",
		"task_id": "task-A",
		"patch":   map[string]interface{}{"status": "running"},
	}))

	expectNoTurnComplete(t, s.events, 100*time.Millisecond)

	if !s.turnManager.HasLiveTasks() {
		t.Error("expected task-A still live after non-terminal patch")
	}

	s.mu.Lock()
	s.bgState.reset()
	s.mu.Unlock()
}

// TestMonitor_TaskNotificationAlsoReleases verifies task_notification provides
// a belt-and-suspenders release path: if task_updated:completed never arrives
// but task_notification does, suppression still releases.
func TestMonitor_TaskNotificationAlsoReleases(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{"command": "echo done"}},
	}
	simulateAssistantToolUse(s, tools)

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started.", IsError: &notErr},
	})
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u1",
		"task_id": "task-A", "tool_use_id": "tool-1", "task_type": "monitor",
	}))
	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})
	expectNoTurnComplete(t, s.events, 50*time.Millisecond)

	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_notification", "session_id": "s", "uuid": "u2",
		"task_id": "task-A", "tool_use_id": "tool-1",
		"status": "completed",
	}))

	waitForTurnComplete(t, s.events, time.Second)
}

// TestMonitor_TaskCompletesBeforeResultMessage verifies the race where
// task_updated:completed arrives before ResultMessage. Without the fix,
// handleResult would activate suppression with an empty liveTasks set and
// no future task event would call maybeReleaseSuppression, leaving the turn
// stuck until the safety timer fires.
func TestMonitor_TaskCompletesBeforeResultMessage(t *testing.T) {
	s := newTestSession(t)

	tools := []protocol.ToolUseBlock{
		{ID: "tool-1", Name: "Monitor", Input: map[string]interface{}{"command": "true"}},
	}
	simulateAssistantToolUse(s, tools)

	simulateUserToolResults(t, s, []protocol.ToolResultBlock{
		{ToolUseID: "tool-1", Content: "Monitor started.", IsError: &notErr},
	})
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_started", "session_id": "s", "uuid": "u1",
		"task_id": "task-fast", "tool_use_id": "tool-1", "task_type": "monitor",
	}))

	// task_updated:completed arrives BEFORE the ResultMessage — the fast-exit race.
	s.handleSystem(makeSystemMessage(t, map[string]interface{}{
		"type": "system", "subtype": "task_updated", "session_id": "s", "uuid": "u2",
		"task_id": "task-fast", "tool_use_id": "tool-1",
		"patch": map[string]interface{}{"status": "completed"},
	}))

	// Now the ResultMessage arrives. All live tasks are already gone; suppression
	// must finalize immediately without waiting for the safety timer.
	_ = s.state.Transition(TransitionUserMessageSent)
	s.handleResult(protocol.ResultMessage{Type: "result", IsError: false})

	waitForTurnComplete(t, s.events, time.Second)
}
