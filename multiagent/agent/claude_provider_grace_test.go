package agent

import (
	"context"
	"errors"
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
	// This wave carries a successful ResultMessage that is merely still gated
	// on a lingering bg tool_use — the turn produced a real answer, so the
	// grace-forced stop returns it as-is with no synthetic error.
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success,
		"a successful result gated on lingering bg work must not be reported as a failure")
	require.GreaterOrEqual(t, elapsed, grace,
		"must wait at least the grace period before forcing completion")
	require.Less(t, elapsed, 3*time.Second,
		"must force completion shortly after the grace period, not hang")
}

// Production failure repro (jiradozer run 610524, /pr-polish round 3): a wave's
// ResultMessage is invalidated by a terminal bg-task notification (a bramble
// reviewer finishing), but no continuation ResultMessage arrives before the
// grace deadline. A late/duplicate TurnCompleteEvent re-arms the grace timer
// while lastResult is nil, so the forced stop yields Success=false with no
// error of its own. Without classification this surfaces as
// Success=false/Error=nil — which a non-interactive caller's retry loop treats
// as a hard "agent failed". consumeTurnEvents must instead return a transient
// error so the resume path re-drives the session.
func TestConsumeTurnEvents_GraceForcedNonSuccessIsTransient(t *testing.T) {
	events := make(chan claude.Event, 16)
	// Wave 1: assistant arms a bg Monitor; CLI fires Result + TurnComplete
	// while the bg task is still live.
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	events <- resultMessage(false)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}
	// The reviewer finishes: a terminal notification invalidates the wave's
	// Result+TurnComplete pair (lastResult -> nil) and completes the bg tool.
	events <- claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "completed",
	}
	// Another bg tool is still outstanding, and a stray/late TurnComplete
	// re-arms the grace timer with no continuation Result — lastResult stays
	// nil, so Success() is false and Err() is nil.
	events <- claude.AssistantMessageEvent{
		TurnNumber: 2,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg2")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task2", ToolUseID: strPtr("toolu_bg2")}
	events <- claude.TurnCompleteEvent{TurnNumber: 2, Success: true}
	// No continuation ResultMessage ever arrives — the grace timer is the backstop.

	grace := 150 * time.Millisecond
	done := make(chan struct{})
	var result *claude.TurnResult
	var err error
	go func() {
		result, err = consumeTurnEvents(context.Background(), events, grace, nil, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumeTurnEvents hung: grace timer did not force completion")
	}
	require.NotNil(t, result)
	require.False(t, result.Success, "the forced stop must carry the non-success result")
	require.Error(t, err)
	var transient *claude.TransientError
	require.ErrorAs(t, err, &transient,
		"a grace-forced non-success stop must be classified transient so it can be resumed")
	ok, _ := ClassifyTransient(err)
	require.True(t, ok, "ClassifyTransient must mark the grace-forced stop retryable")
}

// gatedNonSuccessEvents queues the event shape that leaves the logical turn
// gated on a live bg tool_use with Success=false / Err()==nil — the state that
// the grace path classifies transient. Shared by the race regressions below.
func gatedNonSuccessEvents(ch chan claude.Event) {
	ch <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	ch <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	ch <- resultMessage(false)
	ch <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}
	ch <- claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "completed",
	}
	ch <- claude.AssistantMessageEvent{
		TurnNumber: 2,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg2")},
	}
	ch <- claude.TaskStartedEvent{TaskID: "task2", ToolUseID: strPtr("toolu_bg2")}
	ch <- claude.TurnCompleteEvent{TurnNumber: 2, Success: true}
}

// When the grace timer fires but a continuation event is ALREADY queued (the
// select-tie race where graceCh wins over a ready events case), the grace
// branch must apply that event — never drop it — because it may be the
// ResultMessage that completes the wave. Here a queued successful Result +
// TurnComplete is waiting when grace fires; the turn must complete successfully
// rather than being force-classified transient with the event discarded.
func TestConsumeTurnEvents_GraceForcedAppliesQueuedContinuation(t *testing.T) {
	events := make(chan claude.Event, 16)
	// Wave gated on a bg tool_use with Success=false/Err()==nil — the shape that
	// would otherwise be classified transient on grace expiry.
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	events <- resultMessage(false)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}
	events <- claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "completed",
	}
	// A real continuation is queued: a fresh successful Result completes the turn.
	rm := resultMessage(false)
	rm.TurnNumber = 2
	events <- claude.AssistantMessageEvent{TurnNumber: 2, Blocks: claude.ContentBlocks{asstTextBlock("done")}}
	events <- rm
	events <- claude.TurnCompleteEvent{TurnNumber: 2, Success: true}

	grace := 20 * time.Millisecond
	done := make(chan struct{})
	var result *claude.TurnResult
	var err error
	go func() {
		result, err = consumeTurnEvents(context.Background(), events, grace, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumeTurnEvents hung")
	}
	// The queued continuation must have been applied, completing the turn — not
	// dropped in favor of a transient classification.
	require.NoError(t, err, "a queued continuation must complete the turn, not yield transient")
	require.NotNil(t, result)
	require.True(t, result.Success, "the applied continuation completes the turn successfully")
}

// Bugbot HIGH (PR #258, commit c445b25): when the grace branch applies a queued
// event that does NOT complete the turn but leaves the wave still
// completed-and-gated on live bg work, grace must be RE-ARMED — otherwise the
// loop would wait on the stream with no backstop and a genuinely stuck turn
// hangs forever. Here the queued event keeps the turn gated (SawTurnComplete
// stays true, no continuation Result), then the stream goes silent: grace must
// fire again and classify the stall transient rather than block indefinitely.
func TestConsumeTurnEvents_GraceRearmsAfterAppliedNonCompletingEvent(t *testing.T) {
	events := make(chan claude.Event, 16)
	gatedNonSuccessEvents(events)
	// A queued, non-terminal in-scope event is waiting when grace fires: it is
	// applied (proving the branch consumes it) but does not complete the turn,
	// which stays gated on toolu_bg2 with TurnComplete already seen. No further
	// events arrive — grace must re-arm and fire again.
	events <- claude.AssistantMessageEvent{
		TurnNumber: 2,
		Blocks:     claude.ContentBlocks{asstTextBlock("still working")},
	}
	// channel intentionally left open and silent hereafter.

	grace := 30 * time.Millisecond
	done := make(chan struct{})
	var err error
	go func() {
		_, err = consumeTurnEvents(context.Background(), events, grace, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumeTurnEvents hung: grace was not re-armed after applying a non-completing event")
	}
	// The re-armed grace must fire and classify the stall transient (resumable),
	// not hang and not return success.
	require.Error(t, err, "a re-stalled turn must surface the grace-forced transient")
	var transient *claude.TransientError
	require.ErrorAs(t, err, &transient,
		"the re-armed grace-forced stop must be transient so the session can be resumed")
}

// Contention smoke test: cancel ctx mid-flight while the turn is gated on a bg
// tool_use and the grace timer is the backstop. Depending on scheduling the
// stop returns either ctx.Err() (ctx.Done won) or a TransientError (grace won
// before cancellation landed) — both are legitimate. The invariant the fix
// guarantees is the one this asserts: a TransientError is NEVER returned once
// ctx.Err() is already set when the grace branch runs (verified by the direct
// re-check in the code: `if ctxErr := ctx.Err(); ctxErr != nil`). Here we only
// assert the loop always terminates and never hangs under the race.
func TestConsumeTurnEvents_GraceVsCtxCancelTerminates(t *testing.T) {
	for i := 0; i < 100; i++ {
		ch := make(chan claude.Event, 16)
		gatedNonSuccessEvents(ch)

		ctx, cancel := context.WithCancel(context.Background())
		grace := 10 * time.Millisecond
		done := make(chan struct{})
		var err error
		go func() {
			_, err = consumeTurnEvents(ctx, ch, grace, nil, nil)
			close(done)
		}()
		time.Sleep(time.Duration(i%12) * time.Millisecond) // jitter the contention point
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: consumeTurnEvents hung under ctx-cancel/grace contention", i)
		}
		// Whichever path won, the result must be a sane classification: either a
		// terminal ctx error or a (correct) transient — never a nil error that a
		// caller would read as bare success.
		require.Error(t, err, "iter %d: a gated grace-forced stop must return an error", i)
		if errors.Is(err, context.Canceled) {
			var transient *claude.TransientError
			require.False(t, errors.As(err, &transient),
				"iter %d: a ctx-cancelled stop must not also be transient", i)
		}
	}
}

// Bugbot MEDIUM (PR #258, commit 80ee672): the grace branch's SUCCESS path
// must also propagate ctx cancellation. graceCh can win a select tie with
// ctx.Done(), so a cancelled run whose result happens to be successful must
// return ctx.Err() — not report a stale success. ctx is cancelled before the
// run so it is ready throughout; the successful-but-gated wave arms grace, and
// whichever case wins, the result must be context.Canceled (never nil).
func TestConsumeTurnEvents_GraceForcedSuccessPropagatesCancel(t *testing.T) {
	for i := 0; i < 200; i++ {
		ch := make(chan claude.Event, 8)
		// A successful result still gated on a live bg tool_use: Success=true,
		// Err()==nil, TurnComplete seen, tool never terminates.
		ch <- claude.AssistantMessageEvent{
			TurnNumber: 1,
			Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
		}
		ch <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
		ch <- resultMessage(false) // non-error result -> Success=true
		ch <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancelled before the run: ctx.Done() ready throughout

		grace := time.Millisecond
		done := make(chan struct{})
		var err error
		go func() {
			_, err = consumeTurnEvents(ctx, ch, grace, nil, nil)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: consumeTurnEvents hung", i)
		}
		require.ErrorIs(t, err, context.Canceled,
			"iter %d: a cancelled run must surface ctx.Err() even when the gated result is successful", i)
	}
}

// A genuine ResultError coinciding with grace expiry must be returned
// unwrapped — the transient classification must never mask a real error.
func TestConsumeTurnEvents_GraceForcedRealErrorNotMasked(t *testing.T) {
	events := make(chan claude.Event, 16)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	// An error ResultMessage sets state.err and Success=false; the bg tool
	// keeps the turn gated so the grace timer is the backstop.
	events <- resultMessage(true)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: false}

	grace := 150 * time.Millisecond
	done := make(chan struct{})
	var err error
	go func() {
		_, err = consumeTurnEvents(context.Background(), events, grace, nil, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumeTurnEvents hung: grace timer did not force completion")
	}
	require.Error(t, err)
	var transient *claude.TransientError
	require.False(t, errors.As(err, &transient),
		"a real ResultError must be returned unwrapped, not masked as transient")
	ok, _ := ClassifyTransient(err)
	require.False(t, ok, "the real ResultError must not be classified transient")
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

// Wave-rollover repro: a pure-bg turn arms the grace timer on wave 1, then
// the bg task completes and the CLI auto-continues. The continuation wave
// (wave 2) then itself stalls on a *second* bg tool_use. The grace timer is
// anchored to a wave, not the whole turn: wave 2 must get a fresh full grace
// window, not whatever slack was left on wave 1's deadline. A turn-anchored
// timer would force-complete wave 2 early (after only `grace - elapsed`).
func TestConsumeTurnEvents_GraceResetsAcrossContinuationWave(t *testing.T) {
	grace := 300 * time.Millisecond
	events := make(chan claude.Event, 16)

	// Wave 1: assistant launches bg Monitor; CLI fires Result + TurnComplete
	// while the bg task is still live. This arms the grace timer.
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	events <- resultMessage(false)
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}

	done := make(chan struct{})
	var result *claude.TurnResult
	var err error
	start := time.Now()
	go func() {
		result, err = consumeTurnEvents(context.Background(), events, grace, nil, nil)
		close(done)
	}()

	// Let most of wave 1's grace window elapse before the continuation wave
	// arrives. A turn-anchored timer would have only ~grace/4 left for wave 2.
	time.Sleep(grace * 3 / 4)

	// Wave 2: bg task 1 completes, CLI auto-continues with a fresh Result +
	// TurnComplete — but this wave launches a *second* bg tool that never
	// terminates. The grace timer must re-arm fresh for this wave.
	completed := "completed"
	events <- claude.TaskUpdatedEvent{TaskID: "task1", Status: &completed}
	events <- claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "completed",
	}
	events <- claude.AssistantMessageEvent{
		TurnNumber: 2,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg2")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task2", ToolUseID: strPtr("toolu_bg2")}
	rm := resultMessage(false)
	rm.TurnNumber = 2
	events <- rm
	events <- claude.TurnCompleteEvent{TurnNumber: 2, Success: true}
	// toolu_bg2 never terminates — wave 2's grace timer is the backstop.

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("consumeTurnEvents hung: wave-2 grace timer did not fire")
	}
	require.NoError(t, err)
	require.NotNil(t, result)
	// Wave 2's grace window must be a full `grace` measured from when wave 2
	// re-armed the timer — not the leftover slack from wave 1. Total elapsed
	// is therefore ~ (3/4)*grace (the sleep) + grace (wave 2's full window).
	require.GreaterOrEqual(t, time.Since(start), grace+grace/4,
		"wave 2 must get a fresh grace window, not wave 1's leftover slack")
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
