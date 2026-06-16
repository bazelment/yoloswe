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
//
// Bugbot HIGH (PR #258): reaching the grace branch means LogicalTurnDone() is
// false, so the turn is NOT actually done even if the last wave's result was
// successful — a skill that yielded the turn awaiting a long background join
// keeps lastResult successful for the whole join. Returning that as a final
// success after the 3-minute grace would tell a non-interactive caller
// (jiradozer) the step succeeded while the work is still running. So a
// grace-forced stop gated on a non-terminating bg tool_use is classified
// transient (resumable), not a bare success — for both Success==true and
// Success==false waves.
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
	// The grace fires while the turn is gated on a non-terminating bg tool_use:
	// classified transient so the session is resumed rather than reported done.
	require.NotNil(t, result)
	var transient *claude.TransientError
	require.ErrorAs(t, err, &transient,
		"a grace-forced stop gated on a non-terminating bg tool_use must be transient, not a bare success")
	ok, _ := ClassifyTransient(err)
	require.True(t, ok, "the grace-forced stop must be retryable")
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

// A wave gated on a bg tool_use that is later followed by a real continuation
// (fresh Result + TurnComplete) must complete the turn successfully and NOT be
// classified transient. With a long grace relative to event delivery the
// continuation is consumed via the normal events path before grace fires; this
// pins the end-to-end outcome.
//
// NOTE (codex PR #258): this does NOT exercise the grace branch's own
// apply-queued-continuation code — that path only runs on the rare select tie
// where graceCh wins while a continuation is already queued, which cannot be
// forced deterministically from outside the function (Go select is random).
// That branch is covered by inspection + TestConsumeTurnEvents_GraceForced...
// transient/cancel tests below, which DO reach the grace branch (they stall
// with no continuation). This test guards the outcome contract, not the branch.
func TestConsumeTurnEvents_QueuedContinuationCompletesTurn(t *testing.T) {
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

	// Long grace so the continuation is delivered well before any deadline —
	// the turn completes via the normal path, not a forced stop.
	grace := time.Hour
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
	// The continuation must complete the turn — not be dropped or yield transient.
	require.NoError(t, err, "a continuation must complete the turn, not yield transient")
	require.NotNil(t, result)
	require.True(t, result.Success, "the continuation completes the turn successfully")
}

// A turn left gated on a live bg tool_use (TurnComplete seen, Success=false, no
// continuation) that then goes silent must trip grace and classify the stall
// transient — never hang. The events drain via the normal path (which arms
// grace on the gated wave); then the silent stream lets grace fire. This
// reaches the grace branch's non-success path for real (nothing is queued when
// it fires), unlike the queued-continuation outcome test above.
//
// (Bugbot HIGH, commit c445b25, motivated the unified rearmGrace so a gated
// wave always retains a backstop; the trailing non-completing event here keeps
// the turn gated rather than completing it.)
func TestConsumeTurnEvents_GraceTripsTransientOnGatedThenSilent(t *testing.T) {
	events := make(chan claude.Event, 16)
	gatedNonSuccessEvents(events)
	// A trailing non-terminal in-scope event that does NOT complete the turn —
	// it stays gated on toolu_bg2 with TurnComplete seen. Then silence.
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
		t.Fatal("consumeTurnEvents hung: grace did not fire on a gated-then-silent turn")
	}
	// Grace must fire and classify the stall transient (resumable), not hang or
	// return success.
	require.Error(t, err, "a gated-then-silent turn must surface the grace-forced transient")
	var transient *claude.TransientError
	require.ErrorAs(t, err, &transient,
		"the grace-forced stop must be transient so the session can be resumed")
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

// Bugbot HIGH (PR #258, commit 3643300): a grace-forced stop on a SUCCESS-but-
// still-gated wave must be transient, not a bare success. The /pr-polish flow
// yields the turn awaiting one long run_in_background join; lastResult stays
// successful for the whole join, so returning it after the 3-minute grace would
// report the validate step done to jiradozer while reviewers are still running.
// No ctx cancel and no queued event here — just a successful gated wave that
// goes silent; the stop must be classified transient (resumable).
func TestConsumeTurnEvents_GraceForcedSuccessGatedIsTransient(t *testing.T) {
	events := make(chan claude.Event, 8)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	events <- resultMessage(false) // Success=true
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}
	// bg tool never terminates, stream goes silent — grace is the backstop.

	grace := 100 * time.Millisecond
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
	require.NotNil(t, result)
	var transient *claude.TransientError
	require.ErrorAs(t, err, &transient,
		"a successful-but-gated grace-forced stop must be transient, not returned as a final success")
	ok, _ := ClassifyTransient(err)
	require.True(t, ok, "the success-gated grace-forced stop must be retryable")
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
	require.NotNil(t, result)
	// This test's invariant is the timing one below; the wave-2 grace fires
	// gated on a non-terminating bg tool_use, so the stop is transient
	// (resumable) per the bugbot HIGH fix — not a bare success.
	var transient *claude.TransientError
	require.ErrorAs(t, err, &transient,
		"wave-2 grace-forced stop gated on a live bg tool_use must be transient")
	// Wave 2's grace window must be a full `grace` measured from when wave 2
	// re-armed the timer — not the leftover slack from wave 1. Total elapsed
	// is therefore ~ (3/4)*grace (the sleep) + grace (wave 2's full window).
	require.GreaterOrEqual(t, time.Since(start), grace+grace/4,
		"wave 2 must get a fresh grace window, not wave 1's leftover slack")
}

// INF-1400 repro at the streamTurn layer (jiradozer run 1781627251447569146,
// /pr-polish-style validate round 3): the agent ends its turn on a
// ScheduleWakeup plus a background Monitor that *completes*; a terminal task
// notification invalidates the wave's Result+TurnComplete pair (lastResult ->
// nil), and the CLI then exits cleanly — the event stream CLOSES before any
// continuation ResultMessage arrives. This is distinct from the grace path
// (TestConsumeTurnEvents_GraceForcedNonSuccessIsTransient): there the stream
// stays open and silent with a live bg tool, so the stall is transient. Here
// the stream is closed — the CLI process is gone, there is nothing to resume —
// so a turn that produced a successful wave must resolve as a terminal success,
// NOT the silent Success=false/Error=nil that a caller (jiradozer) reads as a
// bare "agent failed".
func TestConsumeTurnEvents_ClosedStreamAfterInvalidatedSuccessIsSuccess(t *testing.T) {
	events := make(chan claude.Event, 16)
	// Wave 1: assistant ends on a bg Monitor; CLI fires a successful Result +
	// TurnComplete while the bg task is still live.
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	events <- resultMessage(false) // Success=true; recorded as lastSuccessfulResult
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}
	// The bg work completes: a terminal notification invalidates the wave's
	// Result+TurnComplete pair (lastResult -> nil) and the bg tool finishes.
	events <- claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "completed",
	}
	// The CLI exits cleanly — no continuation ResultMessage arrives.
	close(events)

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
		t.Fatal("consumeTurnEvents hung on a closed stream")
	}
	require.NoError(t, err, "a clean EOF must not synthesize an error")
	require.NotNil(t, result)
	require.True(t, result.Success,
		"a clean stream close after a successful-then-invalidated wave must resolve as success")
	require.Equal(t, int64(100), result.DurationMs,
		"terminal resolution must restore the successful wave's duration (resultMessage sets 100)")
}

// A failed/killed/timeout bg task at a clean stream close must NOT be reported
// as success: the successful end-of-turn result fired while the Monitor was
// still live, the Monitor then failed, and the CLI exited without a
// continuation. Without the failed-task gate this would mask the failure as a
// terminal success.
func TestConsumeTurnEvents_ClosedStreamAfterFailedBgTaskIsFailure(t *testing.T) {
	events := make(chan claude.Event, 16)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{monitorToolUse("toolu_bg1")},
	}
	events <- claude.TaskStartedEvent{TaskID: "task1", ToolUseID: strPtr("toolu_bg1")}
	events <- resultMessage(false) // successful end-of-turn while bg still live
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: true}
	// The Monitor FAILS: terminal notification invalidates the wave and marks
	// the bg work failed.
	events <- claude.TaskNotificationEvent{
		TaskID: "task1", ToolUseID: strPtr("toolu_bg1"), Status: "failed",
	}
	close(events) // CLI exits without a continuation ResultMessage

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
		t.Fatal("consumeTurnEvents hung on a closed stream")
	}
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success,
		"a clean stream close after a FAILED bg task must not be reported as success")
}

// Negative case: a stream that closes before ANY successful ResultMessage is a
// real failure — the terminal-EOF resolution must NOT manufacture success.
func TestConsumeTurnEvents_ClosedStreamBeforeAnyResultIsFailure(t *testing.T) {
	events := make(chan claude.Event, 8)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{asstTextBlock("starting")},
	}
	// No ResultMessage ever arrives; the stream just closes.
	close(events)

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
		t.Fatal("consumeTurnEvents hung on a closed stream")
	}
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success,
		"a stream that closes before any successful result must report Success=false")
}

// Negative case: a stream that closes after an ERROR result must surface the
// error unchanged — the terminal-EOF resolution must never mask a real error,
// even though no live bg tool keeps the turn gated.
func TestConsumeTurnEvents_ClosedStreamAfterErrorPreservesError(t *testing.T) {
	events := make(chan claude.Event, 8)
	events <- claude.AssistantMessageEvent{
		TurnNumber: 1,
		Blocks:     claude.ContentBlocks{asstTextBlock("oops")},
	}
	events <- resultMessage(true) // error result -> state.err set, Success=false
	events <- claude.TurnCompleteEvent{TurnNumber: 1, Success: false}
	close(events)

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
		t.Fatal("consumeTurnEvents hung on a closed stream")
	}
	require.Error(t, err, "a closed stream after an error result must surface the error")
	require.NotNil(t, result)
	require.False(t, result.Success)
}

// resolveGracePeriod must honor a positive ExecuteConfig override and fall back
// to the provider default for the zero value. This is the per-step
// configurability that lets long-running background work (e.g. bramble
// reviewers driven by /pr-polish) raise the grace period above the default.
func TestResolveGracePeriod(t *testing.T) {
	// Unset (zero) -> provider default.
	require.Equal(t, streamTurnGracePeriod, resolveGracePeriod(ExecuteConfig{}),
		"a zero override must fall back to the provider default")

	// Positive override -> used verbatim.
	override := 25 * time.Minute
	require.Equal(t, override,
		resolveGracePeriod(ExecuteConfig{StreamTurnGracePeriod: override}),
		"a positive override must take precedence over the default")

	// WithProviderStreamTurnGracePeriod must set the field the resolver reads.
	cfg := ExecuteConfig{}
	WithProviderStreamTurnGracePeriod(override)(&cfg)
	require.Equal(t, override, resolveGracePeriod(cfg),
		"WithProviderStreamTurnGracePeriod must flow through to resolveGracePeriod")
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
