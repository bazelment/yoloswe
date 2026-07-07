package agent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// Helpers for building hand-crafted event sequences.

func strPtr(s string) *string { return &s }

func boolPtr(v bool) *bool { return &v }

func asstTextBlock(text string) claude.ContentBlock {
	return claude.TextBlock{Type: claude.ContentBlockTypeText, Text: text}
}

func toolUseBlock(id, name string, input map[string]interface{}) claude.ContentBlock {
	return claude.ToolUseBlock{
		Type:  claude.ContentBlockTypeToolUse,
		ID:    id,
		Name:  name,
		Input: input,
	}
}

func monitorToolUse(id string) claude.ContentBlock {
	return toolUseBlock(id, "Monitor", map[string]interface{}{
		"command": "tail -f /tmp/foo.log",
	})
}

func bgBashToolUse(id string) claude.ContentBlock {
	return toolUseBlock(id, "Bash", map[string]interface{}{
		"command":           "long-running-job",
		"run_in_background": true,
	})
}

// agentToolUse builds an Agent (sub-agent) tool_use block. The CLI dispatches
// it asynchronously and reports on it via TaskStarted/TaskNotification — the
// input carries no run_in_background flag, so background-ness must be inferred
// from the tool name (see backgroundToolNames).
func agentToolUse(id string) claude.ContentBlock {
	return toolUseBlock(id, "Agent", map[string]interface{}{
		"description": "sub-agent work",
		"prompt":      "do the thing",
	})
}

func toolResult(id string, isError bool) claude.ContentBlock {
	return claude.ToolResultBlock{
		Type:      claude.ContentBlockTypeToolResult,
		ToolUseID: id,
		IsError:   boolPtr(isError),
	}
}

func resultMessage(isError bool) claude.ResultMessageEvent {
	ev := claude.ResultMessageEvent{
		Subtype:      "success",
		TurnNumber:   1,
		NumTurns:     1,
		DurationMs:   100,
		TotalCostUSD: 0.001,
	}
	if isError {
		ev.Subtype = "error"
		ev.IsError = true
		ev.Error = errors.New("failed")
	}
	return ev
}

// endOfCLITurn feeds the paired ResultMessageEvent + TurnCompleteEvent that
// the wrapper emits at the end of every CLI turn. LogicalTurnDone gates on
// observing both, so tests must apply both to drive the state past a turn
// boundary.
func endOfCLITurn(s *logicalTurnState, isError bool) {
	rm := resultMessage(isError)
	s.Apply(rm)
	s.Apply(claude.TurnCompleteEvent{
		TurnNumber: rm.TurnNumber,
		Success:    !isError,
		DurationMs: rm.DurationMs,
		Usage:      rm.Usage,
	})
}

// C1: Pure-bg Monitor turn — ResultMessage fires before task completes.
// Execute must not report done until TaskNotificationEvent + continuation
// ResultMessage.
func TestTurnState_C1_PureBgMonitorTurn(t *testing.T) {
	s := newLogicalTurnState()

	// Turn 1: assistant launches Monitor; CLI fires ResultMessage immediately.
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:     "task1",
		ToolUseID:  strPtr("toolu_bg1"),
		TurnNumber: 1,
	})
	endOfCLITurn(s, false)

	require.False(t, s.LogicalTurnDone(),
		"pure-bg turn must not be done while bg task is still live")
	require.True(t, s.HasLiveTasks())

	// Turn 2 (auto-continuation): task completes, then CLI emits ResultMessage
	// and its paired TurnCompleteEvent.
	status := "completed"
	s.Apply(claude.TaskUpdatedEvent{TaskID: "task1", Status: &status})
	// Gap after terminal task event, before continuation ResultMessage: the
	// wave-1 (Result, TurnComplete) pair is still in state but a terminal
	// bg-task event has landed since. LogicalTurnDone MUST stay false —
	// otherwise streamTurn would return before draining the continuation
	// wave's Result + TurnComplete.
	require.False(t, s.LogicalTurnDone(),
		"terminal TaskUpdatedEvent must invalidate the prior wave's Result+TurnComplete — continuation wave has not arrived")
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg1"),
		Status:    "completed",
	})
	require.False(t, s.LogicalTurnDone(),
		"terminal TaskNotificationEvent must also keep LogicalTurnDone false until continuation wave arrives")
	// After the continuation ResultMessage, the state must require the
	// matching TurnCompleteEvent — a stale wave-1 TurnComplete alone must
	// not satisfy LogicalTurnDone.
	rm := resultMessage(false)
	rm.TurnNumber = 2
	s.Apply(rm)
	require.False(t, s.LogicalTurnDone(),
		"continuation ResultMessage must require a fresh TurnComplete")
	s.Apply(claude.TurnCompleteEvent{
		TurnNumber: 2,
		Success:    true,
		DurationMs: rm.DurationMs,
		Usage:      rm.Usage,
	})

	require.True(t, s.LogicalTurnDone(),
		"logical turn done once bg task terminal and continuation ResultMessage + TurnComplete arrived")
	require.False(t, s.HasLiveTasks())
}

// C2: Mixed sync+bg Monitor turn — INF-401 repro. Sync tools + Monitor arm in
// the same turn; ResultMessage fires after sync work but while Monitor is
// still live. Execute must wait.
func TestTurnState_C2_MixedSyncBgTurn(t *testing.T) {
	s := newLogicalTurnState()

	// Assistant turn with sync Read + bg Monitor in one message.
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks: claude.ContentBlocks{
			toolUseBlock("toolu_sync1", "Read", map[string]interface{}{"file_path": "/tmp/x"}),
			monitorToolUse("toolu_bg1"),
		},
	})
	// Sync tool result arrives as user message.
	s.Apply(claude.UserMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{toolResult("toolu_sync1", false)},
	})
	// Bg Monitor registers as a task.
	s.Apply(claude.TaskStartedEvent{
		TaskID:     "task1",
		ToolUseID:  strPtr("toolu_bg1"),
		TurnNumber: 1,
	})
	// ResultMessage fires while Monitor still running.
	endOfCLITurn(s, false)

	require.False(t, s.LogicalTurnDone(),
		"mixed turn must not be done while Monitor bg task is live")

	// Monitor completes, CLI auto-continues with a new ResultMessage.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg1"),
		Status:    "completed",
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone())
}

// C3: Two parallel Monitors, one fails fast, one runs long. Execute must wait
// for both to reach terminal before declaring done.
func TestTurnState_C3_TwoParallelMonitorsOneFailsFast(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks: claude.ContentBlocks{
			monitorToolUse("toolu_bg_fast"),
			monitorToolUse("toolu_bg_slow"),
		},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task_fast",
		ToolUseID: strPtr("toolu_bg_fast"),
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task_slow",
		ToolUseID: strPtr("toolu_bg_slow"),
	})
	endOfCLITurn(s, false)

	// Fast monitor fails.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task_fast",
		ToolUseID: strPtr("toolu_bg_fast"),
		Status:    "failed",
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone(),
		"one bg task still live — not done")

	// Slow monitor completes.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task_slow",
		ToolUseID: strPtr("toolu_bg_slow"),
		Status:    "completed",
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone())
}

// C4: Pure-bg Bash with run_in_background:true — same lifecycle as C1 but the
// tool is Bash, not Monitor. Classification must pick up run_in_background.
func TestTurnState_C4_PureBgBashRunInBackground(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{bgBashToolUse("toolu_bg_bash")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task_bash",
		ToolUseID: strPtr("toolu_bg_bash"),
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone())
	require.True(t, s.HasLiveTasks())

	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task_bash",
		ToolUseID: strPtr("toolu_bg_bash"),
		Status:    "completed",
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone())
}

// C5: Mixed Monitor + bg-Bash in one turn — wait for both.
func TestTurnState_C5_MixedMonitorAndBgBash(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks: claude.ContentBlocks{
			monitorToolUse("toolu_mon"),
			bgBashToolUse("toolu_bash"),
		},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task_mon", ToolUseID: strPtr("toolu_mon"),
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task_bash", ToolUseID: strPtr("toolu_bash"),
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone())

	// Monitor terminates first.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task_mon", ToolUseID: strPtr("toolu_mon"),
		Status: "completed",
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone(), "bg Bash still live")

	// Bash terminates.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task_bash", ToolUseID: strPtr("toolu_bash"),
		Status: "completed",
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone())
}

// C6: task_notification missing tool_use_id — state machine must still drain
// via taskToToolUse mapping recorded at TaskStartedEvent.
func TestTurnState_C6_TaskNotificationMissingToolUseID(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg"),
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone())

	// TaskNotification with no tool_use_id. Mapping lookup must fire so the
	// bg tool_use is marked done.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: nil,
		Status:    "completed",
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone(),
		"must drain bg tool_use via taskToToolUse mapping")
}

// C7: TaskStartedEvent may arrive AFTER terminal TaskNotificationEvent (when
// ordering reorders — see #162 round 3). State must not leave a ghost task
// alive in that case.
func TestTurnState_C7_ReorderedTaskStartedAfterNotification(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	// TaskNotification arrives before TaskStarted (reordered).
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg"),
		Status:    "completed",
	})
	// The belated TaskStartedEvent must NOT re-register task1 as live — the
	// terminal event already drained it. Without this, a single stray
	// TaskStartedEvent after terminal would strand LogicalTurnDone forever
	// since no further terminal event is guaranteed.
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg"),
	})
	require.False(t, s.HasLiveTasks(),
		"belated TaskStartedEvent must not resurrect a task that already reached terminal")

	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone(),
		"logical turn done: terminal already observed, ResultMessage + TurnComplete closed the turn")
}

// C8: Budget exceeded mid-bg turn — Execute stops cleanly, no orphan. From
// the state's perspective, when an ErrorEvent propagates or the caller cancels
// ctx, no done-flag flips, but Err() is populated.
func TestTurnState_C8_BudgetExceededMidBg(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone())

	// Budget-exceeded surfaces as ErrorEvent.
	s.Apply(claude.ErrorEvent{Error: errors.New("budget exceeded")})
	require.Error(t, s.Err())
	require.True(t, s.HasLiveTasks(),
		"bg task still live even after error — caller must handle orphan risk")
}

// C9: Bg Monitor tool error (HTTP 400 / invalid_request_error) — surfaces as
// TaskNotificationEvent status=failed. Turn still completes cleanly.
func TestTurnState_C9_BgMonitorToolError(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	endOfCLITurn(s, false)

	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
		Status: "failed",
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone(),
		"failed bg task is terminal — turn completes")
}

// C10: Monitor's own timeout_ms exceeded — CLI SIGTERMs child, emits
// TaskNotificationEvent status=timeout.
func TestTurnState_C10_MonitorTimeoutMs(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	endOfCLITurn(s, false)

	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
		Status: "timeout",
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone(),
		"timeout bg task is terminal — turn completes")
}

// C11: No-bg plain turn — text in, text out, ResultMessage. Done immediately.
func TestTurnState_C11_PlainSyncTurn(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{asstTextBlock("hello world")},
	})
	require.False(t, s.LogicalTurnDone(), "no result message yet")
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone(),
		"sync turn with no tools — done immediately on ResultMessage")
	require.Equal(t, "hello world", s.Text())
}

// C12: Close()-while-bg-live — from the state's perspective, consumer stops
// iterating. HasLiveTasks reports truthfully so the caller can log/orphan.
func TestTurnState_C12_CloseWhileBgLive(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	endOfCLITurn(s, false)

	// Consumer stops listening — state still says bg is live.
	require.True(t, s.HasLiveTasks())
	require.False(t, s.LogicalTurnDone())
}

// C13: Tool-error retry after bg settles — retry gating should use
// HasLiveTasks(), not a separate flag. After bg completes and turn finishes
// with an error, HasLiveTasks is false and the caller can retry.
func TestTurnState_C13_ToolErrorRetryAfterBgSettles(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	endOfCLITurn(s, false)
	require.True(t, s.HasLiveTasks(), "should NOT retry while bg live")

	// Bg settles, final continuation ResultMessage carries an error.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
		Status: "completed",
	})
	endOfCLITurn(s, true) // error turn

	require.True(t, s.LogicalTurnDone())
	require.False(t, s.HasLiveTasks(),
		"bg settled — retry gate clear")
	require.Error(t, s.Err())
	require.False(t, s.Success())
}

// Cancelled bg tool (tool_result with IsError) should not hold up the logical
// turn — mirrors session_bg_test cancel-bg coverage.
func TestTurnState_CancelledBgToolDoesNotBlock(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	// tool_result carries IsError — caller cancelled.
	s.Apply(claude.UserMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{toolResult("toolu_bg", true)},
	})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone(),
		"cancelled bg tool marked via tool_result IsError — no continuation expected")
	require.False(t, s.HasLiveTasks())
}

// Usage accumulates across multiple ResultMessageEvents in a logical turn
// (pure-bg turns fire N result messages).
func TestTurnState_UsageAccumulatesAcrossResultMessages(t *testing.T) {
	s := newLogicalTurnState()

	rm1 := resultMessage(false)
	rm1.Usage = claude.TurnUsage{InputTokens: 100, OutputTokens: 20, CostUSD: 0.01}
	s.Apply(rm1)

	rm2 := resultMessage(false)
	rm2.Usage = claude.TurnUsage{InputTokens: 50, OutputTokens: 10, CostUSD: 0.005}
	s.Apply(rm2)

	u := s.Usage()
	require.Equal(t, 150, u.InputTokens)
	require.Equal(t, 30, u.OutputTokens)
	require.InDelta(t, 0.015, u.CostUSD, 1e-9)
}

// A new ResultMessageEvent must drop any previously-observed TurnCompleteEvent
// so LogicalTurnDone requires the TurnComplete paired with *this* Result. The
// bg-continuation flow is the classic trigger: wave 1 fires (Result1,
// TurnComplete1), then wave 2 fires Result2 with its own TurnComplete2. If the
// state kept TurnComplete1 around, streamTurn would exit before consuming
// TurnComplete2 from the session channel.
func TestTurnState_NewResultMessageInvalidatesPriorTurnComplete(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{TurnNumber: 1})
	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone(),
		"a completed sync turn should be done after Result+TurnComplete")

	rm2 := resultMessage(false)
	rm2.TurnNumber = 2
	s.Apply(rm2)
	require.False(t, s.LogicalTurnDone(),
		"a new ResultMessage must require a fresh TurnComplete — the prior wave's TurnComplete is stale")

	s.Apply(claude.TurnCompleteEvent{TurnNumber: 2, Success: true})
	require.True(t, s.LogicalTurnDone(),
		"logical turn done once the TurnComplete paired with the latest Result arrives")
}

// Pure-bg ordering: wave-1 fires (Result1, TurnComplete1) while the bg task
// is still live, then a terminal TaskNotificationEvent drains the task. At
// that instant lastResult+lastTurnComplete are both set AND liveTaskIDs is
// empty AND the bg tool_use is marked done — the naive gate would flip to
// true here. But the CLI auto-continues after any terminal bg event, so the
// logical turn is not done until the continuation wave's Result+TurnComplete
// arrives.
func TestTurnState_TerminalBgDrainAfterResultRequiresContinuation(t *testing.T) {
	s := newLogicalTurnState()

	// Wave 1: assistant launches bg Monitor; CLI fires Result+TurnComplete
	// immediately because it has nothing else to say.
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"),
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone(),
		"wave-1 alone cannot complete the logical turn while bg task is live")

	// Terminal bg event arrives — drains liveTaskIDs AND marks the bg
	// tool_use completed. The prior Result+TurnComplete are stale now; the
	// continuation wave must land before LogicalTurnDone can flip.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"),
		Status: "completed",
	})
	require.False(t, s.LogicalTurnDone(),
		"terminal bg event after wave-1 Result+TurnComplete must NOT satisfy LogicalTurnDone — continuation wave pending")

	// Continuation wave lands: Result2 alone is still not enough.
	rm2 := resultMessage(false)
	rm2.TurnNumber = 2
	s.Apply(rm2)
	require.False(t, s.LogicalTurnDone(),
		"continuation Result alone cannot satisfy LogicalTurnDone without its paired TurnComplete")

	// Paired TurnComplete closes the turn.
	s.Apply(claude.TurnCompleteEvent{TurnNumber: 2, Success: true})
	require.True(t, s.LogicalTurnDone())
}

// Same shape but using TaskUpdatedEvent (status=completed) as the terminal
// signal — both event types must invalidate a prior wave's Result+TurnComplete.
func TestTurnState_TerminalTaskUpdatedAfterResultRequiresContinuation(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"),
	})
	endOfCLITurn(s, false)
	require.False(t, s.LogicalTurnDone())

	status := "completed"
	s.Apply(claude.TaskUpdatedEvent{TaskID: "task1", Status: &status})
	require.False(t, s.LogicalTurnDone(),
		"terminal TaskUpdatedEvent after wave-1 Result+TurnComplete must invalidate the prior wave")

	endOfCLITurn(s, false)
	require.True(t, s.LogicalTurnDone())
}

// INF-1066 repro: an agent backgrounds an unkillable infinite-loop Bash tool
// (run_in_background:true), arms a ScheduleWakeup, and ends the turn. The
// ScheduleWakeup safety timer fires and emits a terminal (WakeupTimedOut)
// TurnCompleteEvent — but the bg tool_use never terminates, so it is never
// cancelled and never lands in completedBgToolUseIDs. Without the
// WakeupTimedOut short-circuit, LogicalTurnDone stays false forever and
// streamTurn loops indefinitely. The terminal event must force the turn done.
func TestTurnState_WakeupTimedOutForcesTurnDone(t *testing.T) {
	s := newLogicalTurnState()

	// Assistant backgrounds an infinite-loop Bash poll.
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{bgBashToolUse("toolu_inf_loop")},
	})
	// The CLI emits a ResultMessage immediately (pure-bg turn). No terminal
	// task event ever follows — the loop never ends.
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone(),
		"bg tool_use still uncancelled/uncompleted — turn not done")
	require.True(t, s.HasLiveTasks())

	// A normal (non-wakeup) TurnCompleteEvent must NOT break the deadlock —
	// the bg tool_use is still tracked as live.
	s.Apply(claude.TurnCompleteEvent{TurnNumber: 1, Success: true})
	require.False(t, s.LogicalTurnDone(),
		"a normal TurnCompleteEvent must keep gating on the live bg tool_use")

	// The ScheduleWakeup safety timer fires: a terminal TurnCompleteEvent
	// with WakeupTimedOut set. The session layer has given up — the logical
	// turn must now be forced done so streamTurn unblocks.
	s.Apply(claude.TurnCompleteEvent{TurnNumber: 1, Success: true, WakeupTimedOut: true})
	require.True(t, s.LogicalTurnDone(),
		"WakeupTimedOut TurnCompleteEvent must force the logical turn done despite the stranded bg tool_use")
}

// A WakeupTimedOut TurnCompleteEvent must force completion even with a live
// background *task* (not just a tool_use) outstanding — the safety timer is
// terminal regardless of what kind of bg work is stranded.
func TestTurnState_WakeupTimedOutForcesDoneWithLiveTask(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg")})
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone())

	s.Apply(claude.TurnCompleteEvent{TurnNumber: 1, Success: true, WakeupTimedOut: true})
	require.True(t, s.LogicalTurnDone(),
		"WakeupTimedOut must force done even with a live bg task")
}

// SawTurnComplete reports whether the current wave's TurnCompleteEvent is
// outstanding. streamTurn arms its grace timer on this signal.
func TestTurnState_SawTurnComplete(t *testing.T) {
	s := newLogicalTurnState()
	require.False(t, s.SawTurnComplete(), "no TurnComplete observed yet")

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{bgBashToolUse("toolu_bg")},
	})
	s.Apply(resultMessage(false))
	require.False(t, s.SawTurnComplete(), "ResultMessage alone is not a TurnComplete")

	s.Apply(claude.TurnCompleteEvent{TurnNumber: 1, Success: true})
	require.True(t, s.SawTurnComplete(),
		"TurnCompleteEvent observed — grace timer may arm")

	// A new ResultMessage starts a fresh wave and clears the stale signal.
	rm2 := resultMessage(false)
	rm2.TurnNumber = 2
	s.Apply(rm2)
	require.False(t, s.SawTurnComplete(),
		"a new ResultMessage clears the prior wave's TurnComplete")
}

// INF-1400 state-machine repro: a successful wave's ResultMessage is recorded
// as lastSuccessfulResult, and a terminal bg-task event then invalidates the
// live wave (lastResult -> nil) awaiting a continuation that never arrives.
// TurnSucceededTerminally must still report true so a clean stream close can
// resolve the turn as success, and ToTerminalTurnResult must flip Success to
// true even though the live-path Success()/ToTurnResult() report false.
func TestTurnState_SuccessfulWaveSurvivesInvalidation(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	})
	s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")})
	endOfCLITurn(s, false) // successful wave -> lastSuccessfulResult recorded

	require.True(t, s.Success(), "live wave is successful before invalidation")
	require.True(t, s.TurnSucceededTerminally())

	// Terminal bg event invalidates the wave: lastResult -> nil, so the live
	// path now reports failure while awaiting a continuation.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "completed",
	})
	require.False(t, s.Success(),
		"after invalidation the live path reports Success=false (lastResult nil)")
	require.False(t, s.ToTurnResult().Success,
		"live-path ToTurnResult mirrors Success() — still gating")
	require.True(t, s.TurnSucceededTerminally(),
		"the successful wave must survive invalidation")

	// Terminal-EOF resolution recovers the success — including the duration
	// from the last successful wave (ToTurnResult derived 0 from the nilled
	// lastResult). resultMessage(false) sets DurationMs=100.
	terminal := s.ToTerminalTurnResult()
	require.True(t, terminal.Success,
		"ToTerminalTurnResult must resolve a clean EOF after a successful wave as success")
	require.NoError(t, terminal.Error)
	require.Equal(t, int64(100), terminal.DurationMs,
		"terminal resolution must restore the successful wave's duration, not fall back to 0")
}

// A background task that reaches a failed/killed/timeout terminal status must
// suppress terminal success: the CLI can emit a successful end-of-turn result
// while a Monitor is still live, then have that Monitor fail and exit without a
// continuation result. Resolving such an EOF as success would mask the failed
// bg work. Covers both terminal event shapes.
func TestTurnState_FailedBgTaskSuppressesTerminalSuccess(t *testing.T) {
	failureCases := []struct {
		name   string
		status string
	}{
		{"failed", "failed"},
		{"killed", "killed"},
		{"timeout", "timeout"},
	}
	for _, tc := range failureCases {
		t.Run("notification/"+tc.name, func(t *testing.T) {
			s := newLogicalTurnState()
			s.Apply(claude.AssistantMessageEvent{
				TurnNumber: 1,
				Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
			})
			s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")})
			endOfCLITurn(s, false) // successful end-of-turn while bg still live
			require.True(t, s.TurnSucceededTerminally())

			s.Apply(claude.TaskNotificationEvent{
				TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: tc.status,
			})
			require.False(t, s.TurnSucceededTerminally(),
				"a %s bg task must suppress terminal success", tc.status)
			require.False(t, s.ToTerminalTurnResult().Success,
				"the failed bg task must not be reported as a terminal success")
		})

		t.Run("updated/"+tc.name, func(t *testing.T) {
			s := newLogicalTurnState()
			s.Apply(claude.AssistantMessageEvent{
				TurnNumber: 1,
				Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
			})
			s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")})
			endOfCLITurn(s, false)
			require.True(t, s.TurnSucceededTerminally())

			status := tc.status
			s.Apply(claude.TaskUpdatedEvent{TaskID: "task1", Status: &status})
			require.False(t, s.TurnSucceededTerminally(),
				"a %s bg task (via TaskUpdatedEvent) must suppress terminal success", tc.status)
			require.False(t, s.ToTerminalTurnResult().Success)
		})
	}
}

// TurnSucceededTerminally / ToTerminalTurnResult must NOT manufacture success
// when no successful ResultMessage was ever seen, nor mask a real error.
func TestTurnState_TerminalResolutionRespectsFailures(t *testing.T) {
	t.Run("no result ever", func(t *testing.T) {
		s := newLogicalTurnState()
		s.Apply(claude.AssistantMessageEvent{
			TurnNumber: 1,
			Blocks:     claude.ContentBlocks{asstTextBlock("starting")},
		})
		require.False(t, s.TurnSucceededTerminally())
		require.False(t, s.ToTerminalTurnResult().Success,
			"a turn with no successful result must not be resolved as success")
	})

	t.Run("error result", func(t *testing.T) {
		s := newLogicalTurnState()
		s.Apply(claude.AssistantMessageEvent{
			TurnNumber: 1,
			Blocks:     claude.ContentBlocks{asstTextBlock("oops")},
		})
		endOfCLITurn(s, true) // error wave -> state.err set
		require.False(t, s.TurnSucceededTerminally(),
			"an outstanding error must suppress terminal success")
		terminal := s.ToTerminalTurnResult()
		require.False(t, terminal.Success)
		require.Error(t, terminal.Error, "a real error must be preserved, not masked")
	})

	t.Run("success then later error", func(t *testing.T) {
		s := newLogicalTurnState()
		// A successful wave, then a continuation that errors: the live error
		// must win over the earlier success.
		s.Apply(claude.AssistantMessageEvent{
			TurnNumber: 1,
			Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
		})
		s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")})
		endOfCLITurn(s, false)
		s.Apply(claude.TaskNotificationEvent{
			TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "completed",
		})
		s.Apply(resultMessage(true)) // continuation errors -> state.err set
		require.False(t, s.TurnSucceededTerminally(),
			"a later error must override an earlier successful wave")
		require.False(t, s.ToTerminalTurnResult().Success)
		require.Error(t, s.ToTerminalTurnResult().Error)
	})
}

// A terminal TaskNotificationEvent that arrives before the corresponding
// TaskStartedEvent (stream reorder) must not leave the task stuck live when
// the belated TaskStartedEvent finally shows up.
func TestTurnState_TerminalBeforeTaskStartedStillCompletes(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	})
	// Terminal first.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg1"),
		Status:    "completed",
	})
	// Belated start.
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg1"),
	})

	endOfCLITurn(s, false)

	require.False(t, s.HasLiveTasks(),
		"belated TaskStartedEvent must not re-add a task that already reached terminal")
	require.True(t, s.LogicalTurnDone())
}

// Regression for the INF-1871 "plan step: agent failed" bug. A turn whose final
// action spawns a sub-agent via the Agent tool must be treated as background:
// the tool_use input has no run_in_background flag, so background-ness comes
// from the tool name alone.
func TestTurnState_LiveAgentSubAgentGatesTurn(t *testing.T) {
	s := newLogicalTurnState()
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{agentToolUse("toolu_agent1")},
	})
	s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_agent1")})
	// The turn's own wave completes while the sub-agent is still working.
	endOfCLITurn(s, false)

	require.True(t, s.HasLiveTasks(),
		"a live Agent sub-agent must keep the turn's background work outstanding")
	require.False(t, s.LogicalTurnDone(),
		"LogicalTurnDone must stay false while an Agent sub-agent is still live")

	// Sub-agent completes -> continuation wave -> turn is done and successful.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_agent1"), Status: "completed",
	})
	endOfCLITurn(s, false)

	require.False(t, s.HasLiveTasks())
	require.True(t, s.LogicalTurnDone())
	require.True(t, s.ToTurnResult().Success)
}

// Even before any TaskStartedEvent lands, an Agent tool_use block in the turn's
// content must gate LogicalTurnDone via hasUncancelledBgToolUse — the same
// protection Monitor blocks get. This mirrors the real failure, where the turn
// resolved before the sub-agent's terminal notification arrived.
func TestTurnState_AgentToolUseGatesBeforeTaskStarted(t *testing.T) {
	s := newLogicalTurnState()
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{agentToolUse("toolu_agent1")},
	})
	endOfCLITurn(s, false)

	require.False(t, s.LogicalTurnDone(),
		"an uncancelled Agent tool_use must gate the turn even before TaskStarted")
	require.True(t, s.HasLiveTasks())
}

// The core INF-1871 regression: the turn had a successful wave, the sub-agent
// was still live, then the session was torn down and the CLI reported the
// sub-agent killed. The terminal resolution must NOT be the silent
// Success=false/Error=nil that callers translate into a bare "agent failed" —
// it must carry claude.ErrBackgroundTaskFailed.
func TestTurnState_InterruptedAgentSubAgentNotSilentFailure(t *testing.T) {
	for _, status := range []string{"failed", "killed", "timeout"} {
		t.Run(status, func(t *testing.T) {
			s := newLogicalTurnState()
			s.Apply(claude.AssistantMessageEvent{
				TurnNumber: 1,
				Blocks:     claude.ContentBlocks{agentToolUse("toolu_agent1")},
			})
			s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_agent1")})
			endOfCLITurn(s, false) // successful wave while sub-agent still live

			// Session torn down mid-flight: the sub-agent's terminal status is a
			// failure, and no continuation ResultMessage ever arrives.
			s.Apply(claude.TaskNotificationEvent{
				TaskID: "task1", ToolUseID: strPtr("toolu_agent1"), Status: status,
			})

			terminal := s.ToTerminalTurnResult()
			require.False(t, terminal.Success,
				"a %s sub-agent must not resolve as terminal success", status)
			require.ErrorIs(t, terminal.Error, claude.ErrBackgroundTaskFailed,
				"a %s sub-agent must surface a real error, never a silent Success=false/Error=nil", status)
		})
	}
}

// A genuine ResultMessage error must still win over ErrBackgroundTaskFailed —
// the sentinel is only a backstop for the no-error case.
func TestTurnState_RealErrorWinsOverBackgroundTaskFailed(t *testing.T) {
	s := newLogicalTurnState()
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{agentToolUse("toolu_agent1")},
	})
	s.Apply(claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_agent1")})
	endOfCLITurn(s, true) // the turn's own wave errored -> state.err set
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_agent1"), Status: "killed",
	})

	terminal := s.ToTerminalTurnResult()
	require.False(t, terminal.Success)
	require.Error(t, terminal.Error)
	require.NotErrorIs(t, terminal.Error, claude.ErrBackgroundTaskFailed,
		"a real ResultMessage error must not be replaced by the bg-task sentinel")
}
