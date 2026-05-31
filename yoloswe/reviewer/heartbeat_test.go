package reviewer

import (
	"context"
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
		result, err = bridgeStreamEvents(context.Background(), ch, handler, "")
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
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "")
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

// withIdleTimeout overrides the package idle deadline for one test and
// restores it on cleanup. Tests using it must NOT run in parallel.
func withIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := idleTimeout
	idleTimeout = d
	t.Cleanup(func() { idleTimeout = prev })
}

// A stream that goes silent past the idle deadline must trip the inactivity
// timeout and return an error, rather than blocking forever.
func TestBridgeStreamEvents_IdleTimeoutTrips(t *testing.T) {
	withHeartbeat(t, 15*time.Millisecond) // tick faster than the idle window
	withIdleTimeout(t, 40*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}

	done := make(chan struct{})
	var err error
	go func() {
		_, err = bridgeStreamEvents(context.Background(), ch, handler, "")
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

// A steadily-active stream must NOT trip the idle timeout: each event resets
// the inactivity clock, so a review longer than the idle window still
// completes normally as long as events keep arriving.
func TestBridgeStreamEvents_IdleTimeoutResetsOnActivity(t *testing.T) {
	withHeartbeat(t, 10*time.Millisecond)
	withIdleTimeout(t, 50*time.Millisecond)

	ch := make(chan agentstream.Event)
	handler := &recordingHandler{}

	done := make(chan struct{})
	var result *bridgeResult
	var err error
	go func() {
		result, err = bridgeStreamEvents(context.Background(), ch, handler, "")
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
