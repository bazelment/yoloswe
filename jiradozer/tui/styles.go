package tui

import (
	"fmt"

	"charm.land/lipgloss/v2"

	"github.com/bazelment/yoloswe/jiradozer"
)

var (
	colorActive  = lipgloss.Color("#FFCC00") // yellow — running steps
	colorWaiting = lipgloss.Color("#00CCCC") // cyan — review/waiting steps
	colorDone    = lipgloss.Color("#00CC66") // green
	colorFailed  = lipgloss.Color("#CC3333") // red
	colorDim     = lipgloss.Color("#666666") // gray — queued
	colorHeader  = lipgloss.Color("#AAAAAA")
	colorCursor  = lipgloss.Color("#FFFFFF")
)

var (
	styleHeader    = lipgloss.NewStyle().Foreground(colorHeader).Bold(true)
	styleCursor    = lipgloss.NewStyle().Foreground(colorCursor).Bold(true)
	styleActive    = lipgloss.NewStyle().Foreground(colorActive)
	styleWaiting   = lipgloss.NewStyle().Foreground(colorWaiting)
	styleDone      = lipgloss.NewStyle().Foreground(colorDone)
	styleFailed    = lipgloss.NewStyle().Foreground(colorFailed)
	styleDim       = lipgloss.NewStyle().Foreground(colorDim)
	styleTitle     = lipgloss.NewStyle().Bold(true)
	styleSubtitle  = lipgloss.NewStyle().Foreground(colorHeader)
	styleStatusBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#AAAAAA")).
			Background(lipgloss.Color("#333333"))
)

// stepStyle returns the lipgloss style for a workflow step.
func stepStyle(step jiradozer.WorkflowStep) lipgloss.Style {
	switch step {
	case jiradozer.StepPlanning, jiradozer.StepBuilding, jiradozer.StepValidating, jiradozer.StepShipping:
		return styleActive
	case jiradozer.StepPlanReview, jiradozer.StepBuildReview, jiradozer.StepValidateReview, jiradozer.StepShipReview:
		return styleWaiting
	case jiradozer.StepDone:
		return styleDone
	case jiradozer.StepFailed:
		return styleFailed
	default:
		return styleDim
	}
}

// stepStatus returns a human-readable status string for a workflow step.
func stepStatus(step jiradozer.WorkflowStep) string {
	switch step {
	case jiradozer.StepPlanReview, jiradozer.StepBuildReview,
		jiradozer.StepValidateReview, jiradozer.StepShipReview:
		return "waiting"
	case jiradozer.StepDone:
		return "done"
	case jiradozer.StepFailed:
		return "failed"
	case jiradozer.StepInit:
		return "queued"
	default:
		return "running"
	}
}

// formatStep returns the step name with round progress appended for multi-round steps.
func formatStep(s jiradozer.IssueStatus) string {
	step := s.Step.String()
	if s.RoundTotal > 0 {
		step = fmt.Sprintf("%s (%d/%d)", step, s.RoundIndex+1, s.RoundTotal)
	}
	return step
}

// stepIcon returns a status icon for a workflow step.
func stepIcon(step jiradozer.WorkflowStep) string {
	switch step {
	case jiradozer.StepPlanning, jiradozer.StepBuilding, jiradozer.StepValidating, jiradozer.StepShipping:
		return "●"
	case jiradozer.StepPlanReview, jiradozer.StepBuildReview, jiradozer.StepValidateReview, jiradozer.StepShipReview:
		return "◎"
	case jiradozer.StepDone:
		return "✓"
	case jiradozer.StepFailed:
		return "✗"
	default:
		return "○"
	}
}
