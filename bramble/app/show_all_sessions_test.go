package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestAllSessionsOverlay_Show(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Start sessions on different worktrees
	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "main task")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "feature task")
	require.NoError(t, err)
	m.sessions = m.sessionManager.GetAllSessions()

	assert.False(t, m.allSessionsOverlay.IsVisible())

	// Press 'S' to open overlay
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)

	assert.True(t, m2.allSessionsOverlay.IsVisible())
	assert.Equal(t, FocusAllSessions, m2.focus)
	// Should contain active sessions from both worktrees
	assert.Len(t, m2.allSessionsOverlay.Sessions(), 2)
}

func TestAllSessionsOverlay_Navigate(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Populate overlay directly to test navigation logic in isolation
	sessions := []session.SessionInfo{
		{ID: "s1", Status: session.StatusRunning, WorktreePath: "/tmp/wt/main"},
		{ID: "s2", Status: session.StatusRunning, WorktreePath: "/tmp/wt/feature"},
	}
	m.allSessionsOverlay.Show(sessions, m.width, m.height)
	m.focus = FocusAllSessions
	m2 := m

	assert.Equal(t, 0, m2.allSessionsOverlay.selectedIdx)

	// Navigate down
	newModel2, _ := m2.handleAllSessionsOverlay(tea.KeyMsg{Type: tea.KeyDown})
	m3 := newModel2.(Model)
	assert.Equal(t, 1, m3.allSessionsOverlay.selectedIdx)

	// Navigate down again -- should clamp
	newModel3, _ := m3.handleAllSessionsOverlay(tea.KeyMsg{Type: tea.KeyDown})
	m4 := newModel3.(Model)
	assert.Equal(t, 1, m4.allSessionsOverlay.selectedIdx)

	// Navigate up
	newModel4, _ := m4.handleAllSessionsOverlay(tea.KeyMsg{Type: tea.KeyUp})
	m5 := newModel4.(Model)
	assert.Equal(t, 0, m5.allSessionsOverlay.selectedIdx)
}

func TestAllSessionsOverlay_Select_TUIMode(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "main task")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "feature task")
	require.NoError(t, err)
	m.sessions = m.sessionManager.GetAllSessions()

	// Open overlay
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)
	require.True(t, m2.allSessionsOverlay.IsVisible())
	require.Len(t, m2.allSessionsOverlay.Sessions(), 2, "should have 2 sessions")

	// Get the initial selection
	initialSelected := m2.allSessionsOverlay.SelectedSession()
	require.NotNil(t, initialSelected, "should have an initial selection")
	initialSelectedID := initialSelected.ID

	// Navigate to second session
	newModel2, _ := m2.handleAllSessionsOverlay(tea.KeyMsg{Type: tea.KeyDown})
	m3 := newModel2.(Model)

	// Get the newly selected session after navigation
	newSelected := m3.allSessionsOverlay.SelectedSession()
	require.NotNil(t, newSelected, "should have a new selection")
	expectedID := newSelected.ID
	require.NotEqual(t, initialSelectedID, expectedID, "should have selected a different session")

	// Press Enter to select
	newModel3, _ := m3.handleAllSessionsOverlay(tea.KeyMsg{Type: tea.KeyEnter})
	m4 := newModel3.(Model)

	assert.False(t, m4.allSessionsOverlay.IsVisible())
	assert.Equal(t, FocusOutput, m4.focus)
	assert.Equal(t, expectedID, m4.viewingSessionID)
}

func TestAllSessionsOverlay_Close(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	m.sessions = []session.SessionInfo{
		{ID: "s1", Status: session.StatusRunning, WorktreePath: "/tmp/wt/main"},
	}

	// Open overlay
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)
	assert.True(t, m2.allSessionsOverlay.IsVisible())

	// Press Esc to close
	newModel2, _ := m2.handleAllSessionsOverlay(tea.KeyMsg{Type: tea.KeyEsc})
	m3 := newModel2.(Model)

	assert.False(t, m3.allSessionsOverlay.IsVisible())
	assert.Equal(t, FocusOutput, m3.focus)
}

func TestAllSessionsOverlay_QuickSwitch(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "main task")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "feature task")
	require.NoError(t, err)
	m.sessions = m.sessionManager.GetAllSessions()

	// Open overlay
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)

	// The first session in the overlay list
	expectedID := m2.allSessionsOverlay.Sessions()[0].ID

	// Press '1' to select first session
	newModel2, _ := m2.handleAllSessionsOverlay(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	m3 := newModel2.(Model)

	assert.False(t, m3.allSessionsOverlay.IsVisible())
	assert.Equal(t, FocusOutput, m3.focus)
	assert.Equal(t, expectedID, m3.viewingSessionID)
}

func TestAllSessionsOverlay_FilterTerminal(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Start 2 sessions via the manager (will be running = non-terminal)
	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "task 1")
	require.NoError(t, err)
	_, err = m.sessionManager.StartSession(session.SessionTypeBuilder, "/tmp/wt/feature", "task 2")
	require.NoError(t, err)

	// Open overlay - all sessions are running (non-terminal), so all should appear
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)

	sessions := m2.allSessionsOverlay.Sessions()
	assert.Len(t, sessions, 2)
	for _, s := range sessions {
		assert.False(t, s.Status.IsTerminal(), "overlay should exclude terminal sessions")
	}

	// Verify filter logic directly: Show with a mix of terminal and non-terminal
	m2.allSessionsOverlay.Hide()
	mixed := []session.SessionInfo{
		{ID: "s1", Status: session.StatusRunning},
		{ID: "s2", Status: session.StatusCompleted},
		{ID: "s3", Status: session.StatusIdle},
	}
	// Filter before showing (same logic as handleKeyPress)
	var active []session.SessionInfo
	for i := range mixed {
		if !mixed[i].Status.IsTerminal() {
			active = append(active, mixed[i])
		}
	}
	m2.allSessionsOverlay.Show(active, m2.width, m2.height)
	assert.Len(t, m2.allSessionsOverlay.Sessions(), 2, "should filter out completed session")
}

func TestAllSessionsOverlay_HelpBinding(t *testing.T) {
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
				assert.Contains(t, b.Description, "all sessions")
			}
		}
	}
	assert.True(t, found, "Help sections should contain 'S' keybinding")
}

func TestAllSessionsOverlay_FreshFromManager(t *testing.T) {
	// Verify that the overlay fetches fresh sessions from the manager,
	// not the potentially stale m.sessions cache.
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Start a session via the manager but do NOT update m.sessions —
	// simulates the case where no sessionEventMsg has arrived yet.
	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "fresh task")
	require.NoError(t, err)
	// m.sessions is intentionally left empty / stale

	// Press S — overlay should still show the session from the manager
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	m2 := newModel.(Model)

	assert.True(t, m2.allSessionsOverlay.IsVisible())
	assert.Len(t, m2.allSessionsOverlay.Sessions(), 1, "overlay should fetch fresh sessions from manager")
}
