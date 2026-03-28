// Package orchestrator implements the single-authority event loop for Symphony.
package orchestrator

import "time"

// Clock abstracts time operations for testability.
// Tests inject a fake clock to advance time explicitly.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer abstracts time.Timer for testability.
type Timer interface {
	Stop() bool
	C() <-chan time.Time
}

// Ticker abstracts time.Ticker for testability.
type Ticker interface {
	Stop()
	C() <-chan time.Time
}

// RealClock uses the standard time package.
type RealClock struct{}

func (RealClock) Now() time.Time                            { return time.Now() }
func (RealClock) NewTimer(d time.Duration) Timer            { return &realTimer{time.NewTimer(d)} }
func (RealClock) NewTicker(d time.Duration) Ticker          { return &realTicker{time.NewTicker(d)} }
func (RealClock) AfterFunc(d time.Duration, f func()) Timer { return &realTimer{time.AfterFunc(d, f)} }

type realTimer struct{ *time.Timer }

func (t *realTimer) C() <-chan time.Time { return t.Timer.C }

type realTicker struct{ *time.Ticker }

func (t *realTicker) C() <-chan time.Time { return t.Ticker.C }
