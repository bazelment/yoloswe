package jiradozer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPhaseForStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		want string
		step WorkflowStep
	}{
		{"", StepInit},
		{PhasePlan, StepPlanning},
		{"", StepPlanReview},
		{PhaseBuild, StepBuilding},
		{PhaseBuild, StepCreatingPR},
		{"", StepBuildReview},
		{PhaseValidate, StepValidating},
		{"", StepValidateReview},
		{PhaseShip, StepShipping},
		{"", StepShipReview},
		{"", StepDone},
		{"", StepFailed},
		{"", StepCancelled},
	}
	for _, c := range cases {
		t.Run(c.step.String(), func(t *testing.T) {
			assert.Equal(t, c.want, phaseForStep(c.step))
		})
	}
}

func TestStartStepForPhase(t *testing.T) {
	t.Parallel()
	assert.Equal(t, StepPlanning, startStepForPhase(PhasePlan))
	assert.Equal(t, StepBuilding, startStepForPhase(PhaseBuild))
	assert.Equal(t, StepValidating, startStepForPhase(PhaseValidate))
	assert.Equal(t, StepShipping, startStepForPhase(PhaseShip))
	assert.Equal(t, StepInit, startStepForPhase("unknown"))
}

func TestPhaseAfter(t *testing.T) {
	t.Parallel()
	assert.Equal(t, PhaseBuild, phaseAfter(PhasePlan))
	assert.Equal(t, PhaseValidate, phaseAfter(PhaseBuild))
	assert.Equal(t, PhaseShip, phaseAfter(PhaseValidate))
	assert.Empty(t, phaseAfter(PhaseShip), "ship is the last phase")
	assert.Empty(t, phaseAfter("bogus"))
}

func TestLabelFormat(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "jiradozer-plan-inprogress", inProgressLabel(PhasePlan))
	assert.Equal(t, "jiradozer-plan-done", doneLabel(PhasePlan))
	assert.Equal(t, "jiradozer-build-inprogress", inProgressLabel(PhaseBuild))
	assert.Equal(t, "jiradozer-validate-done", doneLabel(PhaseValidate))
	assert.Equal(t, "jiradozer-ship-inprogress", inProgressLabel(PhaseShip))
}

func TestHasLabel(t *testing.T) {
	t.Parallel()
	labels := []string{"bug", "jiradozer-plan-done"}
	assert.True(t, hasLabel(labels, "bug"))
	assert.True(t, hasLabel(labels, "jiradozer-plan-done"))
	assert.False(t, hasLabel(labels, "other"))
	assert.False(t, hasLabel(nil, "anything"))
}
