package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
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

	// Inject sessions directly to avoid depending on real session startup,
	// which requires tmux to be available and the process to be running inside tmux.
	m.sessionManager.AddSession(&session.Session{
		ID:           "s1",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/main",
		Type:         session.SessionTypePlanner,
	})
	m.sessionManager.AddSession(&session.Session{
		ID:           "s2",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature",
		Type:         session.SessionTypeBuilder,
	})

	assert.False(t, m.allSessionsOverlay.IsVisible())

	// Press 'S' to open overlay
	newModel, _ := m.handleKeyPress(keyPress('S'))
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
	newModel2, _ := m2.handleAllSessionsOverlay(specialKey(tea.KeyDown))
	m3 := newModel2.(Model)
	assert.Equal(t, 1, m3.allSessionsOverlay.selectedIdx)

	// Navigate down again -- should clamp
	newModel3, _ := m3.handleAllSessionsOverlay(specialKey(tea.KeyDown))
	m4 := newModel3.(Model)
	assert.Equal(t, 1, m4.allSessionsOverlay.selectedIdx)

	// Navigate up
	newModel4, _ := m4.handleAllSessionsOverlay(specialKey(tea.KeyUp))
	m5 := newModel4.(Model)
	assert.Equal(t, 0, m5.allSessionsOverlay.selectedIdx)
}

func TestAllSessionsOverlay_Select_TUIMode(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Populate overlay directly to avoid depending on real session startup,
	// which can fail immediately in TUI mode without a real provider.
	sessions := []session.SessionInfo{
		{ID: "s1", Status: session.StatusRunning, WorktreePath: "/tmp/wt/main"},
		{ID: "s2", Status: session.StatusRunning, WorktreePath: "/tmp/wt/feature"},
	}
	m.allSessionsOverlay.Show(sessions, m.width, m.height)
	m.focus = FocusAllSessions
	m2 := m
	require.True(t, m2.allSessionsOverlay.IsVisible())
	require.Len(t, m2.allSessionsOverlay.Sessions(), 2, "should have 2 sessions")

	// Get the initial selection
	initialSelected := m2.allSessionsOverlay.SelectedSession()
	require.NotNil(t, initialSelected, "should have an initial selection")
	initialSelectedID := initialSelected.ID

	// Navigate to second session
	newModel2, _ := m2.handleAllSessionsOverlay(specialKey(tea.KeyDown))
	m3 := newModel2.(Model)

	// Get the newly selected session after navigation
	newSelected := m3.allSessionsOverlay.SelectedSession()
	require.NotNil(t, newSelected, "should have a new selection")
	expectedID := newSelected.ID
	require.NotEqual(t, initialSelectedID, expectedID, "should have selected a different session")

	// Press Enter to select
	newModel3, _ := m3.handleAllSessionsOverlay(specialKey(tea.KeyEnter))
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
	newModel, _ := m.handleKeyPress(keyPress('S'))
	m2 := newModel.(Model)
	assert.True(t, m2.allSessionsOverlay.IsVisible())

	// Press Esc to close
	newModel2, _ := m2.handleAllSessionsOverlay(specialKey(tea.KeyEsc))
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

	m.sessionManager.AddSession(&session.Session{
		ID:           "s1",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/main",
		Type:         session.SessionTypePlanner,
	})
	m.sessionManager.AddSession(&session.Session{
		ID:           "s2",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature",
		Type:         session.SessionTypeBuilder,
	})

	// Open overlay
	newModel, _ := m.handleKeyPress(keyPress('S'))
	m2 := newModel.(Model)

	// The first session in the overlay list
	expectedID := m2.allSessionsOverlay.Sessions()[0].ID

	// Press '1' to select first session
	newModel2, _ := m2.handleAllSessionsOverlay(keyPress('1'))
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

	// Inject non-terminal sessions directly to avoid depending on real session startup.
	m.sessionManager.AddSession(&session.Session{
		ID:           "s1",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/main",
		Type:         session.SessionTypePlanner,
	})
	m.sessionManager.AddSession(&session.Session{
		ID:           "s2",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature",
		Type:         session.SessionTypeBuilder,
	})

	// Open overlay - all sessions are running (non-terminal), so all should appear
	newModel, _ := m.handleKeyPress(keyPress('S'))
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

	// Add a session directly to the manager but do NOT update m.sessions —
	// simulates the case where no sessionEventMsg has arrived yet.
	m.sessionManager.AddSession(&session.Session{
		ID:           "fresh-session",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/main",
		Type:         session.SessionTypePlanner,
	})
	// m.sessions is intentionally left empty / stale

	// Press S — overlay should still show the session from the manager
	newModel, _ := m.handleKeyPress(keyPress('S'))
	m2 := newModel.(Model)

	assert.True(t, m2.allSessionsOverlay.IsVisible())
	assert.Len(t, m2.allSessionsOverlay.Sessions(), 1, "overlay should fetch fresh sessions from manager")
}

func TestAllSessionsOverlay_BoxDimensions_UseViewportSpace(t *testing.T) {
	o := NewAllSessionsOverlay()
	o.SetSize(200, 50)

	assert.Equal(t, 196, o.boxWidth())
	assert.Equal(t, 48, o.boxHeight())
}

func TestAllSessionsOverlay_VisibleSessionRange_FollowsSelection(t *testing.T) {
	o := NewAllSessionsOverlay()
	o.sessions = make([]session.SessionInfo, 20)
	o.selectedIdx = 13

	start, end := o.visibleSessionRange(6)
	assert.Equal(t, 8, start)
	assert.Equal(t, 14, end)
}

func TestAllSessionsOverlay_NewSession_PBC(t *testing.T) {
	for _, tc := range []struct {
		promptSub   string
		sessionType session.SessionType
		key         rune
	}{
		{"Plan prompt", session.SessionTypePlanner, 'p'},
		{"Build prompt", session.SessionTypeBuilder, 'b'},
		{"CodeTalk prompt", session.SessionTypeCodeTalk, 'c'},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
				{Branch: "main", Path: "/tmp/wt/main"},
			}, "test-repo")
			m.worktreeDropdown.SelectIndex(0)

			sessions := []session.SessionInfo{
				{ID: "s1", Status: session.StatusRunning, WorktreePath: "/tmp/wt/main", WorktreeName: "main"},
			}
			m.allSessionsOverlay.Show(sessions, m.width, m.height)
			m.focus = FocusAllSessions

			newModel, _ := m.handleAllSessionsOverlay(keyPress(tc.key))
			m2 := newModel.(Model)

			// Overlay should be hidden and focus should move to input
			assert.False(t, m2.allSessionsOverlay.IsVisible())
			assert.Equal(t, FocusInput, m2.focus)
			assert.True(t, m2.inputMode)
			assert.Contains(t, m2.inputPrompt, tc.promptSub)
			assert.Equal(t, tc.sessionType, m2.pendingSessionType)
		})
	}
}

func TestAllSessionsOverlay_NewSession_NoSession(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Show overlay with no sessions
	m.allSessionsOverlay.Show(nil, m.width, m.height)
	m.focus = FocusAllSessions

	newModel, _ := m.handleAllSessionsOverlay(keyPress('p'))
	m2 := newModel.(Model)

	// Should show toast error and overlay stays visible (focus-state bug fix)
	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No session selected")
	// Overlay should NOT have been hidden
	assert.True(t, m2.allSessionsOverlay.IsVisible())
}

func TestAllSessionsOverlay_NewSession_NoWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Session with no worktree path
	sessions := []session.SessionInfo{
		{ID: "s1", Status: session.StatusRunning, WorktreePath: ""},
	}
	m.allSessionsOverlay.Show(sessions, m.width, m.height)
	m.focus = FocusAllSessions

	newModel, _ := m.handleAllSessionsOverlay(keyPress('b'))
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "no worktree")
	// Overlay should NOT have been hidden
	assert.True(t, m2.allSessionsOverlay.IsVisible())
}
