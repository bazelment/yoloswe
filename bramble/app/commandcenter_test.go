package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
)

func makeSessions() []session.SessionInfo {
	now := time.Now()
	started := now.Add(-5 * time.Minute)
	return []session.SessionInfo{
		{
			ID: "sess-running", Type: session.SessionTypeBuilder, Status: session.StatusRunning,
			Title: "Fix auth bug", Prompt: "fix the auth bug in login.go",
			WorktreeName: "feature-auth", RepoName: "myrepo",
			Model: "sonnet", StartedAt: &started,
			Progress: session.SessionProgressSnapshot{
				TurnCount: 5, TotalCostUSD: 0.1234, CurrentTool: "Edit",
				LastActivity: now.Add(-1 * time.Minute),
				RecentOutput: []string{"Reading login.go...", "Found the issue in validateToken()"},
			},
		},
		{
			ID: "sess-idle", Type: session.SessionTypePlanner, Status: session.StatusIdle,
			Title: "Plan refactor", Prompt: "plan the auth refactor",
			WorktreeName: "refactor-branch", RepoName: "myrepo",
			Model: "opus", PlanFilePath: "/tmp/plan.md",
			Progress: session.SessionProgressSnapshot{
				TurnCount: 3, TotalCostUSD: 0.05, StatusLine: "Awaiting approval",
				LastActivity: now,
				RecentOutput: []string{"Here is my plan for the refactor:"},
			},
		},
		{
			ID: "sess-pending", Type: session.SessionTypeBuilder, Status: session.StatusPending,
			Title: "Build feature", WorktreeName: "new-feature",
			Progress: session.SessionProgressSnapshot{LastActivity: now.Add(-2 * time.Minute)},
		},
		{
			ID: "sess-completed", Type: session.SessionTypeBuilder, Status: session.StatusCompleted,
			Title:    "Done task",
			Progress: session.SessionProgressSnapshot{LastActivity: now.Add(-10 * time.Minute)},
		},
	}
}

func TestCommandCenter_PrioritySorting(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	cc.Show(sessions, 120, 40)

	sorted := cc.Sessions()
	require.Len(t, sorted, 4)
	// Idle (needs action) first
	assert.Equal(t, session.StatusIdle, sorted[0].Status)
	// Then running
	assert.Equal(t, session.StatusRunning, sorted[1].Status)
	// Then pending
	assert.Equal(t, session.StatusPending, sorted[2].Status)
	// Then terminal
	assert.Equal(t, session.StatusCompleted, sorted[3].Status)
}

func TestCommandCenter_GridLayout(t *testing.T) {
	cc := NewCommandCenter()

	// Width < 80: 1 column
	cc.Show(nil, 60, 40)
	assert.Equal(t, 1, cc.gridColumns())

	// Width 80-159: 2 columns
	cc.Show(nil, 80, 40)
	assert.Equal(t, 2, cc.gridColumns())
	cc.Show(nil, 120, 40)
	assert.Equal(t, 2, cc.gridColumns())

	// Width >= 160: 3 columns
	cc.Show(nil, 160, 40)
	assert.Equal(t, 3, cc.gridColumns())
	cc.Show(nil, 200, 40)
	assert.Equal(t, 3, cc.gridColumns())
}

func TestCommandCenter_NavigationGrid(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	// 2-column grid
	cc.Show(sessions, 120, 40)
	assert.Equal(t, 0, cc.selectedIdx)

	// Move right
	cc.MoveSelection(1)
	assert.Equal(t, 1, cc.selectedIdx)

	// Move right — clamped at end
	cc.MoveSelection(1)
	assert.Equal(t, 2, cc.selectedIdx)

	// Move down from index 0: should go to index 2 (cols=2)
	cc.selectedIdx = 0
	cc.MoveSelectionRow(1)
	assert.Equal(t, 2, cc.selectedIdx)

	// Move up from index 2: should go to index 0
	cc.MoveSelectionRow(-1)
	assert.Equal(t, 0, cc.selectedIdx)

	// Move up from index 0: should stay at 0
	cc.MoveSelectionRow(-1)
	assert.Equal(t, 0, cc.selectedIdx)

	// Move down beyond last row: clamp to last session
	cc.selectedIdx = 2
	cc.MoveSelectionRow(1)
	assert.Equal(t, 3, cc.selectedIdx) // clamped to len-1
}

func TestCommandCenter_RestoreSelectionByID(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	cc.Show(sessions, 120, 40)
	cc.selectedIdx = 0

	// After refresh, the session order may change; restore by ID
	cc.RestoreSelectionByID("sess-pending")
	// sess-pending should be at index 2 after sort (idle, running, pending, completed)
	assert.Equal(t, 2, cc.selectedIdx)

	// Missing ID: clamp to valid range
	cc.RestoreSelectionByID("nonexistent-id")
	assert.True(t, cc.selectedIdx >= 0 && cc.selectedIdx < len(cc.sessions))
}

func TestCommandCenter_SelectByNumber(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	cc.Show(sessions, 120, 40)

	assert.True(t, cc.SelectByNumber(3))
	assert.Equal(t, 2, cc.selectedIdx)

	assert.False(t, cc.SelectByNumber(0))
	assert.False(t, cc.SelectByNumber(10))
}

func TestCommandCenter_CardRendering(t *testing.T) {
	s := NewStyles(Dark)
	sessions := makeSessions()
	sortSessionsByPriority(sessions)

	// Render the idle planner card (first after sort)
	card := renderSessionCard(&sessions[0], 60, 0, false, s)
	assert.Contains(t, card, "[P]")
	assert.Contains(t, card, "planner")
	assert.Contains(t, card, "PLAN READY")
	assert.Contains(t, card, "> plan the auth refactor") // prompt with > prefix
	assert.Contains(t, card, "Here is my plan")          // recent output

	// Render the running builder card
	card = renderSessionCard(&sessions[1], 60, 1, true, s)
	assert.Contains(t, card, "[B]")
	assert.Contains(t, card, "builder")
	assert.Contains(t, card, "[Edit]")
	assert.Contains(t, card, "> fix the auth bug")    // prompt with > prefix
	assert.Contains(t, card, "Reading login.go")      // recent output line 1
	assert.Contains(t, card, "Found the issue")       // recent output line 2
	assert.Contains(t, card, "feature-auth [sonnet]") // repo context on line 1
}

func TestCommandCenter_CardRendering_ZeroProgress(t *testing.T) {
	s := NewStyles(Dark)
	// Session with zero-value Progress (no RecentOutput, no CurrentTool, etc.)
	sess := session.SessionInfo{
		ID: "sess-zero", Type: session.SessionTypeBuilder, Status: session.StatusPending,
		Prompt: "do something",
		// Progress is zero-value: no RecentOutput, no tool, no phase
		Progress: session.SessionProgressSnapshot{},
	}
	// Must not panic; "-" should be rendered for missing output lines.
	card := renderSessionCard(&sess, 60, 0, false, s)
	assert.Contains(t, card, "> do something")
	assert.Contains(t, card, "-") // placeholder for empty recent output
}

func TestCommandCenter_OpenClose(t *testing.T) {
	cc := NewCommandCenter()
	assert.False(t, cc.IsVisible())

	cc.Show(makeSessions(), 120, 40)
	assert.True(t, cc.IsVisible())

	cc.Hide()
	assert.False(t, cc.IsVisible())
}

func TestCommandCenter_View(t *testing.T) {
	cc := NewCommandCenter()
	s := NewStyles(Dark)

	// Empty sessions
	cc.Show(nil, 120, 40)
	view := cc.View(s)
	assert.Contains(t, view, "Command Center")
	assert.Contains(t, view, "No active sessions")

	// With sessions
	cc.Show(makeSessions(), 120, 40)
	view = cc.View(s)
	assert.Contains(t, view, "Command Center")
	assert.Contains(t, view, "running")
	assert.Contains(t, view, "idle")
}

func TestFormatDuration(t *testing.T) {
	assert.Equal(t, "0m00s", formatDuration(0))
	assert.Equal(t, "0m30s", formatDuration(30*time.Second))
	assert.Equal(t, "3m12s", formatDuration(3*time.Minute+12*time.Second))
	assert.Equal(t, "1h5m", formatDuration(1*time.Hour+5*time.Minute))
}

func TestSessionPriority(t *testing.T) {
	idle := &session.SessionInfo{Status: session.StatusIdle}
	running := &session.SessionInfo{Status: session.StatusRunning}
	pending := &session.SessionInfo{Status: session.StatusPending}
	completed := &session.SessionInfo{Status: session.StatusCompleted}

	assert.Less(t, sessionPriority(idle), sessionPriority(running))
	assert.Less(t, sessionPriority(running), sessionPriority(pending))
	assert.Less(t, sessionPriority(pending), sessionPriority(completed))
}
