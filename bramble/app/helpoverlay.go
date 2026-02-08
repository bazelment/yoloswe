package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bazelment/yoloswe/bramble/session"
)

// HelpBinding represents a single key binding entry.
type HelpBinding struct {
	Key         string // e.g. "Alt-W", "?", "Enter"
	Description string // e.g. "Open worktree dropdown"
}

// HelpSection groups related key bindings.
type HelpSection struct {
	Title    string
	Bindings []HelpBinding
}

// HelpOverlay renders a context-aware help screen.
type HelpOverlay struct {
	sections      []HelpSection
	width         int
	height        int
	scrollOffset  int
	previousFocus FocusArea // remember what was focused before opening help
}

// NewHelpOverlay creates a new help overlay.
func NewHelpOverlay() *HelpOverlay {
	return &HelpOverlay{}
}

// SetSize updates the overlay dimensions.
func (h *HelpOverlay) SetSize(w, ht int) {
	h.width = w
	h.height = ht
}

// SetSections replaces the help content.
func (h *HelpOverlay) SetSections(sections []HelpSection) {
	h.sections = sections
	h.scrollOffset = 0
}

// ScrollUp scrolls the help content up by one line.
func (h *HelpOverlay) ScrollUp() {
	if h.scrollOffset > 0 {
		h.scrollOffset--
	}
}

// ScrollDown scrolls the help content down by one line.
func (h *HelpOverlay) ScrollDown() {
	h.scrollOffset++
	// Clamped in View() based on content height
}

// View renders the help overlay as a centered full-screen box.
func (h *HelpOverlay) View() string {
	// Build all content lines
	allLines := []string{
		titleStyle.Render("Bramble Key Bindings"),
		"",
	}

	// Render each section
	for i, section := range h.sections {
		if i > 0 {
			allLines = append(allLines, "")
		}
		// Section title
		allLines = append(allLines, helpSectionTitleStyle.Render(section.Title))

		// Bindings in this section
		for _, binding := range section.Bindings {
			key := helpKeyStyle.Render(helpKeyAlignStyle.Render(binding.Key))
			allLines = append(allLines, "  "+key+"  "+binding.Description)
		}
	}

	// Footer is rendered separately so it is always visible (not scrolled away).
	footer := "\n" + dimStyle.Render("Press ? or Esc to close")

	// Apply scroll offset: clamp to valid range.
	// Box chrome (border + padding) consumes ~6 lines, footer takes 2 lines.
	visibleHeight := h.height - 8
	if visibleHeight < 5 {
		visibleHeight = len(allLines) // show everything if terminal is tiny
	}
	maxScroll := len(allLines) - visibleHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	if h.scrollOffset > maxScroll {
		h.scrollOffset = maxScroll
	}

	// Slice visible lines
	startIdx := h.scrollOffset
	endIdx := startIdx + visibleHeight
	if endIdx > len(allLines) {
		endIdx = len(allLines)
	}
	visibleLines := allLines[startIdx:endIdx]

	// Add scroll indicators
	if h.scrollOffset > 0 {
		visibleLines = append([]string{dimStyle.Render("  (scroll up for more)")}, visibleLines...)
	}
	if endIdx < len(allLines) {
		visibleLines = append(visibleLines, dimStyle.Render("  (scroll down for more)"))
	}

	contentStr := strings.Join(visibleLines, "\n") + footer

	// Calculate box dimensions
	boxWidth := h.width - 10
	if boxWidth > 72 {
		boxWidth = 72
	}
	if boxWidth < 40 {
		boxWidth = 40
	}

	// Create bordered box
	box := helpBoxStyle.
		Width(boxWidth).
		Render(contentStr)

	// Center the box
	if h.width > 0 && h.height > 0 {
		return lipgloss.Place(
			h.width, h.height,
			lipgloss.Center, lipgloss.Center,
			box,
		)
	}
	return box
}

// Styles for help overlay (package-level to avoid allocations in View)
var (
	helpSectionTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	helpKeyStyle          = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	helpKeyAlignStyle     = lipgloss.NewStyle().Width(12).Align(lipgloss.Right)
	helpBoxStyle          = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("240")).
				Padding(1, 2)
)

// buildHelpSections returns help sections appropriate for the given context.
// It reads the model state to determine which keys are relevant.
func buildHelpSections(m *Model) []HelpSection {
	inTmux := m.sessionManager.IsInTmuxMode()
	hasWorktree := m.selectedWorktree() != nil
	hasSession := m.viewingSessionID != ""
	var sessIdle, sessRunning, sessIsPlanner bool
	if sess := m.selectedSession(); sess != nil {
		sessIdle = sess.Status == session.StatusIdle
		sessRunning = sess.Status == session.StatusRunning
		sessIsPlanner = sess.Type == session.SessionTypePlanner
	}

	var sections []HelpSection

	// Always show navigation
	nav := HelpSection{Title: "Navigation"}
	nav.Bindings = append(nav.Bindings,
		HelpBinding{"Alt-W", "Open worktree selector"},
		HelpBinding{"?", "Toggle this help"},
	)
	if !inTmux {
		nav.Bindings = append(nav.Bindings,
			HelpBinding{"Alt-S", "Open session selector"},
			HelpBinding{"F2", "Toggle file tree split"},
			HelpBinding{"Tab", "Switch pane focus (when split)"},
		)
	}
	sections = append(sections, nav)

	// Session actions -- vary based on context
	sess := HelpSection{Title: "Sessions"}
	if hasWorktree {
		sess.Bindings = append(sess.Bindings,
			HelpBinding{"t", "New task (AI picks worktree)"},
			HelpBinding{"p", "Start planner session"},
			HelpBinding{"b", "Start builder session"},
		)
	}
	if !inTmux {
		sess.Bindings = append(sess.Bindings,
			HelpBinding{"1..9", "Quick switch to session N"},
		)
	}
	if hasSession && sessIdle && !inTmux {
		sess.Bindings = append(sess.Bindings,
			HelpBinding{"f", "Follow-up on idle session"},
		)
		if sessIsPlanner {
			sess.Bindings = append(sess.Bindings,
				HelpBinding{"a", "Approve plan & start builder"},
			)
		}
	}
	if hasSession && (sessRunning || sessIdle) && !inTmux {
		sess.Bindings = append(sess.Bindings,
			HelpBinding{"s", "Stop session"},
		)
	}
	if len(sess.Bindings) > 0 {
		sections = append(sections, sess)
	}

	// Worktree actions
	wt := HelpSection{Title: "Worktrees"}
	wt.Bindings = append(wt.Bindings,
		HelpBinding{"n", "Create new worktree"},
	)
	if hasWorktree {
		wt.Bindings = append(wt.Bindings,
			HelpBinding{"d", "Delete worktree"},
			HelpBinding{"e", "Open in editor"},
		)
	}
	wt.Bindings = append(wt.Bindings,
		HelpBinding{"r", "Refresh worktrees"},
	)
	sections = append(sections, wt)

	// Output scrolling (non-tmux only for scroll; tmux has navigate)
	if inTmux {
		tmux := HelpSection{Title: "Session List"}
		tmux.Bindings = append(tmux.Bindings,
			HelpBinding{"Up/k", "Navigate up"},
			HelpBinding{"Down/j", "Navigate down"},
			HelpBinding{"1..9", "Select session N in list"},
			HelpBinding{"Enter", "Switch to tmux window"},
		)
		sections = append(sections, tmux)
	} else {
		out := HelpSection{Title: "Output"}
		out.Bindings = append(out.Bindings,
			HelpBinding{"Up/k", "Scroll up"},
			HelpBinding{"Down/j", "Scroll down"},
			HelpBinding{"PgUp", "Scroll up 10 lines"},
			HelpBinding{"PgDn", "Scroll down 10 lines"},
			HelpBinding{"Home", "Scroll to top"},
			HelpBinding{"End", "Scroll to bottom"},
		)
		if m.splitPane.IsSplit() {
			out.Bindings = append(out.Bindings,
				HelpBinding{"Enter", "Open file in editor (file tree)"},
			)
		}
		sections = append(sections, out)
	}

	// Dropdown mode bindings (shown if user opened help from dropdown)
	if m.helpOverlay.previousFocus == FocusWorktreeDropdown ||
		m.helpOverlay.previousFocus == FocusSessionDropdown {
		dd := HelpSection{Title: "Dropdown"}
		dd.Bindings = append(dd.Bindings,
			HelpBinding{"Up/k", "Move selection up"},
			HelpBinding{"Down/j", "Move selection down"},
			HelpBinding{"Enter", "Confirm selection"},
			HelpBinding{"Esc", "Close dropdown"},
		)
		sections = append(sections, dd)
	}

	// Input mode bindings (shown if user opened help from input)
	if m.helpOverlay.previousFocus == FocusInput {
		inp := HelpSection{Title: "Input Mode"}
		inp.Bindings = append(inp.Bindings,
			HelpBinding{"Tab", "Cycle focus (text/send/cancel)"},
			HelpBinding{"Enter", "Submit prompt (non-empty)"},
			HelpBinding{"Shift+Enter", "Insert newline"},
			HelpBinding{"Ctrl+Enter", "Submit (alternative)"},
			HelpBinding{"Esc", "Cancel input"},
		)
		sections = append(sections, inp)
	}

	// General
	gen := HelpSection{Title: "General"}
	gen.Bindings = append(gen.Bindings,
		HelpBinding{"Esc", "Clear error / close overlay"},
		HelpBinding{"q", "Quit Bramble"},
		HelpBinding{"Ctrl-C", "Force quit"},
	)
	sections = append(sections, gen)

	return sections
}
