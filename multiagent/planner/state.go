// Package planner implements the Planner agent that coordinates sub-agents.
package planner

import (
	"fmt"
	"sync"
)

// PlannerState represents the current execution state of the Planner.
type PlannerState int

const (
	// StateIdle indicates the Planner is ready to accept missions.
	StateIdle PlannerState = iota
	// StatePlanning indicates the Planner is analyzing the mission.
	StatePlanning
	// StateDesigning indicates the Designer sub-agent is active.
	StateDesigning
	// StateBuilding indicates the Builder sub-agent is active.
	StateBuilding
	// StateReviewing indicates the Reviewer sub-agent is active.
	StateReviewing
	// StateWaitingForInput indicates awaiting user input/clarification.
	StateWaitingForInput
	// StateCompleted indicates the mission completed successfully.
	StateCompleted
	// StateFailed indicates the mission failed.
	StateFailed
)

// String returns the string representation of the state.
func (s PlannerState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StatePlanning:
		return "planning"
	case StateDesigning:
		return "designing"
	case StateBuilding:
		return "building"
	case StateReviewing:
		return "reviewing"
	case StateWaitingForInput:
		return "waiting_for_input"
	case StateCompleted:
		return "completed"
	case StateFailed:
		return "failed"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// IsTerminal returns true if this is a terminal state (completed or failed).
func (s PlannerState) IsTerminal() bool {
	return s == StateCompleted || s == StateFailed
}

// IsActive returns true if the Planner is actively working (not idle or terminal).
func (s PlannerState) IsActive() bool {
	return s != StateIdle && !s.IsTerminal()
}

// StateTransition represents a state change with its trigger.
type StateTransition struct {
	Trigger string
	From    PlannerState
	To      PlannerState
}

// validTransitions defines the legal state transitions.
// The map key is (from, to) encoded as from*100 + to.
var validTransitions = map[int]bool{
	// From Idle
	int(StateIdle)*100 + int(StatePlanning): true,

	// From Planning
	int(StatePlanning)*100 + int(StateDesigning):       true,
	int(StatePlanning)*100 + int(StateBuilding):        true,
	int(StatePlanning)*100 + int(StateWaitingForInput): true,
	int(StatePlanning)*100 + int(StateCompleted):       true,
	int(StatePlanning)*100 + int(StateFailed):          true,

	// From Designing
	int(StateDesigning)*100 + int(StatePlanning): true,
	int(StateDesigning)*100 + int(StateBuilding): true,
	int(StateDesigning)*100 + int(StateFailed):   true,

	// From Building
	int(StateBuilding)*100 + int(StatePlanning):  true,
	int(StateBuilding)*100 + int(StateReviewing): true,
	int(StateBuilding)*100 + int(StateFailed):    true,

	// From Reviewing
	int(StateReviewing)*100 + int(StatePlanning):  true,
	int(StateReviewing)*100 + int(StateBuilding):  true,
	int(StateReviewing)*100 + int(StateCompleted): true,
	int(StateReviewing)*100 + int(StateFailed):    true,

	// From WaitingForInput
	int(StateWaitingForInput)*100 + int(StatePlanning): true,
	int(StateWaitingForInput)*100 + int(StateFailed):   true,

	// From terminal states (reset)
	int(StateCompleted)*100 + int(StateIdle): true,
	int(StateFailed)*100 + int(StateIdle):    true,
}

// isValidTransition checks if a state transition is allowed.
func isValidTransition(from, to PlannerState) bool {
	return validTransitions[int(from)*100+int(to)]
}

// StateMachine manages the Planner's state transitions.
type StateMachine struct {
	history []StateTransition
	mu      sync.RWMutex
	state   PlannerState
}

// NewStateMachine creates a new state machine starting in Idle state.
func NewStateMachine() *StateMachine {
	return &StateMachine{
		state:   StateIdle,
		history: make([]StateTransition, 0, 16),
	}
}

// State returns the current state.
func (sm *StateMachine) State() PlannerState {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.state
}

// Transition attempts to transition to a new state.
// Returns an error if the transition is invalid.
func (sm *StateMachine) Transition(to PlannerState, trigger string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !isValidTransition(sm.state, to) {
		return fmt.Errorf("invalid state transition from %s to %s (trigger: %s)",
			sm.state, to, trigger)
	}

	transition := StateTransition{
		Trigger: trigger,
		From:    sm.state,
		To:      to,
	}

	sm.history = append(sm.history, transition)
	sm.state = to

	return nil
}

// ForceState sets the state without validation (for recovery scenarios).
func (sm *StateMachine) ForceState(state PlannerState, reason string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	transition := StateTransition{
		Trigger: "force:" + reason,
		From:    sm.state,
		To:      state,
	}

	sm.history = append(sm.history, transition)
	sm.state = state
}

// History returns a copy of the state transition history.
func (sm *StateMachine) History() []StateTransition {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make([]StateTransition, len(sm.history))
	copy(result, sm.history)
	return result
}

// Reset returns the state machine to Idle state.
func (sm *StateMachine) Reset() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.state != StateIdle {
		sm.history = append(sm.history, StateTransition{
			From:    sm.state,
			To:      StateIdle,
			Trigger: "reset",
		})
		sm.state = StateIdle
	}
}
