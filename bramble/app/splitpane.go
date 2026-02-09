package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// PaneLayout controls the center area display mode.
type PaneLayout int

const (
	// PaneLayoutSingle shows only the right pane (backward-compatible default).
	PaneLayoutSingle PaneLayout = iota
	// PaneLayoutSplit shows left (file tree) and right (agent output) side by side.
	PaneLayoutSplit
)

// SplitPane manages a two-pane layout with a file tree on the left
// and agent output on the right.
type SplitPane struct {
	layout       PaneLayout
	leftWidthPct int // percentage of total width for left pane (default 30)
	focusLeft    bool
}

// NewSplitPane creates a new split pane with sensible defaults.
func NewSplitPane() *SplitPane {
	return &SplitPane{
		layout:       PaneLayoutSingle,
		leftWidthPct: 30,
	}
}

// Toggle switches between single and split layout.
func (sp *SplitPane) Toggle() {
	if sp.layout == PaneLayoutSingle {
		sp.layout = PaneLayoutSplit
	} else {
		sp.layout = PaneLayoutSingle
	}
}

// IsSplit returns true if the layout is split.
func (sp *SplitPane) IsSplit() bool {
	return sp.layout == PaneLayoutSplit
}

// FocusLeft returns true if the left pane has focus.
func (sp *SplitPane) FocusLeft() bool {
	return sp.focusLeft
}

// SetFocusLeft sets whether the left pane has focus.
func (sp *SplitPane) SetFocusLeft(left bool) {
	sp.focusLeft = left
}

// ToggleFocus switches focus between left and right panes.
func (sp *SplitPane) ToggleFocus() {
	sp.focusLeft = !sp.focusLeft
}

// LeftWidth returns the calculated left pane width in columns.
func (sp *SplitPane) LeftWidth(totalWidth int) int {
	return totalWidth * sp.leftWidthPct / 100
}

// RightWidth returns the calculated right pane width in columns.
func (sp *SplitPane) RightWidth(totalWidth int) int {
	return totalWidth - sp.LeftWidth(totalWidth) - 1 // -1 for divider
}

var dividerStyle = lipgloss.NewStyle().
	Foreground(borderColor)

// Render composites left and right content into a split or single layout.
// When PaneLayoutSingle, only rightContent is shown.
func (sp *SplitPane) Render(leftContent, rightContent string, width, height int) string {
	if sp.layout == PaneLayoutSingle {
		return rightContent
	}

	leftWidth := sp.LeftWidth(width)
	rightWidth := sp.RightWidth(width)

	// Build the divider column
	divider := strings.Repeat(dividerStyle.Render("│")+"\n", height)
	divider = strings.TrimRight(divider, "\n")

	// Pad/truncate left and right content to fixed widths
	leftRendered := padToSize(leftContent, leftWidth, height)
	rightRendered := padToSize(rightContent, rightWidth, height)

	return lipgloss.JoinHorizontal(lipgloss.Top, leftRendered, divider, rightRendered)
}

// padToSize ensures content fills exactly width columns and height lines.
func padToSize(content string, width, height int) string {
	lines := strings.Split(content, "\n")

	// Pad or truncate to height
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}

	// Pad each line to width
	for i, line := range lines {
		stripped := stripAnsi(line)
		visualWidth := runewidth.StringWidth(stripped)
		if visualWidth < width {
			lines[i] = line + strings.Repeat(" ", width-visualWidth)
		} else if visualWidth > width {
			lines[i] = truncateVisual(line, width)
		}
	}

	return strings.Join(lines, "\n")
}
