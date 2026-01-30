package codex

import (
	"context"
	"sync"
	"time"
)

// ClientState represents the state of the client connection.
type ClientState int

const (
	// ClientStateUninitialized is the initial state before Start() is called.
	ClientStateUninitialized ClientState = iota

	// ClientStateStarting means the app-server process is being spawned.
	ClientStateStarting

	// ClientStateReady means the client is initialized and ready for operations.
	ClientStateReady

	// ClientStateClosed means the client has been stopped.
	ClientStateClosed
)

// String returns the string representation of the client state.
func (s ClientState) String() string {
	switch s {
	case ClientStateUninitialized:
		return "uninitialized"
	case ClientStateStarting:
		return "starting"
	case ClientStateReady:
		return "ready"
	case ClientStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ThreadState represents the state of a conversation thread.
type ThreadState int

const (
	// ThreadStateCreating means the thread is being created.
	ThreadStateCreating ThreadState = iota

	// ThreadStateStarting means MCP startup is in progress.
	ThreadStateStarting

	// ThreadStateReady means the thread is ready to receive turns.
	ThreadStateReady

	// ThreadStateProcessing means a turn is currently in progress.
	ThreadStateProcessing

	// ThreadStateClosed means the thread has been closed.
	ThreadStateClosed
)

// String returns the string representation of the thread state.
func (s ThreadState) String() string {
	switch s {
	case ThreadStateCreating:
		return "creating"
	case ThreadStateStarting:
		return "starting"
	case ThreadStateReady:
		return "ready"
	case ThreadStateProcessing:
		return "processing"
	case ThreadStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// clientStateManager manages thread-safe client state transitions.
type clientStateManager struct {
	mu    sync.RWMutex
	state ClientState
}

func newClientStateManager() *clientStateManager {
	return &clientStateManager{
		state: ClientStateUninitialized,
	}
}

// Current returns the current state.
func (m *clientStateManager) Current() ClientState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// SetStarting transitions to the starting state.
func (m *clientStateManager) SetStarting() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != ClientStateUninitialized {
		return ErrInvalidState
	}
	m.state = ClientStateStarting
	return nil
}

// SetReady transitions to the ready state.
func (m *clientStateManager) SetReady() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != ClientStateStarting {
		return ErrInvalidState
	}
	m.state = ClientStateReady
	return nil
}

// SetClosed transitions to the closed state.
func (m *clientStateManager) SetClosed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = ClientStateClosed
}

// IsClosed returns true if the client is closed.
func (m *clientStateManager) IsClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == ClientStateClosed
}

// IsReady returns true if the client is ready.
func (m *clientStateManager) IsReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == ClientStateReady
}

// threadStateManager manages thread-safe thread state transitions.
type threadStateManager struct {
	mu       sync.RWMutex
	cond     *sync.Cond
	state    ThreadState
	startErr error // Error from startup (e.g., MCP initialization failure)
}

func newThreadStateManager() *threadStateManager {
	m := &threadStateManager{
		state: ThreadStateCreating,
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// Current returns the current state.
func (m *threadStateManager) Current() ThreadState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// SetStarting transitions to the starting state.
func (m *threadStateManager) SetStarting() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != ThreadStateCreating {
		return ErrInvalidState
	}
	m.state = ThreadStateStarting
	return nil
}

// SetReady transitions to the ready state.
func (m *threadStateManager) SetReady() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != ThreadStateCreating && m.state != ThreadStateStarting && m.state != ThreadStateProcessing {
		return ErrInvalidState
	}
	m.state = ThreadStateReady
	m.cond.Broadcast()
	return nil
}

// SetProcessing transitions to the processing state.
func (m *threadStateManager) SetProcessing() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != ThreadStateReady {
		return ErrInvalidState
	}
	m.state = ThreadStateProcessing
	return nil
}

// SetClosed transitions to the closed state.
func (m *threadStateManager) SetClosed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = ThreadStateClosed
	m.cond.Broadcast()
}

// SetError sets a startup error and wakes any waiters.
// This is used when thread initialization fails (e.g., MCP startup error).
func (m *threadStateManager) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startErr = err
	m.cond.Broadcast()
}

// Error returns the startup error, if any.
func (m *threadStateManager) Error() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.startErr
}

// WaitForReady blocks until the state becomes Ready, Closed, or an error occurs.
// Returns nil if Ready, the startup error if one occurred, or ErrClientClosed if Closed.
func (m *threadStateManager) WaitForReady(ctx context.Context) error {
	// Create channels to communicate with the wait goroutine
	resultCh := make(chan error, 1)
	stopCh := make(chan struct{})

	// Spawn goroutine to wait on the condition
	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		for m.state != ThreadStateReady && m.state != ThreadStateClosed && m.startErr == nil {
			// Non-blocking check for cancellation before waiting
			select {
			case <-stopCh:
				resultCh <- ctx.Err()
				return
			default:
			}

			// Wait for state change
			m.cond.Wait()

			// Check for cancellation after waking up
			select {
			case <-stopCh:
				resultCh <- ctx.Err()
				return
			default:
			}
		}

		var err error
		if m.startErr != nil {
			err = m.startErr
		} else if m.state == ThreadStateClosed {
			err = ErrClientClosed
		}
		resultCh <- err
	}()

	// Goroutine to handle context cancellation
	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			close(stopCh)
			// Continuously broadcast to ensure we wake the waiter
			// even if it hasn't entered Wait() yet
			ticker := time.NewTicker(time.Microsecond)
			defer ticker.Stop()
			for {
				select {
				case <-cancelDone:
					return
				case <-ticker.C:
					m.cond.Broadcast()
				}
			}
		case <-cancelDone:
		}
	}()

	// Wait for result
	err := <-resultCh
	close(cancelDone)
	return err
}

// IsClosed returns true if the thread is closed.
func (m *threadStateManager) IsClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == ThreadStateClosed
}

// IsReady returns true if the thread is ready for turns.
func (m *threadStateManager) IsReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == ThreadStateReady
}

// IsProcessing returns true if a turn is in progress.
func (m *threadStateManager) IsProcessing() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == ThreadStateProcessing
}
