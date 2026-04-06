package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/jiradozer"
)

// dashboard renders the scrollable issue table.
type dashboard struct {
	selectedID  string // issue ID to preserve selection across sorts
	statuses    []jiradozer.IssueStatus
	selectedIdx int
	height      int // visible rows
}

func newDashboard() *dashboard {
	return &dashboard{}
}

func (d *dashboard) setStatuses(statuses []jiradozer.IssueStatus) {
	d.statuses = statuses
	// Restore selection by issue ID after sort/reorder.
	if d.selectedID != "" {
		for i, s := range d.statuses {
			if s.Issue.ID == d.selectedID {
				d.selectedIdx = i
				return
			}
		}
	}
	if d.selectedIdx >= len(d.statuses) {
		d.selectedIdx = max(0, len(d.statuses)-1)
	}
}

func (d *dashboard) moveUp() {
	if d.selectedIdx > 0 {
		d.selectedIdx--
		d.trackSelection()
	}
}

func (d *dashboard) moveDown() {
	if d.selectedIdx < len(d.statuses)-1 {
		d.selectedIdx++
		d.trackSelection()
	}
}

func (d *dashboard) trackSelection() {
	if d.selectedIdx < len(d.statuses) {
		d.selectedID = d.statuses[d.selectedIdx].Issue.ID
	}
}

func (d *dashboard) selected() *jiradozer.IssueStatus {
	if d.selectedIdx < len(d.statuses) {
		return &d.statuses[d.selectedIdx]
	}
	return nil
}

func (d *dashboard) view(width int) string {
	if len(d.statuses) == 0 {
		return styleDim.Render("\n  Waiting for issues...\n")
	}

	var b strings.Builder

	// Header
	header := fmt.Sprintf("  %-12s %-40s %-16s %-8s %s",
		"ID", "Title", "Step", "Status", "Duration")
	b.WriteString(styleHeader.Render(header))
	b.WriteString("\n")
	b.WriteString(styleHeader.Render(strings.Repeat("─", min(width-2, len(header)+10))))
	b.WriteString("\n")

	for i, s := range d.statuses {
		cursor := "  "
		if i == d.selectedIdx {
			cursor = styleCursor.Render("► ")
		}

		title := s.Issue.Title
		if len(title) > 38 {
			title = title[:35] + "..."
		}

		step := s.Step.String()
		style := stepStyle(s.Step)
		icon := stepIcon(s.Step)

		status := stepStatus(s.Step)

		duration := "--"
		if !s.StartedAt.IsZero() {
			end := time.Now()
			if !s.CompletedAt.IsZero() {
				end = s.CompletedAt
			}
			d := end.Sub(s.StartedAt)
			duration = formatDuration(d)
		}

		row := fmt.Sprintf("%s%-12s %-40s %s %-14s %-8s %s",
			cursor,
			s.Issue.Identifier,
			title,
			style.Render(icon),
			style.Render(step),
			style.Render(status),
			styleDim.Render(duration),
		)
		b.WriteString(row)
		b.WriteString("\n")
	}

	return b.String()
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", m, s)
}
