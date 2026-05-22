package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// A turn that completes normally must return as soon as LogicalTurnDone
// flips — the grace timer must never fire.
func TestConsumeTurnEvents_NormalTurnReturnsImmediately(t *testing.T) {
	events := make(chan claude.Event, 8)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{asstTextBlock("hi")},
	}
	events <- resultMessage(false)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}

	done := make(chan struct{})
	var result *claude.TurnResult
	var err error
	go func() {
		result, err = consumeTurnEvents(context.Background(), events, time.Hour, nil, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consumeTurnEvents did not return on a normally-completed turn")
	}
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
}

// INF-1066 repro at the streamTurn layer: the agent backgrounds an
// infinite-loop Bash tool, the session emits a TurnCompleteEvent, then goes
// silent. The bg tool_use never terminates, so LogicalTurnDone never flips on
// its own. consumeTurnEvents must force completion when the grace period
// elapses instead of looping forever.
func TestConsumeTurnEvents_GraceTimerForcesCompletion(t *testing.T) {
	events := make(chan claude.Event, 8)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{bgBashToolUse("toolu_inf_loop")},
	}
	events <- resultMessage(false)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}
	// Channel deliberately left open with no further events — the bg tool
	// never produces a terminal event.

	grace := 150 * time.Millisecond
	done := make(chan struct{})
	var result *claude.TurnResult
	var err error
	start := time.Now()
	go func() {
		result, err = consumeTurnEvents(context.Background(), events, grace, nil, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumeTurnEvents hung: grace timer did not force turn completion")
	}
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.GreaterOrEqual(t, elapsed, grace,
		"must wait at least the grace period before forcing completion")
	require.Less(t, elapsed, 3*time.Second,
		"must force completion shortly after the grace period, not hang")
}

// A WakeupTimedOut TurnCompleteEvent must unblock the loop immediately —
// before the grace timer would have fired — because the session layer has
// already declared the turn terminally done.
func TestConsumeTurnEvents_WakeupTimedOutReturnsBeforeGrace(t *testing.T) {
	events := make(chan claude.Event, 8)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{bgBashToolUse("toolu_inf_loop")},
	}
	events <- resultMessage(false)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true, WakeupTimedOut: true}

	grace := 10 * time.Second
	done := make(chan struct{})
	var err error
	start := time.Now()
	go func() {
		_, err = consumeTurnEvents(context.Background(), events, grace, nil, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("consumeTurnEvents did not return on a WakeupTimedOut TurnCompleteEvent")
	}
	require.NoError(t, err)
	require.Less(t, time.Since(start), grace,
		"WakeupTimedOut must return well before the grace period elapses")
}

// ctx cancellation must unblock the loop even while a bg tool_use is live.
func TestConsumeTurnEvents_ContextCancelUnblocks(t *testing.T) {
	events := make(chan claude.Event, 8)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{bgBashToolUse("toolu_bg")},
	}
	events <- resultMessage(false)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var err error
	go func() {
		_, err = consumeTurnEvents(ctx, events, time.Hour, nil, nil)
		close(done)
	}()

	// Give the loop a moment to drain the queued events and arm the grace
	// timer, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consumeTurnEvents did not return on context cancellation")
	}
	require.ErrorIs(t, err, context.Canceled)
}
