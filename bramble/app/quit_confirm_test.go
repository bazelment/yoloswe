package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestQuitConfirm_NoActiveSessions_QuitsImmediately(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Press 'q' with no active sessions
	newModel, cmd := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := newModel.(Model)

	// Should quit immediately
	assert.False(t, m2.confirmQuit)
	assert.NotNil(t, cmd)
	// cmd should be tea.Quit
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.True(t, isQuit)
}

func TestQuitConfirm_ActiveSessions_ShowsConfirmation(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Inject a running session directly to avoid the race where the
	// runSession goroutine fails before m.sessions is populated.
	m.sessionManager.AddSession(&session.Session{
		ID: "active-session", Status: session.StatusRunning,
		WorktreePath: "/tmp/wt/main", Type: session.SessionTypePlanner,
	})
	m.sessions = m.sessionManager.GetAllSessions()

	// Press 'q' with an active session
	newModel, cmd := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := newModel.(Model)

	// Should set confirmQuit and show toast
	assert.True(t, m2.confirmQuit)
	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "active session")

	// cmd should be toast expiry (not quit)
	assert.NotNil(t, cmd)
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.False(t, isQuit)
}

func TestQuitConfirm_SecondQ_Quits(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	m.sessionManager.AddSession(&session.Session{
		ID: "active-session", Status: session.StatusRunning,
		WorktreePath: "/tmp/wt/main", Type: session.SessionTypePlanner,
	})
	m.sessions = m.sessionManager.GetAllSessions()

	// First 'q' sets confirmQuit
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := newModel.(Model)
	assert.True(t, m2.confirmQuit)

	// Second 'q' should quit
	newModel2, cmd := m2.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m3 := newModel2.(Model)
	assert.False(t, m3.confirmQuit)
	assert.NotNil(t, cmd)
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.True(t, isQuit)
}

func TestQuitConfirm_Y_Quits(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	m.sessionManager.AddSession(&session.Session{
		ID: "active-session", Status: session.StatusRunning,
		WorktreePath: "/tmp/wt/main", Type: session.SessionTypePlanner,
	})
	m.sessions = m.sessionManager.GetAllSessions()

	// First 'q' sets confirmQuit
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := newModel.(Model)
	assert.True(t, m2.confirmQuit)

	// Press 'y' should quit
	newModel2, cmd := m2.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m3 := newModel2.(Model)
	assert.False(t, m3.confirmQuit)
	assert.NotNil(t, cmd)
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.True(t, isQuit)
}

func TestQuitConfirm_OtherKey_Cancels(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	m.sessionManager.AddSession(&session.Session{
		ID: "active-session", Status: session.StatusRunning,
		WorktreePath: "/tmp/wt/main", Type: session.SessionTypePlanner,
	})
	m.sessions = m.sessionManager.GetAllSessions()

	// First 'q' sets confirmQuit
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := newModel.(Model)
	assert.True(t, m2.confirmQuit)

	// Press 'n' should cancel
	newModel2, cmd := m2.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m3 := newModel2.(Model)
	assert.False(t, m3.confirmQuit)
	assert.True(t, m3.toasts.HasToasts())
	// The last toast (index 0 is most recent) should be "Quit cancelled"
	assert.Contains(t, m3.toasts.toasts[len(m3.toasts.toasts)-1].Message, "cancelled")

	// cmd should not be quit
	assert.NotNil(t, cmd)
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.False(t, isQuit)
}

func TestQuitConfirm_CtrlC_AlwaysQuits(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	m.sessionManager.AddSession(&session.Session{
		ID: "active-session", Status: session.StatusRunning,
		WorktreePath: "/tmp/wt/main", Type: session.SessionTypePlanner,
	})
	m.sessions = m.sessionManager.GetAllSessions()

	// Ctrl+C should quit immediately even with active sessions
	newModel, cmd := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyCtrlC})
	m2 := newModel.(Model)
	assert.False(t, m2.confirmQuit)
	assert.NotNil(t, cmd)
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.True(t, isQuit)
}

func TestQuitConfirm_IdleSessions_CountAsActive(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Idle is non-terminal, so it should trigger quit confirmation.
	m.sessionManager.AddSession(&session.Session{
		ID: "idle-session", Status: session.StatusIdle,
		WorktreePath: "/tmp/wt/main", Type: session.SessionTypePlanner,
	})
	m.sessions = m.sessionManager.GetAllSessions()

	// Press 'q' - should require confirmation even for idle sessions
	// (idle is not terminal)
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := newModel.(Model)
	assert.True(t, m2.confirmQuit)
}

func TestQuitConfirm_CompletedSessions_DontCount(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")

	// Inject a completed session directly to avoid the race where the
	// runSession goroutine overwrites UpdateSessionStatus with StatusFailed.
	m.sessionManager.AddSession(&session.Session{
		ID:           "completed-session",
		Status:       session.StatusCompleted,
		WorktreePath: "/tmp/wt/main",
		Type:         session.SessionTypePlanner,
	})

	m.sessions = m.sessionManager.GetAllSessions()
	require.Len(t, m.sessions, 1)
	require.True(t, m.sessions[0].Status.IsTerminal(), "session should be in terminal state")

	// Press 'q' with only completed sessions - should quit immediately
	newModel, cmd := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := newModel.(Model)
	assert.False(t, m2.confirmQuit)
	assert.NotNil(t, cmd)
	msg := cmd()
	_, isQuit := msg.(tea.QuitMsg)
	assert.True(t, isQuit)
}
