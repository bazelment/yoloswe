package reviewer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
)

// syncBuffer is a goroutine-safe sink for heartbeat output: bridgeStreamEvents
// writes from its own goroutine while the test reads concurrently.
type syncBuffer struct {
	b  strings.Builder
	mu sync.Mutex
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// withHeartbeat overrides the package heartbeat globals for one test and
// restores them on cleanup. The tests using it must NOT run in parallel since
// they mutate shared package state.
func withHeartbeat(t *testing.T, interval time.Duration) *syncBuffer {
	t.Helper()
	prevInterval, prevOut := heartbeatInterval, heartbeatOut
	buf := &syncBuffer{}
	heartbeatInterval = interval
	heartbeatOut = buf
	t.Cleanup(func() {
		heartbeatInterval = prevInterval
		heartbeatOut = prevOut
	})
	return buf
}

// On a silent window (no events for longer than a heartbeat interval) the
// bridge must still emit a bare idle pulse, then return cleanly once the turn
// completes.
func TestBridgeStreamEvents_HeartbeatSilentWindow(t *testing.T) {
	buf := withHeartbeat(t, 20*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}

	done := make(chan struct{})
	var result *bridgeResult
	var err error
	go func() {
		result, err = bridgeStreamEvents(context.Background(), ch, handler, "", 0)
		close(done)
	}()

	// A tool is in flight, then the stream goes silent past several intervals.
	ch <- testToolStartEvent{name: "Bash", callID: "c1"}
	time.Sleep(80 * time.Millisecond) // ~4 idle ticks, no further events
	ch <- testTurnCompleteEvent{success: true, durationMs: 10}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not return")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || !result.success {
		t.Fatal("expected a successful result")
	}

	out := buf.String()
	if !strings.Contains(out, "[code-review] heartbeat") {
		t.Fatalf("expected at least one heartbeat line, got:\n%s", out)
	}
	if !strings.Contains(out, "idle (awaiting backend)") {
		t.Errorf("expected an idle pulse on the silent window, got:\n%s", out)
	}
	// The in-flight Bash tool started before the silent window must be surfaced.
	if !strings.Contains(out, "tool(s) in flight") {
		t.Errorf("expected idle pulse to report the in-flight tool, got:\n%s", out)
	}
}

// On a window with activity the heartbeat must summarize what happened (tools
// completed, in-flight count, streamed char volume) instead of a bare pulse.
func TestBridgeStreamEvents_HeartbeatActiveWindow(t *testing.T) {
	buf := withHeartbeat(t, 30*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}

	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "", 0)
		close(done)
	}()

	// Activity within the first window: a completed tool, a still-open tool,
	// and streamed text + reasoning.
	ch <- testToolStartEvent{name: "Read", callID: "c1"}
	ch <- testToolEndEvent{name: "Read", callID: "c1"}
	ch <- testToolStartEvent{name: "Grep", callID: "c2"} // stays in flight
	ch <- testThinkingEvent{delta: strings.Repeat("r", 400)}
	ch <- testTextEvent{delta: strings.Repeat("x", 1200)}
	time.Sleep(60 * time.Millisecond) // let a heartbeat tick fire on this window
	ch <- testToolEndEvent{name: "Grep", callID: "c2"}
	ch <- testTurnCompleteEvent{success: true, durationMs: 10}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not return")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the active-window summary line. A later silent window may also emit
	// an idle pulse — that's legitimate — so assert on the summary line itself
	// rather than the absence of idle lines anywhere in the output.
	out := buf.String()
	var active string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "done: Read") {
			active = line
			break
		}
	}
	if active == "" {
		t.Fatalf("expected an active-window summary line naming the completed tool, got:\n%s", out)
	}
	if !strings.Contains(active, "in flight: 1") {
		t.Errorf("expected 1 tool in flight (Grep) on the active line, got: %q", active)
	}
	if !strings.Contains(active, "chars") {
		t.Errorf("expected streamed char volume on the active line, got: %q", active)
	}
	if !strings.Contains(active, "reasoning") {
		t.Errorf("expected reasoning char volume on the active line, got: %q", active)
	}
	if strings.Contains(active, "idle (awaiting backend)") {
		t.Errorf("active summary line must not be an idle pulse, got: %q", active)
	}

	// Live per-event handler calls must still fire (heartbeat is additive).
	if len(handler.toolStarts) != 2 || len(handler.toolEnds) != 2 {
		t.Errorf("expected live tool handler calls intact, got starts=%v ends=%v",
			handler.toolStarts, handler.toolEnds)
	}
	if len(handler.texts) != 1 || len(handler.reasonings) != 1 {
		t.Errorf("expected live text/reasoning handler calls intact, got texts=%d reasonings=%d",
			len(handler.texts), len(handler.reasonings))
	}
}

// A stream that goes silent past the idle deadline must trip the inactivity
// timeout and return an error, rather than blocking forever. idleTimeout is now
// a per-call parameter (no package global), so each test passes its own value.
func TestBridgeStreamEvents_IdleTimeoutTrips(t *testing.T) {
	withHeartbeat(t, 15*time.Millisecond) // tick faster than the idle window

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}

	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "", 40*time.Millisecond)
		close(done)
	}()

	// One event proves the stream started, then it goes silent forever.
	ch <- testTextEvent{delta: "hello"}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not trip the idle timeout")
	}
	if err == nil || !strings.Contains(err.Error(), "review idle") {
		t.Fatalf("expected an idle-timeout error, got: %v", err)
	}
}

// idleTimeout=0 (the default for callers that don't opt in, e.g. yoloswe/swe.go)
// must DISABLE the idle check entirely: a stream that goes silent forever must
// NOT be killed — it blocks until a terminal event arrives. Guards the HIGH
// regression where a package-global default imposed a stall policy on callers
// that never asked for one.
func TestBridgeStreamEvents_IdleTimeoutZeroDisables(t *testing.T) {
	withHeartbeat(t, 10*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}
	done := make(chan struct{})
	var result *bridgeResult
	var err error
	go func() {
		result, err = bridgeStreamEvents(context.Background(), ch, handler, "", 0)
		close(done)
	}()

	// One event, then a long silence well past any plausible idle window. With
	// idle disabled the loop must keep waiting; the turn only ends when a
	// terminal event finally arrives.
	ch <- testTextEvent{delta: "hello"}
	time.Sleep(120 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("idle=0 must not trip; the loop returned during the silent window")
	default:
	}
	ch <- testTurnCompleteEvent{success: true, durationMs: 1}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not return after the terminal event")
	}
	if err != nil {
		t.Fatalf("idle=0 must not produce an idle error, got: %v", err)
	}
	if result == nil || !result.success {
		t.Fatal("expected a successful result")
	}
}

// A steadily-active stream must NOT trip the idle timeout: each event resets
// the inactivity clock, so a review longer than the idle window still
// completes normally as long as events keep arriving.
func TestBridgeStreamEvents_IdleTimeoutResetsOnActivity(t *testing.T) {
	withHeartbeat(t, 10*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}

	done := make(chan struct{})
	var result *bridgeResult
	var err error
	go func() {
		result, err = bridgeStreamEvents(context.Background(), ch, handler, "", 50*time.Millisecond)
		close(done)
	}()

	// Send events at ~20ms spacing for ~120ms — well past the 50ms idle window
	// in aggregate, but never 50ms apart, so the idle clock keeps resetting.
	for i := 0; i < 6; i++ {
		ch <- testTextEvent{delta: "x"}
		time.Sleep(20 * time.Millisecond)
	}
	ch <- testTurnCompleteEvent{success: true, durationMs: 1}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not return on a steadily-active stream")
	}
	if err != nil {
		t.Fatalf("active stream must not trip the idle timeout, got: %v", err)
	}
	if result == nil || !result.success {
		t.Fatal("expected a successful result on a steadily-active stream")
	}
}

// Regression (codex/cursor PR #258): the idle check must not fire while a real
// event is already queued. select can pick the ticker over a ready events case,
// so the ticker branch must drain pending events before declaring the stream
// stalled — otherwise a healthy review with buffered traffic gets a false
// "review idle" and the queued event is dropped. The pre-sleep makes lastEvent
// stale on entry, so on the first tick the idle deadline IS already exceeded —
// the loop must still drain the queued events rather than trip.
func TestBridgeStreamEvents_IdleDoesNotTripWithPendingEvent(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan agentstream.Event, 8)
	ch <- testTextEvent{delta: "alive"}
	ch <- testTurnCompleteEvent{success: true, durationMs: 1}

	handler := &recordingHandler{}
	done := make(chan struct{})
	var result *bridgeResult
	var err error
	go func() {
		// Sleep past the idle window before consuming so lastEvent is "stale",
		// then the first ticker tick must still drain the queued events.
		time.Sleep(15 * time.Millisecond)
		result, err = bridgeStreamEvents(context.Background(), ch, handler, "", 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents hung")
	}
	if err != nil {
		t.Fatalf("idle must not trip while events are pending, got: %v", err)
	}
	if result == nil || !result.success {
		t.Fatal("expected the queued continuation to complete the turn")
	}
	if len(handler.texts) != 1 {
		t.Errorf("the queued text event must have been processed, got %d", len(handler.texts))
	}
}

// Regression (codex+cursor PR #258, rounds 2-3): only IN-SCOPE events reset the
// idle clock, and PERPETUAL out-of-scope traffic on a multiplexed channel must
// not suppress the idle trip for a stalled in-scope thread. This is the
// production shape: Codex's shared client.Events() carries other threads'
// events continuously. The sender runs until the function returns, so a
// regression that (a) reset lastEvent on out-of-scope events, or (b) gated the
// idle trip on "did we drain anything" rather than lastEvent staleness, would
// hang here instead of tripping.
func TestBridgeStreamEvents_IdleIgnoresPerpetualOutOfScopeTraffic(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}
	done := make(chan struct{})
	stop := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "thread-1", 40*time.Millisecond)
		close(done)
	}()

	// Perpetual out-of-scope noise: keep sending another thread's events the
	// whole time the bridge runs, never stopping until it returns. If
	// out-of-scope traffic suppressed the idle trip, this would never finish.
	go func() {
		for {
			select {
			case <-stop:
				return
			case ch <- testScopedTextEvent{testTextEvent: testTextEvent{delta: "other"}, scopeID: "thread-2"}:
			}
		}
	}()
	defer close(stop)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle timeout never tripped under perpetual out-of-scope traffic")
	}
	if err == nil || !strings.Contains(err.Error(), "review idle") {
		t.Fatalf("expected idle-timeout despite perpetual out-of-scope traffic, got: %v", err)
	}
}

// Unit coverage for the line formatter independent of timing.
func TestFormatHeartbeat(t *testing.T) {
	idle := formatHeartbeat(95*time.Second, heartbeatWindow{}, 1)
	if !strings.Contains(idle, "idle (awaiting backend)") || !strings.Contains(idle, "1 tool(s) in flight") {
		t.Errorf("idle line malformed: %q", idle)
	}

	active := formatHeartbeat(
		95*time.Second,
		heartbeatWindow{toolsCompleted: []string{"Read", "Grep", "Read"}, textChars: 1234, reasoningChars: 400, events: 5},
		2,
	)
	if !strings.Contains(active, "done: Grep,Read(2)") { // sorted, with count
		t.Errorf("expected sorted tool summary with counts, got: %q", active)
	}
	if !strings.Contains(active, "in flight: 2") {
		t.Errorf("expected in-flight count, got: %q", active)
	}
	if !strings.Contains(active, "+1.2k chars") {
		t.Errorf("expected k-suffixed char count, got: %q", active)
	}
	if !strings.Contains(active, "+400 reasoning") {
		t.Errorf("expected reasoning char count, got: %q", active)
	}
}

// --- Liveness is a transport fact, not a semantic one --------------------
//
// Regression tests for kernel#8682: the bridge only reset the idle clock on
// events it could RENDER, so a backend that was demonstrably alive — sending
// events the agentstream subset doesn't cover — was killed as "stalled". These
// pin the corrected contract: scopeID alone decides liveness.

// testNonStreamEvent implements NO agentstream interface at all — the shape of
// codex.ItemStartedEvent / ItemCompletedEvent / TokenUsageEvent /
// CommandOutputEvent, which agentstream/doc.go deliberately excludes from the
// common subset. Arrival still proves the backend is alive.
type testNonStreamEvent struct{ note string }

// testScopedUnknownEvent returns KindUnknown but carries a scope — the shape of
// a conditional event (e.g. ACP ToolCallUpdateEvent mid-status).
type testScopedUnknownEvent struct{ scopeID string }

func (e testScopedUnknownEvent) StreamEventKind() agentstream.EventKind {
	return agentstream.KindUnknown
}
func (e testScopedUnknownEvent) ScopeID() string { return e.scopeID }

// The r3 gap: item/started and item/completed were the ONLY events for 132s.
// Neither implements agentstream.Event, so the old bridge saw dead air and
// killed a working review.
func TestBridgeStreamEvents_UnrenderableEventsKeepStreamAlive(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan interface{})
	handler := &recordingHandler{}
	done := make(chan struct{})
	var result *bridgeResult
	var err error
	go func() {
		result, err = bridgeStreamEvents(context.Background(), ch, handler, "", 200*time.Millisecond)
		close(done)
	}()

	// Drip unrenderable events for well over the idle window. Each must reset
	// the clock even though none of them can be displayed.
	for i := 0; i < 8; i++ {
		ch <- testNonStreamEvent{note: "item/started"}
		time.Sleep(20 * time.Millisecond)
		select {
		case <-done:
			t.Fatalf("bridge died at drip %d: an unrenderable in-scope event must reset the idle clock (err=%v)", i, err)
		default:
		}
	}
	ch <- testTurnCompleteEvent{success: true, durationMs: 1}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not return after the terminal event")
	}
	if err != nil {
		t.Fatalf("unrenderable events must not produce an idle error, got: %v", err)
	}
	if result == nil || !result.success {
		t.Fatal("expected a successful result")
	}
	// Liveness only — they must still never be rendered.
	if len(handler.texts) != 0 || len(handler.toolStarts) != 0 {
		t.Errorf("unrenderable events must not reach the handler: texts=%d toolStarts=%d",
			len(handler.texts), len(handler.toolStarts))
	}
}

// A KindUnknown event that passes the scope check is also proof of life.
func TestBridgeStreamEvents_UnknownKindKeepsStreamAlive(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}
	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "thread-1", 200*time.Millisecond)
		close(done)
	}()

	for i := 0; i < 6; i++ {
		ch <- testScopedUnknownEvent{scopeID: "thread-1"}
		time.Sleep(20 * time.Millisecond)
		select {
		case <-done:
			t.Fatalf("bridge died at drip %d: an in-scope KindUnknown event must reset the idle clock (err=%v)", i, err)
		default:
		}
	}
	ch <- testTurnCompleteEvent{success: true, durationMs: 1}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not return after the terminal event")
	}
	if err != nil {
		t.Fatalf("in-scope KindUnknown must not produce an idle error, got: %v", err)
	}
}

// The multiplex guard must SURVIVE the fix: another thread's traffic still must
// not keep a stalled thread alive, whether or not it is renderable.
func TestBridgeStreamEvents_OutOfScopeEventsStillTripIdle(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}
	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "thread-1", 200*time.Millisecond)
		close(done)
	}()

	// Our thread speaks once, then only OTHER threads talk. The idle timer must
	// still trip — out-of-scope noise is not our liveness.
	ch <- testScopedTextEvent{testTextEvent: testTextEvent{delta: "ours"}, scopeID: "thread-1"}
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			select {
			case ch <- testScopedUnknownEvent{scopeID: "thread-2"}:
			case <-done:
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("out-of-scope traffic suppressed the idle trip — the multiplex guard regressed")
	}
	if err == nil || !strings.Contains(err.Error(), "review idle") {
		t.Fatalf("expected an idle-timeout error despite out-of-scope traffic, got: %v", err)
	}
}

// A stream that genuinely emits NOTHING must still be killed — the fix must not
// disable the timeout wholesale.
func TestBridgeStreamEvents_TrueSilenceStillTripsIdle(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}
	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "", 40*time.Millisecond)
		close(done)
	}()

	ch <- testTextEvent{delta: "hello"}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("true silence must still trip the idle timeout")
	}
	if err == nil || !strings.Contains(err.Error(), "review idle") {
		t.Fatalf("expected an idle-timeout error, got: %v", err)
	}
}

// Replay of the kernel#8682 r2 timing, scaled down 1000x. Real numbers:
// agentMessage/delta at t, item/started at t+17s, kill at t+309s on a 300s
// timer. Bramble measured 309s of silence; the wire had 293s. With item/started
// counted the turn survives.
func TestBridgeStreamEvents_Kernel8682R2TimingSurvives(t *testing.T) {
	withHeartbeat(t, 3*time.Millisecond)

	// Margins are wide on purpose: the point is the ORDERING (item/started
	// resets the clock), not tight timing. A 30ms gap against a 300ms bound
	// made this a scheduler race under parallel `bazel test`.
	const idle = 900 * time.Millisecond // stands in for 300s
	ch := make(chan interface{})
	handler := &recordingHandler{}
	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "", idle)
		close(done)
	}()

	ch <- testTextEvent{delta: "agentMessage/delta"} // t
	time.Sleep(180 * time.Millisecond)               // +17s, scaled up for timer slack
	ch <- testNonStreamEvent{note: "item/started"}   // the event the old code discarded

	// Now wait past the point where the OLD code would have died. The old clock
	// ran from agentMessage/delta, so it trips at t+900ms; the corrected clock
	// runs from item/started and does not trip until t+1080ms. Sleeping to
	// ~t+990ms straddles the two: dead under the old rule, alive under the new,
	// with a 90ms cushion on each side rather than 30ms.
	// (Bounded by the terminal event below, so a slow machine cannot flake into
	// a false pass — it would time out at the select instead.)
	time.Sleep(810 * time.Millisecond)
	select {
	case <-done:
		t.Fatalf("regressed: killed at the 8682 r2 timing — item/started must reset the clock (err=%v)", err)
	default:
	}

	ch <- testTurnCompleteEvent{success: true, durationMs: 1}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridgeStreamEvents did not return after the terminal event")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// testUnscopedThreadEvent is the pre-fix shape of codex's ItemStartedEvent &
// friends: carries a thread id in a plain field, implements neither
// agentstream.Event nor agentstream.Scoped. Before ScopeID() was added to those
// types, the bridge could not tell whose traffic this was — and once ANY
// arriving event counted as liveness, another thread's chatter kept a stalled
// thread alive. Consensus finding (cursor + claude) on PR #314.
type testUnscopedThreadEvent struct{ threadID string }

// testScopedNonStreamEvent is the POST-fix shape: still outside the
// agentstream.Event subset, but scoped, so the bridge can attribute it.
type testScopedNonStreamEvent struct{ scopeID string }

func (e testScopedNonStreamEvent) ScopeID() string { return e.scopeID }

// The multiplex guard must hold for unrenderable events too: an event scoped to
// ANOTHER thread must not reset our idle clock, even though it is now liveness
// evidence for its own thread.
func TestBridgeStreamEvents_UnrenderableOutOfScopeDoesNotKeepUsAlive(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan interface{})
	handler := &recordingHandler{}
	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "thread-1", 200*time.Millisecond)
		close(done)
	}()

	// Our thread speaks once, then only thread-2's unrenderable traffic flows.
	ch <- testScopedTextEvent{testTextEvent: testTextEvent{delta: "ours"}, scopeID: "thread-1"}
	go func() {
		for {
			select {
			case ch <- testScopedNonStreamEvent{scopeID: "thread-2"}:
			case <-done:
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("another thread's unrenderable traffic suppressed the idle trip")
	}
	if err == nil || !strings.Contains(err.Error(), "review idle") {
		t.Fatalf("expected an idle-timeout error, got: %v", err)
	}
}

// Documents the residual hole this design accepts: an event carrying a thread
// id that does NOT implement Scoped is unattributable, so it counts as liveness
// for every bridge on a shared channel. The fix is at the producer — every
// multiplexed codex event now implements ScopeID() — and
// TestEveryThreadedCodexEventIsScoped in the codex package is what keeps that
// true. This test pins the bridge's half of the contract so the two cannot
// drift apart silently.
func TestBridgeStreamEvents_UnscopedEventCountsAsLivenessByDesign(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)

	ch := make(chan interface{})
	handler := &recordingHandler{}
	done := make(chan struct{})
	go func() {
		_, _ = bridgeStreamEvents(context.Background(), ch, handler, "thread-1", 200*time.Millisecond)
		close(done)
	}()

	for i := 0; i < 5; i++ {
		ch <- testUnscopedThreadEvent{threadID: "thread-2"}
		time.Sleep(20 * time.Millisecond)
		select {
		case <-done:
			t.Fatalf("drip %d: an unscoped event cannot be attributed, so it counts "+
				"as liveness — if this now trips, the bridge changed and the "+
				"producer-side ScopeID() contract needs revisiting", i)
		default:
		}
	}
	ch <- testTurnCompleteEvent{success: true, durationMs: 1}
	<-done
}

// Partial preservation must apply on BOTH terminal paths. The idle timeout
// returns accumulated text; a channel close without TurnComplete has to do the
// same, or the same rule holds on one sibling and not the other (cursor, r2).
func TestBridgeStreamEvents_ChannelCloseKeepsPartialText(t *testing.T) {
	withHeartbeat(t, 10*time.Millisecond)

	body := `{"verdict":"rejected","summary":"s","issues":[]}`
	ch := make(chan agentstream.Event, 2)
	ch <- testTextEvent{delta: body}
	close(ch) // stream drops before TurnComplete

	res, err := bridgeStreamEvents(context.Background(), ch, &recordingHandler{}, "", 0)
	if err == nil {
		t.Fatal("a close without TurnComplete must still be an error")
	}
	if res == nil {
		t.Fatal("streamed text must be returned with the error, not only counted in it")
	}
	if res.responseText != body {
		t.Errorf("responseText = %q, want the streamed body", res.responseText)
	}
}

func TestBridgeStreamEvents_ChannelCloseWithNoTextReturnsNil(t *testing.T) {
	withHeartbeat(t, 10*time.Millisecond)
	ch := make(chan agentstream.Event)
	close(ch)
	res, err := bridgeStreamEvents(context.Background(), ch, &recordingHandler{}, "", 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if res != nil {
		t.Errorf("nothing was streamed, so there is nothing to preserve; got %+v", res)
	}
}

// Every terminal path preserves partial work — asserted as ONE rule, not per
// exit. Three consecutive review rounds each found a different exit that
// dropped accumulated text (idle timeout, channel close, ctx cancellation)
// because each decided the question for itself. This table is the rule.
//
// The ctx case is the one that fires in production: SKILL Step 3.b wraps every
// reviewer in `timeout 2400`, GNU timeout sends SIGTERM, and codereview.go
// installs signal.NotifyContext(SIGINT, SIGTERM) on the review context — so
// the absolute backstop cancels here, not at the idle timeout.
func TestBridgeStreamEvents_EveryFailurePathPreservesPartialWork(t *testing.T) {
	const body = `{"verdict":"rejected","summary":"s","issues":[]}`

	t.Run("ctx cancelled", func(t *testing.T) {
		withHeartbeat(t, 10*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan agentstream.Event)
		done := make(chan struct{})
		var res *bridgeResult
		var err error
		go func() {
			res, err = bridgeStreamEvents(ctx, ch, &recordingHandler{}, "", 0)
			close(done)
		}()
		ch <- testTextEvent{delta: body}
		cancel()
		<-done
		if err == nil {
			t.Fatal("cancellation must still be an error")
		}
		if res == nil || res.responseText != body {
			t.Fatalf("ctx cancellation dropped the streamed body: %+v", res)
		}
	})

	t.Run("idle timeout", func(t *testing.T) {
		withHeartbeat(t, 5*time.Millisecond)
		ch := make(chan agentstream.Event)
		done := make(chan struct{})
		var res *bridgeResult
		go func() {
			res, _ = bridgeStreamEvents(context.Background(), ch, &recordingHandler{}, "", 40*time.Millisecond)
			close(done)
		}()
		ch <- testTextEvent{delta: body}
		<-done
		if res == nil || res.responseText != body {
			t.Fatalf("idle timeout dropped the streamed body: %+v", res)
		}
	})

	t.Run("channel closed", func(t *testing.T) {
		withHeartbeat(t, 10*time.Millisecond)
		ch := make(chan agentstream.Event, 1)
		ch <- testTextEvent{delta: body}
		close(ch)
		res, err := bridgeStreamEvents(context.Background(), ch, &recordingHandler{}, "", 0)
		if err == nil {
			t.Fatal("a close without TurnComplete must still be an error")
		}
		if res == nil || res.responseText != body {
			t.Fatalf("channel close dropped the streamed body: %+v", res)
		}
	})

	t.Run("backend error event", func(t *testing.T) {
		// The arm that stayed nil for one round after the consolidation, while
		// the helper's comment already claimed totality. Triple consensus
		// (codex + cursor + claude), PR #314 r4.
		withHeartbeat(t, 10*time.Millisecond)
		ch := make(chan agentstream.Event, 2)
		ch <- testTextEvent{delta: body}
		ch <- testErrorEvent{err: errors.New("transport reset"), ctx: "stream"}
		res, err := bridgeStreamEvents(context.Background(), ch, &recordingHandler{}, "", 0)
		if err == nil {
			t.Fatal("a backend error event must still be an error")
		}
		if res == nil || res.responseText != body {
			t.Fatalf("KindError dropped the streamed body: %+v", res)
		}
	})

	t.Run("nothing streamed yields nil on every path", func(t *testing.T) {
		withHeartbeat(t, 5*time.Millisecond)
		ch := make(chan agentstream.Event)
		done := make(chan struct{})
		var res *bridgeResult
		go func() {
			res, _ = bridgeStreamEvents(context.Background(), ch, &recordingHandler{}, "", 30*time.Millisecond)
			close(done)
		}()
		<-done
		if res != nil {
			t.Errorf("no text accumulated, so there is nothing to preserve; got %+v", res)
		}
	})
}

// A partial result must report how long it actually ran. Token counts live on
// the TurnComplete event that never arrives on this path, but elapsed time is
// known — and "duration_ms: 0" on a review that ran for minutes before stalling
// is the exact misreading this change exists to prevent.
func TestBridgeStreamEvents_PartialResultCarriesElapsedTime(t *testing.T) {
	withHeartbeat(t, 5*time.Millisecond)
	ch := make(chan agentstream.Event)
	done := make(chan struct{})
	var res *bridgeResult
	go func() {
		res, _ = bridgeStreamEvents(context.Background(), ch, &recordingHandler{}, "", 40*time.Millisecond)
		close(done)
	}()
	ch <- testTextEvent{delta: `{"verdict":"accepted","summary":"s","issues":[]}`}
	<-done
	if res == nil {
		t.Fatal("expected the partial result")
	}
	if res.durationMs <= 0 {
		t.Errorf("durationMs = %d, want the elapsed time of the run", res.durationMs)
	}
}

// A terminal condition can win the select while a wave of already-emitted
// events is still queued — the SDK channels are buffered (codex
// EventBufferSize defaults to 100). The idle arm drained for exactly this
// reason; ctx.Done did not, so a review that finished in the same instant the
// context died was discarded along with the TurnComplete that would have made
// it a success.
func TestBridgeStreamEvents_CancellationDrainsQueuedEvents(t *testing.T) {
	withHeartbeat(t, 10*time.Millisecond)

	// Buffer the whole turn, then cancel before the loop reads any of it.
	ch := make(chan agentstream.Event, 4)
	ch <- testTextEvent{delta: `{"verdict":"accepted","summary":"s","issues":[]}`}
	ch <- testTurnCompleteEvent{success: true, durationMs: 7}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := bridgeStreamEvents(ctx, ch, &recordingHandler{}, "", 0)
	if err != nil {
		t.Fatalf("a completed turn sitting in the buffer must be delivered, got: %v", err)
	}
	if res == nil || !res.success {
		t.Fatalf("expected the queued TurnComplete to win, got %+v", res)
	}
}
