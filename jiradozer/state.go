package jiradozer

import (
	"fmt"
	"sync"
	"time"
)

// WorkflowStep represents a step in the issue-driven build workflow.
type WorkflowStep int

const (
	StepInit           WorkflowStep = iota
	StepPlanning                    // Agent running in plan mode
	StepPlanReview                  // Plan posted, waiting for human feedback
	StepBuilding                    // Agent running in bypass/execution mode
	StepBuildReview                 // Build done, waiting for human feedback
	StepValidating                  // Running validation commands
	StepValidateReview              // Validation results posted, waiting for feedback
	StepShipping                    // Creating PR
	StepShipReview                  // PR created, waiting for CI + human feedback
	StepDone                        // Terminal: issue marked done
	StepFailed                      // Terminal: workflow failed
)

// String returns the string representation of the step.
func (s WorkflowStep) String() string {
	switch s {
	case StepInit:
		return "init"
	case StepPlanning:
		return "planning"
	case StepPlanReview:
		return "plan_review"
	case StepBuilding:
		return "building"
	case StepBuildReview:
		return "build_review"
	case StepValidating:
		return "validating"
	case StepValidateReview:
		return "validate_review"
	case StepShipping:
		return "shipping"
	case StepShipReview:
		return "ship_review"
	case StepDone:
		return "done"
	case StepFailed:
		return "failed"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// IsTerminal returns true if this is a terminal step.
func (s WorkflowStep) IsTerminal() bool {
	return s == StepDone || s == StepFailed
}

// IsReview returns true if this step is waiting for human feedback.
func (s WorkflowStep) IsReview() bool {
	switch s {
	case StepPlanReview, StepBuildReview, StepValidateReview, StepShipReview:
		return true
	}
	return false
}

// StepTransition records a state change with its trigger and timestamp.
type StepTransition struct {
	Timestamp time.Time
	Trigger   string
	From      WorkflowStep
	To        WorkflowStep
}

// validTransitions defines allowed step transitions.
// Key is from*100 + to.
var validTransitions = map[int]bool{
	// Init
	key(StepInit, StepPlanning): true,

	// Planning
	key(StepPlanning, StepPlanReview): true,
	key(StepPlanning, StepFailed):     true,

	// PlanReview
	key(StepPlanReview, StepPlanning): true, // redo with feedback
	key(StepPlanReview, StepBuilding): true, // approved
	key(StepPlanReview, StepFailed):   true,

	// Building
	key(StepBuilding, StepBuildReview): true,
	key(StepBuilding, StepFailed):      true,

	// BuildReview
	key(StepBuildReview, StepBuilding):   true, // redo with feedback
	key(StepBuildReview, StepPlanning):   true, // back to plan
	key(StepBuildReview, StepValidating): true, // approved
	key(StepBuildReview, StepFailed):     true,

	// Validating
	key(StepValidating, StepValidateReview): true,
	key(StepValidating, StepFailed):         true,

	// ValidateReview
	key(StepValidateReview, StepValidating): true, // redo
	key(StepValidateReview, StepBuilding):   true, // fix issues
	key(StepValidateReview, StepShipping):   true, // approved
	key(StepValidateReview, StepFailed):     true,

	// Shipping
	key(StepShipping, StepShipReview): true,
	key(StepShipping, StepFailed):     true,

	// ShipReview
	key(StepShipReview, StepShipping): true, // redo ship (e.g. PR-only issue)
	key(StepShipReview, StepBuilding): true, // fix CI issues
	key(StepShipReview, StepDone):     true, // approved + CI green
	key(StepShipReview, StepFailed):   true,
}

func key(from, to WorkflowStep) int {
	return int(from)*100 + int(to)
}

// StateMachine manages workflow step transitions with validation and history.
type StateMachine struct {
	history []StepTransition
	mu      sync.RWMutex
	step    WorkflowStep
}

// NewStateMachine creates a new state machine starting at StepInit.
func NewStateMachine() *StateMachine {
	return &StateMachine{
		step:    StepInit,
		history: make([]StepTransition, 0, 16),
	}
}

// Current returns the current workflow step.
func (sm *StateMachine) Current() WorkflowStep {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.step
}

// Transition attempts to move to a new step. Returns an error if the transition is invalid.
func (sm *StateMachine) Transition(to WorkflowStep, trigger string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !validTransitions[key(sm.step, to)] {
		return fmt.Errorf("invalid transition from %s to %s (trigger: %s)", sm.step, to, trigger)
	}

	sm.history = append(sm.history, StepTransition{
		From:      sm.step,
		To:        to,
		Trigger:   trigger,
		Timestamp: time.Now(),
	})
	sm.step = to
	return nil
}

// History returns a copy of the transition history.
func (sm *StateMachine) History() []StepTransition {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]StepTransition, len(sm.history))
	copy(result, sm.history)
	return result
}
