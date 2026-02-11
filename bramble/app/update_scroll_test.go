package app

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestScrollPositionPreservedOnSessionSwitch(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, worktrees, 80, 24)
	m.worktreeDropdown.SelectIndex(0)

	// Create two sessions
	sessionA, _ := mgr.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "session A")
	sessionB, _ := mgr.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "session B")

	// View session A and scroll
	m.viewingSessionID = sessionA
	m.scrollOffset = 10

	// Update dropdown items to include both sessions
	m.updateSessionDropdown()

	// Select session B via dropdown (simulate user selecting from dropdown)
	// Find index of session B
	for i, item := range m.sessionDropdown.items {
		if item.ID == string(sessionB) {
			m.sessionDropdown.SelectIndex(i)
			break
		}
	}

	// Trigger Enter key in dropdown mode
	m.focus = FocusSessionDropdown
	newModel, _ := m.handleDropdownMode(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := newModel.(Model)

	// Check that session A's scroll was saved
	assert.Equal(t, 10, m2.scrollPositions[sessionA])
	// Check that we're now viewing session B
	assert.Equal(t, sessionB, m2.viewingSessionID)
	// Check that scroll was reset for session B (or restored to 0)
	assert.Equal(t, 0, m2.scrollOffset)

	// Switch back to session A
	for i, item := range m2.sessionDropdown.items {
		if item.ID == string(sessionA) {
			m2.sessionDropdown.SelectIndex(i)
			break
		}
	}
	m2.focus = FocusSessionDropdown
	m2.sessionDropdown.Open()
	newModel2, _ := m2.handleDropdownMode(tea.KeyMsg{Type: tea.KeyEnter})
	m3 := newModel2.(Model)

	// Check that scroll was restored for session A
	assert.Equal(t, 10, m3.scrollOffset)
	assert.Equal(t, sessionA, m3.viewingSessionID)
}

func TestScrollPositionClearedOnWorktreeSwitch(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, worktrees, 80, 24)
	m.worktreeDropdown.SelectIndex(0)
	m.updateSessionDropdown()

	// Create session on worktree 1
	sessionA, _ := mgr.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "session A")
	m.viewingSessionID = sessionA
	m.scrollOffset = 15

	// Switch to worktree 2
	m.worktreeDropdown.SelectIndex(1)
	m.focus = FocusWorktreeDropdown
	m.worktreeDropdown.Open()
	newModel, _ := m.handleDropdownMode(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := newModel.(Model)

	// Check that session A's scroll was saved
	assert.Equal(t, 15, m2.scrollPositions[sessionA])
	// Check that viewing session was cleared
	assert.Equal(t, session.SessionID(""), m2.viewingSessionID)
	// Check that scroll was reset
	assert.Equal(t, 0, m2.scrollOffset)
}

func TestNewSessionStartsAtBottom(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, worktrees, 80, 24)
	m.worktreeDropdown.SelectIndex(0)

	// Create and view session A
	sessionA, _ := mgr.StartSession(session.SessionTypePlanner, "/tmp/wt/main", "session A")
	m.viewingSessionID = sessionA
	m.scrollOffset = 20

	// Start a new session
	newModel, _ := m.startSession(session.SessionTypePlanner, "new session")
	m2 := newModel.(Model)

	// Check that old session's scroll was saved
	assert.Equal(t, 20, m2.scrollPositions[sessionA])
	// Check that new session starts at bottom (scroll = 0)
	assert.Equal(t, 0, m2.scrollOffset)
	// Check that we're viewing the new session
	assert.NotEqual(t, sessionA, m2.viewingSessionID)
}
