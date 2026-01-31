// Package control provides centralized control for long-running missions.
package control

import (
	"context"
	"sync"
	"time"
)

// State represents the current state of a controlled mission.
type State int

const (
	// StateRunning indicates the mission is actively executing.
	StateRunning State = iota
	// StatePaused indicates the mission has been paused by user request.
	StatePaused
	// StateCancelled indicates the mission has been cancelled.
	StateCancelled
	// StateCompleted indicates the mission finished successfully.
	StateCompleted
	// StateFailed indicates the mission failed with an error.
	StateFailed
)

// String returns the string representation of the state.
func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateCancelled:
		return "cancelled"
	case StateCompleted:
		return "completed"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Controller provides control operations for a long-running mission.
type Controller struct {
	startTime      time.Time
	lastProgress   time.Time
	cancelFunc     context.CancelFunc
	state          State
	mu             sync.RWMutex
	pauseRequested bool
}

// NewController creates a new mission controller.
func NewController() *Controller {
	now := time.Now()
	return &Controller{
		state:        StateRunning,
		startTime:    now,
		lastProgress: now,
	}
}

// SetCancelFunc registers the cancellation function for the mission context.
func (c *Controller) SetCancelFunc(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelFunc = cancel
}

// Cancel requests cancellation of the mission.
// Returns true if cancellation was initiated, false if already cancelled/completed.
func (c *Controller) Cancel() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == StateCancelled || c.state == StateCompleted || c.state == StateFailed {
		return false
	}

	c.state = StateCancelled
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	return true
}

// Pause requests the mission to pause at the next safe point.
// The actual pause happens when the mission checks ShouldPause().
func (c *Controller) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateRunning {
		c.pauseRequested = true
	}
}

// Resume continues a paused mission.
func (c *Controller) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StatePaused {
		c.state = StateRunning
		c.pauseRequested = false
	}
}

// ShouldPause returns true if a pause has been requested.
// The mission should check this periodically and call MarkPaused() when paused.
func (c *Controller) ShouldPause() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pauseRequested && c.state == StateRunning
}

// MarkPaused transitions to the paused state.
func (c *Controller) MarkPaused() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pauseRequested {
		c.state = StatePaused
		c.pauseRequested = false
	}
}

// MarkCompleted marks the mission as successfully completed.
func (c *Controller) MarkCompleted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateCompleted
}

// MarkFailed marks the mission as failed.
func (c *Controller) MarkFailed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateFailed
}

// State returns the current state.
func (c *Controller) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// UpdateProgress records that progress was made.
// This resets the stall timer.
func (c *Controller) UpdateProgress() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastProgress = time.Now()
}

// TimeSinceProgress returns how long since the last progress update.
func (c *Controller) TimeSinceProgress() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.lastProgress)
}

// ElapsedTime returns the total time since the mission started.
func (c *Controller) ElapsedTime() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.startTime)
}

// IsCancelled returns true if the mission has been cancelled.
func (c *Controller) IsCancelled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == StateCancelled
}

// IsActive returns true if the mission is running or paused (not terminal).
func (c *Controller) IsActive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == StateRunning || c.state == StatePaused
}
