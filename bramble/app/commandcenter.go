package app

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/sessionmodel"
)

// CommandCenter provides a full-screen card-based grid view of all sessions.
type CommandCenter struct {
	sessions    []session.SessionInfo
	previewText []string // captured pane text lines for expanded preview
	selectedIdx int
	width       int
	height      int
	scrollY     int // scroll offset in card-rows
	previewIdx  int // index of session with expanded pane preview, -1 if none
	visible     bool
}

// NewCommandCenter creates a new command center.
func NewCommandCenter() *CommandCenter {
	return &CommandCenter{previewIdx: -1}
}

// Show populates the command center with sessions, sorts by priority, and makes it visible.
func (cc *CommandCenter) Show(sessions []session.SessionInfo, w, h int) {
	cc.sessions = make([]session.SessionInfo, len(sessions))
	copy(cc.sessions, sessions)
	sortSessionsByPriority(cc.sessions)
	cc.width = w
	cc.height = h
	cc.visible = true
	cc.scrollY = 0
	cc.previewIdx = -1
	cc.previewText = nil
	if cc.selectedIdx >= len(cc.sessions) {
		cc.selectedIdx = 0
	}
	cc.clampScrollY()
}

// Hide closes the command center.
func (cc *CommandCenter) Hide() {
	cc.visible = false
}

// IsVisible returns whether the command center is visible.
func (cc *CommandCenter) IsVisible() bool {
	return cc.visible
}

// SetSize updates the command center dimensions.
func (cc *CommandCenter) SetSize(w, h int) {
	cc.width = w
	cc.height = h
	cc.clampScrollY()
}

// totalCardAndPreviewRows returns the total number of display rows,
// including the preview row if a preview is open.
func (cc *CommandCenter) totalCardAndPreviewRows() int {
	cols := cc.gridColumns()
	rows := (len(cc.sessions) + cols - 1) / cols
	if cc.previewIdx >= 0 {
		rows++ // preview row is inserted after the card row containing the previewed session
	}
	return rows
}

// clampScrollY ensures scrollY is within valid bounds for the current sessions/dimensions.
// Must be called only from update-path methods, not from View().
func (cc *CommandCenter) clampScrollY() {
	if len(cc.sessions) == 0 {
		cc.scrollY = 0
		return
	}
	totalRows := cc.totalCardAndPreviewRows()
	visibleRows := cc.visibleRows()
	if cc.scrollY > totalRows-visibleRows {
		cc.scrollY = totalRows - visibleRows
	}
	if cc.scrollY < 0 {
		cc.scrollY = 0
	}
}

// MoveSelection moves selection horizontally by delta (±1).
func (cc *CommandCenter) MoveSelection(delta int) {
	if len(cc.sessions) == 0 {
		return
	}
	cc.selectedIdx = clamp(cc.selectedIdx+delta, 0, len(cc.sessions)-1)
	cc.ensureSelectedVisible()
}

// MoveSelectionRow moves selection vertically by one row (±columns).
func (cc *CommandCenter) MoveSelectionRow(delta int) {
	if len(cc.sessions) == 0 {
		return
	}
	cols := cc.gridColumns()
	cc.selectedIdx = clamp(cc.selectedIdx+delta*cols, 0, len(cc.sessions)-1)
	cc.ensureSelectedVisible()
}

// SelectByNumber selects session by 1-based number. Returns false if out of range.
func (cc *CommandCenter) SelectByNumber(n int) bool {
	idx := n - 1
	if idx < 0 || idx >= len(cc.sessions) {
		return false
	}
	cc.selectedIdx = idx
	cc.ensureSelectedVisible()
	return true
}

// SelectedSession returns the currently selected session, or nil.
func (cc *CommandCenter) SelectedSession() *session.SessionInfo {
	if len(cc.sessions) == 0 || cc.selectedIdx >= len(cc.sessions) {
		return nil
	}
	return &cc.sessions[cc.selectedIdx]
}

// Sessions returns the current session list.
func (cc *CommandCenter) Sessions() []session.SessionInfo {
	return cc.sessions
}

// RestoreSelectionByID finds a session by ID and restores selection to it.
func (cc *CommandCenter) RestoreSelectionByID(id session.SessionID) {
	for i := range cc.sessions {
		if cc.sessions[i].ID == id {
			cc.selectedIdx = i
			cc.ensureSelectedVisible()
			return
		}
	}
	// ID not found — clamp to valid range
	if cc.selectedIdx >= len(cc.sessions) {
		cc.selectedIdx = len(cc.sessions) - 1
	}
	if cc.selectedIdx < 0 {
		cc.selectedIdx = 0
	}
}

// TogglePreview toggles the expanded pane preview for the selected session.
// Returns the selected session if preview is being opened, nil if being closed.
func (cc *CommandCenter) TogglePreview() *session.SessionInfo {
	if cc.previewIdx == cc.selectedIdx {
		// Close preview
		cc.previewIdx = -1
		cc.previewText = nil
		return nil
	}
	cc.previewIdx = cc.selectedIdx
	cc.previewText = nil // will be populated by SetPreviewText
	return cc.SelectedSession()
}

// SetPreviewText sets the captured pane text lines for the preview.
func (cc *CommandCenter) SetPreviewText(lines []string) {
	cc.previewText = lines
}

// gridColumns returns the responsive column count based on width.
func (cc *CommandCenter) gridColumns() int {
	if cc.width >= 160 {
		return 3
	}
	if cc.width >= 80 {
		return 2
	}
	return 1
}

// ensureSelectedVisible auto-scrolls to keep the selected card's row visible.
func (cc *CommandCenter) ensureSelectedVisible() {
	cols := cc.gridColumns()
	selectedCardRow := cc.selectedIdx / cols

	// Convert card-row index to display-row index, accounting for the preview
	// row that is inserted after the card row containing the previewed session.
	selectedDisplayRow := selectedCardRow
	if cc.previewIdx >= 0 {
		previewAfterCardRow := cc.previewIdx / cols
		if selectedCardRow > previewAfterCardRow {
			selectedDisplayRow++ // selected card is below the preview row
		}
	}

	if selectedDisplayRow < cc.scrollY {
		cc.scrollY = selectedDisplayRow
	}
	visibleRows := cc.visibleRows()
	if selectedDisplayRow >= cc.scrollY+visibleRows {
		cc.scrollY = selectedDisplayRow - visibleRows + 1
	}
	cc.clampScrollY()
}

// previewRowHeight returns the rendered height of the preview row in terminal lines.
// The preview box has: top border (1) + title line (1) + up to previewMaxLines content
// lines + bottom border (1) = previewMaxLines + 3.
func (cc *CommandCenter) previewRowHeight() int {
	const previewMaxLines = 10
	n := len(cc.previewText)
	if n == 0 {
		n = 1 // "Capturing pane..." or unavailable message
	}
	if n > previewMaxLines {
		n = previewMaxLines
	}
	return n + 3 // top border + title + content lines + bottom border
}

// visibleRows returns how many card rows fit in the viewport.
// When a preview is open, its actual height is subtracted from the available
// space before dividing by cardHeight, so the visible content never overflows.
func (cc *CommandCenter) visibleRows() int {
	// Header: 3 lines (title + summary + blank), Footer: 2 lines (blank + keys)
	// Scroll indicators: up to 2 lines (one scrollUp + one scrollDown) rendered conditionally
	// in View(). Reserve the full 2 lines here so that activating scroll never pushes content
	// past the terminal height.
	contentHeight := cc.height - 7
	cardHeight := 8 // 6 content lines + 2 border lines

	// When the preview is open, its row is taller than a card row. Subtract the
	// extra height so that scroll calculations don't overcount available space.
	if cc.previewIdx >= 0 {
		contentHeight -= cc.previewRowHeight()
	}

	if contentHeight <= 0 {
		return 1
	}
	rows := contentHeight / cardHeight
	if cc.previewIdx >= 0 {
		rows++ // add back 1 slot for the preview row (already accounted for by height subtraction)
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// View renders the full-screen command center.
func (cc *CommandCenter) View(s *Styles) string {
	if len(cc.sessions) == 0 {
		box := s.AllSessionsBox.Width(cc.width - 10).Render(
			s.Title.Render("Command Center") + "\n\n" +
				s.Dim.Render("No active sessions.") + "\n\n" +
				s.Dim.Render("[Esc] Close"),
		)
		return lipgloss.Place(cc.width, cc.height, lipgloss.Center, lipgloss.Center, box)
	}

	cols := cc.gridColumns()
	cardWidth := (cc.width - 2) / cols // leave margin

	// Header
	header := cc.renderHeader(s)

	// Build card rows
	var allRows []string
	for i := 0; i < len(cc.sessions); i += cols {
		var rowCards []string
		for j := 0; j < cols && i+j < len(cc.sessions); j++ {
			idx := i + j
			card := renderSessionCard(&cc.sessions[idx], cardWidth, idx, idx == cc.selectedIdx, s)
			rowCards = append(rowCards, card)
		}
		allRows = append(allRows, lipgloss.JoinHorizontal(lipgloss.Top, rowCards...))

		// If the preview is open and the previewed card is in this row,
		// insert an expanded preview row right after the card row.
		if cc.previewIdx >= i && cc.previewIdx < i+cols {
			preview := cc.renderPreviewRow(s)
			if preview != "" {
				allRows = append(allRows, preview)
			}
		}
	}

	// Apply vertical scrolling — use local variables only; View() must not mutate state.
	totalRows := len(allRows)
	visibleRows := cc.visibleRows()
	scrollY := cc.scrollY
	if scrollY > totalRows-visibleRows {
		scrollY = totalRows - visibleRows
	}
	if scrollY < 0 {
		scrollY = 0
	}
	endRow := scrollY + visibleRows
	if endRow > totalRows {
		endRow = totalRows
	}
	visibleCardRows := allRows[scrollY:endRow]

	// Scroll indicators
	var scrollUp, scrollDown string
	if scrollY > 0 {
		scrollUp = s.Dim.Render("  ▲ scroll up")
	}
	if endRow < totalRows {
		scrollDown = s.Dim.Render("  ▼ scroll down")
	}

	// Footer
	footer := cc.renderFooter(s)

	// Assemble
	var parts []string
	parts = append(parts, header)
	if scrollUp != "" {
		parts = append(parts, scrollUp)
	}
	parts = append(parts, visibleCardRows...)
	if scrollDown != "" {
		parts = append(parts, scrollDown)
	}
	parts = append(parts, footer)

	content := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Fill screen
	return lipgloss.Place(cc.width, cc.height, lipgloss.Center, lipgloss.Top, content)
}

// renderHeader renders the command center header with summary counts.
func (cc *CommandCenter) renderHeader(s *Styles) string {
	var running, idle, pending, terminal int
	for i := range cc.sessions {
		switch cc.sessions[i].Status {
		case session.StatusRunning:
			running++
		case session.StatusIdle:
			idle++
		case session.StatusPending:
			pending++
		default:
			terminal++
		}
	}

	var summaryParts []string
	if running > 0 {
		summaryParts = append(summaryParts, s.Running.Render(fmt.Sprintf("%d running", running)))
	}
	if idle > 0 {
		summaryParts = append(summaryParts, s.Idle.Render(fmt.Sprintf("%d idle (action needed)", idle)))
	}
	if pending > 0 {
		summaryParts = append(summaryParts, s.Pending.Render(fmt.Sprintf("%d pending", pending)))
	}
	if terminal > 0 {
		summaryParts = append(summaryParts, s.Dim.Render(fmt.Sprintf("%d terminal", terminal)))
	}
	summary := strings.Join(summaryParts, ", ")

	title := s.Title.Render("Command Center")
	return title + "\n" + summary + "\n"
}

// renderFooter renders the footer keybinding hints.
func (cc *CommandCenter) renderFooter(s *Styles) string {
	previewKey := "[p] Preview pane"
	if cc.previewIdx >= 0 {
		previewKey = "[p] Close preview"
	}
	keys := []string{
		"[←/→/↑/↓] Navigate",
		"[Enter] Jump in",
		"[1-9] Quick select",
		previewKey,
		"[f] Follow-up",
		"[a] Approve plan",
		"[Esc] Close",
	}
	return "\n" + s.Dim.Render(strings.Join(keys, "  "))
}

// renderPreviewRow renders the expanded pane preview below the card row.
func (cc *CommandCenter) renderPreviewRow(s *Styles) string {
	if cc.previewIdx < 0 || cc.previewIdx >= len(cc.sessions) {
		return ""
	}

	sess := &cc.sessions[cc.previewIdx]
	previewWidth := cc.width - 6

	var content string
	if len(cc.previewText) == 0 {
		if sess.RunnerType != session.RunnerTypeTmux && sess.RunnerType != session.RunnerTypeTmuxTracked {
			content = s.Dim.Render("Preview is only available for tmux sessions")
		} else {
			content = s.Dim.Render("Capturing pane...")
		}
	} else {
		const previewMaxLines = 10
		text := cc.previewText
		if len(text) > previewMaxLines {
			text = text[len(text)-previewMaxLines:]
		}
		var lines []string
		for _, line := range text {
			lines = append(lines, truncate(line, previewWidth-4))
		}
		content = s.Dim.Render(strings.Join(lines, "\n"))
	}

	previewStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(s.Palette.Accent)).
		Width(previewWidth).
		Padding(0, 1)

	title := s.Title.Render(fmt.Sprintf("Pane Preview — %s", sess.TmuxWindowName))
	return previewStyle.Render(title + "\n" + content)
}

// sessionPriority returns a sort priority for a session (lower = higher priority).
func sessionPriority(sess *session.SessionInfo) int {
	switch sess.Status {
	case session.StatusIdle:
		return 0 // needs action — highest priority
	case session.StatusRunning:
		return 1
	case session.StatusPending:
		return 2
	default:
		return 3 // terminal states
	}
}

// sortSessionsByPriority sorts sessions by priority tier, then LastActivity descending.
func sortSessionsByPriority(sessions []session.SessionInfo) {
	sort.SliceStable(sessions, func(i, j int) bool {
		pi := sessionPriority(&sessions[i])
		pj := sessionPriority(&sessions[j])
		if pi != pj {
			return pi < pj
		}
		return sessions[i].Progress.LastActivity.After(sessions[j].Progress.LastActivity)
	})
}

// renderSessionCard renders a single session card.
func renderSessionCard(sess *session.SessionInfo, cardWidth, idx int, selected bool, s *Styles) string {
	// styleWidth is passed to lipgloss Width(). With Padding(0, 1), lipgloss wraps text at
	// styleWidth - 2 (subtracting left+right padding). We use styleWidth = cardWidth - 2
	// (border only) so the rendered card occupies cardWidth columns. For text truncation we
	// use innerWidth = styleWidth - 2 (= cardWidth - 4) to match the actual character area.
	styleWidth := cardWidth - 2  // border left + border right
	innerWidth := styleWidth - 2 // left padding + right padding (Padding(0, 1))

	// Line 1: Number + type icon + type label + status + repo/model context
	typeIcon := "[P]"
	typeLabel := "planner"
	if sess.Type == session.SessionTypeBuilder {
		typeIcon = "[B]"
		typeLabel = "builder"
	}
	statusStr := statusText(sess.Status)
	line1Left := fmt.Sprintf("%d. %s %s  %s %s",
		idx+1, typeIcon, typeLabel,
		statusIconPlain(sess.Status), statusStr)

	// Append repo/model context to the right of status
	var context string
	if sess.WorktreeName != "" {
		context = sess.WorktreeName
	} else if sess.RepoName != "" {
		context = sess.RepoName
	}
	if sess.Model != "" {
		if context != "" {
			context += " [" + sess.Model + "]"
		} else {
			context = "[" + sess.Model + "]"
		}
	}
	if context != "" {
		line1Left += "  " + context
	}
	line1 := truncate(line1Left, innerWidth)

	// Line 2: User prompt with > prefix (dim)
	prompt := sess.Prompt
	if prompt == "" && sess.Title != "" {
		prompt = sess.Title
	}
	var line2 string
	if prompt != "" {
		line2 = s.Dim.Render(truncate("> "+prompt, innerWidth))
	} else {
		line2 = s.Dim.Render(truncate("-", innerWidth))
	}

	// Line 3: Current activity — truncate plain text before applying ANSI styles.
	var line3 string
	switch {
	case sess.Status == session.StatusRunning && sess.Progress.CurrentTool != "":
		line3 = truncate(fmt.Sprintf("[%s]", sess.Progress.CurrentTool), innerWidth)
	case sess.Status == session.StatusIdle && sess.Type == session.SessionTypePlanner && sess.PlanFilePath != "":
		line3 = s.Idle.Render(truncate("PLAN READY", innerWidth))
	case sess.Status == session.StatusIdle:
		line3 = s.Idle.Render(truncate("AWAITING FOLLOW-UP", innerWidth))
	case sess.Status == session.StatusRunning && sess.Progress.CurrentPhase != "":
		line3 = truncate(sess.Progress.CurrentPhase, innerWidth)
	default:
		line3 = s.Dim.Render(truncate("-", innerWidth))
	}

	// Lines 4-6: Recent agent output (dim)
	outputLines := make([]string, sessionmodel.RecentOutputDisplayLines)
	for i := range outputLines {
		if i < len(sess.Progress.RecentOutput) {
			outputLines[i] = s.Dim.Render(truncate(sess.Progress.RecentOutput[i], innerWidth))
		} else {
			outputLines[i] = s.Dim.Render(truncate("-", innerWidth))
		}
	}

	content := strings.Join(append([]string{line1, line2, line3}, outputLines...), "\n")

	// Card border
	borderColor := cardBorderColor(sess, s.Palette)
	if selected {
		borderColor = lipgloss.Color(s.Palette.Accent)
	}

	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(styleWidth).
		Padding(0, 1)

	return cardStyle.Render(content)
}

// cardBorderColor returns the border color for a session card based on status.
func cardBorderColor(sess *session.SessionInfo, palette ColorPalette) color.Color {
	switch sess.Status {
	case session.StatusIdle:
		return lipgloss.Color(palette.Idle)
	case session.StatusRunning:
		return lipgloss.Color(palette.Running)
	case session.StatusPending:
		return lipgloss.Color(palette.Pending)
	default:
		return lipgloss.Color(palette.Dim)
	}
}

// statusText returns a human-readable status string.
func statusText(status session.SessionStatus) string {
	switch status {
	case session.StatusPending:
		return "Pending"
	case session.StatusRunning:
		return "Running"
	case session.StatusIdle:
		return "Idle"
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

// statusIconPlain returns a plain (unstyled) status icon character.
func statusIconPlain(status session.SessionStatus) string {
	switch status {
	case session.StatusPending:
		return "○"
	case session.StatusRunning:
		return "●"
	case session.StatusIdle:
		return "◐"
	case session.StatusCompleted:
		return "✓"
	case session.StatusFailed:
		return "✗"
	case session.StatusStopped:
		return "◌"
	default:
		return "?"
	}
}
