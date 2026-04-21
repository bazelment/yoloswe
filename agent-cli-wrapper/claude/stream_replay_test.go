package claude

import (
	"bufio"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// drainEvents non-blockingly drains the session's event channel into a slice.
// Used by replay tests that synchronously push CLI protocol frames into
// handleLine and immediately inspect the resulting events.
func drainEvents(t *testing.T, s *Session) []Event {
	t.Helper()
	var out []Event
	for {
		select {
		case ev := <-s.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// countEvents returns how many events of the given concrete type exist in
// the slice. Used as a coarse assertion that the raw-stream API emits one
// envelope per CLI message without coalescing or dropping.
func countEvents[T Event](events []Event) int {
	n := 0
	for _, e := range events {
		if _, ok := e.(T); ok {
			n++
		}
	}
	return n
}

// TestStream_Replay_RawEventsAlwaysEmitted is the PR-1 smoke test for the new
// raw-streaming API (Query + AssistantMessageEvent + ResultMessageEvent +
// UserMessageEvent). Feeds a minimal three-line CLI protocol sequence into
// handleLine — assistant text, user tool_result (simulating a CLI-executed
// tool), and ResultMessage — and asserts the raw events are present
// regardless of any wrapper-level coalescing.
func TestStream_Replay_RawEventsAlwaysEmitted(t *testing.T) {
	s := newTestSession(t)
	// Start a turn so handleAssistant/handleUser see a current turn number.
	s.turnManager.StartTurn("hello")

	// Assistant message with a single text block.
	assistantLine := []byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"text","text":"hi"}]},"session_id":"s1","uuid":"u1"}`)
	s.handleLine(assistantLine)

	// User message containing a tool_result (CLI auto-executed a tool).
	userLine := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},"session_id":"s1","uuid":"u2"}`)
	s.handleLine(userLine)

	// ResultMessage — CLI's turn boundary.
	resultLine := []byte(`{"type":"result","subtype":"success","session_id":"s1","uuid":"u3","result":"hi","num_turns":1,"duration_ms":10,"duration_api_ms":5,"total_cost_usd":0.001,"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	s.handleLine(resultLine)

	events := drainEvents(t, s)

	// Each top-level CLI message produces exactly one raw envelope event,
	// independent of any existing TurnComplete coalescing path.
	require.Equal(t, 1, countEvents[AssistantMessageEvent](events),
		"exactly one AssistantMessageEvent per assistant CLI frame")
	require.Equal(t, 1, countEvents[UserMessageEvent](events),
		"exactly one UserMessageEvent per user CLI frame")
	require.Equal(t, 1, countEvents[ResultMessageEvent](events),
		"exactly one ResultMessageEvent per result CLI frame")

	// Order check: raw envelopes fire in protocol order (assistant → user →
	// result). Coalesced events (TextEvent, CLIToolResultEvent,
	// TurnCompleteEvent) can still interleave — we only assert the raw
	// envelopes' relative order here.
	var seen []EventType
	for _, e := range events {
		switch e.(type) {
		case AssistantMessageEvent, UserMessageEvent, ResultMessageEvent:
			seen = append(seen, e.Type())
		}
	}
	require.Equal(t, []EventType{
		EventTypeAssistantMessage,
		EventTypeUserMessage,
		EventTypeResultMessage,
	}, seen, "raw envelopes preserve protocol order")
}

// TestStream_Replay_ResultMessageEvent_Subtype asserts ResultMessageEvent
// carries subtype and IsError faithfully. Consumers use these to
// distinguish success vs error vs max-turns-exceeded vs bg-auto-continued
// turns without running wrapper-level suppression.
func TestStream_Replay_ResultMessageEvent_Subtype(t *testing.T) {
	s := newTestSession(t)
	s.turnManager.StartTurn("prompt")

	errorLine := []byte(`{"type":"result","subtype":"error_max_turns","session_id":"s1","uuid":"u_err","is_error":true,"errors":["max turns exceeded"],"num_turns":3,"duration_ms":1000,"duration_api_ms":800,"total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	s.handleLine(errorLine)

	events := drainEvents(t, s)
	var rm *ResultMessageEvent
	for _, e := range events {
		if got, ok := e.(ResultMessageEvent); ok {
			rm = &got
			break
		}
	}
	require.NotNil(t, rm, "ResultMessageEvent must fire for error turns")
	require.Equal(t, "error_max_turns", rm.Subtype)
	require.True(t, rm.IsError)
	require.Error(t, rm.Error, "Error field populated for error turns")
	require.Equal(t, 3, rm.NumTurns)
	require.Equal(t, int64(1000), rm.DurationMs)
	require.InDelta(t, 0.05, rm.TotalCostUSD, 1e-9)
}

// replaySessionFromLines feeds each raw line through handleLine and returns
// the events emitted. Newlines are trimmed before injection.
func replaySessionFromLines(t *testing.T, lines [][]byte) []Event {
	t.Helper()
	s := newTestSession(t)
	s.turnManager.StartTurn("replay")
	for _, line := range lines {
		s.handleLine(line)
	}
	return drainEvents(t, s)
}

// TestStream_Replay_R2_MixedSyncBgTurn (R2 in the plan matrix) — a single CLI
// turn that runs a sync tool_use, launches a bg Monitor, and emits
// ResultMessage before the Monitor task completes. The raw stream must contain
// both tool_use events AND the ResultMessage, independent of any later task
// settlement. Post-refactor: wrapper never coalesces — consumer policy in
// logicalTurnState decides done-ness.
func TestStream_Replay_R2_MixedSyncBgTurn(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_sync","name":"Bash","input":{"command":"pwd"}}]},"session_id":"s","uuid":"a1"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_sync","content":"/tmp"}]},"session_id":"s","uuid":"u1"}`),
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_mon","name":"Monitor","input":{"command":"tail -f","description":"watch","timeout_ms":60000}}]},"session_id":"s","uuid":"a2"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_mon","description":"watch"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_mon","content":"Monitor started (task t1)"}]},"session_id":"s","uuid":"u2"}`),
		[]byte(`{"type":"result","subtype":"success","session_id":"s","uuid":"r1","result":"armed","num_turns":1,"duration_ms":100,"duration_api_ms":80,"total_cost_usd":0.01,"stop_reason":"end_turn"}`),
	}
	events := replaySessionFromLines(t, lines)

	require.Equal(t, 2, countEvents[AssistantMessageEvent](events),
		"both assistant messages (text+tool_use and tool_use-only) emit raw envelopes")
	require.Equal(t, 2, countEvents[UserMessageEvent](events),
		"both tool_result user messages emit raw envelopes")
	require.Equal(t, 1, countEvents[ResultMessageEvent](events),
		"ResultMessage fires exactly once for the CLI turn — no coalescing")
	require.GreaterOrEqual(t, countEvents[TaskStartedEvent](events), 1,
		"task_started is always surfaced raw to consumers")
}

// TestStream_Replay_R3_TwoParallelMonitorsOneFailsFast (R3) — two Monitor
// tool_uses in the same assistant message; the first fails quickly, the
// second runs long. The raw stream must emit both task_started and both
// task_notification events with distinct statuses.
func TestStream_Replay_R3_TwoParallelMonitorsOneFailsFast(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"Monitor","input":{"command":"bad-model","description":"a","timeout_ms":60000}},{"type":"tool_use","id":"toolu_b","name":"Monitor","input":{"command":"tail -f","description":"b","timeout_ms":60000}}]},"session_id":"s","uuid":"a1"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts1","task_id":"ta","task_type":"bg_tool","tool_use_id":"toolu_a","description":"a"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts2","task_id":"tb","task_type":"bg_tool","tool_use_id":"toolu_b","description":"b"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"Monitor started ta"},{"type":"tool_result","tool_use_id":"toolu_b","content":"Monitor started tb"}]},"session_id":"s","uuid":"u1"}`),
		[]byte(`{"type":"result","subtype":"success","session_id":"s","uuid":"r1","result":"armed","num_turns":1,"duration_ms":50,"duration_api_ms":40,"total_cost_usd":0.005,"stop_reason":"end_turn"}`),
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn1","task_id":"ta","tool_use_id":"toolu_a","status":"failed","summary":"HTTP 400 bad model"}`),
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn2","task_id":"tb","tool_use_id":"toolu_b","status":"completed","summary":"done"}`),
	}
	events := replaySessionFromLines(t, lines)

	starts := 0
	notifs := map[string]int{}
	for _, e := range events {
		if _, ok := e.(TaskStartedEvent); ok {
			starts++
		}
		if n, ok := e.(TaskNotificationEvent); ok {
			notifs[n.Status]++
		}
	}
	require.Equal(t, 2, starts, "both Monitors fire task_started")
	require.Equal(t, 1, notifs["failed"], "one failure notification")
	require.Equal(t, 1, notifs["completed"], "one completion notification")
}

// TestStream_Replay_R4_PureBgBashRunInBackground (R4) — a Bash tool_use with
// run_in_background:true. The CLI registers it as a bg task the same way as
// Monitor. Raw events must include task_started with a tool_use_id that
// matches the bg Bash block.
func TestStream_Replay_R4_PureBgBashRunInBackground(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_bgb","name":"Bash","input":{"command":"sleep 30","run_in_background":true}}]},"session_id":"s","uuid":"a1"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"tb","task_type":"bg_tool","tool_use_id":"toolu_bgb","description":"Bash (bg)"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bgb","content":"started in background"}]},"session_id":"s","uuid":"u1"}`),
		[]byte(`{"type":"result","subtype":"success","session_id":"s","uuid":"r1","result":"armed","num_turns":1,"duration_ms":50,"duration_api_ms":40,"total_cost_usd":0.005,"stop_reason":"end_turn"}`),
	}
	events := replaySessionFromLines(t, lines)

	var ts *TaskStartedEvent
	for _, e := range events {
		if t, ok := e.(TaskStartedEvent); ok {
			ev := t
			ts = &ev
			break
		}
	}
	require.NotNil(t, ts, "task_started emitted for bg Bash")
	require.NotNil(t, ts.ToolUseID)
	require.Equal(t, "toolu_bgb", *ts.ToolUseID)
}

// TestStream_Replay_R5_MixedMonitorAndBgBash (R5) — one assistant turn
// that launches both a Monitor and a bg-Bash. Both must fire task_started
// events; the raw stream preserves both tool_use blocks.
func TestStream_Replay_R5_MixedMonitorAndBgBash(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_m","name":"Monitor","input":{"command":"watch"}},{"type":"tool_use","id":"toolu_bgb","name":"Bash","input":{"command":"sleep 10","run_in_background":true}}]},"session_id":"s","uuid":"a1"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts1","task_id":"tm","task_type":"bg_tool","tool_use_id":"toolu_m"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts2","task_id":"tb","task_type":"bg_tool","tool_use_id":"toolu_bgb"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_m","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_bgb","content":"ok"}]},"session_id":"s","uuid":"u1"}`),
		[]byte(`{"type":"result","subtype":"success","session_id":"s","uuid":"r1","result":"armed","num_turns":1,"duration_ms":50,"duration_api_ms":40,"total_cost_usd":0.005,"stop_reason":"end_turn"}`),
	}
	events := replaySessionFromLines(t, lines)
	require.Equal(t, 2, countEvents[TaskStartedEvent](events), "both bg tools fire task_started")
}

// TestStream_Replay_R6_TaskNotificationMissingToolUseID (R6) — the CLI's
// task_notification occasionally ships without tool_use_id. The raw event
// must still be emitted; consumers that need to correlate to a tool_use
// use the task_id↔tool_use_id map built from task_started.
func TestStream_Replay_R6_TaskNotificationMissingToolUseID(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_x"}`),
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"t1","status":"completed","summary":"done"}`),
	}
	events := replaySessionFromLines(t, lines)

	var notif *TaskNotificationEvent
	for _, e := range events {
		if n, ok := e.(TaskNotificationEvent); ok {
			ev := n
			notif = &ev
			break
		}
	}
	require.NotNil(t, notif, "task_notification always surfaced even without tool_use_id")
	require.Equal(t, "t1", notif.TaskID)
	require.Equal(t, "completed", notif.Status)
	// tool_use_id may be nil or empty — the wrapper does not fabricate one.
	if notif.ToolUseID != nil {
		require.Equal(t, "", *notif.ToolUseID, "missing tool_use_id surfaces as empty, not as the task_started one")
	}
}

// TestStream_Replay_R7_ReorderedTaskStartedAfterNotification (R7) — the CLI
// sometimes ships task_started after its matching task_notification for
// very short tasks. Both events still emit raw; consumer-level policy
// handles the reorder.
func TestStream_Replay_R7_ReorderedTaskStartedAfterNotification(t *testing.T) {
	lines := [][]byte{
		// task_notification BEFORE task_started (CLI race condition)
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"t1","tool_use_id":"toolu_x","status":"completed","summary":"done"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_x"}`),
	}
	events := replaySessionFromLines(t, lines)

	// Both raw events emitted, in the order they arrived.
	var order []EventType
	for _, e := range events {
		switch e.(type) {
		case TaskStartedEvent, TaskNotificationEvent:
			order = append(order, e.Type())
		}
	}
	require.Equal(t, []EventType{EventTypeTaskNotification, EventTypeTaskStarted}, order,
		"raw events preserve arrival order even when semantically reversed")
}

// TestStream_Replay_R8_BudgetExceededMidBg (R8) — a bg-heavy turn where the
// CLI surfaces a ResultMessage with subtype=error_budget_exceeded while a
// task is still live. Raw stream must emit the error-flavored ResultMessage
// AND any task_notification that still arrives after — the wrapper does not
// swallow either. Consumer policy (turn_state.go C8) decides how to unwind.
func TestStream_Replay_R8_BudgetExceededMidBg(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_bg","name":"Monitor","input":{"command":"tail -f","description":"watch","timeout_ms":60000}}]},"session_id":"s","uuid":"a1"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_bg"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bg","content":"Monitor started"}]},"session_id":"s","uuid":"u1"}`),
		// Budget exceeded: CLI emits ResultMessage with is_error + error subtype.
		[]byte(`{"type":"result","subtype":"error_budget_exceeded","session_id":"s","uuid":"r1","is_error":true,"errors":["budget exceeded"],"result":"","num_turns":1,"duration_ms":50,"duration_api_ms":40,"total_cost_usd":2.5,"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`),
		// Late-arriving task_notification (would have been dropped by the old
		// suppression path if the wrapper treated the error ResultMessage as
		// the turn's terminal state).
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"t1","tool_use_id":"toolu_bg","status":"killed","summary":"session budget exceeded — killed by runtime"}`),
	}
	events := replaySessionFromLines(t, lines)

	// Error-flavored ResultMessage emits raw.
	var rm *ResultMessageEvent
	for _, e := range events {
		if got, ok := e.(ResultMessageEvent); ok {
			ev := got
			rm = &ev
			break
		}
	}
	require.NotNil(t, rm, "ResultMessageEvent fires even on budget-exceeded")
	require.Equal(t, "error_budget_exceeded", rm.Subtype)
	require.True(t, rm.IsError)

	// Late task_notification is surfaced raw — no coalescing / dropping.
	require.Equal(t, 1, countEvents[TaskNotificationEvent](events),
		"task_notification surfaces even after the error ResultMessage — consumer decides what to do")
}

// TestStream_Replay_R9_BgMonitorToolError (R9) — Monitor sub-process errors
// (HTTP 400). The CLI emits task_notification with status=failed. Raw
// stream preserves both the error signal and a summary.
func TestStream_Replay_R9_BgMonitorToolError(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_x","description":"bad"}`),
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"t1","tool_use_id":"toolu_x","status":"failed","summary":"HTTP 400 invalid_request_error"}`),
	}
	events := replaySessionFromLines(t, lines)

	var notif *TaskNotificationEvent
	for _, e := range events {
		if n, ok := e.(TaskNotificationEvent); ok {
			ev := n
			notif = &ev
			break
		}
	}
	require.NotNil(t, notif)
	require.Equal(t, "failed", notif.Status)
	require.Contains(t, notif.Summary, "HTTP 400")
}

// TestStream_Replay_R10_MonitorTimeoutStatus (R10) — Monitor's own timeout_ms
// fires; the CLI SIGTERMs the subprocess and emits task_notification with
// status=timeout (or status=killed, depending on CLI version). The raw
// stream must emit whichever terminal status the CLI chose.
func TestStream_Replay_R10_MonitorTimeoutStatus(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_x"}`),
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"t1","tool_use_id":"toolu_x","status":"timeout","summary":"timed out after 5000ms"}`),
	}
	events := replaySessionFromLines(t, lines)

	var notif *TaskNotificationEvent
	for _, e := range events {
		if n, ok := e.(TaskNotificationEvent); ok {
			ev := n
			notif = &ev
			break
		}
	}
	require.NotNil(t, notif)
	require.Equal(t, "timeout", notif.Status, "timeout status is surfaced verbatim, not remapped")
}

// TestStream_Replay_R11_ScheduleWakeupContinuation (R11) — after
// ScheduleWakeup fires, the CLI emits a SystemMessage with subtype
// task_started that references the wakeup. Raw stream emits it as a
// TaskStartedEvent; consumer policy (if ScheduleWakeup is preserved)
// joins the continuation to the logical turn via shared task semantics.
func TestStream_Replay_R11_ScheduleWakeupContinuation(t *testing.T) {
	lines := [][]byte{
		// Initial Monitor tool_use
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_m","name":"Monitor","input":{"command":"watch"}}]},"session_id":"s","uuid":"a1"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"tm","task_type":"bg_tool","tool_use_id":"toolu_m"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_m","content":"ok"}]},"session_id":"s","uuid":"u1"}`),
		[]byte(`{"type":"result","subtype":"success","session_id":"s","uuid":"r1","result":"armed","num_turns":1,"duration_ms":50,"duration_api_ms":40,"total_cost_usd":0.005,"stop_reason":"end_turn"}`),
		// Later, after wakeup, task settles; CLI auto-continues with a new turn.
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"tm","tool_use_id":"toolu_m","status":"completed","summary":"woke up"}`),
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"text","text":"Wakeup handled."}]},"session_id":"s","uuid":"a2"}`),
		[]byte(`{"type":"result","subtype":"success","session_id":"s","uuid":"r2","result":"done","num_turns":1,"duration_ms":10,"duration_api_ms":5,"total_cost_usd":0.001,"stop_reason":"end_turn"}`),
	}
	events := replaySessionFromLines(t, lines)
	require.Equal(t, 2, countEvents[ResultMessageEvent](events),
		"both ResultMessages emit raw — wrapper never coalesces the wakeup continuation")
	require.Equal(t, 1, countEvents[TaskNotificationEvent](events))
}

// TestStream_Replay_R12_CloseSessionWhileBgLive (R12) — session.Stop() is
// called while a bg task is still live. Before Stop the raw stream has
// task_started but no task_notification; after Stop the events channel
// closes without emitting a synthetic terminal notification.
//
// Replay-level: we can't invoke Stop() without a real process, so we
// assert the simpler invariant: if no task_notification arrives, the
// wrapper never synthesizes one. Consumer-level policy treats the
// session close itself as "drain live tasks".
func TestStream_Replay_R12_CloseSessionWhileBgLive(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_x"}`),
		// No task_notification — simulates the "session closed while bg was live" case.
	}
	events := replaySessionFromLines(t, lines)
	require.Equal(t, 0, countEvents[TaskNotificationEvent](events),
		"wrapper never fabricates a terminal task_notification at close")
}

// TestStream_Replay_R14_INF401_ValidateRound2 (R14) — the regression test.
// Replays the real pr-polish-shaped session from testdata: sync Bash tool,
// two Monitor launches, end_turn ResultMessage, then task_notification
// events arrive (one failed, one completed), then a final ResultMessage
// for the auto-continued turn.
//
// Pre-refactor: wrapper would have coalesced the two ResultMessages into
// one TurnResult with HasLiveBackgroundWork=true while the bg was live,
// and jiradozer's guard would refuse to advance.
// Post-refactor: both ResultMessageEvents emit raw; the consumer
// (logicalTurnState) sees live tasks drain and returns only after the
// second ResultMessage. This test is the clearest pre/post regression
// signal: same fixture, same session code, different consumer outcome.
func TestStream_Replay_R14_INF401_ValidateRound2(t *testing.T) {
	f, err := os.Open("testdata/replay/inf401_validate_round2.jsonl")
	require.NoError(t, err, "INF-401 fixture must exist for the regression test")
	defer f.Close()

	s := newTestSession(t)
	s.turnManager.StartTurn("inf-401 round 2 replay")
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy since scanner reuses the underlying slice.
		s.handleLine(append([]byte(nil), line...))
	}
	require.NoError(t, scanner.Err())
	events := drainEvents(t, s)

	// Two CLI turns in the fixture: the end_turn after arming Monitors and
	// the auto-continuation after both tasks settled.
	require.Equal(t, 2, countEvents[ResultMessageEvent](events),
		"both CLI turns surface ResultMessageEvent raw — no coalescing")

	// The fixture carries two Monitor launches, with task_started for each.
	require.Equal(t, 2, countEvents[TaskStartedEvent](events),
		"task_started emits for both Monitors in round 2")

	// Both task_notifications arrive after the first ResultMessage, with
	// one failed (codex model error) and one completed (cursor success).
	statusCounts := map[string]int{}
	for _, e := range events {
		if n, ok := e.(TaskNotificationEvent); ok {
			statusCounts[n.Status]++
		}
	}
	require.Equal(t, 1, statusCounts["failed"], "one Monitor failed (bramble-codex HTTP 400)")
	require.Equal(t, 1, statusCounts["completed"], "one Monitor completed (cursor)")

	// Critical ordering invariant for the regression: the FIRST
	// ResultMessageEvent must fire BEFORE the task_notification events.
	// This is what the pre-refactor guard mis-interpreted as "turn done
	// with live bg work".
	var firstResultIdx, firstNotifIdx = -1, -1
	for i, e := range events {
		switch e.(type) {
		case ResultMessageEvent:
			if firstResultIdx == -1 {
				firstResultIdx = i
			}
		case TaskNotificationEvent:
			if firstNotifIdx == -1 {
				firstNotifIdx = i
			}
		}
	}
	require.GreaterOrEqual(t, firstResultIdx, 0, "first ResultMessageEvent present")
	require.GreaterOrEqual(t, firstNotifIdx, 0, "first TaskNotificationEvent present")
	require.Less(t, firstResultIdx, firstNotifIdx,
		"ResultMessage precedes task_notification — the condition that broke the old coalescing guard")
}

// TestStream_Replay_R15_PersistentMonitor (R15) — a persistent Monitor
// that never emits a terminal task_notification on its own. The raw
// stream emits task_started + task_updated progress events. Terminal
// only arrives when the caller invokes TaskStop (simulated here as a
// task_notification with status=killed). Gating semantics live entirely
// in the consumer.
func TestStream_Replay_R15_PersistentMonitor(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_p","description":"persistent watcher"}`),
		[]byte(`{"type":"system","subtype":"task_progress","session_id":"s","uuid":"tp1","task_id":"t1","summary":"line 1"}`),
		[]byte(`{"type":"system","subtype":"task_progress","session_id":"s","uuid":"tp2","task_id":"t1","summary":"line 2"}`),
		// Simulated TaskStop: CLI emits killed notification.
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"t1","tool_use_id":"toolu_p","status":"killed","summary":"stopped by user"}`),
	}
	events := replaySessionFromLines(t, lines)

	progressCount := 0
	var notifStatus string
	for _, e := range events {
		if _, ok := e.(TaskProgressEvent); ok {
			progressCount++
		}
		if n, ok := e.(TaskNotificationEvent); ok {
			notifStatus = n.Status
		}
	}
	require.Equal(t, 2, progressCount, "both progress events surfaced raw")
	require.Equal(t, "killed", notifStatus, "TaskStop is surfaced as status=killed")
}

// TestStream_Replay_R16_CancelledBgTool (R16) — a bg tool_use is cancelled
// mid-turn (user Ctrl-C or wrapper cancels). The CLI emits a tool_result
// with is_error:true before the terminal task_notification. The raw
// stream must preserve both. Consumer policy treats is_error tool_result
// as "don't wait on this tool_use" so logicalTurnState does not hang.
func TestStream_Replay_R16_CancelledBgTool(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-4-7","role":"assistant","content":[{"type":"tool_use","id":"toolu_c","name":"Monitor","input":{"command":"watch"}}]},"session_id":"s","uuid":"a1"}`),
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"tc","task_type":"bg_tool","tool_use_id":"toolu_c"}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_c","content":"cancelled","is_error":true}]},"session_id":"s","uuid":"u1"}`),
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"tc","tool_use_id":"toolu_c","status":"killed","summary":"cancelled"}`),
	}
	events := replaySessionFromLines(t, lines)

	var cancelledUserMsg *UserMessageEvent
	for _, e := range events {
		if u, ok := e.(UserMessageEvent); ok {
			ev := u
			cancelledUserMsg = &ev
			break
		}
	}
	require.NotNil(t, cancelledUserMsg)
	require.Len(t, cancelledUserMsg.Blocks, 1)
	require.True(t, cancelledUserMsg.Blocks[0].IsError, "tool_result is_error surfaced raw")
	require.Equal(t, 1, countEvents[TaskNotificationEvent](events),
		"terminal task_notification also fires for cancelled bg tool")
}

// TestStream_Replay_R17_TaskNotificationBeforeResultMessage (R17) — a
// pathological CLI ordering where task_notification arrives before the
// turn's ResultMessage (safety-timer race from #162 round 4). Both raw
// events must emit in arrival order; consumer policy (logicalTurnState)
// must not deadlock waiting for ResultMessage after the last notification.
func TestStream_Replay_R17_TaskNotificationBeforeResultMessage(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"system","subtype":"task_started","session_id":"s","uuid":"ts","task_id":"t1","task_type":"bg_tool","tool_use_id":"toolu_x"}`),
		// task_notification arrives BEFORE the ResultMessage for the turn
		[]byte(`{"type":"system","subtype":"task_notification","session_id":"s","uuid":"tn","task_id":"t1","tool_use_id":"toolu_x","status":"completed","summary":"fast-path"}`),
		[]byte(`{"type":"result","subtype":"success","session_id":"s","uuid":"r1","result":"ok","num_turns":1,"duration_ms":10,"duration_api_ms":5,"total_cost_usd":0.001,"stop_reason":"end_turn"}`),
	}
	events := replaySessionFromLines(t, lines)

	var order []EventType
	for _, e := range events {
		switch e.(type) {
		case TaskStartedEvent, TaskNotificationEvent, ResultMessageEvent:
			order = append(order, e.Type())
		}
	}
	require.Equal(t, []EventType{
		EventTypeTaskStarted,
		EventTypeTaskNotification,
		EventTypeResultMessage,
	}, order, "arrival order is preserved — consumer handles the race, not the wrapper")
}
