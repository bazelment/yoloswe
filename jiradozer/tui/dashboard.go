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
	height      int // visible terminal rows
	scrollY     int // index of the first visible row
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
		d.clampScroll()
	}
}

func (d *dashboard) moveDown() {
	if d.selectedIdx < len(d.statuses)-1 {
		d.selectedIdx++
		d.trackSelection()
		d.clampScroll()
	}
}

// clampScroll adjusts scrollY so the selected row is always within the visible window.
func (d *dashboard) clampScroll() {
	visibleRows := d.visibleRows()
	if visibleRows <= 0 {
		return
	}
	// Scroll up if the selection is above the viewport.
	if d.selectedIdx < d.scrollY {
		d.scrollY = d.selectedIdx
	}
	// Scroll down if the selection is below the viewport.
	if d.selectedIdx >= d.scrollY+visibleRows {
		d.scrollY = d.selectedIdx - visibleRows + 1
	}
}

// visibleRows returns the number of data rows that fit in the terminal height.
// Two header lines (column labels + separator) are reserved for chrome.
func (d *dashboard) visibleRows() int {
	const headerLines = 2
	v := d.height - headerLines
	if v < 1 {
		return 1
	}
	return v
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

	visibleRows := d.visibleRows()
	end := min(d.scrollY+visibleRows, len(d.statuses))
	start := d.scrollY
	if start > end {
		start = end
	}

	for i := start; i < end; i++ {
		s := d.statuses[i]
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
			endTime := time.Now()
			if !s.CompletedAt.IsZero() {
				endTime = s.CompletedAt
			}
			dur := endTime.Sub(s.StartedAt)
			duration = formatDuration(dur)
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
