package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestConfirmPrompt_MatchesOptionKey(t *testing.T) {
	cp := NewConfirmPrompt("Delete?", []ConfirmOption{
		{Key: "y", Label: "yes"},
		{Key: "d", Label: "yes + delete branch"},
	})

	result := cp.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	assert.Equal(t, "y", result.Matched)
	assert.False(t, result.Cancelled)
	assert.False(t, result.Quit)
}

func TestConfirmPrompt_MatchesSecondOption(t *testing.T) {
	cp := NewConfirmPrompt("Delete?", []ConfirmOption{
		{Key: "y", Label: "yes"},
		{Key: "d", Label: "yes + delete branch"},
	})

	result := cp.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	assert.Equal(t, "d", result.Matched)
}

func TestConfirmPrompt_EscCancels(t *testing.T) {
	cp := NewConfirmPrompt("Delete?", []ConfirmOption{
		{Key: "y", Label: "yes"},
	})

	result := cp.HandleKey(tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, result.Cancelled)
	assert.Equal(t, "", result.Matched)
}

func TestConfirmPrompt_CtrlCQuits(t *testing.T) {
	cp := NewConfirmPrompt("Delete?", []ConfirmOption{
		{Key: "y", Label: "yes"},
	})

	result := cp.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.True(t, result.Quit)
	assert.Equal(t, "", result.Matched)
}

func TestConfirmPrompt_UnrecognizedKeyIgnored(t *testing.T) {
	cp := NewConfirmPrompt("Delete?", []ConfirmOption{
		{Key: "y", Label: "yes"},
	})

	result := cp.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	assert.Equal(t, "", result.Matched)
	assert.False(t, result.Cancelled)
	assert.False(t, result.Quit)
}

func TestConfirmPrompt_ViewRendersMessage(t *testing.T) {
	cp := NewConfirmPrompt("Stop session 'abc'?", []ConfirmOption{
		{Key: "y", Label: "yes"},
	})

	view := cp.View(NewStyles(Dark))
	assert.Contains(t, view, "Stop session 'abc'?")
	assert.Contains(t, view, "[y] yes")
	assert.Contains(t, view, "[Esc] cancel")
}

func TestConfirmPrompt_ViewRendersMultipleOptions(t *testing.T) {
	cp := NewConfirmPrompt("Delete worktree 'feat'?", []ConfirmOption{
		{Key: "y", Label: "yes, keep branch"},
		{Key: "d", Label: "yes + delete branch"},
	})

	view := cp.View(NewStyles(Dark))
	assert.Contains(t, view, "Delete worktree 'feat'?")
	assert.Contains(t, view, "[y] yes, keep branch")
	assert.Contains(t, view, "[d] yes + delete branch")
	assert.Contains(t, view, "[Esc] cancel")
}
