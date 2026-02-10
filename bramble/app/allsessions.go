package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/bramble/session"
)

// AllSessionsOverlay displays all active sessions across all worktrees.
type AllSessionsOverlay struct {
	sessions     []session.SessionInfo
	selectedIdx  int
	scrollOffset int
	width        int
	height       int
	visible      bool
	inTmuxMode   bool
}

// NewAllSessionsOverlay creates a new overlay.
func NewAllSessionsOverlay() *AllSessionsOverlay {
	return &AllSessionsOverlay{}
}

// Show populates and displays the overlay with the given sessions.
func (o *AllSessionsOverlay) Show(sessions []session.SessionInfo, inTmuxMode bool, w, h int) {
	o.sessions = sessions
	o.inTmuxMode = inTmuxMode
	o.width = w
	o.height = h
	o.selectedIdx = 0
	o.scrollOffset = 0
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
func (o *AllSessionsOverlay) View() string {
	// Build content lines
	var lines []string

	lines = append(lines, titleStyle.Render("All Active Sessions"))
	lines = append(lines, "")

	// Calculate box width first so we can size columns dynamically
	boxWidth := o.width - 4
	if boxWidth > 140 {
		boxWidth = 140
	}
	if boxWidth < 60 {
		boxWidth = 60
	}

	// Content width inside box (subtract border + padding: 2 border + 4 padding)
	contentWidth := boxWidth - 6

	if len(o.sessions) == 0 {
		lines = append(lines, dimStyle.Render("  No active sessions across any worktree."))
		lines = append(lines, "")
	} else {
		// Fixed-width columns: #(3) + Type(4) + gaps(8) + Worktree(20) + Name(20) + Status(12) = 67
		// Prompt gets the rest
		promptWidth := contentWidth - 67
		if promptWidth < 15 {
			promptWidth = 15
		}

		// Table header
		header := dimStyle.Render(fmt.Sprintf("   %-4s %-4s %-20s %-20s %-12s %s", "#", "Type", "Worktree", "Name", "Status", "Prompt"))
		lines = append(lines, header)
		sepWidth := contentWidth - 3
		if sepWidth < 40 {
			sepWidth = 40
		}
		lines = append(lines, "   "+strings.Repeat("─", sepWidth))

		// Session rows
		for i := range o.sessions {
			sess := &o.sessions[i]

			// Number
			num := "  "
			if i < 9 {
				num = fmt.Sprintf("%d.", i+1)
			}

			// Type icon
			typeIcon := "📋"
			if sess.Type == session.SessionTypeBuilder {
				typeIcon = "🔨"
			}

			// Worktree name
			wtName := truncate(sess.WorktreeName, 19)

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
			nameDisplay = truncate(nameDisplay, 19)

			// Status
			statusStr := fmt.Sprintf("%s %-8s", statusIcon(sess.Status), sess.Status)

			// Prompt
			prompt := sess.Prompt
			if prompt != "" && prompt[0] == '"' {
				prompt = strings.Trim(prompt, `"`)
			}
			promptDisplay := truncate(prompt, promptWidth)

			line := fmt.Sprintf(" %s %s  %-20s %-20s %-12s %s", num, typeIcon, wtName, nameDisplay, statusStr, promptDisplay)

			if i == o.selectedIdx {
				line = selectedStyle.Render(line)
			}

			lines = append(lines, line)
		}
	}

	lines = append(lines, "")

	// Footer
	footer := dimStyle.Render("[↑/↓] Navigate  [Enter] Switch  [1-9] Quick select  [Esc] Close")
	lines = append(lines, footer)

	contentStr := strings.Join(lines, "\n")

	box := allSessionsBoxStyle.
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

// allSessionsBoxStyle defines the overlay box style.
var allSessionsBoxStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(borderColor).
	Padding(1, 2)

