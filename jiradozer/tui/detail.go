package tui

import (
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/jiradozer"
)

// detail renders the drill-down view for a single issue.
type detail struct {
	status  *jiradozer.IssueStatus
	scrollY int
	height  int
}

func newDetail() *detail {
	return &detail{}
}

func (d *detail) setStatus(s *jiradozer.IssueStatus) {
	d.status = s
	d.scrollY = 0
}

func (d *detail) scrollUp() {
	if d.scrollY > 0 {
		d.scrollY--
	}
}

func (d *detail) scrollDown() {
	d.scrollY++
}

func (d *detail) view(width int) string {
	if d.status == nil {
		return styleDim.Render("\n  No issue selected\n")
	}

	s := d.status
	var b strings.Builder

	// Header
	b.WriteString(styleTitle.Render(fmt.Sprintf("  %s — %s", s.Issue.Identifier, s.Issue.Title)))
	b.WriteString("\n\n")

	// Metadata
	if s.Issue.Description != nil && *s.Issue.Description != "" {
		desc := *s.Issue.Description
		if len(desc) > 500 {
			desc = desc[:497] + "..."
		}
		b.WriteString(styleSubtitle.Render("  Description:"))
		b.WriteString("\n  ")
		b.WriteString(desc)
		b.WriteString("\n\n")
	}

	if len(s.Issue.Labels) > 0 {
		b.WriteString(styleSubtitle.Render("  Labels: "))
		b.WriteString(strings.Join(s.Issue.Labels, ", "))
		b.WriteString("\n")
	}

	if s.Issue.URL != nil {
		b.WriteString(styleSubtitle.Render("  URL: "))
		b.WriteString(*s.Issue.URL)
		b.WriteString("\n")
	}

	b.WriteString("\n")

	// Workflow status
	step := s.Step.String()
	style := stepStyle(s.Step)
	icon := stepIcon(s.Step)
	b.WriteString(styleSubtitle.Render("  Current Step: "))
	b.WriteString(style.Render(fmt.Sprintf("%s %s", icon, step)))
	b.WriteString("\n")

	if s.Error != nil {
		b.WriteString(styleFailed.Render(fmt.Sprintf("  Error: %s", s.Error.Error())))
		b.WriteString("\n")
	}

	if s.WorktreePath != "" {
		b.WriteString(styleSubtitle.Render("  Worktree: "))
		b.WriteString(styleDim.Render(s.WorktreePath))
		b.WriteString("\n")
	}

	if !s.StartedAt.IsZero() {
		b.WriteString(styleSubtitle.Render("  Started: "))
		b.WriteString(s.StartedAt.Format("15:04:05"))
		if !s.CompletedAt.IsZero() {
			b.WriteString(styleSubtitle.Render("  Completed: "))
			b.WriteString(s.CompletedAt.Format("15:04:05"))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(styleDim.Render("  Press Esc to return to dashboard"))
	b.WriteString("\n")

	// Apply scroll
	lines := strings.Split(b.String(), "\n")
	if d.scrollY >= len(lines) {
		d.scrollY = max(0, len(lines)-1)
	}
	visibleEnd := min(len(lines), d.scrollY+d.height)
	if d.height > 0 && d.scrollY < len(lines) {
		lines = lines[d.scrollY:visibleEnd]
	}

	return strings.Join(lines, "\n")
}
