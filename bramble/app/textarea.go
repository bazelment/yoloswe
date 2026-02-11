package app

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
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

// TextArea is a multi-line text input component backed by charmbracelet/bubbles
// textarea. It adds focus-cycling between the text input and Send/Cancel
// buttons, and maps Enter/Esc/Ctrl+C to action enums the caller can act on.
type TextArea struct {
	inner       textarea.Model
	pendingCmd  tea.Cmd // command from last inner.Update, retrieved via Cmd()
	prompt      string
	placeholder string
	sendLabel   string
	cancelLabel string
	width       int
	maxHeight   int
	minHeight   int
	focus       TextAreaFocus
}

// NewTextArea creates a new multi-line text input.
func NewTextArea() *TextArea {
	inner := textarea.New()
	inner.ShowLineNumbers = false
	inner.CharLimit = 0
	inner.Prompt = ""
	inner.MaxHeight = 10
	inner.SetHeight(3)
	inner.SetWidth(60)
	inner.FocusedStyle = textarea.Style{
		Base:        lipgloss.NewStyle(),
		Placeholder: lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		Text:        lipgloss.NewStyle(),
		CursorLine:  lipgloss.NewStyle(),
	}
	inner.BlurredStyle = inner.FocusedStyle

	// Rebind InsertNewline from enter → shift+enter (Bramble convention)
	inner.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter"),
		key.WithHelp("shift+enter", "insert newline"),
	)
	// Disable clipboard paste (ctrl+v) to avoid clipboard dependency issues
	inner.KeyMap.Paste.SetEnabled(false)

	inner.Focus()

	return &TextArea{
		inner:       inner,
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
	if f == FocusTextInput {
		t.pendingCmd = t.inner.Focus()
	} else {
		t.inner.Blur()
	}
}

// CycleForward moves focus to the next element (Tab).
func (t *TextArea) CycleForward() {
	switch t.focus {
	case FocusTextInput:
		t.focus = FocusSendButton
		t.inner.Blur()
	case FocusSendButton:
		t.focus = FocusCancelButton
	case FocusCancelButton:
		t.focus = FocusTextInput
		t.pendingCmd = t.inner.Focus()
	}
}

// CycleBackward moves focus to the previous element (Shift+Tab).
func (t *TextArea) CycleBackward() {
	switch t.focus {
	case FocusTextInput:
		t.focus = FocusCancelButton
		t.inner.Blur()
	case FocusSendButton:
		t.focus = FocusTextInput
		t.pendingCmd = t.inner.Focus()
	case FocusCancelButton:
		t.focus = FocusSendButton
	}
}

// SetWidth sets the text area width.
func (t *TextArea) SetWidth(w int) {
	t.width = w
	t.inner.SetWidth(w - 4) // Account for padding and border
}

// SetMaxHeight sets the maximum height.
func (t *TextArea) SetMaxHeight(h int) {
	t.maxHeight = h
	t.inner.MaxHeight = h - 2 // Reserve for prompt and button row
	if t.inner.MaxHeight < 1 {
		t.inner.MaxHeight = 1
	}
}

// SetMinHeight sets the minimum height.
func (t *TextArea) SetMinHeight(h int) {
	t.minHeight = h
}

// SetPrompt sets the prompt label shown above the text area.
func (t *TextArea) SetPrompt(p string) {
	t.prompt = p
}

// SetPlaceholder sets the placeholder text shown when the value is empty.
func (t *TextArea) SetPlaceholder(p string) {
	t.placeholder = p
	t.inner.Placeholder = p
}

// SetValue sets the text content.
func (t *TextArea) SetValue(s string) {
	t.inner.SetValue(s)
}

// Value returns the current text content.
func (t *TextArea) Value() string {
	return t.inner.Value()
}

// Reset clears the text area content and resets focus to text input.
func (t *TextArea) Reset() {
	t.inner.Reset()
	t.focus = FocusTextInput
	t.pendingCmd = t.inner.Focus()
	// Preserve placeholder
	t.inner.Placeholder = t.placeholder
}

// InsertChar inserts a character at the cursor position.
func (t *TextArea) InsertChar(r rune) {
	t.inner.InsertRune(r)
}

// InsertString inserts a string at the cursor position.
func (t *TextArea) InsertString(s string) {
	t.inner.InsertString(s)
}

// InsertNewline inserts a newline at the cursor position.
func (t *TextArea) InsertNewline() {
	t.inner.InsertRune('\n')
}

// LineCount returns the number of lines.
func (t *TextArea) LineCount() int {
	return t.inner.LineCount()
}

// Cmd returns and clears any pending tea.Cmd produced by the last HandleKey
// call (e.g. cursor blink scheduling from the inner bubbles textarea).
// Callers should batch this into their returned command.
func (t *TextArea) Cmd() tea.Cmd {
	cmd := t.pendingCmd
	t.pendingCmd = nil
	return cmd
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

	case "ctrl+enter":
		return TextAreaSubmit

	case "enter":
		switch t.focus {
		case FocusTextInput:
			if strings.TrimSpace(t.Value()) != "" {
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

	case "ctrl+c":
		return TextAreaQuit

	default:
		if t.focus == FocusTextInput {
			var cmd tea.Cmd
			t.inner, cmd = t.inner.Update(msg)
			t.pendingCmd = cmd
			return TextAreaHandled
		}
		return TextAreaUnhandled
	}
}

// View renders the text area.
func (t *TextArea) View(s *Styles) string {
	contentWidth := t.width - 4 // Account for padding and border
	if contentWidth < 1 {
		contentWidth = 1
	}

	// Configure inner dimensions before rendering
	t.inner.SetWidth(contentWidth)
	lineCount := t.inner.LineCount()
	desiredHeight := lineCount
	if desiredHeight < t.minHeight {
		desiredHeight = t.minHeight
	}
	maxInnerHeight := t.maxHeight - 2 // Reserve for prompt and button row
	if maxInnerHeight < 1 {
		maxInnerHeight = 1
	}
	if desiredHeight > maxInnerHeight {
		desiredHeight = maxInnerHeight
	}
	t.inner.SetHeight(desiredHeight)

	var b strings.Builder

	// Prompt
	if t.prompt != "" {
		b.WriteString(s.Dim.Render(t.prompt))
		b.WriteString("\n")
	}

	// Render the bubbles textarea
	b.WriteString(t.inner.View())
	b.WriteString("\n")

	// Render buttons with focus indication
	var sendBtn, cancelBtn string
	if t.focus == FocusSendButton {
		sendBtn = s.Selected.Render("[ " + t.sendLabel + " ]")
	} else {
		sendBtn = s.Dim.Render("[ " + t.sendLabel + " ]")
	}
	if t.focus == FocusCancelButton {
		cancelBtn = s.Selected.Render("[ " + t.cancelLabel + " ]")
	} else {
		cancelBtn = s.Dim.Render("[ " + t.cancelLabel + " ]")
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
	boxStyle := s.InputBox.Width(t.width)

	return boxStyle.Render(content)
}
