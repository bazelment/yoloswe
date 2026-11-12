// Package clock abstracts time operations for deterministic orchestration tests.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock abstracts wall-clock and timer operations.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer abstracts time.Timer for testability.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker abstracts time.Ticker for testability.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// RealClock delegates to the standard library time package.
type RealClock struct{}

// NewRealClock returns a Clock backed by the standard time package.
func NewRealClock() Clock {
	return RealClock{}
}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// NewTimer creates a real timer.
func (RealClock) NewTimer(d time.Duration) Timer { return &realTimer{time.NewTimer(d)} }

// NewTicker creates a real ticker.
func (RealClock) NewTicker(d time.Duration) Ticker { return &realTicker{time.NewTicker(d)} }

// AfterFunc schedules f to run after d using a real timer.
func (RealClock) AfterFunc(d time.Duration, f func()) Timer {
	return &realTimer{time.AfterFunc(d, f)}
}

type realTimer struct {
	*time.Timer
}

func (t *realTimer) C() <-chan time.Time { return t.Timer.C }

type realTicker struct {
	*time.Ticker
}

func (t *realTicker) C() <-chan time.Time { return t.Ticker.C }

// FakeClock is a deterministic, manually-advanced Clock for tests.
type FakeClock struct {
	now     time.Time
	timers  []*fakeTimer
	tickers []*fakeTicker
	nextID  uint64
	mu      sync.Mutex
}

// NewFakeClock creates a FakeClock initialized to initial (in UTC).
func NewFakeClock(initial time.Time) *FakeClock {
	if initial.IsZero() {
		initial = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return &FakeClock{
		now: initial.UTC(),
	}
}

// Now returns the fake clock's current time.
func (fc *FakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

// Waiters returns the count of active timers and tickers registered on the clock.
func (fc *FakeClock) Waiters() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	count := 0
	for _, t := range fc.timers {
		if t.active {
			count++
		}
	}
	for _, tk := range fc.tickers {
		if tk.active {
			count++
		}
	}
	return count
}

// Set sets the fake clock's current time to target and fires any due timers or tickers.
func (fc *FakeClock) Set(target time.Time) {
	fc.mu.Lock()
	target = target.UTC()
	if !target.After(fc.now) {
		fc.now = target
		fc.mu.Unlock()
		return
	}
	fc.advanceLocked(target)
}

// Advance moves the fake clock forward by d and fires any timers or tickers
// whose deadlines fall on or before the new time in chronological order.
func (fc *FakeClock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	fc.mu.Lock()
	fc.advanceLocked(fc.now.Add(d))
}

func (fc *FakeClock) advanceLocked(target time.Time) {
	for {
		nextTime, hasNext := fc.earliestEventLocked(target)
		if !hasNext {
			fc.now = target
			fc.mu.Unlock()
			return
		}
		fc.now = nextTime
		callbacks := fc.fireDueLocked(nextTime)
		fc.mu.Unlock()
		for _, fn := range callbacks {
			fn()
		}
		fc.mu.Lock()
	}
}

func (fc *FakeClock) earliestEventLocked(limit time.Time) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, t := range fc.timers {
		if !t.active || t.when.After(limit) {
			continue
		}
		if !found || t.when.Before(earliest) {
			earliest = t.when
			found = true
		}
	}
	for _, tk := range fc.tickers {
		if !tk.active || tk.next.After(limit) {
			continue
		}
		if !found || tk.next.Before(earliest) {
			earliest = tk.next
			found = true
		}
	}
	return earliest, found
}

func (fc *FakeClock) fireDueLocked(at time.Time) []func() {
	type dueTimer struct {
		timer *fakeTimer
		id    uint64
	}
	var due []dueTimer
	for _, t := range fc.timers {
		if t.active && !t.when.After(at) {
			due = append(due, dueTimer{timer: t, id: t.id})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		return due[i].id < due[j].id
	})

	var callbacks []func()
	for _, d := range due {
		t := d.timer
		if !t.active {
			continue
		}
		t.active = false
		if t.afterFn != nil {
			callbacks = append(callbacks, t.afterFn)
			continue
		}
		select {
		case t.ch <- at:
		default:
		}
	}

	for _, tk := range fc.tickers {
		if !tk.active || tk.next.After(at) {
			continue
		}
		select {
		case tk.ch <- at:
		default:
		}
		tk.next = at.Add(tk.period)
	}
	return callbacks
}

// NewTimer creates a timer on the fake clock.
func (fc *FakeClock) NewTimer(d time.Duration) Timer {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.nextID++
	t := &fakeTimer{
		clock:  fc,
		ch:     make(chan time.Time, 1),
		when:   fc.now.Add(d),
		id:     fc.nextID,
		active: true,
	}
	fc.timers = append(fc.timers, t)
	if d <= 0 {
		t.active = false
		t.ch <- fc.now
	}
	return t
}

// AfterFunc schedules f to run after d on the fake clock.
func (fc *FakeClock) AfterFunc(d time.Duration, f func()) Timer {
	fc.mu.Lock()
	if d <= 0 {
		fc.nextID++
		t := &fakeTimer{
			clock:   fc,
			ch:      make(chan time.Time, 1),
			afterFn: f,
			when:    fc.now,
			id:      fc.nextID,
			active:  false,
		}
		fc.timers = append(fc.timers, t)
		fc.mu.Unlock()
		f()
		return t
	}
	fc.nextID++
	t := &fakeTimer{
		clock:   fc,
		ch:      make(chan time.Time, 1),
		afterFn: f,
		when:    fc.now.Add(d),
		id:      fc.nextID,
		active:  true,
	}
	fc.timers = append(fc.timers, t)
	fc.mu.Unlock()
	return t
}

// NewTicker creates a ticker on the fake clock. Period must be > 0.
func (fc *FakeClock) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("orch/clock: non-positive interval for NewTicker")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	tk := &fakeTicker{
		clock:  fc,
		ch:     make(chan time.Time, 1),
		next:   fc.now.Add(d),
		period: d,
		active: true,
	}
	fc.tickers = append(fc.tickers, tk)
	return tk
}

type fakeTimer struct {
	when    time.Time
	clock   *FakeClock
	ch      chan time.Time
	afterFn func()
	id      uint64
	active  bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	wasActive := t.active
	t.clock.nextID++
	t.id = t.clock.nextID
	t.when = t.clock.now.Add(d)
	if d <= 0 {
		t.active = false
		now := t.clock.now
		fn := t.afterFn
		t.clock.mu.Unlock()
		if fn != nil {
			fn()
		} else {
			select {
			case t.ch <- now:
			default:
			}
		}
		return wasActive
	}
	t.active = true
	t.clock.mu.Unlock()
	return wasActive
}

type fakeTicker struct {
	next   time.Time
	clock  *FakeClock
	ch     chan time.Time
	period time.Duration
	active bool
}

func (tk *fakeTicker) C() <-chan time.Time { return tk.ch }

func (tk *fakeTicker) Stop() {
	tk.clock.mu.Lock()
	defer tk.clock.mu.Unlock()
	tk.active = false
}

func (tk *fakeTicker) Reset(d time.Duration) {
	if d <= 0 {
		panic("orch/clock: non-positive interval for Ticker.Reset")
	}
	tk.clock.mu.Lock()
	defer tk.clock.mu.Unlock()
	tk.period = d
	tk.next = tk.clock.now.Add(d)
	tk.active = true
}
