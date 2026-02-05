package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTextAreaBasicOperations(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)

	// Test initial state
	assert.Equal(t, "", ta.Value())
	assert.Equal(t, 1, ta.LineCount())

	// Test character insertion
	ta.InsertChar('H')
	ta.InsertChar('e')
	ta.InsertChar('l')
	ta.InsertChar('l')
	ta.InsertChar('o')
	assert.Equal(t, "Hello", ta.Value())

	// Test newline
	ta.InsertNewline()
	ta.InsertString("World")
	assert.Equal(t, "Hello\nWorld", ta.Value())
	assert.Equal(t, 2, ta.LineCount())

	// Test backspace
	ta.DeleteChar()
	assert.Equal(t, "Hello\nWorl", ta.Value())

	// Test reset
	ta.Reset()
	assert.Equal(t, "", ta.Value())
	assert.Equal(t, 1, ta.LineCount())
}

func TestTextAreaCursorMovement(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)
	ta.SetValue("Line1\nLine2\nLine3")

	// Cursor should be at end (after "Line3")
	// Insert X at end
	ta.InsertChar('X')
	assert.Equal(t, "Line1\nLine2\nLine3X", ta.Value())

	// Move to start of line (start of "Line3X")
	ta.MoveCursorToLineStart()
	ta.InsertChar('Y')
	assert.Equal(t, "Line1\nLine2\nYLine3X", ta.Value())

	// Move up - cursor is now at position 1 in line 3, move to line 2 position 1
	ta.MoveCursorUp()
	ta.InsertChar('Z')
	assert.Equal(t, "Line1\nLZine2\nYLine3X", ta.Value())
}

func TestTextAreaCursorUpDown(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)
	ta.SetValue("abc\ndefgh\nij")

	// Cursor starts at end (line 3, col 2)
	ta.MoveCursorUp()
	// Should be on line 2, col 2 (or end of line if shorter)
	ta.InsertChar('X')
	assert.Equal(t, "abc\ndeXfgh\nij", ta.Value())
}

func TestTextAreaDeleteForward(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("Hello World")

	// Move to position 5 (after "Hello")
	for i := 0; i < 6; i++ {
		ta.MoveCursorLeft()
	}
	ta.DeleteCharForward()
	assert.Equal(t, "HelloWorld", ta.Value())
}

func TestTextAreaWordWrap(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(20)
	ta.SetMaxHeight(10)

	// Add a long line that should wrap
	ta.SetValue("This is a very long line that should wrap")

	// The view should render without panic
	view := ta.View()
	assert.NotEmpty(t, view)

	// Verify line count shows correctly
	assert.Contains(t, view, "line")
}

func TestTextAreaMultipleLines(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)
	ta.SetMinHeight(3)
	ta.SetMaxHeight(5)

	ta.SetValue("Line 1\nLine 2\nLine 3\nLine 4")

	assert.Equal(t, 4, ta.LineCount())

	view := ta.View()
	assert.NotEmpty(t, view)
	// Check for button labels
	assert.Contains(t, view, "Send")
	assert.Contains(t, view, "Cancel")
}

func TestTextAreaInsertString(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("Hello ")
	ta.InsertString("World")
	assert.Equal(t, "Hello World", ta.Value())

	// Insert at cursor position (which is at the end)
	ta.InsertString("!")
	assert.Equal(t, "Hello World!", ta.Value())
}

func TestTextAreaLineCount(t *testing.T) {
	ta := NewTextArea()

	// Empty text area has 1 line (the empty line)
	assert.Equal(t, 1, ta.LineCount())

	ta.SetValue("one")
	assert.Equal(t, 1, ta.LineCount())

	ta.SetValue("one\ntwo")
	assert.Equal(t, 2, ta.LineCount())

	ta.SetValue("one\ntwo\nthree\nfour\nfive")
	assert.Equal(t, 5, ta.LineCount())
}

func TestTextAreaPrompt(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)
	ta.SetPrompt("Enter text:")
	ta.SetValue("Hello")

	view := ta.View()
	assert.Contains(t, view, "Enter text:")
	assert.Contains(t, view, "Hello")
}

func TestTextAreaFocusCycling(t *testing.T) {
	ta := NewTextArea()

	// Initial focus is text input
	assert.Equal(t, FocusTextInput, ta.Focus())

	// Tab cycles forward
	ta.CycleForward()
	assert.Equal(t, FocusSendButton, ta.Focus())

	ta.CycleForward()
	assert.Equal(t, FocusCancelButton, ta.Focus())

	ta.CycleForward()
	assert.Equal(t, FocusTextInput, ta.Focus())

	// Shift+Tab cycles backward
	ta.CycleBackward()
	assert.Equal(t, FocusCancelButton, ta.Focus())

	ta.CycleBackward()
	assert.Equal(t, FocusSendButton, ta.Focus())

	ta.CycleBackward()
	assert.Equal(t, FocusTextInput, ta.Focus())
}

func TestTextAreaCustomLabels(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)
	ta.SetLabels("Continue", "Back")

	view := ta.View()
	assert.Contains(t, view, "Continue")
	assert.Contains(t, view, "Back")
}

func TestTextAreaViewRendering(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(30)
	ta.SetMinHeight(3)
	ta.SetMaxHeight(10)
	ta.SetPrompt("Test prompt:")
	ta.SetValue("Test content")

	view := ta.View()

	// Should contain the prompt
	assert.Contains(t, view, "Test prompt:")

	// Should contain the button labels
	assert.Contains(t, view, "Send")
	assert.Contains(t, view, "Cancel")

	// Should have border characters (from lipgloss)
	assert.True(t, strings.Contains(view, "─") || strings.Contains(view, "│"))
}

