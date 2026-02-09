package app

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/bazelment/yoloswe/wt"
)

// Styles specific to the welcome screen
var (
	welcomeTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(accentColor).
				MarginBottom(1)

	welcomeKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(idleColor)

	welcomeDescStyle = lipgloss.NewStyle().
				Foreground(barFgColor)
)

// renderWelcome renders the welcome/empty state for the center area.
// It adapts based on whether worktrees exist and what state they're in.
func (m Model) renderWelcome(width, height int) string {
	var b strings.Builder

	hasWorktrees := len(m.worktrees) > 0
	wt := m.selectedWorktree()
	inTmux := m.sessionManager.IsInTmuxMode()

	// Show worktree operation messages if any (e.g. "Creating worktree...")
	if len(m.worktreeOpMessages) > 0 {
		b.WriteString("\n")
		for _, msg := range m.worktreeOpMessages {
			b.WriteString("  ")
			b.WriteString(msg)
			b.WriteString("\n")
		}
		return b.String()
	}

	b.WriteString("\n")

	if !hasWorktrees {
		// Variant 1: No worktrees at all
		b.WriteString(welcomeTitleStyle.Render("  Welcome to Bramble"))
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render("  No worktrees found for " + m.repoName))
		b.WriteString("\n\n")
		b.WriteString("  Get started:\n\n")
		b.WriteString(renderKeyHint("t", "New task", "Describe what you want; AI picks the branch"))
		b.WriteString(renderKeyHint("n", "New worktree", "Create a branch manually"))
		b.WriteString(renderKeyHint("Alt-W", "Worktrees", "Browse and select worktrees"))
		b.WriteString(renderKeyHint("?", "Help", "Show all keyboard shortcuts"))
		b.WriteString("\n")
	} else {
		// Variant 2: Worktrees exist, no session selected
		b.WriteString(welcomeTitleStyle.Render("  Bramble"))
		b.WriteString("\n\n")
		b.WriteString("  Quick start:\n\n")
		b.WriteString(renderKeyHint("t", "New task", "Describe what you want; AI picks the worktree"))
		b.WriteString(renderKeyHint("p", "Plan", "Start a planning session on current worktree"))
		b.WriteString(renderKeyHint("b", "Build", "Start a builder session on current worktree"))
		if !inTmux {
			b.WriteString(renderKeyHint("Alt-S", "Sessions", "Browse and switch sessions"))
		}
		b.WriteString(renderKeyHint("?", "Help", "Show all keyboard shortcuts"))
		b.WriteString("\n")

		// Current worktree summary
		if wt != nil {
			b.WriteString(renderWorktreeSummary(m, wt))
		}

		// Session summary
		sessions := m.currentWorktreeSessions()
		if len(sessions) > 0 {
			b.WriteString("\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("  %d active session(s) on this worktree", len(sessions))))
			b.WriteString("\n")
			if !inTmux {
				b.WriteString(dimStyle.Render("  Press [Alt-S] to view them"))
			} else {
				b.WriteString(dimStyle.Render("  Press [Enter] to switch to a session window"))
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderKeyHint renders a single key hint line for the welcome screen.
func renderKeyHint(key, action, description string) string {
	// Fixed-width columns for alignment:
	//   "    [t]  New task         Describe what you want; AI picks the branch"
	keyCol := fmt.Sprintf("  %s", welcomeKeyStyle.Render(fmt.Sprintf("[%s]", key)))
	// Pad key column to 14 chars visual width for alignment
	keyVisual := runewidth.StringWidth(stripAnsi(keyCol))
	padding := 14 - keyVisual
	if padding < 1 {
		padding = 1
	}
	// Pad the action text to a fixed visual width before applying ANSI styles,
	// since fmt's %-Ns counts bytes (including escape codes), not visual width.
	actionPadded := action
	if len(action) < 18 {
		actionPadded = action + strings.Repeat(" ", 18-len(action))
	}
	return fmt.Sprintf("%s%s%s %s\n",
		keyCol,
		strings.Repeat(" ", padding),
		welcomeDescStyle.Render(actionPadded),
		dimStyle.Render(description),
	)
}

// renderWorktreeSummary renders a summary of the current worktree.
func renderWorktreeSummary(m Model, wt *wt.Worktree) string {
	var b strings.Builder
	b.WriteString(dimStyle.Render("  Current worktree: "))
	b.WriteString(titleStyle.Render(wt.Branch))

	// Add status details if available
	if m.worktreeStatuses != nil {
		if status, ok := m.worktreeStatuses[wt.Branch]; ok {
			var details []string
			if status.IsDirty {
				details = append(details, failedStyle.Render("dirty"))
			} else {
				details = append(details, completedStyle.Render("clean"))
			}
			if status.Ahead > 0 {
				details = append(details, runningStyle.Render(fmt.Sprintf("↑%d ahead", status.Ahead)))
			}
			if status.Behind > 0 {
				details = append(details, pendingStyle.Render(fmt.Sprintf("↓%d behind", status.Behind)))
			}
			if status.PRNumber > 0 {
				prText := fmt.Sprintf("PR#%d %s", status.PRNumber, status.PRState)
				details = append(details, dimStyle.Render(prText))
			}
			if len(details) > 0 {
				b.WriteString(" (")
				b.WriteString(strings.Join(details, ", "))
				b.WriteString(")")
			}
		}
	}

	// File tree summary
	if m.fileTree != nil && m.fileTree.FileCount() > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf(" -- %d files changed", m.fileTree.FileCount())))
	}

	b.WriteString("\n")
	return b.String()
}
