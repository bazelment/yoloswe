package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/bazelment/yoloswe/bramble/session"
)

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12"))

	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("240")).
			Foreground(lipgloss.Color("15"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("242"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	topBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("252")).
			Padding(0, 1)

	statusBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("242"))

	runningStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10"))

	idleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("14"))

	pendingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("11"))

	completedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	failedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("9"))

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			BorderLeft(false).
			BorderRight(false)
)

// View renders the model.
func (m Model) View() string {
	// Use sensible defaults before WindowSizeMsg arrives so the first
	// render shows the real UI instead of a blank "Loading..." screen.
	if m.width == 0 {
		m.width = 80
	}
	if m.height == 0 {
		m.height = 24
	}

	// Layout: top bar (1 line) + center + toast area (dynamic) + input area (dynamic) + status bar (1 line)
	topBarHeight := 1
	statusBarHeight := 1
	toastHeight := m.toasts.Height()
	inputHeight := 0
	if m.inputMode {
		// Dynamic input height based on content (min 5, max 12 lines including border and status)
		lineCount := m.inputArea.LineCount()
		inputHeight = lineCount + 4 // +4 for prompt, status line, and borders
		if inputHeight < 5 {
			inputHeight = 5
		}
		maxInputHeight := m.height * 40 / 100 // 40% of screen max
		if maxInputHeight < 8 {
			maxInputHeight = 8
		}
		if inputHeight > maxInputHeight {
			inputHeight = maxInputHeight
		}
	}
	centerHeight := m.height - topBarHeight - statusBarHeight - toastHeight - inputHeight - 2 // borders

	// Build components
	topBar := m.renderTopBar()
	center := m.renderCenter(m.width, centerHeight)
	statusBar := m.renderStatusBar()

	// Add border to center
	centerBordered := borderStyle.Width(m.width).Height(centerHeight).Render(center)

	// Build layout
	parts := []string{topBar, centerBordered}

	// Add toast notifications if any
	if m.toasts.HasToasts() {
		m.toasts.SetWidth(m.width)
		parts = append(parts, m.toasts.View())
	}

	// Add input area if in input mode
	if m.inputMode {
		m.inputArea.SetWidth(m.width - 4)
		m.inputArea.SetMaxHeight(inputHeight - 2)
		m.inputArea.SetPrompt(m.inputPrompt)
		parts = append(parts, m.inputArea.View())
	}

	parts = append(parts, statusBar)

	// Overlay dropdowns if open
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Show dropdown overlay if open
	if m.focus == FocusWorktreeDropdown && m.worktreeDropdown.IsOpen() {
		overlay := m.worktreeDropdown.ViewOverlay()
		// Position overlay below the top bar
		content = overlayAt(content, overlay, 2, 1)
	}
	if m.focus == FocusSessionDropdown && m.sessionDropdown.IsOpen() {
		overlay := m.sessionDropdown.ViewOverlay()
		// Right-align the session dropdown overlay
		dropdownWidth := m.sessionDropdown.Width()
		overlayX := m.width - dropdownWidth - 4
		if overlayX < 0 {
			overlayX = 0
		}
		content = overlayAt(content, overlay, overlayX, 1)
	}

	// Show help overlay if active
	if m.focus == FocusHelp {
		return m.helpOverlay.View()
	}

	// Show task modal if visible
	if m.taskModal.IsVisible() {
		return m.taskModal.View()
	}

	return content
}

// renderTopBar renders the top bar with repo, worktree dropdown, and session dropdown.
func (m Model) renderTopBar() string {
	// Left side: repo name + worktree dropdown
	left := dimStyle.Render(m.repoName)

	// Worktree dropdown header
	left += "  "
	if m.focus == FocusWorktreeDropdown {
		left += selectedStyle.Render(m.worktreeDropdown.ViewHeader())
	} else {
		left += m.worktreeDropdown.ViewHeader()
	}
	left += "  " + dimStyle.Render("[Alt-W]")

	// Right side: session info (different for tmux vs TUI mode)
	right := ""
	if m.sessionManager.IsInTmuxMode() {
		// Tmux mode: show worktree path with ~ for home
		if wt := m.selectedWorktree(); wt != nil {
			path := wt.Path
			// Replace home directory with ~
			if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home) {
				path = "~" + strings.TrimPrefix(path, home)
			}
			right = dimStyle.Render(path)
		}
	} else {
		// TUI mode: show current session + session dropdown
		if sess := m.selectedSession(); sess != nil {
			icon := "📋"
			if sess.Type == session.SessionTypeBuilder {
				icon = "🔨"
			}
			title := sess.Title
			if title == "" {
				title = string(sess.ID)[:12]
			}
			right = fmt.Sprintf("%s %s %s", icon, title, statusIcon(sess.Status))
		} else {
			right = dimStyle.Render("(no session)")
		}

		// Session dropdown trigger
		if m.focus == FocusSessionDropdown {
			right = selectedStyle.Render(right + " ▼")
		} else {
			right += " " + dimStyle.Render("▼")
		}
		right += "  " + dimStyle.Render("[Alt-S]")
	}

	// Combine with padding
	padding := m.width - len(stripAnsi(left)) - len(stripAnsi(right)) - 4
	if padding < 1 {
		padding = 1
	}

	bar := left + strings.Repeat(" ", padding) + right
	return topBarStyle.Width(m.width).Render(bar)
}

// renderSessionListView renders the session list for tmux mode.
func (m Model) renderSessionListView(width, height int) string {
	var b strings.Builder

	// Table header
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(dimStyle.Render("Type    Name            Status        Prompt"))
	b.WriteString("\n")
	b.WriteString("  ")
	b.WriteString(strings.Repeat("─", width-4))
	b.WriteString("\n")

	// Get sessions for current worktree
	var currentSessions []session.SessionInfo
	if wt := m.selectedWorktree(); wt != nil {
		allSessions := m.sessionManager.GetAllSessions()
		for i := range allSessions {
			if allSessions[i].WorktreePath == wt.Path {
				currentSessions = append(currentSessions, allSessions[i])
			}
		}
	}

	if len(currentSessions) == 0 {
		return m.renderWelcome(width, height)
	}

	// Ensure selected index is in bounds
	if m.selectedSessionIndex >= len(currentSessions) {
		m.selectedSessionIndex = len(currentSessions) - 1
	}
	if m.selectedSessionIndex < 0 {
		m.selectedSessionIndex = 0
	}

	// Render sessions
	for i := range currentSessions {
		sess := &currentSessions[i]
		typeIcon := "📋"
		if sess.Type == session.SessionTypeBuilder {
			typeIcon = "🔨"
		}

		// Session name (tmux window name or ID)
		nameDisplay := sess.TmuxWindowName
		if nameDisplay == "" {
			nameDisplay = string(sess.ID)[:minInt(15, len(sess.ID))]
		}
		nameDisplay = truncate(nameDisplay, 15)

		// Status with icon
		statusStr := fmt.Sprintf("%s %-8s", statusIcon(sess.Status), sess.Status)

		// Prompt gets remaining width (more space) - strip quotes if present
		prompt := sess.Prompt
		if prompt != "" && prompt[0] == '"' {
			prompt = strings.Trim(prompt, `"`)
		}
		promptDisplay := truncate(prompt, 80)

		// Format line: icon + name + status + prompt
		line := fmt.Sprintf("  %s  %-15s  %-13s  %s", typeIcon, nameDisplay, statusStr, promptDisplay)

		// Highlight selected row
		if i == m.selectedSessionIndex {
			line = selectedStyle.Render(line)
		}

		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}

// minInt returns the minimum of two integers.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// renderCenter renders the main center area (session output + input).
func (m Model) renderCenter(width, height int) string {
	// In tmux mode, always show session list (no split pane)
	if m.sessionManager.IsInTmuxMode() {
		return m.renderSessionListView(width, height)
	}

	// If split pane is active, render file tree on left, output on right
	if m.splitPane.IsSplit() {
		m.fileTree.SetFocused(m.splitPane.FocusLeft())
		rightWidth := m.splitPane.RightWidth(width)
		leftContent := m.fileTree.Render(m.splitPane.LeftWidth(width), height)
		rightContent := m.renderOutputArea(rightWidth, height)
		return m.splitPane.Render(leftContent, rightContent, width, height)
	}

	return m.renderOutputArea(width, height)
}

// renderOutputArea renders the session output content (used by renderCenter).
func (m Model) renderOutputArea(width, height int) string {
	var b strings.Builder

	if m.viewingSessionID == "" {
		return m.renderWelcome(width, height)
	}

	// Check if viewing history session
	if m.viewingHistoryData != nil {
		return m.renderHistorySession(width, height)
	}

	// Get session info
	info, ok := m.sessionManager.GetSessionInfo(m.viewingSessionID)
	if !ok {
		b.WriteString(errorStyle.Render("  Session not found"))
		return b.String()
	}

	// Session header
	typeIcon := "📋"
	if info.Type == session.SessionTypeBuilder {
		typeIcon = "🔨"
	}
	title := info.Title
	if title == "" {
		title = string(info.ID)
	}
	headerLine := fmt.Sprintf("  %s %s  %s  %s", typeIcon, info.Type, title, statusIcon(info.Status))
	if info.Model != "" {
		headerLine += "  " + dimStyle.Render("["+info.Model+"]")
	}
	if info.Progress.TurnCount > 0 || info.Progress.TotalCostUSD > 0 {
		headerLine += "  " + dimStyle.Render(fmt.Sprintf("T:%d $%.4f", info.Progress.TurnCount, info.Progress.TotalCostUSD))
	}
	// Add idle indicator with follow-up hint
	if info.Status == session.StatusIdle {
		if info.Type == session.SessionTypePlanner {
			headerLine += idleStyle.Render("  (plan ready - 'a' approve & build / 'f' iterate)")
		} else {
			headerLine += idleStyle.Render("  (awaiting follow-up - press 'f')")
		}
	}
	b.WriteString(headerLine)
	b.WriteString("\n")

	// Prompt
	promptLine := fmt.Sprintf("  %q", truncate(info.Prompt, width-8))
	b.WriteString(dimStyle.Render(promptLine))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", width-2))
	b.WriteString("\n")

	// Output lines
	lines := m.sessionManager.GetSessionOutput(m.viewingSessionID)

	// Pre-render all output lines into visual lines for proper scrolling.
	// Each OutputLine may produce multiple visual lines (e.g., markdown text).
	var allVisualLines []string
	for i := range lines {
		formatted := m.formatOutputLine(lines[i], width)
		visualLines := strings.Split(formatted, "\n")
		allVisualLines = append(allVisualLines, visualLines...)
	}

	// Scroll on visual lines, not logical OutputLine count
	outputHeight := height - 5 // Account for header, prompt, separator
	totalVisual := len(allVisualLines)
	scrollOffset := m.scrollOffset

	// Determine visible window with scroll indicators at edges.
	// scrollOffset=0 means "at bottom" (latest), higher values scroll toward top.
	//
	// Layout cases:
	// - At bottom (scrollOffset=0): no indicators, full outputHeight for content
	// - Scrolled to top: only ↓ indicator at bottom, content = outputHeight-1
	// - Scrolled in middle: ↑ at top + ↓ at bottom, content = outputHeight-2

	if scrollOffset == 0 {
		// At bottom: no indicators
		startIdx := totalVisual - outputHeight
		if startIdx < 0 {
			startIdx = 0
		}
		for i := startIdx; i < totalVisual; i++ {
			b.WriteString(allVisualLines[i])
			b.WriteString("\n")
		}
	} else {
		// Scrolled up: try with 2 indicators first (most common scrolled case)
		contentHeight := outputHeight - 2 // room for ↑ and ↓
		if contentHeight < 1 {
			contentHeight = 1
		}

		maxScroll := 0
		if totalVisual > contentHeight {
			maxScroll = totalVisual - contentHeight
		}
		if scrollOffset > maxScroll {
			scrollOffset = maxScroll
		}

		endIdx := totalVisual - scrollOffset
		startIdx := endIdx - contentHeight
		if startIdx < 0 {
			startIdx = 0
		}

		if startIdx == 0 {
			// At/near top: only need ↓ indicator, reclaim the ↑ line for content
			contentHeight = outputHeight - 1
			maxScroll = 0
			if totalVisual > contentHeight {
				maxScroll = totalVisual - contentHeight
			}
			if scrollOffset > maxScroll {
				scrollOffset = maxScroll
			}
			endIdx = totalVisual - scrollOffset
			// startIdx stays 0

			for i := 0; i < endIdx; i++ {
				b.WriteString(allVisualLines[i])
				b.WriteString("\n")
			}
			hiddenBelow := totalVisual - endIdx
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ↓ %d more lines (press End to jump to latest)", hiddenBelow)))
			b.WriteString("\n")
		} else {
			// Middle: both ↑ and ↓ indicators
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ↑ %d more lines (press Home to jump to top)", startIdx)))
			b.WriteString("\n")
			for i := startIdx; i < endIdx; i++ {
				b.WriteString(allVisualLines[i])
				b.WriteString("\n")
			}
			hiddenBelow := totalVisual - endIdx
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ↓ %d more lines (press End to jump to latest)", hiddenBelow)))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// formatOutputLine formats a single OutputLine for display in the center view.
func (m Model) formatOutputLine(line session.OutputLine, width int) string {
	var formatted string
	switch line.Type {
	case session.OutputTypeError:
		formatted = errorStyle.Render("  ✗ " + line.Content)

	case session.OutputTypeThinking:
		formatted = dimStyle.Render("  💭 " + truncate(line.Content, width-8))

	case session.OutputTypeTool:
		formatted = "  🔧 " + line.Content

	case session.OutputTypeToolStart:
		toolDisplay := formatToolDisplay(line.ToolName, line.ToolInput, width-12)
		switch line.ToolState {
		case session.ToolStateRunning:
			elapsed := time.Since(line.StartTime)
			elapsedStr := fmt.Sprintf("%.1fs", elapsed.Seconds())
			formatted = "  🔧 " + toolDisplay + " " + runningStyle.Render("⏳ "+elapsedStr)
		case session.ToolStateComplete:
			durationStr := fmt.Sprintf("%.2fs", float64(line.DurationMs)/1000)
			formatted = "  ✓ " + dimStyle.Render(toolDisplay+" ("+durationStr+")")
		case session.ToolStateError:
			durationStr := fmt.Sprintf("%.2fs", float64(line.DurationMs)/1000)
			formatted = "  " + errorStyle.Render("✗ "+toolDisplay+" ("+durationStr+")")
		default:
			formatted = "  🔧 " + toolDisplay
		}

	case session.OutputTypeTurnEnd:
		turnInfo := fmt.Sprintf("─── Turn %d complete ($%.4f) ───", line.TurnNumber, line.CostUSD)
		formatted = dimStyle.Render("  " + turnInfo)

	case session.OutputTypeStatus:
		formatted = dimStyle.Render("  → " + line.Content)

	case session.OutputTypePlanReady:
		header := dimStyle.Render("  " + strings.Repeat("═", 20) + " Plan Ready " + strings.Repeat("═", 20))
		rendered := ""
		if m.mdRenderer != nil && line.Content != "" {
			r, err := m.mdRenderer.Render(line.Content)
			if err == nil {
				rendered = strings.TrimRight(r, "\n")
			}
		}
		if rendered == "" {
			rendered = "  " + line.Content
		}
		formatted = header + "\n" + rendered

	case session.OutputTypeText:
		if m.mdRenderer != nil && line.Content != "" {
			rendered, err := m.mdRenderer.Render(line.Content)
			if err == nil {
				formatted = strings.TrimRight(rendered, "\n")
			} else {
				formatted = "  " + line.Content
			}
		} else {
			formatted = "  " + line.Content
		}

	default:
		formatted = "  " + line.Content
	}

	// Truncate width if needed (skip for markdown-rendered content which may have ANSI)
	if line.Type != session.OutputTypeText && line.Type != session.OutputTypePlanReady && len(stripAnsi(formatted)) > width-2 {
		formatted = formatted[:width-5] + "..."
	}

	return formatted
}

// renderHistorySession renders a history session (read-only replay).
func (m Model) renderHistorySession(width, height int) string {
	var b strings.Builder

	data := m.viewingHistoryData

	// Session header with replay indicator
	typeIcon := "📋"
	if data.Type == session.SessionTypeBuilder {
		typeIcon = "🔨"
	}
	headerLine := fmt.Sprintf("  %s %s  %s  %s", typeIcon, data.Type, data.ID, dimStyle.Render("[Replay]"))
	b.WriteString(headerLine)
	b.WriteString("\n")

	// Prompt
	promptLine := fmt.Sprintf("  %q", truncate(data.Prompt, width-8))
	b.WriteString(dimStyle.Render(promptLine))
	b.WriteString("\n")

	// Timestamp
	timeLine := fmt.Sprintf("  Recorded: %s", data.CreatedAt.Format("2006-01-02 15:04"))
	b.WriteString(dimStyle.Render(timeLine))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", width-2))
	b.WriteString("\n")

	// Output lines from history - use formatOutputLine and visual line scroll
	lines := data.Output

	var allVisualLines []string
	for i := range lines {
		formatted := m.formatOutputLine(lines[i], width)
		visualLines := strings.Split(formatted, "\n")
		allVisualLines = append(allVisualLines, visualLines...)
	}

	outputHeight := height - 6 // Account for header, prompt, timestamp, separator
	totalVisual := len(allVisualLines)
	scrollOffset := m.scrollOffset

	if scrollOffset == 0 {
		startIdx := totalVisual - outputHeight
		if startIdx < 0 {
			startIdx = 0
		}
		for i := startIdx; i < totalVisual; i++ {
			b.WriteString(allVisualLines[i])
			b.WriteString("\n")
		}
	} else {
		contentHeight := outputHeight - 2
		if contentHeight < 1 {
			contentHeight = 1
		}

		maxScroll := 0
		if totalVisual > contentHeight {
			maxScroll = totalVisual - contentHeight
		}
		if scrollOffset > maxScroll {
			scrollOffset = maxScroll
		}

		endIdx := totalVisual - scrollOffset
		startIdx := endIdx - contentHeight
		if startIdx < 0 {
			startIdx = 0
		}

		if startIdx == 0 {
			contentHeight = outputHeight - 1
			maxScroll = 0
			if totalVisual > contentHeight {
				maxScroll = totalVisual - contentHeight
			}
			if scrollOffset > maxScroll {
				scrollOffset = maxScroll
			}
			endIdx = totalVisual - scrollOffset

			for i := 0; i < endIdx; i++ {
				b.WriteString(allVisualLines[i])
				b.WriteString("\n")
			}
			hiddenBelow := totalVisual - endIdx
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ↓ %d more lines (press End to jump to latest)", hiddenBelow)))
			b.WriteString("\n")
		} else {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ↑ %d more lines (press Home to jump to top)", startIdx)))
			b.WriteString("\n")
			for i := startIdx; i < endIdx; i++ {
				b.WriteString(allVisualLines[i])
				b.WriteString("\n")
			}
			hiddenBelow := totalVisual - endIdx
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ↓ %d more lines (press End to jump to latest)", hiddenBelow)))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderStatusBar renders the bottom status bar.
func (m Model) renderStatusBar() string {
	// Build keybinding hints based on state
	var hints []string
	hasWorktree := m.selectedWorktree() != nil
	inTmuxMode := m.sessionManager.IsInTmuxMode()

	if m.inputMode {
		hints = []string{"[Tab] Switch", "[Ctrl+Enter] Send", "[Esc] Cancel", "[?]help", "[q]uit"}
	} else if m.focus == FocusWorktreeDropdown || m.focus == FocusSessionDropdown {
		hints = []string{"[↑/↓]select", "[Enter]choose", "[Esc]close", "[?]help", "[q]uit"}
	} else if inTmuxMode {
		// Tmux mode: show session list navigation hints
		hints = []string{"[↑/↓] Navigate", "[Enter] Switch to session"}
		if hasWorktree {
			hints = append(hints, "[p] Plan", "[b] Build")
		}
		hints = append(hints, "[Alt-W] Worktree", "[?]help", "[q] Quit")
	} else if m.viewingSessionID != "" {
		// SDK mode: session is selected - show contextual actions
		sess := m.selectedSession()
		hints = []string{"[↑/↓]scroll"}
		if sess != nil && sess.Status == session.StatusIdle {
			hints = append(hints, "[f]ollow-up")
		}
		if sess != nil && (sess.Status == session.StatusRunning || sess.Status == session.StatusIdle) {
			hints = append(hints, "[s]top")
		}
		hints = append(hints, "[F2]split", "[Alt-W]worktree", "[Alt-S]session", "[?]help", "[q]uit")
	} else {
		// SDK mode: no session selected - show worktree-dependent actions
		hints = []string{"[Alt-W]worktree", "[Alt-S]session", "[t]ask", "[F2]split"}
		if hasWorktree {
			hints = append(hints, "[e]dit", "[p]lan", "[b]uild", "[n]ew wt", "[d]elete wt")
		} else {
			hints = append(hints, "[n]ew wt")
		}
		hints = append(hints, "[?]help", "[q]uit")
	}

	left := strings.Join(hints, "  ")

	// Session counts
	counts := m.sessionManager.CountByStatus()
	running := counts[session.StatusRunning]
	idle := counts[session.StatusIdle]
	right := fmt.Sprintf("Running: %d  Idle: %d", running, idle)

	// New output indicator when scrolled up
	if m.scrollOffset > 0 {
		right = dimStyle.Render(fmt.Sprintf("(%d lines above)", m.scrollOffset)) + "  " + right
	}

	// Pad to fill width
	padding := m.width - len(stripAnsi(left)) - len(stripAnsi(right)) - 2
	if padding < 1 {
		padding = 1
	}

	bar := left + strings.Repeat(" ", padding) + right
	return statusBarStyle.Width(m.width).Render(bar)
}

// statusIcon returns a status icon for the session status.
func statusIcon(status session.SessionStatus) string {
	switch status {
	case session.StatusPending:
		return pendingStyle.Render("○")
	case session.StatusRunning:
		return runningStyle.Render("●")
	case session.StatusIdle:
		return idleStyle.Render("◐")
	case session.StatusCompleted:
		return completedStyle.Render("✓")
	case session.StatusFailed:
		return failedStyle.Render("✗")
	case session.StatusStopped:
		return dimStyle.Render("◌")
	default:
		return "?"
	}
}

// truncate truncates a string to max length.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// formatToolDisplay formats a tool invocation for display.
func formatToolDisplay(toolName string, input map[string]interface{}, maxLen int) string {
	if input == nil {
		return fmt.Sprintf("[%s]", toolName)
	}

	var detail string
	switch toolName {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			detail = truncatePath(path, maxLen-len(toolName)-4)
		}
	case "Write", "Edit":
		if path, ok := input["file_path"].(string); ok {
			detail = "→ " + truncatePath(path, maxLen-len(toolName)-6)
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			detail = truncate(cmd, maxLen-len(toolName)-4)
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			detail = pattern
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			detail = truncate(pattern, maxLen-len(toolName)-4)
		}
	case "Task":
		if desc, ok := input["description"].(string); ok {
			detail = desc
		}
	}

	if detail != "" {
		return fmt.Sprintf("[%s] %s", toolName, detail)
	}
	return fmt.Sprintf("[%s]", toolName)
}

// truncatePath truncates a path, keeping the end visible.
func truncatePath(path string, max int) string {
	if len(path) <= max {
		return path
	}
	if max <= 7 {
		return truncate(path, max)
	}
	// Keep last part of path visible
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		return truncate(path, max)
	}
	suffix := parts[len(parts)-1]
	if len(suffix)+4 >= max {
		return truncate(path, max)
	}
	return ".../" + suffix
}

// stripAnsi removes ANSI escape codes from a string (approximation for length calculation).
func stripAnsi(s string) string {
	// Simple approximation - just remove escape sequences
	var result strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		result.WriteRune(r)
	}
	return result.String()
}

// overlayAt places an overlay string at visual column x, line y.
// It correctly handles ANSI escape sequences and wide characters in the base string.
func overlayAt(base, overlay string, x, y int) string {
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")

	for i, overlayLine := range overlayLines {
		lineIdx := y + i
		if lineIdx < 0 || lineIdx >= len(baseLines) {
			continue
		}

		baseLine := baseLines[lineIdx]
		baseLines[lineIdx] = spliceAtColumn(baseLine, overlayLine, x)
	}

	return strings.Join(baseLines, "\n")
}

// spliceAtColumn replaces the base string starting at visual column col with the overlay.
// ANSI escape sequences are preserved from the prefix portion.
func spliceAtColumn(base, overlay string, col int) string {
	var result strings.Builder
	visualCol := 0
	i := 0
	runes := []rune(base)

	// Copy base content up to visual column col
	for i < len(runes) && visualCol < col {
		if runes[i] == '\x1b' {
			// Copy entire ANSI escape sequence
			for i < len(runes) {
				result.WriteRune(runes[i])
				if runes[i] == 'm' {
					i++
					break
				}
				i++
			}
			continue
		}
		w := runewidth.RuneWidth(runes[i])
		if visualCol+w > col {
			// Wide character would cross the column boundary; pad with space
			result.WriteRune(' ')
			visualCol++
			break
		}
		result.WriteRune(runes[i])
		visualCol += w
		i++
	}

	// Pad with spaces if base was shorter than col
	for visualCol < col {
		result.WriteRune(' ')
		visualCol++
	}

	// Write the overlay
	result.WriteString(overlay)

	return result.String()
}
