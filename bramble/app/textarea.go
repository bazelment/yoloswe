package app

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// TextAreaFocus indicates which element has focus in the text area.
type TextAreaFocus int

const (
	FocusTextInput TextAreaFocus = iota
	FocusSendButton
	FocusCancelButton
)

// TextArea is a multi-line text input component with word wrapping and focusable buttons.
type TextArea struct {
	value        string
	prompt       string
	sendLabel    string
	cancelLabel  string
	cursorPos    int
	width        int
	maxHeight    int
	minHeight    int
	scrollOffset int
	focus        TextAreaFocus
}

// NewTextArea creates a new multi-line text input.
func NewTextArea() *TextArea {
	return &TextArea{
		width:       60,
		maxHeight:   10,
		minHeight:   3,
		focus:       FocusTextInput,
		sendLabel:   "Send",
		cancelLabel: "Cancel",
	}
}

// SetLabels sets the button labels.
func (t *TextArea) SetLabels(send, cancel string) {
	t.sendLabel = send
	t.cancelLabel = cancel
}

// Focus returns the current focus.
func (t *TextArea) Focus() TextAreaFocus {
	return t.focus
}

// SetFocus sets the focus.
func (t *TextArea) SetFocus(f TextAreaFocus) {
	t.focus = f
}

// CycleForward moves focus to the next element (Tab).
func (t *TextArea) CycleForward() {
	switch t.focus {
	case FocusTextInput:
		t.focus = FocusSendButton
	case FocusSendButton:
		t.focus = FocusCancelButton
	case FocusCancelButton:
		t.focus = FocusTextInput
	}
}

// CycleBackward moves focus to the previous element (Shift+Tab).
func (t *TextArea) CycleBackward() {
	switch t.focus {
	case FocusTextInput:
		t.focus = FocusCancelButton
	case FocusSendButton:
		t.focus = FocusTextInput
	case FocusCancelButton:
		t.focus = FocusSendButton
	}
}

// SetWidth sets the text area width.
func (t *TextArea) SetWidth(w int) {
	t.width = w
}

// SetMaxHeight sets the maximum height.
func (t *TextArea) SetMaxHeight(h int) {
	t.maxHeight = h
}

// SetMinHeight sets the minimum height.
func (t *TextArea) SetMinHeight(h int) {
	t.minHeight = h
}

// SetPrompt sets the prompt label.
func (t *TextArea) SetPrompt(p string) {
	t.prompt = p
}

// SetValue sets the text content.
func (t *TextArea) SetValue(s string) {
	t.value = s
	t.cursorPos = len(s)
}

// Value returns the current text content.
func (t *TextArea) Value() string {
	return t.value
}

// Reset clears the text area and resets focus to text input.
func (t *TextArea) Reset() {
	t.value = ""
	t.cursorPos = 0
	t.scrollOffset = 0
	t.focus = FocusTextInput
}

// InsertChar inserts a character at the cursor position.
func (t *TextArea) InsertChar(r rune) {
	t.value = t.value[:t.cursorPos] + string(r) + t.value[t.cursorPos:]
	t.cursorPos++
}

// InsertString inserts a string at the cursor position.
func (t *TextArea) InsertString(s string) {
	t.value = t.value[:t.cursorPos] + s + t.value[t.cursorPos:]
	t.cursorPos += len(s)
}

// InsertNewline inserts a newline at the cursor position.
func (t *TextArea) InsertNewline() {
	t.InsertChar('\n')
}

// DeleteChar deletes the character before the cursor.
func (t *TextArea) DeleteChar() {
	if t.cursorPos > 0 {
		t.value = t.value[:t.cursorPos-1] + t.value[t.cursorPos:]
		t.cursorPos--
	}
}

// DeleteCharForward deletes the character after the cursor.
func (t *TextArea) DeleteCharForward() {
	if t.cursorPos < len(t.value) {
		t.value = t.value[:t.cursorPos] + t.value[t.cursorPos+1:]
	}
}

// MoveCursorLeft moves the cursor left.
func (t *TextArea) MoveCursorLeft() {
	if t.cursorPos > 0 {
		t.cursorPos--
	}
}

// MoveCursorRight moves the cursor right.
func (t *TextArea) MoveCursorRight() {
	if t.cursorPos < len(t.value) {
		t.cursorPos++
	}
}

// MoveCursorUp moves the cursor up one line.
func (t *TextArea) MoveCursorUp() {
	lines := t.getLines()
	row, col := t.getCursorRowCol(lines)
	if row > 0 {
		newRow := row - 1
		newCol := min(col, len(lines[newRow]))
		t.cursorPos = t.posFromRowCol(lines, newRow, newCol)
	}
}

// MoveCursorDown moves the cursor down one line.
func (t *TextArea) MoveCursorDown() {
	lines := t.getLines()
	row, col := t.getCursorRowCol(lines)
	if row < len(lines)-1 {
		newRow := row + 1
		newCol := min(col, len(lines[newRow]))
		t.cursorPos = t.posFromRowCol(lines, newRow, newCol)
	}
}

// MoveCursorToLineStart moves cursor to start of current line.
func (t *TextArea) MoveCursorToLineStart() {
	lines := t.getLines()
	row, _ := t.getCursorRowCol(lines)
	t.cursorPos = t.posFromRowCol(lines, row, 0)
}

// MoveCursorToLineEnd moves cursor to end of current line.
func (t *TextArea) MoveCursorToLineEnd() {
	lines := t.getLines()
	row, _ := t.getCursorRowCol(lines)
	t.cursorPos = t.posFromRowCol(lines, row, len(lines[row]))
}

// getLines splits the value into lines.
func (t *TextArea) getLines() []string {
	if t.value == "" {
		return []string{""}
	}
	return strings.Split(t.value, "\n")
}

// getCursorRowCol returns the cursor row and column.
func (t *TextArea) getCursorRowCol(lines []string) (int, int) {
	pos := 0
	for row, line := range lines {
		if pos+len(line) >= t.cursorPos {
			return row, t.cursorPos - pos
		}
		pos += len(line) + 1 // +1 for newline
	}
	// Cursor at end
	lastRow := len(lines) - 1
	return lastRow, len(lines[lastRow])
}

// posFromRowCol converts row/col to absolute position.
func (t *TextArea) posFromRowCol(lines []string, row, col int) int {
	pos := 0
	for i := 0; i < row && i < len(lines); i++ {
		pos += len(lines[i]) + 1 // +1 for newline
	}
	return pos + col
}

// LineCount returns the number of lines.
func (t *TextArea) LineCount() int {
	return len(t.getLines())
}

// wrapLine wraps a single line to fit the width.
func (t *TextArea) wrapLine(line string, width int) []string {
	if width <= 0 || len(line) <= width {
		return []string{line}
	}

	var wrapped []string
	for len(line) > width {
		// Find break point (prefer space)
		breakAt := width
		for i := width - 1; i > 0; i-- {
			if line[i] == ' ' {
				breakAt = i + 1
				break
			}
		}
		wrapped = append(wrapped, line[:breakAt])
		line = line[breakAt:]
	}
	if line != "" {
		wrapped = append(wrapped, line)
	}
	return wrapped
}

// View renders the text area.
func (t *TextArea) View() string {
	contentWidth := t.width - 4 // Account for padding and border

	var b strings.Builder

	// Prompt
	if t.prompt != "" {
		b.WriteString(dimStyle.Render(t.prompt))
		b.WriteString("\n")
	}

	// Render lines with word wrap
	lines := t.getLines()
	cursorRow, cursorCol := t.getCursorRowCol(lines)

	var displayLines []string
	cursorDisplayRow := 0
	cursorDisplayCol := cursorCol

	for i, line := range lines {
		wrappedLines := t.wrapLine(line, contentWidth)

		// Track cursor position in wrapped lines
		if i == cursorRow {
			// Find which wrapped line the cursor is on
			col := cursorCol
			for j, wl := range wrappedLines {
				if col <= len(wl) {
					cursorDisplayRow = len(displayLines) + j
					cursorDisplayCol = col
					break
				}
				col -= len(wl)
			}
		}

		displayLines = append(displayLines, wrappedLines...)
	}

	// Calculate visible area
	visibleHeight := t.maxHeight - 2 // Reserve space for prompt and status
	if visibleHeight < t.minHeight {
		visibleHeight = t.minHeight
	}

	// Adjust scroll to keep cursor visible
	if cursorDisplayRow < t.scrollOffset {
		t.scrollOffset = cursorDisplayRow
	}
	if cursorDisplayRow >= t.scrollOffset+visibleHeight {
		t.scrollOffset = cursorDisplayRow - visibleHeight + 1
	}

	// Render visible lines
	endIdx := t.scrollOffset + visibleHeight
	if endIdx > len(displayLines) {
		endIdx = len(displayLines)
	}

	for i := t.scrollOffset; i < endIdx; i++ {
		line := displayLines[i]

		// Show cursor on current line
		if i == cursorDisplayRow {
			if cursorDisplayCol <= len(line) {
				line = line[:cursorDisplayCol] + "█" + line[cursorDisplayCol:]
			} else {
				line += "█"
			}
		}

		// Pad line to width
		for len(line) < contentWidth {
			line += " "
		}

		b.WriteString(line)
		b.WriteString("\n")
	}

	// Pad to minimum height
	for i := len(displayLines); i < visibleHeight; i++ {
		b.WriteString(strings.Repeat(" ", contentWidth))
		b.WriteString("\n")
	}

	// Render buttons with focus indication
	var sendBtn, cancelBtn string
	if t.focus == FocusSendButton {
		sendBtn = selectedStyle.Render("[ " + t.sendLabel + " ]")
	} else {
		sendBtn = dimStyle.Render("[ " + t.sendLabel + " ]")
	}
	if t.focus == FocusCancelButton {
		cancelBtn = selectedStyle.Render("[ " + t.cancelLabel + " ]")
	} else {
		cancelBtn = dimStyle.Render("[ " + t.cancelLabel + " ]")
	}
	status := sendBtn + "  " + cancelBtn
	statusPadding := contentWidth - runewidth.StringWidth(stripAnsi(status))
	if statusPadding < 0 {
		statusPadding = 0
	}
	b.WriteString(strings.Repeat(" ", statusPadding))
	b.WriteString(status)

	// Add border
	content := b.String()
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("12")).
		Padding(0, 1).
		Width(t.width)

	return boxStyle.Render(content)
}
