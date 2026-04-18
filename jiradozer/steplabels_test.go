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

func TestIsJiradozerLabel_AllowlistOnly(t *testing.T) {
	t.Parallel()
	// All eight phase × state labels should be recognized.
	for _, p := range phaseTable {
		assert.True(t, isJiradozerLabel(inProgressLabel(p.name)), "%s in-progress", p.name)
		assert.True(t, isJiradozerLabel(doneLabel(p.name)), "%s done", p.name)
	}
	// Unrelated jiradozer-prefixed labels must NOT be filtered, so teams
	// can use the namespace without their labels being hidden from agents.
	assert.False(t, isJiradozerLabel("jiradozer-backlog"))
	assert.False(t, isJiradozerLabel("jiradozer-plan-unknown"))
	assert.False(t, isJiradozerLabel("jiradozer-"))
	assert.False(t, isJiradozerLabel(""))
	assert.False(t, isJiradozerLabel("bug"))
}
