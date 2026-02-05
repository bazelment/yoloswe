package app

import (
	"fmt"
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
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	// Layout: top bar (1 line) + center + input area (dynamic) + status bar (1 line)
	topBarHeight := 1
	statusBarHeight := 1
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
	centerHeight := m.height - topBarHeight - statusBarHeight - inputHeight - 2 // borders

	// Build components
	topBar := m.renderTopBar()
	center := m.renderCenter(m.width, centerHeight)
	statusBar := m.renderStatusBar()

	// Add border to center
	centerBordered := borderStyle.Width(m.width).Height(centerHeight).Render(center)

	// Build layout
	parts := []string{topBar, centerBordered}

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

	// Right side: current session + session dropdown
	right := ""
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

	// Combine with padding
	padding := m.width - len(stripAnsi(left)) - len(stripAnsi(right)) - 4
	if padding < 1 {
		padding = 1
	}

	bar := left + strings.Repeat(" ", padding) + right
	return topBarStyle.Width(m.width).Render(bar)
}

// renderCenter renders the main center area (session output + input).
func (m Model) renderCenter(width, height int) string {
	var b strings.Builder

	if m.viewingSessionID == "" {
		// No session selected - show worktree operation messages if any
		if len(m.worktreeOpMessages) > 0 {
			b.WriteString("\n")
			for _, msg := range m.worktreeOpMessages {
				b.WriteString("  ")
				b.WriteString(msg)
				b.WriteString("\n")
			}
			return b.String()
		}

		// Default empty state
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  No session selected"))
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render("  Choose a session with [Alt-S]"))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("  Or start a new session with [p]lan or [b]uild"))
		return b.String()
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
	// Add idle indicator with follow-up hint
	if info.Status == session.StatusIdle {
		headerLine += idleStyle.Render("  (awaiting follow-up - press 'f')")
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
	if line.Type != session.OutputTypeText && len(stripAnsi(formatted)) > width-2 {
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

	// Output lines from history
	lines := data.Output

	// Show last N lines that fit
	outputHeight := height - 6 // Account for header, prompt, timestamp, separator
	startIdx := 0
	if len(lines) > outputHeight {
		startIdx = len(lines) - outputHeight
	}

	// Track visual lines written to avoid overflow
	visualLinesWritten := 0

	for i := startIdx; i < len(lines) && visualLinesWritten < outputHeight; i++ {
		line := lines[i]

		// Format based on type (same as renderCenter)
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
			case session.ToolStateComplete:
				durationStr := fmt.Sprintf("%.2fs", float64(line.DurationMs)/1000)
				formatted = "  ✓ " + dimStyle.Render(toolDisplay+" ("+durationStr+")")
			case session.ToolStateError:
				durationStr := fmt.Sprintf("%.2fs", float64(line.DurationMs)/1000)
				formatted = "  " + errorStyle.Render("✗ "+toolDisplay+" ("+durationStr+")")
			default:
				formatted = "  🔧 " + toolDisplay
			}

		case session.OutputTypeToolResult:
			// Legacy format
			if line.IsError {
				formatted = errorStyle.Render("  ✗ " + truncate(line.Content, width-10))
			} else {
				formatted = dimStyle.Render("  ✓ " + line.Content)
			}

		case session.OutputTypeTurnEnd:
			turnInfo := fmt.Sprintf("─── Turn %d complete ($%.4f) ───", line.TurnNumber, line.CostUSD)
			formatted = dimStyle.Render("  " + turnInfo)

		case session.OutputTypeStatus:
			formatted = dimStyle.Render("  → " + line.Content)

		case session.OutputTypeText:
			// Render markdown for assistant text
			if m.mdRenderer != nil && line.Content != "" {
				rendered, err := m.mdRenderer.Render(line.Content)
				if err == nil {
					rendered = strings.TrimRight(rendered, "\n")
					formatted = rendered
				} else {
					formatted = "  " + line.Content
				}
			} else {
				formatted = "  " + line.Content
			}

		default:
			formatted = "  " + line.Content
		}

		// Truncate width if needed (skip for markdown-rendered content)
		if line.Type != session.OutputTypeText && len(stripAnsi(formatted)) > width-2 {
			formatted = formatted[:width-5] + "..."
		}

		// Count visual lines this content will take
		contentLines := strings.Split(formatted, "\n")
		linesNeeded := len(contentLines)

		// Truncate multi-line content if it would overflow
		if visualLinesWritten+linesNeeded > outputHeight {
			remaining := outputHeight - visualLinesWritten
			if remaining > 0 {
				truncatedLines := contentLines[:remaining]
				b.WriteString(strings.Join(truncatedLines, "\n"))
				b.WriteString("\n")
			}
			break
		}

		b.WriteString(formatted)
		b.WriteString("\n")
		visualLinesWritten += linesNeeded
	}

	return b.String()
}

// renderStatusBar renders the bottom status bar.
func (m Model) renderStatusBar() string {
	// Build keybinding hints based on state
	var hints []string
	if m.inputMode {
		hints = []string{"[Tab] Switch", "[Enter] Select"}
	} else if m.focus == FocusWorktreeDropdown || m.focus == FocusSessionDropdown {
		hints = []string{"[↑/↓]select", "[Enter]choose", "[Esc]close"}
	} else if m.viewingSessionID != "" {
		// Session is selected - show scroll hints along with main actions
		sess := m.selectedSession()
		if sess != nil && sess.Status == session.StatusIdle {
			// Show follow-up hint for idle sessions
			hints = []string{"[f]ollow-up", "[↑/↓]scroll", "[Alt-S]session", "[s]top", "[q]uit"}
		} else {
			hints = []string{"[↑/↓]scroll", "[Alt-W]worktree", "[Alt-S]session", "[s]top", "[q]uit"}
		}
	} else {
		hints = []string{"[e]dit", "[p]lan", "[b]uild", "[n]ew wt", "[d]elete wt", "[t]ask", "[s]top", "[q]uit"}
	}

	left := strings.Join(hints, "  ")

	// Session counts
	counts := m.sessionManager.CountByStatus()
	running := counts[session.StatusRunning]
	idle := counts[session.StatusIdle]
	right := fmt.Sprintf("Running: %d  Idle: %d", running, idle)

	// Error message if any
	if m.lastError != "" {
		right = errorStyle.Render("Error: " + truncate(m.lastError, 40))
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
