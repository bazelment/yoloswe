package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

// renderWelcome renders the welcome/empty state for the center area.
// It adapts based on whether worktrees exist and what state they're in.
func (m Model) renderWelcome(width, height int) string {
	var b strings.Builder

	hasWorktrees := len(m.worktrees) > 0
	wt := m.selectedWorktree()
	inTmux := m.sessionManager.IsInTmuxMode()

	b.WriteString("\n")

	if !hasWorktrees {
		// Variant 1: No worktrees at all
		b.WriteString(m.styles.WelcomeTitle.Render("  Welcome to Bramble"))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Dim.Render("  No worktrees found for " + m.repoName))
		b.WriteString("\n\n")
		b.WriteString("  Get started:\n\n")
		b.WriteString(renderKeyHint("t", "New task", "Describe what you want; AI picks the branch", m.styles))
		b.WriteString(renderKeyHint("n", "New worktree", "Create a branch manually", m.styles))
		b.WriteString(renderKeyHint("Alt-W", "Worktrees", "Browse and select worktrees", m.styles))
		b.WriteString(renderKeyHint("?", "Help", "Show all keyboard shortcuts", m.styles))
		b.WriteString("\n")
	} else {
		// Variant 2: Worktrees exist, no session selected
		b.WriteString(m.styles.WelcomeTitle.Render("  Bramble"))
		b.WriteString("\n\n")
		b.WriteString("  Quick start:\n\n")
		b.WriteString(renderKeyHint("t", "New task", "Describe what you want; AI picks the worktree", m.styles))
		b.WriteString(renderKeyHint("p", "Plan", "Start a planning session on current worktree", m.styles))
		b.WriteString(renderKeyHint("b", "Build", "Start a builder session on current worktree", m.styles))
		if !inTmux {
			b.WriteString(renderKeyHint("Alt-S", "Sessions", "Browse and switch sessions", m.styles))
		}
		b.WriteString(renderKeyHint("?", "Help", "Show all keyboard shortcuts", m.styles))
		b.WriteString("\n")

		// Current worktree summary
		if wt != nil {
			b.WriteString(renderWorktreeSummary(m, wt, m.styles))
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
			b.WriteString(renderTimeline(timeline, maxTimelineLines, m.styles))
		}
	}

	// Show worktree operation messages if any (e.g. "Creating worktree...")
	if len(m.worktreeOpMessages) > 0 {
		b.WriteString("\n")
		for _, msg := range m.worktreeOpMessages {
			b.WriteString("  ")
			b.WriteString(msg)
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderKeyHint renders a single key hint line for the welcome screen.
func renderKeyHint(key, action, description string, s *Styles) string {
	// Fixed-width columns for alignment:
	//   "    [t]  New task         Describe what you want; AI picks the branch"
	keyCol := fmt.Sprintf("  %s", s.WelcomeKey.Render(fmt.Sprintf("[%s]", key)))
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
		s.WelcomeDesc.Render(actionPadded),
		s.Dim.Render(description),
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

		icon := sessionTypeIcon(sess.Type)
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

	// Add history sessions (dedup by ID).
	// Compare by directory name (w.Name()) since historyBranch stores the
	// directory name, not the git branch — this survives branch checkouts.
	w := m.selectedWorktree()
	currentName := ""
	if w != nil {
		currentName = w.Name()
	}
	if currentName != "" && m.historyBranch == currentName {
		for _, hist := range m.cachedHistory {
			if liveIDs[hist.ID] {
				continue
			}

			icon := sessionTypeIcon(hist.Type)
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

// sessionTypeIcon returns a fixed-width ASCII icon for a session type.
// Uses ASCII glyphs instead of emoji to avoid inconsistent terminal widths.
func sessionTypeIcon(t session.SessionType) string {
	if t == session.SessionTypeBuilder {
		return "[B]"
	}
	return "[P]"
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
func renderTimeline(entries []timelineEntry, maxLines int, s *Styles) string {
	var b strings.Builder
	b.WriteString(s.Dim.Render("  Session timeline:"))
	b.WriteString("\n")

	visible := entries
	overflow := 0
	if len(visible) > maxLines {
		overflow = len(visible) - maxLines
		visible = visible[:maxLines]
	}

	for _, e := range visible {
		styledEvent := styleTimelineEvent(e.event, s)
		// Pad event column to fixed visual width (9 = len("Completed"))
		// so columns align regardless of status string length.
		eventVisual := runewidth.StringWidth(stripAnsi(styledEvent))
		eventPad := 9 - eventVisual
		if eventPad > 0 {
			styledEvent += strings.Repeat(" ", eventPad)
		}
		promptExcerpt := truncate(e.prompt, 40)
		b.WriteString(fmt.Sprintf("  %s  %s  %s  %s\n",
			e.icon,
			s.Dim.Render(timeAgo(e.timestamp)),
			styledEvent,
			s.Dim.Render(promptExcerpt),
		))
	}

	if overflow > 0 {
		b.WriteString(s.Dim.Render(fmt.Sprintf("  ... %d more\n", overflow)))
	}

	return b.String()
}

// styleTimelineEvent applies color to a timeline event name.
func styleTimelineEvent(event string, s *Styles) string {
	switch event {
	case "Running":
		return s.Running.Render(event)
	case "Idle":
		return s.Idle.Render(event)
	case "Pending":
		return s.Pending.Render(event)
	case "Completed":
		return s.Completed.Render(event)
	case "Failed":
		return s.Failed.Render(event)
	case "Stopped":
		return s.Dim.Render(event)
	default:
		return event
	}
}

// renderWorktreeSummary renders a summary of the current worktree.
func renderWorktreeSummary(m Model, w *wt.Worktree, s *Styles) string {
	var b strings.Builder
	b.WriteString(s.Dim.Render("  Current worktree: "))
	b.WriteString(s.Title.Render(w.Branch))

	// Add status details if available
	if m.worktreeStatuses != nil {
		if status, ok := m.worktreeStatuses[w.Branch]; ok {
			var details []string
			if status.IsDirty {
				details = append(details, s.Failed.Render("dirty"))
			} else {
				details = append(details, s.Completed.Render("clean"))
			}
			if status.Ahead > 0 {
				details = append(details, s.Running.Render(fmt.Sprintf("↑%d ahead", status.Ahead)))
			}
			if status.Behind > 0 {
				details = append(details, s.Pending.Render(fmt.Sprintf("↓%d behind", status.Behind)))
			}
			if status.PRNumber > 0 {
				prText := fmt.Sprintf("PR#%d %s", status.PRNumber, status.PRState)
				details = append(details, s.Dim.Render(prText))
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
		b.WriteString(s.Dim.Render(fmt.Sprintf(" -- %d files changed", m.fileTree.FileCount())))
	}

	b.WriteString("\n")
	return b.String()
}
