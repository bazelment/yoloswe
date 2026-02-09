package app

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

// setupModel creates a Model for testing with the given parameters.
func setupModel(t *testing.T, mode session.SessionMode, worktrees []wt.Worktree, repoName string) Model {
	t.Helper()
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: mode})
	t.Cleanup(func() { mgr.Close() })
	m := NewModel(ctx, "/tmp/wt", repoName, "", mgr, nil, worktrees, 80, 24)
	return m
}

func TestKeyFeedback_P_NoWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Select a worktree first")
}

func TestKeyFeedback_B_NoWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Select a worktree first")
}

func TestKeyFeedback_E_NoWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Select a worktree first")
}

func TestKeyFeedback_D_NoWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Select a worktree first")
}

func TestKeyFeedback_N_NoRepo(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No repository loaded")
}

func TestKeyFeedback_S_NoSession(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No active session to stop")
}

func TestKeyFeedback_F_NoIdleSession(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No idle session")
}

func TestKeyFeedback_A_NoPlan(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No plan ready")
}

func TestKeyFeedback_Enter_TmuxNoSessions(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No sessions to switch to")
}

func TestKeyFeedback_AltS_TmuxMode(t *testing.T) {
	m := setupModel(t, session.SessionModeTmux, nil, "test-repo")

	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}, Alt: true})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Sessions are in tmux windows")
}
