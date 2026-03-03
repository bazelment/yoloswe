package app

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/bazelment/yoloswe/bramble/session"
)

// AllSessionsOverlay displays all active sessions across all worktrees.
type AllSessionsOverlay struct {
	sessions    []session.SessionInfo
	selectedIdx int
	width       int
	height      int
	visible     bool
}

// NewAllSessionsOverlay creates a new overlay.
func NewAllSessionsOverlay() *AllSessionsOverlay {
	return &AllSessionsOverlay{}
}

// Show populates and displays the overlay with the given sessions.
func (o *AllSessionsOverlay) Show(sessions []session.SessionInfo, w, h int) {
	o.sessions = sessions
	o.width = w
	o.height = h
	o.selectedIdx = 0
	o.visible = true
}

// Hide closes the overlay.
func (o *AllSessionsOverlay) Hide() {
	o.visible = false
}

// IsVisible returns whether the overlay is showing.
func (o *AllSessionsOverlay) IsVisible() bool {
	return o.visible
}

// SetSize updates the overlay dimensions.
func (o *AllSessionsOverlay) SetSize(w, h int) {
	o.width = w
	o.height = h
}

// MoveSelection moves the selection by delta (positive = down, negative = up).
func (o *AllSessionsOverlay) MoveSelection(delta int) {
	o.selectedIdx += delta
	if o.selectedIdx < 0 {
		o.selectedIdx = 0
	}
	if o.selectedIdx >= len(o.sessions) {
		o.selectedIdx = len(o.sessions) - 1
	}
	if o.selectedIdx < 0 {
		o.selectedIdx = 0
	}
}

// SelectByNumber selects a session by 1-based number. Returns false if out of range.
func (o *AllSessionsOverlay) SelectByNumber(n int) bool {
	idx := n - 1
	if idx < 0 || idx >= len(o.sessions) {
		return false
	}
	o.selectedIdx = idx
	return true
}

// SelectedSession returns the currently selected session, or nil if none.
func (o *AllSessionsOverlay) SelectedSession() *session.SessionInfo {
	if len(o.sessions) == 0 || o.selectedIdx < 0 || o.selectedIdx >= len(o.sessions) {
		return nil
	}
	return &o.sessions[o.selectedIdx]
}

// Sessions returns the overlay's session list.
func (o *AllSessionsOverlay) Sessions() []session.SessionInfo {
	return o.sessions
}

// View renders the overlay as a centered box.
func (o *AllSessionsOverlay) View(s *Styles) string {
	// Build content lines
	var lines []string

	lines = append(lines, s.Title.Render("All Active Sessions"), "")

	// Calculate box width — use most of the terminal but cap at 140
	boxWidth := o.width - 4
	if boxWidth > 140 {
		boxWidth = 140
	}
	if boxWidth < 40 {
		boxWidth = 40
	}

	// Content width inside box (subtract border + padding: 2 border + 4 padding)
	contentWidth := boxWidth - 6
	if contentWidth < 30 {
		contentWidth = 30
	}

	if len(o.sessions) == 0 {
		lines = append(lines, s.Dim.Render("  No active sessions across any worktree."), "")
	} else {
		// Check if sessions span multiple repos to decide whether to show a Repo column.
		multiRepo := false
		if len(o.sessions) > 1 {
			first := o.sessions[0].RepoName
			for i := 1; i < len(o.sessions); i++ {
				if o.sessions[i].RepoName != first {
					multiRepo = true
					break
				}
			}
		}

		// Scale column widths to fit contentWidth.
		// Fixed overhead: " #. 🔨  " prefix (~9 cols) + status (~12 cols) + gaps = ~27 cols
		fixedCols := 27 // num(3) + icon(4) + status(12) + spacing(8)
		flexBudget := contentWidth - fixedCols
		if flexBudget < 30 {
			flexBudget = 30
		}

		// Allocate flex budget; add a Repo column when multiple repos are present.
		var repoColWidth int
		if multiRepo {
			repoColWidth = flexBudget * 15 / 100
			if repoColWidth < 8 {
				repoColWidth = 8
			}
			flexBudget -= repoColWidth
		}

		// Allocate: 30% worktree, 25% name, 45% prompt (with minimums)
		wtColWidth := flexBudget * 30 / 100
		if wtColWidth < 8 {
			wtColWidth = 8
		}
		nameColWidth := flexBudget * 25 / 100
		if nameColWidth < 8 {
			nameColWidth = 8
		}
		promptWidth := flexBudget - wtColWidth - nameColWidth
		if promptWidth < 10 {
			promptWidth = 10
		}

		// Table header
		wtFmt := fmt.Sprintf("%%-%ds", wtColWidth)
		nameFmt := fmt.Sprintf("%%-%ds", nameColWidth)

		var headerFmt, rowFmt string
		if multiRepo {
			repoFmt := fmt.Sprintf("%%-%ds", repoColWidth)
			headerFmt = " %-3s %-4s " + repoFmt + " " + wtFmt + " " + nameFmt + " %-12s %s"
			rowFmt = " %-3s %s  " + repoFmt + " " + wtFmt + " " + nameFmt + " %-12s %s"
			header := s.Dim.Render(fmt.Sprintf(headerFmt, "#", "Type", "Repo", "Worktree", "Name", "Status", "Prompt"))
			lines = append(lines, header)
		} else {
			headerFmt = " %-3s %-4s " + wtFmt + " " + nameFmt + " %-12s %s"
			rowFmt = " %-3s %s  " + wtFmt + " " + nameFmt + " %-12s %s"
			header := s.Dim.Render(fmt.Sprintf(headerFmt, "#", "Type", "Worktree", "Name", "Status", "Prompt"))
			lines = append(lines, header)
		}
		sepWidth := contentWidth - 1
		if sepWidth < 20 {
			sepWidth = 20
		}
		lines = append(lines, " "+strings.Repeat("─", sepWidth))

		// Session rows
		for i := range o.sessions {
			sess := &o.sessions[i]

			// Number (1-9 for quick select, blank otherwise)
			num := ""
			if i < 9 {
				num = fmt.Sprintf("%d.", i+1)
			}

			// Type icon
			typeIcon := "📋"
			if sess.Type == session.SessionTypeBuilder {
				typeIcon = "🔨"
			}

			// Worktree name
			wtName := truncate(sess.WorktreeName, wtColWidth-1)

			// Session name
			nameDisplay := sess.TmuxWindowName
			if nameDisplay == "" {
				nameDisplay = sess.Title
			}
			if nameDisplay == "" && len(sess.ID) > 12 {
				nameDisplay = string(sess.ID)[:12]
			} else if nameDisplay == "" {
				nameDisplay = string(sess.ID)
			}
			nameDisplay = truncate(nameDisplay, nameColWidth-1)

			// Status
			statusStr := fmt.Sprintf("%s %-8s", statusIcon(sess.Status, s), sess.Status)

			// Prompt
			prompt := sess.Prompt
			if prompt != "" && prompt[0] == '"' {
				prompt = strings.Trim(prompt, `"`)
			}
			promptDisplay := truncate(prompt, promptWidth)

			var line string
			if multiRepo {
				repoName := truncate(sess.RepoName, repoColWidth-1)
				line = fmt.Sprintf(rowFmt, num, typeIcon, repoName, wtName, nameDisplay, statusStr, promptDisplay)
			} else {
				line = fmt.Sprintf(rowFmt, num, typeIcon, wtName, nameDisplay, statusStr, promptDisplay)
			}

			if i == o.selectedIdx {
				line = s.Selected.Render(line)
			}

			lines = append(lines, line)
		}
	}

	lines = append(lines, "")

	// Footer
	footer := s.Dim.Render("[↑/↓] Navigate  [Enter] Switch  [1-9] Quick select  [Esc] Close")
	lines = append(lines, footer)

	contentStr := strings.Join(lines, "\n")

	box := s.AllSessionsBox.
		Width(boxWidth).
		Render(contentStr)

	// Center the box
	if o.width > 0 && o.height > 0 {
		return lipgloss.Place(
			o.width, o.height,
			lipgloss.Center, lipgloss.Center,
			box,
		)
	}
	return box
}
