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

// phaseTable is the single source of truth for phase ordering and the
// starting WorkflowStep of each phase. Review gates and StepCreatingPR are
// not primary starts; they are folded into the preceding phase by
// phaseForStep.
var phaseTable = []struct {
	name      string
	startStep WorkflowStep
}{
	{PhasePlan, StepPlanning},
	{PhaseBuild, StepBuilding},
	{PhaseValidate, StepValidating},
	{PhaseShip, StepShipping},
}

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

func startStepForPhase(phase string) WorkflowStep {
	for _, p := range phaseTable {
		if p.name == phase {
			return p.startStep
		}
	}
	return StepInit
}

func phaseAfter(phase string) string {
	for i, p := range phaseTable {
		if p.name == phase && i+1 < len(phaseTable) {
			return phaseTable[i+1].name
		}
	}
	return ""
}

func inProgressLabel(phase string) string { return "jiradozer-" + phase + "-inprogress" }
func doneLabel(phase string) string       { return "jiradozer-" + phase + "-done" }
