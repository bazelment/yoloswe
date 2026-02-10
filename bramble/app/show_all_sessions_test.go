package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestShowAllSessions_Toggle(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	assert.False(t, m.showAllSessions)

	// Press 'S' to toggle on
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)

	assert.True(t, m2.showAllSessions)
	assert.Equal(t, 0, m2.selectedSessionIndex)
	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "all active sessions")

	// Press 'S' again to toggle off
	newModel2, _ := m2.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m3 := newModel2.(Model)

	assert.False(t, m3.showAllSessions)
	assert.Contains(t, m3.toasts.toasts[1].Message, "current worktree sessions")
}

func TestShowAllSessions_TUIMode_ShowsToast(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Press 'S' in TUI mode -- should show info toast, not toggle
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)

	assert.False(t, m2.showAllSessions)
	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Alt-S")
}

func TestVisibleSessions_CurrentWorktreeOnly(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "main session")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "feature session")
	require.NoError(t, err)
	m.sessions = m.sessionManager.GetAllSessions()

	// Default: only current worktree sessions
	sessions := m.visibleSessions()
	assert.Len(t, sessions, 1)
	assert.Equal(t, "/tmp/wt/main", sessions[0].WorktreePath)
}

func TestVisibleSessions_AllNonTerminal(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "main session")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "feature session")
	require.NoError(t, err)
	m.sessions = m.sessionManager.GetAllSessions()

	// Toggle to show all — both running sessions should be visible
	m.showAllSessions = true
	sessions := m.visibleSessions()
	assert.Len(t, sessions, 2)

	// Verify filtering logic: visibleSessions only returns non-terminal.
	// All started sessions are in Running state, so all should appear.
	for _, s := range sessions {
		assert.False(t, s.Status.IsTerminal(), "visibleSessions should exclude terminal sessions")
	}
}

func TestShowAllSessions_ResetOnWorktreeSwitch(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Toggle on
	m.showAllSessions = true
	m.selectedSessionIndex = 3

	// Simulate worktree dropdown selection
	m.focus = FocusWorktreeDropdown
	m.worktreeDropdown.Open()
	m.worktreeDropdown.MoveSelection(1) // select "feature"

	newModel, _ := m.handleDropdownMode(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := newModel.(Model)

	assert.False(t, m2.showAllSessions)
	assert.Equal(t, 0, m2.selectedSessionIndex)
}

func TestShowAllSessions_QuickSwitch_TmuxMode(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "main session")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "feature session")
	require.NoError(t, err)
	m.sessions = m.sessionManager.GetAllSessions()

	// Toggle to show all sessions
	m.showAllSessions = true

	// Press '2' to select second session (from other worktree)
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m2 := newModel.(Model)
	assert.Equal(t, 1, m2.selectedSessionIndex)

	// Press '3' -- out of range
	newModel2, _ := m2.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	m3 := newModel2.(Model)
	assert.True(t, m3.toasts.HasToasts())
}

func TestShowAllSessions_HelpContainsSBinding(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	sections := buildHelpSections(&m)
	found := false
	for _, s := range sections {
		for _, b := range s.Bindings {
			if b.Key == "S" {
				found = true
			}
		}
	}
	assert.True(t, found, "Help sections should contain 'S' keybinding")
}

func TestShowAllSessions_NavigateDown(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "s1")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "s2")
	require.NoError(t, err)
	m.sessions = m.sessionManager.GetAllSessions()

	m.showAllSessions = true
	m.selectedSessionIndex = 0

	// Navigate down
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	m2 := newModel.(Model)
	assert.Equal(t, 1, m2.selectedSessionIndex)

	// Navigate down again -- should clamp
	newModel2, _ := m2.handleKeyPress(tea.KeyMsg{Type: tea.KeyDown})
	m3 := newModel2.(Model)
	assert.Equal(t, 1, m3.selectedSessionIndex)
}
