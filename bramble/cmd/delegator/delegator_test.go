package delegator

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestInteractiveLoop_IdleAfterChildNotification verifies that when a
// child-notification turn cycle produces a new idle signal, the interactive
// loop re-emits the status. This is the regression test for the bug where
// the loop was stuck in an inner select waiting for stdin, missing idleCh
// signals from child notification processing.
func TestInteractiveLoop_IdleAfterChildNotification(t *testing.T) {
	t.Parallel()

	idleCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	stdinCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	childrenActive := true
	var childMu sync.Mutex

	var statusMsgs []string
	var statusMu sync.Mutex
	writeStatus := func(msg string) {
		statusMu.Lock()
		statusMsgs = append(statusMsgs, msg)
		statusMu.Unlock()
	}

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		runInteractiveLoop(interactiveLoopConfig{
			hasActiveChildren: func() bool {
				childMu.Lock()
				defer childMu.Unlock()
				return childrenActive
			},
			sendFollowUp: func(msg string) error { return nil },
			idleCh:       idleCh,
			doneCh:       doneCh,
			stdinCh:      stdinCh,
			writeStatus:  writeStatus,
			ctx:          ctx,
		})
	}()

	// Step 1: Delegator goes idle with children still active.
	idleCh <- struct{}{}
	require.Eventually(t, func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()
		return len(statusMsgs) >= 1
	}, 2*time.Second, 10*time.Millisecond)
	statusMu.Lock()
	require.Equal(t, "idle-children-active", statusMsgs[0])
	statusMu.Unlock()

	// Step 2: Children complete, delegator goes idle again.
	// This is the critical part: the loop must process this new idleCh signal
	// and emit "idle" (not children-active), even though no stdin input arrived.
	childMu.Lock()
	childrenActive = false
	childMu.Unlock()
	idleCh <- struct{}{}
	require.Eventually(t, func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()
		return len(statusMsgs) >= 2
	}, 2*time.Second, 10*time.Millisecond)
	statusMu.Lock()
	require.Equal(t, "idle", statusMsgs[1])
	statusMu.Unlock()

	// Step 3: Quit.
	stdinCh <- "quit"
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive loop did not exit after quit")
	}
}

// TestInteractiveLoop_DrainBeforeSend verifies that rapid idle signals
// (from back-to-back child notification cycles) don't get dropped.
// This reproduces the exact bug: the event loop goroutine does
// drain-before-send on idleCh, so the final signal is never lost.
func TestInteractiveLoop_DrainBeforeSend(t *testing.T) {
	t.Parallel()

	idleCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	stdinCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	childrenActive := true
	var childMu sync.Mutex

	var statusMsgs []string
	var statusMu sync.Mutex
	writeStatus := func(msg string) {
		statusMu.Lock()
		statusMsgs = append(statusMsgs, msg)
		statusMu.Unlock()
	}

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		runInteractiveLoop(interactiveLoopConfig{
			hasActiveChildren: func() bool {
				childMu.Lock()
				defer childMu.Unlock()
				return childrenActive
			},
			sendFollowUp: func(msg string) error { return nil },
			idleCh:       idleCh,
			doneCh:       doneCh,
			stdinCh:      stdinCh,
			writeStatus:  writeStatus,
			ctx:          ctx,
		})
	}()

	// Step 1: First idle (children active).
	idleCh <- struct{}{}
	require.Eventually(t, func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()
		return len(statusMsgs) >= 1
	}, 2*time.Second, 10*time.Millisecond)

	// Step 2: Simulate drain-before-send pattern from event loop goroutine.
	// This is what the fixed event loop does: drain the stale signal, then
	// send the new one. Even if the loop hasn't consumed the previous idle
	// from idleCh yet, the new signal replaces it.
	childMu.Lock()
	childrenActive = false
	childMu.Unlock()
	// Drain stale (same as fix in event loop goroutine).
	select {
	case <-idleCh:
	default:
	}
	// Send fresh signal — blocking because we just drained.
	idleCh <- struct{}{}

	// The loop must eventually see this and emit "idle".
	require.Eventually(t, func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()
		for _, msg := range statusMsgs {
			if msg == "idle" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)

	// Quit.
	stdinCh <- "quit"
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive loop did not exit after quit")
	}
}

// TestInteractiveLoop_QuitExits verifies basic quit handling.
func TestInteractiveLoop_QuitExits(t *testing.T) {
	t.Parallel()

	idleCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	stdinCh := make(chan string, 1)
	ctx := context.Background()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		runInteractiveLoop(interactiveLoopConfig{
			hasActiveChildren: func() bool { return false },
			sendFollowUp:      func(msg string) error { return nil },
			idleCh:            idleCh,
			doneCh:            doneCh,
			stdinCh:           stdinCh,
			writeStatus:       func(string) {},
			ctx:               ctx,
		})
	}()

	stdinCh <- "quit"
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive loop did not exit after quit")
	}
}

// TestSpinner_StartStopIdempotent verifies that Start/Stop are safe to call
// multiple times and that Stop is safe when the spinner was never started.
//
// Note: these tests only exercise the no-op (non-terminal) path because
// bytes.Buffer is not a PTY. The animation goroutine, ticker cleanup, and
// ANSI clear sequence can only be exercised on a real terminal.
func TestSpinner_StartStopIdempotent(t *testing.T) {
	t.Parallel()

	// Spinner with a non-terminal writer should be a no-op.
	buf := &bytes.Buffer{}
	s := NewSpinner(buf)

	// Stop without Start is safe.
	s.Stop()

	// Start with non-terminal writer is a no-op (no goroutine spawned).
	s.Start("test")
	s.Stop()

	// Double stop is safe.
	s.Stop()
	s.Stop()
}

// TestInteractiveLoop_CallbacksInvoked verifies that onInputSent is called
// after a successful sendFollowUp and onIdle is called on idle signals.
func TestInteractiveLoop_CallbacksInvoked(t *testing.T) {
	t.Parallel()

	idleCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	stdinCh := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var events []string
	var eventsMu sync.Mutex
	record := func(name string) {
		eventsMu.Lock()
		events = append(events, name)
		eventsMu.Unlock()
	}

	var promptSet string
	var promptMu sync.Mutex

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		runInteractiveLoop(interactiveLoopConfig{
			hasActiveChildren: func() bool { return false },
			sendFollowUp:      func(msg string) error { return nil },
			idleCh:            idleCh,
			doneCh:            doneCh,
			stdinCh:           stdinCh,
			writeStatus:       func(string) {},
			ctx:               ctx,
			onInputSent:       func() { record("inputSent") },
			onIdle:            func() { record("idle") },
			setPrompt: func(p string) {
				promptMu.Lock()
				promptSet = p
				promptMu.Unlock()
			},
		})
	}()

	// Send input — should trigger onInputSent.
	stdinCh <- "hello"

	// Wait for idle to come back, which triggers onIdle and setPrompt.
	idleCh <- struct{}{}

	require.Eventually(t, func() bool {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		return len(events) >= 2
	}, 2*time.Second, 10*time.Millisecond)

	eventsMu.Lock()
	require.Contains(t, events, "inputSent")
	require.Contains(t, events, "idle")
	eventsMu.Unlock()

	promptMu.Lock()
	require.Equal(t, ">>> ", promptSet)
	promptMu.Unlock()

	stdinCh <- "quit"
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive loop did not exit after quit")
	}
}

// TestInputReader_CloseUnblocksGoroutine verifies that calling Close() on an
// InputReader causes its internal goroutine to exit even when the consumer has
// stopped reading from Lines(). This guards against the goroutine-leak scenario
// where the goroutine is blocked on an unbuffered channel send.
func TestInputReader_CloseUnblocksGoroutine(t *testing.T) {
	t.Parallel()

	// Use a pipe so we can control both ends: the scanner goroutine reads from
	// pr, and we write to pw to simulate a line of input.
	pr, pw := io.Pipe()

	ir := &InputReader{
		lines: make(chan string),
		quit:  make(chan struct{}),
	}
	ir.startScanner(pr, "")

	// Write a line into the pipe so the scanner unblocks and tries to send on
	// ir.lines. Since nobody is reading ir.lines, the goroutine will block on
	// the channel send — exactly the leak scenario.
	go func() {
		pw.Write([]byte("hello\n")) //nolint:errcheck
	}()

	// Give the goroutine a moment to reach the blocked channel send, then close.
	time.Sleep(20 * time.Millisecond)

	// Close should unblock the goroutine via the quit channel.
	ir.Close()

	// After Close(), the goroutine must exit and close ir.lines. Drain any
	// pending values and then confirm the channel is closed.
	goroutineExited := false
	for !goroutineExited {
		select {
		case _, ok := <-ir.lines:
			if !ok {
				goroutineExited = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("InputReader goroutine did not exit after Close()")
		}
	}

	// Close the pipe to clean up.
	pw.Close()
	pr.Close()
}

// TestInteractiveLoop_DoneChExits verifies the loop exits when doneCh closes.
func TestInteractiveLoop_DoneChExits(t *testing.T) {
	t.Parallel()

	idleCh := make(chan struct{}, 1)
	doneCh := make(chan struct{})
	stdinCh := make(chan string, 1)
	ctx := context.Background()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		runInteractiveLoop(interactiveLoopConfig{
			hasActiveChildren: func() bool { return false },
			sendFollowUp:      func(msg string) error { return nil },
			idleCh:            idleCh,
			doneCh:            doneCh,
			stdinCh:           stdinCh,
			writeStatus:       func(string) {},
			ctx:               ctx,
		})
	}()

	close(doneCh)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive loop did not exit after doneCh closed")
	}
}
