package codex

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestClientState_String(t *testing.T) {
	tests := []struct {
		state    ClientState
		expected string
	}{
		{ClientStateUninitialized, "uninitialized"},
		{ClientStateStarting, "starting"},
		{ClientStateReady, "ready"},
		{ClientStateClosed, "closed"},
		{ClientState(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("ClientState(%d).String() = %q, want %q", tt.state, got, tt.expected)
		}
	}
}

func TestThreadState_String(t *testing.T) {
	tests := []struct {
		state    ThreadState
		expected string
	}{
		{ThreadStateCreating, "creating"},
		{ThreadStateStarting, "starting"},
		{ThreadStateReady, "ready"},
		{ThreadStateProcessing, "processing"},
		{ThreadStateClosed, "closed"},
		{ThreadState(99), "unknown"},
	}

	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("ThreadState(%d).String() = %q, want %q", tt.state, got, tt.expected)
		}
	}
}

func TestClientStateManager_Transitions(t *testing.T) {
	m := newClientStateManager()

	// Initial state
	if m.Current() != ClientStateUninitialized {
		t.Errorf("expected initial state uninitialized, got %v", m.Current())
	}

	// SetStarting from uninitialized
	if err := m.SetStarting(); err != nil {
		t.Errorf("SetStarting failed: %v", err)
	}
	if m.Current() != ClientStateStarting {
		t.Errorf("expected starting, got %v", m.Current())
	}

	// SetStarting again should fail
	if err := m.SetStarting(); err == nil {
		t.Error("SetStarting should fail when not uninitialized")
	}

	// SetReady from starting
	if err := m.SetReady(); err != nil {
		t.Errorf("SetReady failed: %v", err)
	}
	if m.Current() != ClientStateReady {
		t.Errorf("expected ready, got %v", m.Current())
	}
	if !m.IsReady() {
		t.Error("IsReady should return true")
	}

	// SetReady again should fail
	if err := m.SetReady(); err == nil {
		t.Error("SetReady should fail when not starting")
	}

	// SetClosed
	m.SetClosed()
	if m.Current() != ClientStateClosed {
		t.Errorf("expected closed, got %v", m.Current())
	}
	if !m.IsClosed() {
		t.Error("IsClosed should return true")
	}
}

func TestClientStateManager_SetClosedFromAnyState(t *testing.T) {
	states := []struct {
		name  string
		setup func(*clientStateManager)
	}{
		{"uninitialized", func(m *clientStateManager) {}},
		{"starting", func(m *clientStateManager) { m.SetStarting() }},
		{"ready", func(m *clientStateManager) { m.SetStarting(); m.SetReady() }},
	}

	for _, tt := range states {
		t.Run(tt.name, func(t *testing.T) {
			m := newClientStateManager()
			tt.setup(m)
			m.SetClosed()
			if !m.IsClosed() {
				t.Error("SetClosed should work from any state")
			}
		})
	}
}

func TestThreadStateManager_Transitions(t *testing.T) {
	m := newThreadStateManager()

	// Initial state
	if m.Current() != ThreadStateCreating {
		t.Errorf("expected initial state creating, got %v", m.Current())
	}

	// SetStarting from creating
	if err := m.SetStarting(); err != nil {
		t.Errorf("SetStarting failed: %v", err)
	}
	if m.Current() != ThreadStateStarting {
		t.Errorf("expected starting, got %v", m.Current())
	}

	// SetReady from starting
	if err := m.SetReady(); err != nil {
		t.Errorf("SetReady failed: %v", err)
	}
	if m.Current() != ThreadStateReady {
		t.Errorf("expected ready, got %v", m.Current())
	}
	if !m.IsReady() {
		t.Error("IsReady should return true")
	}

	// SetProcessing from ready
	if err := m.SetProcessing(); err != nil {
		t.Errorf("SetProcessing failed: %v", err)
	}
	if m.Current() != ThreadStateProcessing {
		t.Errorf("expected processing, got %v", m.Current())
	}
	if !m.IsProcessing() {
		t.Error("IsProcessing should return true")
	}

	// SetReady from processing (turn completed)
	if err := m.SetReady(); err != nil {
		t.Errorf("SetReady from processing failed: %v", err)
	}
	if m.Current() != ThreadStateReady {
		t.Errorf("expected ready, got %v", m.Current())
	}

	// SetClosed
	m.SetClosed()
	if m.Current() != ThreadStateClosed {
		t.Errorf("expected closed, got %v", m.Current())
	}
	if !m.IsClosed() {
		t.Error("IsClosed should return true")
	}
}

func TestThreadStateManager_InvalidTransitions(t *testing.T) {
	m := newThreadStateManager()

	// SetProcessing from creating should fail
	if err := m.SetProcessing(); err == nil {
		t.Error("SetProcessing should fail from creating")
	}

	// Advance to ready
	m.SetStarting()
	m.SetReady()

	// SetStarting from ready should fail
	if err := m.SetStarting(); err == nil {
		t.Error("SetStarting should fail from ready")
	}
}

func TestClientStateManager_Concurrent(t *testing.T) {
	m := newClientStateManager()
	m.SetStarting()
	m.SetReady()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Current()
			_ = m.IsReady()
			_ = m.IsClosed()
		}()
	}
	wg.Wait()
}

func TestThreadStateManager_Concurrent(t *testing.T) {
	m := newThreadStateManager()
	m.SetReady()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Current()
			_ = m.IsReady()
			_ = m.IsProcessing()
			_ = m.IsClosed()
		}()
	}
	wg.Wait()
}

func TestThreadStateManager_WaitForReady_AlreadyReady(t *testing.T) {
	m := newThreadStateManager()
	m.SetReady()

	ctx := context.Background()
	if err := m.WaitForReady(ctx); err != nil {
		t.Errorf("WaitForReady failed when already ready: %v", err)
	}
}

func TestThreadStateManager_WaitForReady_BecomesReady(t *testing.T) {
	m := newThreadStateManager()

	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		done <- m.WaitForReady(ctx)
	}()

	// Give the goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Transition to ready
	m.SetReady()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WaitForReady failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("WaitForReady did not return after SetReady")
	}
}

func TestThreadStateManager_WaitForReady_Closed(t *testing.T) {
	m := newThreadStateManager()

	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		done <- m.WaitForReady(ctx)
	}()

	// Give the goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Close the thread
	m.SetClosed()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClientClosed) {
			t.Errorf("WaitForReady returned %v, want ErrClientClosed", err)
		}
	case <-time.After(time.Second):
		t.Error("WaitForReady did not return after SetClosed")
	}
}

func TestThreadStateManager_WaitForReady_ContextCancelled(t *testing.T) {
	m := newThreadStateManager()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- m.WaitForReady(ctx)
	}()

	// Give the goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Cancel context
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("WaitForReady returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Error("WaitForReady did not return after context cancellation")
	}
}

func TestThreadStateManager_WaitForReady_ContextAlreadyCancelled(t *testing.T) {
	m := newThreadStateManager()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before calling WaitForReady

	err := m.WaitForReady(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WaitForReady returned %v, want context.Canceled", err)
	}
}

func TestThreadStateManager_WaitForReady_Error(t *testing.T) {
	m := newThreadStateManager()
	testErr := errors.New("startup failed")

	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		done <- m.WaitForReady(ctx)
	}()

	// Give the goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Set error
	m.SetError(testErr)

	select {
	case err := <-done:
		if err != testErr {
			t.Errorf("WaitForReady returned %v, want %v", err, testErr)
		}
	case <-time.After(time.Second):
		t.Error("WaitForReady did not return after SetError")
	}
}

func TestThreadStateManager_Error(t *testing.T) {
	m := newThreadStateManager()

	// No error initially
	if err := m.Error(); err != nil {
		t.Errorf("Error() returned %v, want nil", err)
	}

	testErr := errors.New("test error")
	m.SetError(testErr)

	if err := m.Error(); err != testErr {
		t.Errorf("Error() returned %v, want %v", err, testErr)
	}
}

// TestThreadStateManager_WaitForReady_ConcurrentCancellation tests that
// multiple concurrent WaitForReady calls with context cancellation don't deadlock.
func TestThreadStateManager_WaitForReady_ConcurrentCancellation(t *testing.T) {
	m := newThreadStateManager()

	const numWaiters = 50
	var wg sync.WaitGroup
	errs := make(chan error, numWaiters)

	// Launch many concurrent waiters
	for i := 0; i < numWaiters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Cancel immediately or after a short delay
			if idx%2 == 0 {
				cancel()
			} else {
				go func() {
					time.Sleep(time.Millisecond)
					cancel()
				}()
			}

			err := m.WaitForReady(ctx)
			errs <- err
		}(i)
	}

	// Complete all waiters
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// Ensure all waiters complete (no deadlock)
	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForReady deadlocked with concurrent cancellations")
	}

	// Verify all returned context errors
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	}
}

// TestThreadStateManager_WaitForReady_RapidCancellation tests rapid context
// cancellation to ensure no race conditions or goroutine leaks.
func TestThreadStateManager_WaitForReady_RapidCancellation(t *testing.T) {
	const iterations = 100

	for i := 0; i < iterations; i++ {
		m := newThreadStateManager()
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() {
			done <- m.WaitForReady(ctx)
		}()

		// Cancel immediately without any sleep
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("iteration %d: expected context.Canceled, got %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: WaitForReady did not return after immediate cancellation", i)
		}
	}
}

// TestThreadStateManager_WaitForReady_CancellationRaceWindow tests the specific
// race condition where context is cancelled after the goroutine checks cancelCh
// but before it enters cond.Wait(). This test uses rapid iteration to increase
// the likelihood of hitting this narrow window.
func TestThreadStateManager_WaitForReady_CancellationRaceWindow(t *testing.T) {
	const iterations = 1000

	for i := 0; i < iterations; i++ {
		m := newThreadStateManager()
		ctx, cancel := context.WithCancel(context.Background())

		// Start WaitForReady
		done := make(chan error, 1)
		go func() {
			done <- m.WaitForReady(ctx)
		}()

		// Cancel immediately to try to hit the race window
		// where the goroutine has started but hasn't entered Wait() yet
		cancel()

		// WaitForReady should return context.Canceled, not hang
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("iteration %d: expected context.Canceled, got %v", i, err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("iteration %d: WaitForReady hung - likely hit the race window", i)
		}
	}
}

// TestThreadStateManager_WaitForReady_MixedCompletion tests concurrent waiters
// where some complete successfully and others are cancelled.
func TestThreadStateManager_WaitForReady_MixedCompletion(t *testing.T) {
	m := newThreadStateManager()

	const numWaiters = 20
	var wg sync.WaitGroup
	successCount := 0
	cancelCount := 0
	var mu sync.Mutex

	// Launch waiters with different behaviors
	for i := 0; i < numWaiters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			var ctx context.Context
			var cancel context.CancelFunc

			if idx%3 == 0 {
				// This waiter will be cancelled
				ctx, cancel = context.WithCancel(context.Background())
				go func() {
					time.Sleep(5 * time.Millisecond)
					cancel()
				}()
			} else {
				// This waiter will complete successfully
				ctx = context.Background()
				cancel = func() {}
			}
			defer cancel()

			err := m.WaitForReady(ctx)

			mu.Lock()
			if err == nil {
				successCount++
			} else if errors.Is(err, context.Canceled) {
				cancelCount++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
			mu.Unlock()
		}(i)
	}

	// Let waiters start
	time.Sleep(10 * time.Millisecond)

	// Set ready to wake successful waiters
	m.SetReady()

	// Wait for all waiters to complete
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("Success: %d, Cancelled: %d", successCount, cancelCount)
		// Verify we got both types of completion
		if successCount == 0 {
			t.Error("expected some successful completions")
		}
		if cancelCount == 0 {
			t.Error("expected some cancellations")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForReady deadlocked with mixed completion")
	}
}
