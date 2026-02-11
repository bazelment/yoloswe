package app

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// ConfirmOption is a single-keypress choice in a confirmation prompt.
type ConfirmOption struct {
	Key   string // single character key (e.g. "y", "d")
	Label string // human-readable label (e.g. "yes", "yes + delete branch")
}

// ConfirmPrompt is a lightweight single-keypress confirmation widget.
// It replaces the TextArea-based y/n confirmations with a faster UX.
type ConfirmPrompt struct {
	message string
	options []ConfirmOption
}

// NewConfirmPrompt creates a new confirmation prompt.
func NewConfirmPrompt(message string, options []ConfirmOption) *ConfirmPrompt {
	return &ConfirmPrompt{
		message: message,
		options: options,
	}
}

// ConfirmResult represents the outcome of a key press in the confirm prompt.
type ConfirmResult struct {
	// Matched is the option key that was pressed, or "" if cancelled/unhandled.
	Matched string
	// Cancelled is true when Esc was pressed.
	Cancelled bool
	// Quit is true when Ctrl+C was pressed.
	Quit bool
}

// HandleKey processes a single key press and returns the result.
func (c *ConfirmPrompt) HandleKey(msg tea.KeyMsg) ConfirmResult {
	switch msg.String() {
	case "esc":
		return ConfirmResult{Cancelled: true}
	case "ctrl+c":
		return ConfirmResult{Quit: true}
	default:
		keyStr := msg.String()
		for _, opt := range c.options {
			if keyStr == opt.Key {
				return ConfirmResult{Matched: opt.Key}
			}
		}
		// Unrecognized key — ignore (do not cancel)
		return ConfirmResult{}
	}
}

// View renders the confirmation prompt with its keybinding hints.
func (c *ConfirmPrompt) View(s *Styles) string {
	var b strings.Builder
	b.WriteString(c.message)
	b.WriteString("\n\n")

	// Build hint line: [key] label  [key] label  [Esc] cancel
	hints := make([]string, 0, len(c.options)+1)
	for _, opt := range c.options {
		hints = append(hints, formatKeyHints(opt.Key, opt.Label))
	}
	hints = append(hints, formatKeyHints("Esc", "cancel"))
	b.WriteString(s.Dim.Render(strings.Join(hints, "  ")))

	content := b.String()
	return s.InputBox.Render(content)
}
