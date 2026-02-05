package app

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bazelment/yoloswe/bramble/session"
)

// OutputModel is a standalone model for rendering session output.
// It can be used for testing with teatest.
type OutputModel struct {
	info       *session.SessionInfo
	mdRenderer *MarkdownRenderer
	lines      []session.OutputLine
	width      int
	height     int
	isReplay   bool
}

// NewOutputModel creates a new output model for testing.
func NewOutputModel(info *session.SessionInfo, lines []session.OutputLine) OutputModel {
	return OutputModel{
		lines:    lines,
		info:     info,
		isReplay: false,
		width:    80,
		height:   24,
	}
}

// NewOutputModelWithMarkdown creates a new output model with markdown rendering.
func NewOutputModelWithMarkdown(info *session.SessionInfo, lines []session.OutputLine, width int) OutputModel {
	md, _ := NewMarkdownRenderer(width)
	return OutputModel{
		lines:      lines,
		info:       info,
		isReplay:   false,
		width:      width,
		height:     24,
		mdRenderer: md,
	}
}

// NewReplayOutputModel creates a new output model for replay testing.
func NewReplayOutputModel(stored *session.StoredSession) OutputModel {
	info := session.StoredToSessionInfo(stored)
	return OutputModel{
		lines:    stored.Output,
		info:     &info,
		isReplay: true,
		width:    80,
		height:   24,
	}
}

// SetSize sets the terminal size for rendering.
func (m *OutputModel) SetSize(width, height int) {
	m.width = width
	m.height = height
	// Update markdown renderer width if present
	if m.mdRenderer != nil {
		m.mdRenderer.SetWidth(width)
	}
}

// EnableMarkdown enables markdown rendering for text content.
func (m *OutputModel) EnableMarkdown() {
	if m.mdRenderer == nil {
		m.mdRenderer, _ = NewMarkdownRenderer(m.width)
	}
}

// Init initializes the model.
func (m OutputModel) Init() tea.Cmd {
	return nil
}

// Update handles messages.
func (m OutputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		if msg.String() == "q" {
			return m, tea.Quit
		}
	}
	return m, nil
}

// View renders the output.
func (m OutputModel) View() string {
	var b strings.Builder

	if m.info == nil {
		b.WriteString(dimStyle.Render("  No session"))
		return b.String()
	}

	// Session header
	typeIcon := "📋"
	if m.info.Type == session.SessionTypeBuilder {
		typeIcon = "🔨"
	}

	if m.isReplay {
		b.WriteString(typeIcon + " " + string(m.info.ID) + "  " + dimStyle.Render("[Replay]"))
	} else {
		b.WriteString(typeIcon + " " + string(m.info.ID) + "  " + statusIcon(m.info.Status))
	}
	b.WriteString("\n")

	// Prompt
	b.WriteString(dimStyle.Render("\"" + truncate(m.info.Prompt, m.width-4) + "\""))
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", m.width-2))
	b.WriteString("\n")

	// Output lines
	outputHeight := m.height - 4
	startIdx := 0
	if len(m.lines) > outputHeight {
		startIdx = len(m.lines) - outputHeight
	}

	for i := startIdx; i < len(m.lines); i++ {
		line := m.lines[i]
		// Handle text with optional markdown rendering
		if line.Type == session.OutputTypeText && m.mdRenderer != nil && line.Content != "" {
			rendered, err := m.mdRenderer.Render(line.Content)
			if err == nil {
				rendered = strings.TrimRight(rendered, "\n")
				b.WriteString(rendered)
				b.WriteString("\n")
				continue
			}
		}
		b.WriteString(formatOutputLine(line, m.width))
		b.WriteString("\n")
	}

	return b.String()
}

// formatOutputLine formats a single output line for display.
func formatOutputLine(line session.OutputLine, width int) string {
	var formatted string
	switch line.Type {
	case session.OutputTypeError:
		formatted = errorStyle.Render("✗ " + line.Content)

	case session.OutputTypeThinking:
		formatted = dimStyle.Render("💭 " + truncate(line.Content, width-4))

	case session.OutputTypeTool:
		// Legacy tool type - kept for backward compat
		formatted = "🔧 " + line.Content

	case session.OutputTypeToolStart:
		// Tool invocation with name and formatted input
		toolDisplay := formatToolDisplay(line.ToolName, line.ToolInput, width-12)

		switch line.ToolState {
		case session.ToolStateRunning:
			// Show running indicator with elapsed time
			elapsed := time.Since(line.StartTime)
			elapsedStr := fmt.Sprintf("%.1fs", elapsed.Seconds())
			formatted = "🔧 " + toolDisplay + " " + runningStyle.Render("⏳ "+elapsedStr)
		case session.ToolStateComplete:
			// Show checkmark with duration
			durationStr := fmt.Sprintf("%.2fs", float64(line.DurationMs)/1000)
			formatted = "✓ " + dimStyle.Render(toolDisplay+" ("+durationStr+")")
		case session.ToolStateError:
			// Show error indicator with duration
			durationStr := fmt.Sprintf("%.2fs", float64(line.DurationMs)/1000)
			formatted = errorStyle.Render("✗ " + toolDisplay + " (" + durationStr + ")")
		default:
			// Fallback for legacy or unset state
			formatted = "🔧 " + toolDisplay
		}

	case session.OutputTypeTurnEnd:
		// Turn summary with cost
		turnInfo := fmt.Sprintf("─── Turn %d complete ($%.4f) ───", line.TurnNumber, line.CostUSD)
		formatted = dimStyle.Render(turnInfo)

	case session.OutputTypeStatus:
		formatted = dimStyle.Render("→ " + line.Content)

	default:
		formatted = line.Content
	}

	// Truncate if needed (skip for markdown content which may have multi-line)
	if line.Type != session.OutputTypeText && len(stripAnsi(formatted)) > width-2 {
		formatted = formatted[:width-5] + "..."
	}

	return formatted
}
