package agent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// Helpers for building hand-crafted event sequences.

func strPtr(s string) *string { return &s }

func asstTextBlock(text string) claude.ContentBlock {
	return claude.ContentBlock{Type: claude.ContentBlockTypeText, Text: text}
}

func toolUseBlock(id, name string, input map[string]interface{}) claude.ContentBlock {
	return claude.ContentBlock{
		Type:      claude.ContentBlockTypeToolUse,
		ToolUseID: id,
		ToolName:  name,
		ToolInput: input,
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

func toolResult(id string, isError bool) claude.ContentBlock {
	return claude.ContentBlock{
		Type:      claude.ContentBlockTypeToolResult,
		ToolUseID: id,
		IsError:   isError,
	}
}

func resultMessage(isError bool) claude.ResultMessageEvent {
	ev := claude.ResultMessageEvent{
		Subtype:      "success",
		StopReason:   "end_turn",
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

// C1: Pure-bg Monitor turn — ResultMessage fires before task completes.
// Execute must not report done until TaskNotificationEvent + continuation
// ResultMessage.
func TestTurnState_C1_PureBgMonitorTurn(t *testing.T) {
	s := newLogicalTurnState()

	// Turn 1: assistant launches Monitor; CLI fires ResultMessage immediately.
	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg1")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:     "task1",
		ToolUseID:  strPtr("toolu_bg1"),
		TurnNumber: 1,
	})
	s.Apply(resultMessage(false))

	require.False(t, s.LogicalTurnDone(),
		"pure-bg turn must not be done while bg task is still live")
	require.True(t, s.HasLiveTasks())

	// Turn 2 (auto-continuation): task completes, then CLI emits ResultMessage.
	status := "completed"
	s.Apply(claude.TaskUpdatedEvent{TaskID: "task1", Status: &status})
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg1"),
		Status:    "completed",
	})
	rm := resultMessage(false)
	rm.TurnNumber = 2
	s.Apply(rm)

	require.True(t, s.LogicalTurnDone(),
		"logical turn done once bg task terminal and continuation ResultMessage arrived")
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
		Blocks: []claude.ContentBlock{
			toolUseBlock("toolu_sync1", "Read", map[string]interface{}{"file_path": "/tmp/x"}),
			monitorToolUse("toolu_bg1"),
		},
	})
	// Sync tool result arrives as user message.
	s.Apply(claude.UserMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{toolResult("toolu_sync1", false)},
	})
	// Bg Monitor registers as a task.
	s.Apply(claude.TaskStartedEvent{
		TaskID:     "task1",
		ToolUseID:  strPtr("toolu_bg1"),
		TurnNumber: 1,
	})
	// ResultMessage fires while Monitor still running.
	s.Apply(resultMessage(false))

	require.False(t, s.LogicalTurnDone(),
		"mixed turn must not be done while Monitor bg task is live")

	// Monitor completes, CLI auto-continues with a new ResultMessage.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg1"),
		Status:    "completed",
	})
	s.Apply(resultMessage(false))
	require.True(t, s.LogicalTurnDone())
}

// C3: Two parallel Monitors, one fails fast, one runs long. Execute must wait
// for both to reach terminal before declaring done.
func TestTurnState_C3_TwoParallelMonitorsOneFailsFast(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks: []claude.ContentBlock{
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
	s.Apply(resultMessage(false))

	// Fast monitor fails.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task_fast",
		ToolUseID: strPtr("toolu_bg_fast"),
		Status:    "failed",
	})
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone(),
		"one bg task still live — not done")

	// Slow monitor completes.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task_slow",
		ToolUseID: strPtr("toolu_bg_slow"),
		Status:    "completed",
	})
	s.Apply(resultMessage(false))
	require.True(t, s.LogicalTurnDone())
}

// C4: Pure-bg Bash with run_in_background:true — same lifecycle as C1 but the
// tool is Bash, not Monitor. Classification must pick up run_in_background.
func TestTurnState_C4_PureBgBashRunInBackground(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{bgBashToolUse("toolu_bg_bash")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task_bash",
		ToolUseID: strPtr("toolu_bg_bash"),
	})
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone())
	require.True(t, s.HasLiveTasks())

	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task_bash",
		ToolUseID: strPtr("toolu_bg_bash"),
		Status:    "completed",
	})
	s.Apply(resultMessage(false))
	require.True(t, s.LogicalTurnDone())
}

// C5: Mixed Monitor + bg-Bash in one turn — wait for both.
func TestTurnState_C5_MixedMonitorAndBgBash(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks: []claude.ContentBlock{
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
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone())

	// Monitor terminates first.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task_mon", ToolUseID: strPtr("toolu_mon"),
		Status: "completed",
	})
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone(), "bg Bash still live")

	// Bash terminates.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task_bash", ToolUseID: strPtr("toolu_bash"),
		Status: "completed",
	})
	s.Apply(resultMessage(false))
	require.True(t, s.LogicalTurnDone())
}

// C6: task_notification missing tool_use_id — state machine must still drain
// via taskToToolUse mapping recorded at TaskStartedEvent.
func TestTurnState_C6_TaskNotificationMissingToolUseID(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg"),
	})
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone())

	// TaskNotification with no tool_use_id. Mapping lookup must fire so the
	// bg tool_use is marked done.
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: nil,
		Status:    "completed",
	})
	s.Apply(resultMessage(false))
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
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	// TaskNotification arrives before TaskStarted (reordered).
	s.Apply(claude.TaskNotificationEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg"),
		Status:    "completed",
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID:    "task1",
		ToolUseID: strPtr("toolu_bg"),
	})
	// The delayed TaskStartedEvent re-registers task1 as live. But the tool_use
	// was already marked completed, so logical turn still finishes when the
	// terminal ResultMessage arrives, because the bg tool_use is completed.
	// liveTaskIDs contains task1 though — that is a known wrapper-state
	// artifact of reorder. In practice, a subsequent TaskNotificationEvent
	// will drain it; if not, the turn stays gated on HasLiveTasks.
	s.Apply(resultMessage(false))
	require.False(t, s.LogicalTurnDone(),
		"reordered TaskStartedEvent re-inserts live task — wait for drain")

	// Drain arrives.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
		Status: "completed",
	})
	s.Apply(resultMessage(false))
	require.True(t, s.LogicalTurnDone())
}

// C8: Budget exceeded mid-bg turn — Execute stops cleanly, no orphan. From
// the state's perspective, when an ErrorEvent propagates or the caller cancels
// ctx, no done-flag flips, but Err() is populated.
func TestTurnState_C8_BudgetExceededMidBg(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	s.Apply(resultMessage(false))
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
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	s.Apply(resultMessage(false))

	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
		Status: "failed",
	})
	s.Apply(resultMessage(false))
	require.True(t, s.LogicalTurnDone(),
		"failed bg task is terminal — turn completes")
}

// C10: Monitor's own timeout_ms exceeded — CLI SIGTERMs child, emits
// TaskNotificationEvent status=timeout.
func TestTurnState_C10_MonitorTimeoutMs(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	s.Apply(resultMessage(false))

	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
		Status: "timeout",
	})
	s.Apply(resultMessage(false))
	require.True(t, s.LogicalTurnDone(),
		"timeout bg task is terminal — turn completes")
}

// C11: No-bg plain turn — text in, text out, ResultMessage. Done immediately.
func TestTurnState_C11_PlainSyncTurn(t *testing.T) {
	s := newLogicalTurnState()

	s.Apply(claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{asstTextBlock("hello world")},
	})
	require.False(t, s.LogicalTurnDone(), "no result message yet")
	s.Apply(resultMessage(false))
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
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	s.Apply(resultMessage(false))

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
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	s.Apply(claude.TaskStartedEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
	})
	s.Apply(resultMessage(false))
	require.True(t, s.HasLiveTasks(), "should NOT retry while bg live")

	// Bg settles, final continuation ResultMessage carries an error.
	s.Apply(claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg"),
		Status: "completed",
	})
	s.Apply(resultMessage(true)) // error turn

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
		Blocks:     []claude.ContentBlock{monitorToolUse("toolu_bg")},
	})
	// tool_result carries IsError — caller cancelled.
	s.Apply(claude.UserMessageEvent{
		TurnNumber: 1,
		Blocks:     []claude.ContentBlock{toolResult("toolu_bg", true)},
	})
	s.Apply(resultMessage(false))
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
