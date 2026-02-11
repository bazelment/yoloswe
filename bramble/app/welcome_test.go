package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestWelcomeNoWorktrees(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	// No worktrees, no sessions
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24)

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
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)

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
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)
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
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)

	// Add a session for this worktree
	mgr.AddSession(&session.Session{
		ID:           "sess-1",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature-auth",
		Prompt:       "plan auth",
	})

	view := m.renderWelcome(80, 20)

	// Timeline should show the running session
	assert.Contains(t, view, "Session timeline")
	assert.Contains(t, view, "Running")
	assert.Contains(t, view, "plan auth")
}

func TestWelcomeWorktreeOpMessages(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, nil, 80, 24)
	m.worktreeOpMessages = []string{"Creating worktree feature-new..."}

	view := m.renderWelcome(80, 20)

	// Should show both welcome content and operation messages
	assert.Contains(t, view, "Creating worktree")
	assert.Contains(t, view, "Welcome")
}

func TestRenderOutputAreaDelegatesToWelcome(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)
	// viewingSessionID is "" by default

	output := m.renderOutputArea(80, 20)

	// Should contain welcome content, not the old "No session selected"
	assert.NotContains(t, output, "No session selected")
	assert.Contains(t, output, "Quick start")
}

func TestBuildTimelineMergeLiveAndHistory(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)

	now := time.Now()
	earlier := now.Add(-10 * time.Minute)

	// Add a live session
	mgr.AddSession(&session.Session{
		ID:           "sess-live",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature-auth",
		Prompt:       "plan auth flow",
		CreatedAt:    now,
	})

	// Add history for the same worktree
	completedAt := earlier
	m.cachedHistory = []*session.SessionMeta{
		{
			ID:          "sess-old",
			Type:        session.SessionTypeBuilder,
			Status:      session.StatusCompleted,
			Prompt:      "build login page",
			CreatedAt:   earlier.Add(-5 * time.Minute),
			CompletedAt: &completedAt,
		},
	}
	m.historyBranch = "feature-auth"

	timeline := m.buildTimeline()

	require.Len(t, timeline, 2)
	// Newest first: live session (now) before historical (earlier)
	assert.Equal(t, session.SessionID("sess-live"), timeline[0].sessionID)
	assert.Equal(t, session.SessionID("sess-old"), timeline[1].sessionID)
	assert.Equal(t, "Running", timeline[0].event)
	assert.Equal(t, "Completed", timeline[1].event)
}

func TestBuildTimelineDeduplicatesLiveOverHistory(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)

	now := time.Now()

	// Same session ID appears in both live and history
	mgr.AddSession(&session.Session{
		ID:           "sess-1",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature-auth",
		Prompt:       "plan auth",
		CreatedAt:    now,
	})

	completedAt := now.Add(-5 * time.Minute)
	m.cachedHistory = []*session.SessionMeta{
		{
			ID:          "sess-1", // Same ID as live
			Type:        session.SessionTypePlanner,
			Status:      session.StatusCompleted,
			Prompt:      "plan auth",
			CreatedAt:   now.Add(-10 * time.Minute),
			CompletedAt: &completedAt,
		},
	}
	m.historyBranch = "feature-auth"

	timeline := m.buildTimeline()

	// Should have only 1 entry (live takes precedence)
	require.Len(t, timeline, 1)
	assert.Equal(t, session.SessionID("sess-1"), timeline[0].sessionID)
	assert.Equal(t, "Running", timeline[0].event) // live status, not "Completed"
}

func TestBuildTimelineHistoryOnly(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	worktrees := []wt.Worktree{
		{Branch: "feature-auth", Path: "/tmp/wt/feature-auth"},
	}
	m := NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, nil, worktrees, 80, 24)

	now := time.Now()
	t1 := now.Add(-5 * time.Minute)
	t2 := now.Add(-10 * time.Minute)

	m.cachedHistory = []*session.SessionMeta{
		{
			ID:          "sess-a",
			Type:        session.SessionTypePlanner,
			Status:      session.StatusCompleted,
			Prompt:      "plan feature",
			CreatedAt:   now.Add(-15 * time.Minute),
			CompletedAt: &t1,
		},
		{
			ID:          "sess-b",
			Type:        session.SessionTypeBuilder,
			Status:      session.StatusFailed,
			Prompt:      "build feature",
			CreatedAt:   now.Add(-20 * time.Minute),
			CompletedAt: &t2,
		},
	}
	m.historyBranch = "feature-auth"

	timeline := m.buildTimeline()

	require.Len(t, timeline, 2)
	// Newest first
	assert.Equal(t, session.SessionID("sess-a"), timeline[0].sessionID)
	assert.Equal(t, session.SessionID("sess-b"), timeline[1].sessionID)
	assert.Equal(t, "[P]", timeline[0].icon)
	assert.Equal(t, "[B]", timeline[1].icon)
}

func TestRenderTimelineOverflow(t *testing.T) {
	entries := []timelineEntry{
		{timestamp: time.Now(), icon: "[P]", event: "Running", prompt: "task 1"},
		{timestamp: time.Now().Add(-1 * time.Minute), icon: "[B]", event: "Completed", prompt: "task 2"},
		{timestamp: time.Now().Add(-2 * time.Minute), icon: "[P]", event: "Failed", prompt: "task 3"},
		{timestamp: time.Now().Add(-3 * time.Minute), icon: "[B]", event: "Stopped", prompt: "task 4"},
	}

	// maxLines=2 should show 2 entries + overflow indicator
	output := renderTimeline(entries, 2, NewStyles(DefaultDark))

	assert.Contains(t, output, "Session timeline")
	assert.Contains(t, output, "task 1")
	assert.Contains(t, output, "task 2")
	assert.NotContains(t, output, "task 3")
	assert.NotContains(t, output, "task 4")
	assert.Contains(t, output, "2 more")
}

func TestRenderTimelineNoOverflow(t *testing.T) {
	entries := []timelineEntry{
		{timestamp: time.Now(), icon: "[P]", event: "Running", prompt: "task 1"},
		{timestamp: time.Now().Add(-1 * time.Minute), icon: "[B]", event: "Completed", prompt: "task 2"},
	}

	output := renderTimeline(entries, 10, NewStyles(DefaultDark))

	assert.Contains(t, output, "task 1")
	assert.Contains(t, output, "task 2")
	assert.NotContains(t, output, "more")
}

func TestSessionTypeIcon(t *testing.T) {
	assert.Equal(t, "[P]", sessionTypeIcon(session.SessionTypePlanner))
	assert.Equal(t, "[B]", sessionTypeIcon(session.SessionTypeBuilder))
}

func TestSessionStatusEvent(t *testing.T) {
	assert.Equal(t, "Running", sessionStatusEvent(session.StatusRunning))
	assert.Equal(t, "Idle", sessionStatusEvent(session.StatusIdle))
	assert.Equal(t, "Completed", sessionStatusEvent(session.StatusCompleted))
	assert.Equal(t, "Failed", sessionStatusEvent(session.StatusFailed))
	assert.Equal(t, "Stopped", sessionStatusEvent(session.StatusStopped))
	assert.Equal(t, "Pending", sessionStatusEvent(session.StatusPending))
}
