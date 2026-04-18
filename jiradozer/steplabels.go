package jiradozer

// Phase names used as label segments for step tracking. Four high-level
// phases are surfaced to users as labels; review gates and create_pr fold
// into the adjacent agent phase.
const (
	PhasePlan     = "plan"
	PhaseBuild    = "build"
	PhaseValidate = "validate"
	PhaseShip     = "ship"
)

// allPhases lists phases in workflow order. Used for skip-done detection.
var allPhases = []string{PhasePlan, PhaseBuild, PhaseValidate, PhaseShip}

// phaseForStep maps a WorkflowStep to its phase name, or "" if the step
// does not belong to a labelled phase (review gates, terminal states).
// StepCreatingPR folds into PhaseBuild.
func phaseForStep(s WorkflowStep) string {
	switch s {
	case StepPlanning:
		return PhasePlan
	case StepBuilding, StepCreatingPR:
		return PhaseBuild
	case StepValidating:
		return PhaseValidate
	case StepShipping:
		return PhaseShip
	}
	return ""
}

// startStepForPhase returns the first WorkflowStep of the given phase.
// Used when forcing the workflow to start at a mid-phase step after
// skipping completed phases.
func startStepForPhase(phase string) WorkflowStep {
	switch phase {
	case PhasePlan:
		return StepPlanning
	case PhaseBuild:
		return StepBuilding
	case PhaseValidate:
		return StepValidating
	case PhaseShip:
		return StepShipping
	}
	return StepInit
}

func inProgressLabel(phase string) string { return "jiradozer-" + phase + "-inprogress" }
func doneLabel(phase string) string       { return "jiradozer-" + phase + "-done" }

// hasLabel reports whether the label set contains name.
func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if l == name {
			return true
		}
	}
	return false
}
