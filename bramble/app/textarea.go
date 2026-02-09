package app

import (
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
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
	placeholder  string
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

// SetPlaceholder sets the placeholder text shown when the value is empty.
func (t *TextArea) SetPlaceholder(p string) {
	t.placeholder = p
}

// runeOffset converts a rune-based cursor position to a byte offset in s.
func runeOffset(s string, runePos int) int {
	byteOff := 0
	for i := 0; i < runePos && byteOff < len(s); i++ {
		_, size := utf8.DecodeRuneInString(s[byteOff:])
		byteOff += size
	}
	return byteOff
}

// SetValue sets the text content.
func (t *TextArea) SetValue(s string) {
	t.value = s
	t.cursorPos = utf8.RuneCountInString(s)
}

// Value returns the current text content.
func (t *TextArea) Value() string {
	return t.value
}

// Reset clears the text area and resets focus to text input.
func (t *TextArea) Reset() {
	t.value = ""
	t.placeholder = ""
	t.cursorPos = 0
	t.scrollOffset = 0
	t.focus = FocusTextInput
}

// InsertChar inserts a character at the cursor position.
func (t *TextArea) InsertChar(r rune) {
	byteOff := runeOffset(t.value, t.cursorPos)
	t.value = t.value[:byteOff] + string(r) + t.value[byteOff:]
	t.cursorPos++
}

// InsertString inserts a string at the cursor position.
func (t *TextArea) InsertString(s string) {
	byteOff := runeOffset(t.value, t.cursorPos)
	t.value = t.value[:byteOff] + s + t.value[byteOff:]
	t.cursorPos += utf8.RuneCountInString(s)
}

// InsertNewline inserts a newline at the cursor position.
func (t *TextArea) InsertNewline() {
	t.InsertChar('\n')
}

// DeleteChar deletes the character before the cursor.
func (t *TextArea) DeleteChar() {
	if t.cursorPos > 0 {
		byteOff := runeOffset(t.value, t.cursorPos)
		prevByteOff := runeOffset(t.value, t.cursorPos-1)
		t.value = t.value[:prevByteOff] + t.value[byteOff:]
		t.cursorPos--
	}
}

// DeleteCharForward deletes the character after the cursor.
func (t *TextArea) DeleteCharForward() {
	runeCount := utf8.RuneCountInString(t.value)
	if t.cursorPos < runeCount {
		byteOff := runeOffset(t.value, t.cursorPos)
		nextByteOff := runeOffset(t.value, t.cursorPos+1)
		t.value = t.value[:byteOff] + t.value[nextByteOff:]
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
	if t.cursorPos < utf8.RuneCountInString(t.value) {
		t.cursorPos++
	}
}

// MoveCursorUp moves the cursor up one line.
func (t *TextArea) MoveCursorUp() {
	lines := t.getLines()
	row, col := t.getCursorRowCol(lines)
	if row > 0 {
		newRow := row - 1
		newCol := min(col, utf8.RuneCountInString(lines[newRow]))
		t.cursorPos = t.posFromRowCol(lines, newRow, newCol)
	}
}

// MoveCursorDown moves the cursor down one line.
func (t *TextArea) MoveCursorDown() {
	lines := t.getLines()
	row, col := t.getCursorRowCol(lines)
	if row < len(lines)-1 {
		newRow := row + 1
		newCol := min(col, utf8.RuneCountInString(lines[newRow]))
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
	t.cursorPos = t.posFromRowCol(lines, row, utf8.RuneCountInString(lines[row]))
}

// getLines splits the value into lines.
func (t *TextArea) getLines() []string {
	if t.value == "" {
		return []string{""}
	}
	return strings.Split(t.value, "\n")
}

// getCursorRowCol returns the cursor row and column (in runes).
func (t *TextArea) getCursorRowCol(lines []string) (int, int) {
	pos := 0
	for row, line := range lines {
		lineRunes := utf8.RuneCountInString(line)
		if pos+lineRunes >= t.cursorPos {
			return row, t.cursorPos - pos
		}
		pos += lineRunes + 1 // +1 for newline
	}
	// Cursor at end
	lastRow := len(lines) - 1
	return lastRow, utf8.RuneCountInString(lines[lastRow])
}

// posFromRowCol converts row/col (in runes) to absolute rune position.
func (t *TextArea) posFromRowCol(lines []string, row, col int) int {
	pos := 0
	for i := 0; i < row && i < len(lines); i++ {
		pos += utf8.RuneCountInString(lines[i]) + 1 // +1 for newline
	}
	return pos + col
}

// LineCount returns the number of lines.
func (t *TextArea) LineCount() int {
	return len(t.getLines())
}

// wrapLine wraps a single line to fit the width (rune-based).
func (t *TextArea) wrapLine(line string, width int) []string {
	runes := []rune(line)
	if width <= 0 || len(runes) <= width {
		return []string{line}
	}

	var wrapped []string
	for len(runes) > width {
		// Find break point (prefer space)
		breakAt := width
		for i := width - 1; i > 0; i-- {
			if runes[i] == ' ' {
				breakAt = i + 1
				break
			}
		}
		wrapped = append(wrapped, string(runes[:breakAt]))
		runes = runes[breakAt:]
	}
	if len(runes) > 0 {
		wrapped = append(wrapped, string(runes))
	}
	return wrapped
}

// TextAreaAction represents the result of handling a key press in a TextArea.
type TextAreaAction int

const (
	// TextAreaHandled means the key was consumed by the TextArea (cursor move, char insert, etc.).
	TextAreaHandled TextAreaAction = iota
	// TextAreaSubmit means the user triggered submit (Ctrl+Enter, or Enter on Send button).
	TextAreaSubmit
	// TextAreaCancel means the user triggered cancel (Esc, or Enter on Cancel button).
	TextAreaCancel
	// TextAreaQuit means the user pressed Ctrl+C (global quit).
	TextAreaQuit
	// TextAreaUnhandled means the key was not consumed (caller should handle it).
	TextAreaUnhandled
)

// HandleKey processes a key message against this TextArea and returns the
// resulting action. The TextArea is mutated in place for cursor movement,
// character insertion, focus cycling, etc. The caller is responsible for
// acting on Submit, Cancel, and Quit.
func (t *TextArea) HandleKey(msg tea.KeyMsg) TextAreaAction {
	switch msg.String() {
	case "tab":
		t.CycleForward()
		return TextAreaHandled

	case "shift+tab":
		t.CycleBackward()
		return TextAreaHandled

	case "shift+enter":
		if t.Focus() == FocusTextInput {
			t.InsertNewline()
		}
		return TextAreaHandled

	case "ctrl+enter":
		return TextAreaSubmit

	case "enter":
		switch t.Focus() {
		case FocusTextInput:
			if strings.TrimSpace(t.value) != "" {
				return TextAreaSubmit
			}
			return TextAreaHandled
		case FocusSendButton:
			return TextAreaSubmit
		case FocusCancelButton:
			return TextAreaCancel
		}
		return TextAreaHandled

	case "esc":
		return TextAreaCancel

	case "backspace":
		if t.Focus() == FocusTextInput {
			t.DeleteChar()
		}
		return TextAreaHandled

	case "delete":
		if t.Focus() == FocusTextInput {
			t.DeleteCharForward()
		}
		return TextAreaHandled

	case "up":
		if t.Focus() == FocusTextInput {
			t.MoveCursorUp()
		}
		return TextAreaHandled

	case "down":
		if t.Focus() == FocusTextInput {
			t.MoveCursorDown()
		}
		return TextAreaHandled

	case "left":
		if t.Focus() == FocusTextInput {
			t.MoveCursorLeft()
		}
		return TextAreaHandled

	case "right":
		if t.Focus() == FocusTextInput {
			t.MoveCursorRight()
		}
		return TextAreaHandled

	case "ctrl+c":
		return TextAreaQuit

	default:
		if t.Focus() == FocusTextInput {
			keyStr := msg.String()
			if keyStr == "space" {
				t.InsertChar(' ')
			} else if len(keyStr) == 1 {
				t.InsertChar(rune(keyStr[0]))
			} else if len(msg.Runes) > 0 {
				for _, r := range msg.Runes {
					t.InsertChar(r)
				}
			}
			return TextAreaHandled
		}
		return TextAreaUnhandled
	}
}

// View renders the text area.
func (t *TextArea) View() string {
	contentWidth := t.width - 4 // Account for padding and border
	if contentWidth < 1 {
		contentWidth = 1
	}

	var b strings.Builder

	// Prompt
	if t.prompt != "" {
		b.WriteString(dimStyle.Render(t.prompt))
		b.WriteString("\n")
	}

	// Show placeholder text when value is empty
	showPlaceholder := t.value == "" && t.placeholder != ""

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
				wlRunes := utf8.RuneCountInString(wl)
				if col <= wlRunes {
					cursorDisplayRow = len(displayLines) + j
					cursorDisplayCol = col
					break
				}
				col -= wlRunes
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

		if showPlaceholder && i == 0 {
			// Show cursor then placeholder in dim style
			b.WriteString("█")
			b.WriteString(dimStyle.Render(t.placeholder))
			b.WriteString("\n")
			continue
		}

		// Show cursor on current line
		if i == cursorDisplayRow {
			lineRunes := []rune(line)
			if cursorDisplayCol <= len(lineRunes) {
				line = string(lineRunes[:cursorDisplayCol]) + "█" + string(lineRunes[cursorDisplayCol:])
			} else {
				line += "█"
			}
		}

		// Pad line to width (rune-based)
		for utf8.RuneCountInString(line) < contentWidth {
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
	boxStyle := inputBoxStyle.Width(t.width)

	return boxStyle.Render(content)
}
