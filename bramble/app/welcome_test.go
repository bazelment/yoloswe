package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestWelcomeNoWorktrees(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	// No worktrees, no sessions
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, 80, 24)

	view := m.renderWelcome(80, 20)

	assert.Contains(t, view, "Welcome to Bramble")
	assert.Contains(t, view, "No worktrees found")
	assert.Contains(t, view, "[t]")
	assert.Contains(t, view, "[n]")
	assert.Contains(t, view, "[?]")
}

func TestWelcomeWithWorktrees(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
		{Branch: "fix-bug", Path: "/tmp/wt/fix-bug"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)

	view := m.renderWelcome(80, 20)

	assert.Contains(t, view, "Bramble")
	assert.Contains(t, view, "Quick start")
	assert.Contains(t, view, "[t]")
	assert.Contains(t, view, "[p]")
	assert.Contains(t, view, "[b]")
	assert.Contains(t, view, "feature-auth") // current worktree
}

func TestWelcomeWithWorktreeStatus(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature-auth": {
			IsDirty:  true,
			Ahead:    2,
			PRNumber: 42,
			PRState:  "OPEN",
		},
	}

	view := m.renderWelcome(80, 20)

	assert.Contains(t, view, "dirty")
	assert.Contains(t, view, "↑2")
	assert.Contains(t, view, "PR#42")
}

func TestWelcomeWithSessions(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)

	// Add a session for this worktree
	mgr.AddSession(&session.Session{
		ID:           "sess-1",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature-auth",
		Prompt:       "plan auth",
	})

	view := m.renderWelcome(80, 20)

	assert.Contains(t, view, "1 active session")
}

func TestWelcomeWorktreeOpMessages(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, 80, 24)
	m.worktreeOpMessages = []string{"Creating worktree feature-new..."}

	view := m.renderWelcome(80, 20)

	// Should show operation messages instead of welcome
	assert.Contains(t, view, "Creating worktree")
	assert.NotContains(t, view, "Welcome")
}

func TestRenderOutputAreaDelegatesToWelcome(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, worktrees, 80, 24)
	// viewingSessionID is "" by default

	output := m.renderOutputArea(80, 20)

	// Should contain welcome content, not the old "No session selected"
	assert.NotContains(t, output, "No session selected")
	assert.Contains(t, output, "Quick start")
}
