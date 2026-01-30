package claude

import (
	"fmt"
	"sync"
)

// SessionState represents the current state of a session.
type SessionState int

const (
	// StateUninitialized is the initial state before Start() is called.
	StateUninitialized SessionState = iota
	// StateStarting is the state while the CLI process is starting.
	StateStarting
	// StateReady is the state when the session is ready to receive messages.
	StateReady
	// StateProcessing is the state while processing a user message.
	StateProcessing
	// StateClosed is the final state after Stop() is called.
	StateClosed
)

// String returns a string representation of the state.
func (s SessionState) String() string {
	switch s {
	case StateUninitialized:
		return "uninitialized"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateProcessing:
		return "processing"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// StateTransition represents a state transition event.
type StateTransition int

const (
	// TransitionStarted transitions from Uninitialized to Starting.
	TransitionStarted StateTransition = iota
	// TransitionInitReceived transitions from Starting to Ready.
	TransitionInitReceived
	// TransitionUserMessageSent transitions from Ready to Processing.
	TransitionUserMessageSent
	// TransitionResultReceived transitions from Processing to Ready.
	TransitionResultReceived
	// TransitionClosed transitions any state to Closed.
	TransitionClosed
)

// validTransitions defines the valid state transitions.
var validTransitions = map[SessionState]map[StateTransition]SessionState{
	StateUninitialized: {
		TransitionStarted: StateStarting,
		TransitionClosed:  StateClosed,
	},
	StateStarting: {
		TransitionInitReceived: StateReady,
		TransitionClosed:       StateClosed,
	},
	StateReady: {
		TransitionUserMessageSent: StateProcessing,
		TransitionClosed:          StateClosed,
	},
	StateProcessing: {
		TransitionResultReceived: StateReady,
		TransitionClosed:         StateClosed,
	},
	StateClosed: {},
}

// sessionState manages the session state machine.
type sessionState struct {
	mu      sync.RWMutex
	current SessionState
}

// newSessionState creates a new session state machine.
func newSessionState() *sessionState {
	return &sessionState{
		current: StateUninitialized,
	}
}

// Current returns the current state.
func (s *sessionState) Current() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Transition attempts to transition to a new state.
// Returns an error if the transition is invalid.
func (s *sessionState) Transition(t StateTransition) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	transitions, ok := validTransitions[s.current]
	if !ok {
		return fmt.Errorf("%w: no transitions from state %s", ErrInvalidState, s.current)
	}

	newState, ok := transitions[t]
	if !ok {
		return fmt.Errorf("%w: invalid transition %d from state %s", ErrInvalidState, t, s.current)
	}

	s.current = newState
	return nil
}

// TransitionTo transitions to a specific state if possible.
// This is a convenience method that finds the appropriate transition.
func (s *sessionState) TransitionTo(target SessionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	transitions, ok := validTransitions[s.current]
	if !ok {
		return fmt.Errorf("%w: no transitions from state %s", ErrInvalidState, s.current)
	}

	// Find a transition that leads to the target state
	for t, state := range transitions {
		if state == target {
			s.current = target
			_ = t // We found the transition, mark it as used
			return nil
		}
	}

	return fmt.Errorf("%w: cannot transition from %s to %s", ErrInvalidState, s.current, target)
}

// IsClosed returns true if the session is closed.
func (s *sessionState) IsClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current == StateClosed
}

// IsReady returns true if the session is ready for messages.
func (s *sessionState) IsReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current == StateReady
}

// IsProcessing returns true if the session is processing.
func (s *sessionState) IsProcessing() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current == StateProcessing
}
