package cliapp

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// signalSelf delivers SIGINT to the running test process. The signal handler
// goroutine in notifyContext picks it up the same as a user-typed Ctrl-C.
// We use SIGINT (not a fake channel) because notifyContext owns the
// signal.Notify wiring; testing the real path is the point.
func signalSelf(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("kill self: %v", err)
	}
}

func TestNotifyContext_FirstSignalCancels(t *testing.T) {
	// NOTE: Cannot run in parallel — this test sends a real SIGINT to the
	// test process, which would interfere with sibling tests using the
	// same signal.
	parent := context.Background()
	var forced atomic.Bool
	ctx, cancel := notifyContext(parent, func() { forced.Store(true) })
	defer cancel()

	signalSelf(t)

	select {
	case <-ctx.Done():
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("ctx not cancelled after first signal")
	}
	if forced.Load() {
		t.Error("forceExit fired on first signal; should only fire on second")
	}
}

func TestNotifyContext_SecondSignalForcesExit(t *testing.T) {
	parent := context.Background()

	forced := make(chan struct{}, 1)
	ctx, cancel := notifyContext(parent, func() {
		select {
		case forced <- struct{}{}:
		default:
		}
	})
	defer cancel()

	signalSelf(t)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ctx not cancelled after first signal")
	}

	signalSelf(t)
	select {
	case <-forced:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("forceExit not invoked on second signal")
	}
}

func TestNotifyContext_ParentCancelStopsGoroutine(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(context.Background())
	var forced atomic.Bool
	ctx, cancel := notifyContext(parent, func() { forced.Store(true) })
	defer cancel()

	parentCancel()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ctx not cancelled when parent was")
	}
	// Give the goroutine a moment to exit; it should not have called forceExit.
	time.Sleep(50 * time.Millisecond)
	if forced.Load() {
		t.Error("forceExit fired on parent cancel; should only fire on second signal")
	}
}

func TestNotifyContext_CancelStopsBeforeAnySignal(t *testing.T) {
	t.Parallel()
	var forced atomic.Bool
	_, cancel := notifyContext(context.Background(), func() { forced.Store(true) })
	cancel()
	// Cancel must not panic on double-call.
	cancel()
	time.Sleep(20 * time.Millisecond)
	if forced.Load() {
		t.Error("forceExit fired without any signal")
	}
}
