package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestQuickSwitch_SwitchesToSession(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Start two sessions
	sess1, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "first prompt", "")
	require.NoError(t, err)
	sess2, err := m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/main", "second prompt", "")
	require.NoError(t, err)

	m.sessions = m.sessionManager.GetAllSessions()
	m.updateSessionDropdown()

	// Get the actual sessions in order
	liveSessions := m.currentWorktreeSessions()
	require.Len(t, liveSessions, 2)

	// Press '1' to switch to first session in the list
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m2 := newModel.(Model)

	// Should switch to the first session in currentWorktreeSessions
	assert.Equal(t, liveSessions[0].ID, m2.viewingSessionID)
	// Viewing session should be one of the two we created
	assert.True(t, m2.viewingSessionID == sess1 || m2.viewingSessionID == sess2)
}

func TestQuickSwitch_OutOfRange_ShowsToast(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Start only one session
	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "first prompt", "")
	require.NoError(t, err)

	m.sessions = m.sessionManager.GetAllSessions()
	m.updateSessionDropdown()

	// Press '5' when only 1 session exists
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'5'}})
	m2 := newModel.(Model)

	// Should show toast
	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No session #5")
}

func TestQuickSwitch_NoSessions_ShowsToast(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.updateSessionDropdown()

	// Press '1' when no sessions exist
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m2 := newModel.(Model)

	// Should show toast
	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No session #1")
}

func TestQuickSwitch_TmuxMode_SelectsIndex(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Start three sessions
	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "first prompt", "")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/main", "second prompt", "")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "third prompt", "")
	require.NoError(t, err)

	m.sessions = m.sessionManager.GetAllSessions()
	m.selectedSessionIndex = 0

	// Press '3' to select third session in list (index 2)
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	m2 := newModel.(Model)

	// Should set selectedSessionIndex to 2
	assert.Equal(t, 2, m2.selectedSessionIndex)
}

func TestQuickSwitch_SessionDropdownShowsNumbers(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Start several sessions to test numbering
	for i := 0; i < 5; i++ {
		_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "prompt", "")
		require.NoError(t, err)
	}

	m.sessions = m.sessionManager.GetAllSessions()
	m.updateSessionDropdown()

	// Check that dropdown items have number prefixes
	items := m.sessionDropdown.effectiveItems()
	require.Len(t, items, 5)

	// First 5 sessions should have numbers 1-5
	assert.Contains(t, items[0].Label, "1. ")
	assert.Contains(t, items[1].Label, "2. ")
	assert.Contains(t, items[2].Label, "3. ")
	assert.Contains(t, items[3].Label, "4. ")
	assert.Contains(t, items[4].Label, "5. ")
}
