package clock_test

import (
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
)

func TestFakeClockTimersAndAfterFunc(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	fc := clock.NewFakeClock(start)

	if !fc.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", fc.Now(), start)
	}

	var order []string
	fc.AfterFunc(20*time.Second, func() {
		order = append(order, "20s")
	})
	fc.AfterFunc(10*time.Second, func() {
		order = append(order, "10s")
	})
	stopped := fc.AfterFunc(15*time.Second, func() {
		order = append(order, "15s-stopped")
	})
	if !stopped.Stop() {
		t.Fatal("expected Stop() on active timer to return true")
	}
	if stopped.Stop() {
		t.Fatal("expected second Stop() to return false")
	}

	timer := fc.NewTimer(15 * time.Second)
	if fc.Waiters() != 3 {
		t.Fatalf("Waiters() = %d, want 3", fc.Waiters())
	}

	fc.Advance(25 * time.Second)

	select {
	case firedAt := <-timer.C():
		want := start.Add(15 * time.Second)
		if !firedAt.Equal(want) {
			t.Fatalf("timer fired at %v, want %v", firedAt, want)
		}
	default:
		t.Fatal("expected 15s timer to have fired")
	}

	if len(order) != 2 || order[0] != "10s" || order[1] != "20s" {
		t.Fatalf("AfterFunc order = %v, want [10s 20s]", order)
	}
	if fc.Waiters() != 0 {
		t.Fatalf("Waiters() after firing = %d, want 0", fc.Waiters())
	}

	timer.Reset(5 * time.Second)
	fc.Advance(5 * time.Second)
	select {
	case <-timer.C():
	default:
		t.Fatal("expected reset timer to fire")
	}
}

func TestFakeClockTicker(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC)
	fc := clock.NewFakeClock(start)

	tk := fc.NewTicker(10 * time.Second)
	defer tk.Stop()

	fc.Advance(10 * time.Second)
	select {
	case got := <-tk.C():
		if !got.Equal(start.Add(10 * time.Second)) {
			t.Fatalf("tick 1 = %v, want %v", got, start.Add(10*time.Second))
		}
	default:
		t.Fatal("expected tick 1")
	}

	tk.Reset(5 * time.Second)
	fc.Set(start.Add(15 * time.Second))
	select {
	case got := <-tk.C():
		if !got.Equal(start.Add(15 * time.Second)) {
			t.Fatalf("tick 2 = %v, want %v", got, start.Add(15*time.Second))
		}
	default:
		t.Fatal("expected tick 2 after Reset + Set")
	}
}
