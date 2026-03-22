package delegator

import (
	"context"
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
