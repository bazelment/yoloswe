package control

import (
	"testing"
	"time"
)

func TestStateString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		want  string
		state State
	}{
		{name: "running", state: StateRunning, want: "running"},
		{name: "paused", state: StatePaused, want: "paused"},
		{name: "cancelled", state: StateCancelled, want: "cancelled"},
		{name: "completed", state: StateCompleted, want: "completed"},
		{name: "failed", state: StateFailed, want: "failed"},
		{name: "unknown", state: State(99), want: "unknown"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.state.String(); got != tt.want {
				t.Fatalf("%v.String() = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

func TestControllerPauseResumeFlow(t *testing.T) {
	t.Parallel()

	c := NewController()
	if got := c.State(); got != StateRunning {
		t.Fatalf("initial State() = %v, want %v", got, StateRunning)
	}
	if !c.IsActive() {
		t.Fatal("new controller should be active")
	}

	c.Pause()
	if got := c.State(); got != StateRunning {
		t.Fatalf("State() after Pause() = %v, want %v", got, StateRunning)
	}
	if !c.ShouldPause() {
		t.Fatal("ShouldPause() should report a pending pause")
	}

	c.MarkPaused()
	if got := c.State(); got != StatePaused {
		t.Fatalf("State() after MarkPaused() = %v, want %v", got, StatePaused)
	}
	if c.ShouldPause() {
		t.Fatal("ShouldPause() should clear once the controller is paused")
	}

	c.Resume()
	if got := c.State(); got != StateRunning {
		t.Fatalf("State() after Resume() = %v, want %v", got, StateRunning)
	}
}

func TestControllerCancelIsTerminal(t *testing.T) {
	t.Parallel()

	c := NewController()
	cancelled := make(chan struct{})
	c.SetCancelFunc(func() { close(cancelled) })

	c.Pause()
	if !c.Cancel() {
		t.Fatal("Cancel() = false, want true for a running controller")
	}

	select {
	case <-cancelled:
	default:
		t.Fatal("Cancel() did not call the registered cancel function")
	}

	if got := c.State(); got != StateCancelled {
		t.Fatalf("State() after Cancel() = %v, want %v", got, StateCancelled)
	}
	if c.ShouldPause() {
		t.Fatal("Cancel() should clear any pending pause")
	}
	if c.IsActive() {
		t.Fatal("cancelled controller should not be active")
	}
	if !c.IsCancelled() {
		t.Fatal("IsCancelled() = false, want true")
	}

	c.MarkPaused()
	c.MarkCompleted()
	c.MarkFailed()
	c.Resume()
	if got := c.State(); got != StateCancelled {
		t.Fatalf("terminal state changed to %v, want %v", got, StateCancelled)
	}
	if c.Cancel() {
		t.Fatal("second Cancel() = true, want false")
	}
}

func TestControllerCompletedAndFailedAreTerminal(t *testing.T) {
	t.Parallel()

	completed := NewController()
	completed.MarkCompleted()
	completed.MarkFailed()
	if completed.Cancel() {
		t.Fatal("Cancel() after MarkCompleted() = true, want false")
	}
	if got := completed.State(); got != StateCompleted {
		t.Fatalf("completed terminal state changed to %v", got)
	}

	failed := NewController()
	failed.MarkFailed()
	failed.MarkCompleted()
	if failed.Cancel() {
		t.Fatal("Cancel() after MarkFailed() = true, want false")
	}
	if got := failed.State(); got != StateFailed {
		t.Fatalf("failed terminal state changed to %v", got)
	}
}

func TestControllerProgressTimers(t *testing.T) {
	t.Parallel()

	c := NewController()
	if got := c.ElapsedTime(); got < 0 {
		t.Fatalf("ElapsedTime() = %s, want non-negative duration", got)
	}
	if got := c.TimeSinceProgress(); got < 0 {
		t.Fatalf("TimeSinceProgress() = %s, want non-negative duration", got)
	}

	c.UpdateProgress()
	if got := c.TimeSinceProgress(); got < 0 || got > time.Second {
		t.Fatalf("TimeSinceProgress() after UpdateProgress() = %s, want recent non-negative duration", got)
	}
}
