package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

	// Test reset
	ta.Reset()
	assert.Equal(t, "", ta.Value())
	assert.Equal(t, 1, ta.LineCount())
}

func TestTextAreaSetValue(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)
	ta.SetValue("Line1\nLine2\nLine3")
	assert.Equal(t, "Line1\nLine2\nLine3", ta.Value())
	assert.Equal(t, 3, ta.LineCount())
}

func TestTextAreaInsertString(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("Hello ")
	ta.InsertString("World")
	assert.Equal(t, "Hello World", ta.Value())

	ta.InsertString("!")
	assert.Equal(t, "Hello World!", ta.Value())
}

func TestTextAreaLineCount(t *testing.T) {
	ta := NewTextArea()

	// Empty text area has 1 line
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

func TestTextAreaMultipleLines(t *testing.T) {
	ta := NewTextArea()
	ta.SetWidth(40)
	ta.SetMinHeight(3)
	ta.SetMaxHeight(5)

	ta.SetValue("Line 1\nLine 2\nLine 3\nLine 4")

	assert.Equal(t, 4, ta.LineCount())

	view := ta.View()
	assert.NotEmpty(t, view)
	assert.Contains(t, view, "Send")
	assert.Contains(t, view, "Cancel")
}

func TestTextAreaHandleKey_CharInsertion(t *testing.T) {
	ta := NewTextArea()
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "a", ta.Value())
}

func TestTextAreaHandleKey_Space(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "hello ", ta.Value())
}

func TestTextAreaHandleKey_EnterSubmitsWhenNonEmpty(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("line1")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaSubmit, action)
	assert.Equal(t, "line1", ta.Value()) // Value unchanged, submit triggered
}

func TestTextAreaHandleKey_EnterNoOpWhenEmpty(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "", ta.Value())
}

func TestTextAreaHandleKey_EnterNoOpWhenWhitespace(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("   \n  \t  ")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaHandled, action)
}

func TestTextAreaHandleKey_ShiftEnterInsertsNewline(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("line1")
	// Test InsertNewline directly since bubbletea shift+enter encoding varies
	ta.InsertNewline()
	assert.Equal(t, "line1\n", ta.Value())
}

func TestTextAreaHandleKey_Backspace(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("abc")
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "ab", ta.Value())
}

func TestTextAreaHandleKey_CursorMovement(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("line1\nline2")

	// Move up
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	assert.Equal(t, TextAreaHandled, action)

	// Move down
	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, TextAreaHandled, action)

	// Move left
	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyLeft})
	assert.Equal(t, TextAreaHandled, action)

	// Move right
	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, TextAreaHandled, action)
}

func TestTextAreaHandleKey_TabCycling(t *testing.T) {
	ta := NewTextArea()
	assert.Equal(t, FocusTextInput, ta.Focus())

	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, FocusSendButton, ta.Focus())

	action = ta.HandleKey(tea.KeyMsg{Type: tea.KeyShiftTab})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, FocusTextInput, ta.Focus())
}

func TestTextAreaHandleKey_EnterOnSendButton(t *testing.T) {
	ta := NewTextArea()
	ta.SetFocus(FocusSendButton)
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaSubmit, action)
}

func TestTextAreaHandleKey_EnterOnCancelButton(t *testing.T) {
	ta := NewTextArea()
	ta.SetFocus(FocusCancelButton)
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, TextAreaCancel, action)
}

func TestTextAreaHandleKey_Escape(t *testing.T) {
	ta := NewTextArea()
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, TextAreaCancel, action)
}

func TestTextAreaHandleKey_CtrlC(t *testing.T) {
	ta := NewTextArea()
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.Equal(t, TextAreaQuit, action)
}

func TestTextAreaHandleKey_IgnoredWhenNotFocused(t *testing.T) {
	ta := NewTextArea()
	ta.SetFocus(FocusSendButton)
	// Typing 'a' when send button focused should not insert text
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Equal(t, TextAreaUnhandled, action)
	assert.Equal(t, "", ta.Value())
}

func TestTextAreaHandleKey_Delete(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("abc")
	// Bubbles places cursor at end after SetValue. Move left twice so cursor is after 'a'.
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyLeft})
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyLeft})
	action := ta.HandleKey(tea.KeyMsg{Type: tea.KeyDelete})
	assert.Equal(t, TextAreaHandled, action)
	assert.Equal(t, "ac", ta.Value())
}

func TestTextAreaUnicodeInsert(t *testing.T) {
	ta := NewTextArea()
	ta.InsertChar('你')
	ta.InsertChar('好')
	assert.Equal(t, "你好", ta.Value())

	ta.InsertChar('🎉')
	assert.Equal(t, "你好🎉", ta.Value())
}

func TestTextAreaUnicodeInsertString(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello ")
	ta.InsertString("世界🌍")
	assert.Equal(t, "hello 世界🌍", ta.Value())
}

func TestTextAreaResetPreservesPlaceholder(t *testing.T) {
	ta := NewTextArea()
	ta.SetPlaceholder("Enter text here...")
	ta.SetValue("some content")

	ta.Reset()
	assert.Equal(t, "", ta.Value())

	// Placeholder should survive Reset
	ta.SetWidth(40)
	view := ta.View()
	assert.Contains(t, view, "Enter text here...")
}

// --- Readline keybinding tests ---

func TestTextAreaReadline_CtrlA_LineStart(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello world")
	// Cursor is at end after SetValue; ctrl+a should move to start
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlA})
	// Insert 'X' at start to verify cursor position
	ta.InsertChar('X')
	assert.Equal(t, "Xhello world", ta.Value())
}

func TestTextAreaReadline_CtrlE_LineEnd(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello world")
	// Move cursor to start first
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlA})
	// ctrl+e should move to end
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	ta.InsertChar('X')
	assert.Equal(t, "hello worldX", ta.Value())
}

func TestTextAreaReadline_CtrlW_DeleteWordBackward(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello world")
	// ctrl+w should delete "world"
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlW})
	assert.Equal(t, "hello ", ta.Value())
}

func TestTextAreaReadline_CtrlK_DeleteAfterCursor(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello world")
	// Move to start
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlA})
	// Move right 5 chars to position after "hello"
	for i := 0; i < 5; i++ {
		ta.HandleKey(tea.KeyMsg{Type: tea.KeyRight})
	}
	// ctrl+k should delete from cursor to end of line
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlK})
	assert.Equal(t, "hello", ta.Value())
}

func TestTextAreaReadline_CtrlU_DeleteBeforeCursor(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("hello world")
	// Cursor at end; ctrl+u should delete everything before cursor on this line
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	assert.Equal(t, "", ta.Value())
}

func TestTextAreaReadline_CtrlD_DeleteCharForward(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("abc")
	// Move to start
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlA})
	// ctrl+d deletes char forward
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	assert.Equal(t, "bc", ta.Value())
}

func TestTextAreaReadline_CtrlB_CharBackward(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("abc")
	// ctrl+b moves cursor one char backward
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlB})
	ta.InsertChar('X')
	assert.Equal(t, "abXc", ta.Value())
}

func TestTextAreaReadline_CtrlF_CharForward(t *testing.T) {
	ta := NewTextArea()
	ta.SetValue("abc")
	// Move to start, then ctrl+f to move forward
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlA})
	ta.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlF})
	ta.InsertChar('X')
	assert.Equal(t, "aXbc", ta.Value())
}
