package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

// Styles specific to the welcome screen
var (
	welcomeTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("12")).
				MarginBottom(1)

	welcomeKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("14"))

	welcomeDescStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252"))
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

		// Session timeline (live + history merged)
		timeline := m.buildTimeline()
		if len(timeline) > 0 {
			// Calculate max lines from remaining height.
			// Count lines used so far (rough estimate: title+blank+quickstart+blank+worktree ~ 10-12 lines).
			usedLines := 12
			maxTimelineLines := height - usedLines
			if maxTimelineLines < 3 {
				maxTimelineLines = 3
			}
			if maxTimelineLines > 15 {
				maxTimelineLines = 15
			}
			b.WriteString("\n")
			b.WriteString(renderTimeline(timeline, maxTimelineLines))
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

// timelineEntry represents a single entry in the session timeline.
type timelineEntry struct {
	timestamp time.Time
	icon      string
	event     string
	prompt    string
	sessionID session.SessionID
}

// buildTimeline merges live sessions and cached history for the current worktree
// into a unified timeline, sorted newest-first. Live sessions take precedence
// when a session ID appears in both lists.
func (m Model) buildTimeline() []timelineEntry {
	liveSessions := m.currentWorktreeSessions()

	// Collect live session IDs for dedup
	liveIDs := make(map[session.SessionID]bool, len(liveSessions))
	var entries []timelineEntry

	for i := range liveSessions {
		sess := &liveSessions[i]
		liveIDs[sess.ID] = true

		icon := "📋"
		if sess.Type == session.SessionTypeBuilder {
			icon = "🔨"
		}

		event := sessionStatusEvent(sess.Status)

		// Pick the most relevant timestamp
		ts := sess.CreatedAt
		if sess.Status == session.StatusRunning || sess.Status == session.StatusPending {
			if sess.StartedAt != nil {
				ts = *sess.StartedAt
			}
		} else if sess.CompletedAt != nil {
			ts = *sess.CompletedAt
		}

		entries = append(entries, timelineEntry{
			timestamp: ts,
			icon:      icon,
			event:     event,
			prompt:    sess.Prompt,
			sessionID: sess.ID,
		})
	}

	// Add history sessions (dedup by ID)
	w := m.selectedWorktree()
	currentBranch := ""
	if w != nil {
		currentBranch = w.Branch
	}
	if currentBranch != "" && m.historyBranch == currentBranch {
		for _, hist := range m.cachedHistory {
			if liveIDs[hist.ID] {
				continue
			}

			icon := "📋"
			if hist.Type == session.SessionTypeBuilder {
				icon = "🔨"
			}

			event := sessionStatusEvent(hist.Status)

			ts := hist.CreatedAt
			if hist.CompletedAt != nil {
				ts = *hist.CompletedAt
			}

			entries = append(entries, timelineEntry{
				timestamp: ts,
				icon:      icon,
				event:     event,
				prompt:    hist.Prompt,
				sessionID: hist.ID,
			})
		}
	}

	// Sort newest first
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].timestamp.After(entries[j].timestamp)
	})

	return entries
}

// sessionStatusEvent maps a session status to a human-readable event name.
func sessionStatusEvent(status session.SessionStatus) string {
	switch status {
	case session.StatusRunning:
		return "Running"
	case session.StatusIdle:
		return "Idle"
	case session.StatusPending:
		return "Pending"
	case session.StatusCompleted:
		return "Completed"
	case session.StatusFailed:
		return "Failed"
	case session.StatusStopped:
		return "Stopped"
	default:
		return string(status)
	}
}

// renderTimeline renders a list of timeline entries, capped at maxLines.
func renderTimeline(entries []timelineEntry, maxLines int) string {
	var b strings.Builder
	b.WriteString(dimStyle.Render("  Session timeline:"))
	b.WriteString("\n")

	visible := entries
	overflow := 0
	if len(visible) > maxLines {
		overflow = len(visible) - maxLines
		visible = visible[:maxLines]
	}

	for _, e := range visible {
		styledEvent := styleTimelineEvent(e.event)
		promptExcerpt := truncate(e.prompt, 40)
		b.WriteString(fmt.Sprintf("  %s  %s  %s  %s\n",
			e.icon,
			dimStyle.Render(timeAgo(e.timestamp)),
			styledEvent,
			dimStyle.Render(promptExcerpt),
		))
	}

	if overflow > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  ... %d more\n", overflow)))
	}

	return b.String()
}

// styleTimelineEvent applies color to a timeline event name.
func styleTimelineEvent(event string) string {
	switch event {
	case "Running":
		return runningStyle.Render(event)
	case "Idle":
		return idleStyle.Render(event)
	case "Pending":
		return pendingStyle.Render(event)
	case "Completed":
		return completedStyle.Render(event)
	case "Failed":
		return failedStyle.Render(event)
	case "Stopped":
		return dimStyle.Render(event)
	default:
		return event
	}
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
