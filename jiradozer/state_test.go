package jiradozer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowStepString(t *testing.T) {
	tests := []struct {
		want string
		step WorkflowStep
	}{
		{want: "init", step: StepInit},
		{want: "planning", step: StepPlanning},
		{want: "plan_review", step: StepPlanReview},
		{want: "building", step: StepBuilding},
		{want: "creating_pr", step: StepCreatingPR},
		{want: "build_review", step: StepBuildReview},
		{want: "validating", step: StepValidating},
		{want: "validate_review", step: StepValidateReview},
		{want: "shipping", step: StepShipping},
		{want: "ship_review", step: StepShipReview},
		{want: "done", step: StepDone},
		{want: "failed", step: StepFailed},
		{want: "cancelled", step: StepCancelled},
		{want: "unknown(99)", step: WorkflowStep(99)},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.step.String())
	}
}

func TestWorkflowStepIsTerminal(t *testing.T) {
	assert.True(t, StepDone.IsTerminal())
	assert.True(t, StepFailed.IsTerminal())
	assert.True(t, StepCancelled.IsTerminal())
	assert.False(t, StepPlanning.IsTerminal())
	assert.False(t, StepInit.IsTerminal())
}

func TestWorkflowStepIsReview(t *testing.T) {
	assert.True(t, StepPlanReview.IsReview())
	assert.True(t, StepBuildReview.IsReview())
	assert.True(t, StepValidateReview.IsReview())
	assert.True(t, StepShipReview.IsReview())
	assert.False(t, StepPlanning.IsReview())
	assert.False(t, StepCreatingPR.IsReview())
	assert.False(t, StepDone.IsReview())
}

func TestStateMachineHappyPath(t *testing.T) {
	sm := NewStateMachine()
	assert.Equal(t, StepInit, sm.Current())

	steps := []struct {
		trigger string
		to      WorkflowStep
	}{
		{trigger: "start", to: StepPlanning},
		{trigger: "plan_complete", to: StepPlanReview},
		{trigger: "approved", to: StepBuilding},
		{trigger: "build_complete", to: StepCreatingPR},
		{trigger: "pr_created", to: StepBuildReview},
		{trigger: "approved", to: StepValidating},
		{trigger: "validation_complete", to: StepValidateReview},
		{trigger: "approved", to: StepShipping},
		{trigger: "pr_created", to: StepShipReview},
		{trigger: "approved", to: StepDone},
	}

	for _, s := range steps {
		require.NoError(t, sm.Transition(s.to, s.trigger), "transition to %s", s.to)
	}

	assert.Equal(t, StepDone, sm.Current())
	assert.True(t, sm.Current().IsTerminal())
	assert.Len(t, sm.History(), len(steps))
}

func TestStateMachineFeedbackLoops(t *testing.T) {
	t.Run("plan redo", func(t *testing.T) {
		sm := NewStateMachine()
		require.NoError(t, sm.Transition(StepPlanning, "start"))
		require.NoError(t, sm.Transition(StepPlanReview, "plan_done"))
		require.NoError(t, sm.Transition(StepPlanning, "redo_with_feedback"))
		require.NoError(t, sm.Transition(StepPlanReview, "plan_done"))
		require.NoError(t, sm.Transition(StepBuilding, "approved"))
		assert.Equal(t, StepBuilding, sm.Current())
	})

	t.Run("build redo", func(t *testing.T) {
		sm := NewStateMachine()
		require.NoError(t, sm.Transition(StepPlanning, "start"))
		require.NoError(t, sm.Transition(StepPlanReview, "plan_done"))
		require.NoError(t, sm.Transition(StepBuilding, "approved"))
		require.NoError(t, sm.Transition(StepCreatingPR, "build_done"))
		require.NoError(t, sm.Transition(StepBuildReview, "pr_created"))
		require.NoError(t, sm.Transition(StepBuilding, "redo_with_feedback"))
		assert.Equal(t, StepBuilding, sm.Current())
	})

	t.Run("build review back to plan", func(t *testing.T) {
		sm := NewStateMachine()
		require.NoError(t, sm.Transition(StepPlanning, "start"))
		require.NoError(t, sm.Transition(StepPlanReview, "plan_done"))
		require.NoError(t, sm.Transition(StepBuilding, "approved"))
		require.NoError(t, sm.Transition(StepCreatingPR, "build_done"))
		require.NoError(t, sm.Transition(StepBuildReview, "pr_created"))
		require.NoError(t, sm.Transition(StepPlanning, "back_to_plan"))
		assert.Equal(t, StepPlanning, sm.Current())
	})

	t.Run("validate redo triggers build", func(t *testing.T) {
		sm := NewStateMachine()
		require.NoError(t, sm.Transition(StepPlanning, "start"))
		require.NoError(t, sm.Transition(StepPlanReview, "done"))
		require.NoError(t, sm.Transition(StepBuilding, "approved"))
		require.NoError(t, sm.Transition(StepCreatingPR, "done"))
		require.NoError(t, sm.Transition(StepBuildReview, "done"))
		require.NoError(t, sm.Transition(StepValidating, "approved"))
		require.NoError(t, sm.Transition(StepValidateReview, "done"))
		require.NoError(t, sm.Transition(StepBuilding, "fix_failures"))
		assert.Equal(t, StepBuilding, sm.Current())
	})

	t.Run("ship review fix CI", func(t *testing.T) {
		sm := NewStateMachine()
		require.NoError(t, sm.Transition(StepPlanning, "start"))
		require.NoError(t, sm.Transition(StepPlanReview, "done"))
		require.NoError(t, sm.Transition(StepBuilding, "approved"))
		require.NoError(t, sm.Transition(StepCreatingPR, "done"))
		require.NoError(t, sm.Transition(StepBuildReview, "done"))
		require.NoError(t, sm.Transition(StepValidating, "approved"))
		require.NoError(t, sm.Transition(StepValidateReview, "done"))
		require.NoError(t, sm.Transition(StepShipping, "approved"))
		require.NoError(t, sm.Transition(StepShipReview, "pr_created"))
		require.NoError(t, sm.Transition(StepBuilding, "fix_ci"))
		assert.Equal(t, StepBuilding, sm.Current())
	})
}

func TestStateMachineInvalidTransition(t *testing.T) {
	sm := NewStateMachine()
	err := sm.Transition(StepDone, "skip_everything")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
	assert.Equal(t, StepInit, sm.Current())
}

func TestStateMachineFailFromAnyStep(t *testing.T) {
	stepsBeforeFail := []WorkflowStep{
		StepInit, StepPlanning, StepPlanReview, StepBuilding, StepCreatingPR, StepBuildReview,
		StepValidating, StepValidateReview, StepShipping, StepShipReview,
	}

	for _, step := range stepsBeforeFail {
		t.Run(step.String(), func(t *testing.T) {
			sm := NewStateMachine()
			// Walk to the target step via the happy path
			walkTo(t, sm, step)
			if step == StepInit {
				// Init can't go to Failed directly, only to Planning
				err := sm.Transition(StepFailed, "error")
				require.Error(t, err)
				return
			}
			require.NoError(t, sm.Transition(StepFailed, "error"))
			assert.Equal(t, StepFailed, sm.Current())
		})
	}
}

func TestStateMachineHistory(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StepPlanning, "start"))
	require.NoError(t, sm.Transition(StepPlanReview, "done"))

	history := sm.History()
	assert.Len(t, history, 2)
	assert.Equal(t, StepInit, history[0].From)
	assert.Equal(t, StepPlanning, history[0].To)
	assert.Equal(t, "start", history[0].Trigger)
	assert.Equal(t, StepPlanning, history[1].From)
	assert.Equal(t, StepPlanReview, history[1].To)

	// Ensure returned history is a copy
	history[0].Trigger = "modified"
	assert.Equal(t, "start", sm.History()[0].Trigger)
}

func TestShouldAutoApprove(t *testing.T) {
	cfg := &Config{}
	w := &Workflow{config: cfg}

	// No auto-approve by default.
	for _, step := range []WorkflowStep{StepPlanReview, StepBuildReview, StepValidateReview, StepShipReview} {
		assert.False(t, w.shouldAutoApprove(step), "step %s should not auto-approve by default", step)
	}

	// Non-review steps never auto-approve.
	assert.False(t, w.shouldAutoApprove(StepPlanning))
	assert.False(t, w.shouldAutoApprove(StepDone))

	// Enable selectively.
	cfg.Plan.AutoApprove = true
	cfg.Validate.AutoApprove = true
	assert.True(t, w.shouldAutoApprove(StepPlanReview))
	assert.False(t, w.shouldAutoApprove(StepBuildReview))
	assert.True(t, w.shouldAutoApprove(StepValidateReview))
	assert.False(t, w.shouldAutoApprove(StepShipReview))

	// Enable all.
	cfg.Build.AutoApprove = true
	cfg.Ship.AutoApprove = true
	for _, step := range []WorkflowStep{StepPlanReview, StepBuildReview, StepValidateReview, StepShipReview} {
		assert.True(t, w.shouldAutoApprove(step), "step %s should auto-approve", step)
	}
}

// walkTo transitions the state machine to the target step via the happy path.
func walkTo(t *testing.T, sm *StateMachine, target WorkflowStep) {
	t.Helper()
	path := []struct {
		trigger string
		step    WorkflowStep
	}{
		{trigger: "start", step: StepPlanning},
		{trigger: "done", step: StepPlanReview},
		{trigger: "approved", step: StepBuilding},
		{trigger: "done", step: StepCreatingPR},
		{trigger: "pr_created", step: StepBuildReview},
		{trigger: "approved", step: StepValidating},
		{trigger: "done", step: StepValidateReview},
		{trigger: "approved", step: StepShipping},
		{trigger: "pr_created", step: StepShipReview},
	}

	for _, p := range path {
		if sm.Current() == target {
			return
		}
		require.NoError(t, sm.Transition(p.step, p.trigger))
	}
}
