package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
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

func TestCommandCenter_UpdateSessionsPreservesPreview(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	cc.Show(sessions, 120, 40)

	// Select second session and open preview
	cc.selectedIdx = 1
	sess := cc.TogglePreview()
	require.NotNil(t, sess)
	assert.Equal(t, 1, cc.previewIdx)
	previewedID := sess.ID
	assert.Equal(t, previewedID, cc.PreviewedSessionID())
	cc.SetPreviewText([]string{"line1", "line2"})

	// UpdateSessions should preserve the preview
	cc.UpdateSessions(sessions, 120, 40)
	assert.Equal(t, previewedID, cc.PreviewedSessionID())
	assert.NotEqual(t, -1, cc.previewIdx)
	// Preview text is preserved (not cleared by UpdateSessions)
	assert.Equal(t, []string{"line1", "line2"}, cc.previewText)
}

func TestCommandCenter_UpdateSessionsClearsPreviewIfGone(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	cc.Show(sessions, 120, 40)

	// Open preview on a session
	cc.selectedIdx = 0
	cc.TogglePreview()
	cc.SetPreviewText([]string{"some text"})

	// UpdateSessions with a list that doesn't contain the previewed session
	cc.UpdateSessions([]session.SessionInfo{sessions[2], sessions[3]}, 120, 40)
	assert.Equal(t, session.SessionID(""), cc.PreviewedSessionID())
	assert.Equal(t, -1, cc.previewIdx)
	assert.Nil(t, cc.previewText)
}

func TestCommandCenter_HideClearsPreviewState(t *testing.T) {
	cc := NewCommandCenter()
	cc.Show(makeSessions(), 120, 40)
	cc.selectedIdx = 0
	cc.TogglePreview()
	cc.SetPreviewText([]string{"text"})

	cc.Hide()
	assert.Equal(t, -1, cc.previewIdx)
	assert.Nil(t, cc.previewText)
	assert.Equal(t, session.SessionID(""), cc.PreviewedSessionID())
}

func TestCommandCenter_TogglePreviewTracksSessionID(t *testing.T) {
	cc := NewCommandCenter()
	cc.Show(makeSessions(), 120, 40)

	// Open preview
	cc.selectedIdx = 1
	sess := cc.TogglePreview()
	require.NotNil(t, sess)
	assert.Equal(t, sess.ID, cc.PreviewedSessionID())

	// Close preview
	result := cc.TogglePreview()
	assert.Nil(t, result)
	assert.Equal(t, session.SessionID(""), cc.PreviewedSessionID())
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

func TestCommandCenter_NewSession_PBC(t *testing.T) {
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
			m.commandCenter.Show(sessions, m.width, m.height)
			m.focus = FocusCommandCenter

			newModel, _ := m.handleCommandCenter(keyPress(tc.key))
			m2 := newModel.(Model)

			assert.False(t, m2.commandCenter.IsVisible())
			assert.Equal(t, FocusInput, m2.focus)
			assert.True(t, m2.inputMode)
			assert.Contains(t, m2.inputPrompt, tc.promptSub)
			assert.Equal(t, tc.sessionType, m2.pendingSessionType)
		})
	}
}

// TestCommandCenter_UpdateSessionsPreservesSelectionByID exercises the
// repro for the "wrong worktree" bug at the unit level: the user
// highlights a non-first session, a status-driven re-sort happens, and
// the cursor must still point at the same session by ID, not at whatever
// numeric index it held before.
func TestCommandCenter_UpdateSessionsPreservesSelectionByID(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	cc.Show(sessions, 120, 40)

	// Navigate to a non-first session. After Show()'s priority sort the
	// order is [idle, running, pending, completed]; pick the running one
	// at index 1 so a sort-order change is observable.
	cc.selectedIdx = 1
	require.NotNil(t, cc.SelectedSession())
	selID := cc.SelectedSession().ID
	require.Equal(t, session.SessionID("sess-running"), selID)

	// Mutate the running session into idle so the next sort lifts it to
	// the top of the list (idle priority < running).
	mutated := makeSessions()
	for i := range mutated {
		if mutated[i].ID == "sess-running" {
			mutated[i].Status = session.StatusIdle
		}
	}
	cc.UpdateSessions(mutated, 120, 40)

	// Same session by ID, possibly different numeric index.
	require.NotNil(t, cc.SelectedSession())
	assert.Equal(t, selID, cc.SelectedSession().ID,
		"selection must follow the session by ID across re-sort")
}

// TestCommandCenter_ShowResetsSelection documents that Show() always lands
// the cursor on the highest-priority card, regardless of where it was on
// the previous visit. Opening the overlay is a fresh survey.
func TestCommandCenter_ShowResetsSelection(t *testing.T) {
	cc := NewCommandCenter()
	cc.Show(makeSessions(), 120, 40)
	cc.selectedIdx = 2 // user navigates somewhere
	cc.Hide()

	cc.Show(makeSessions(), 120, 40)
	assert.Equal(t, 0, cc.selectedIdx)
	require.NotNil(t, cc.SelectedSession())
	// After priority sort the idle session leads.
	assert.Equal(t, session.StatusIdle, cc.SelectedSession().Status)
}

// TestCommandCenter_UpdateSessionsClampsToTailWhenSelectedGone covers the
// case where the previously selected session disappears entirely and the
// list also shrinks below the old numeric index. The cursor lands on the
// new tail rather than the head.
func TestCommandCenter_UpdateSessionsClampsToTailWhenSelectedGone(t *testing.T) {
	cc := NewCommandCenter()
	sessions := makeSessions()
	cc.Show(sessions, 120, 40)
	cc.selectedIdx = 3 // last card

	// Remove the previously selected session and shrink the list.
	sorted := cc.Sessions()
	keep := []session.SessionInfo{sorted[0], sorted[1]}
	cc.UpdateSessions(keep, 120, 40)

	require.NotNil(t, cc.SelectedSession())
	assert.Equal(t, 1, cc.selectedIdx, "cursor should land on new tail, not bounce to head")
}

// TestCommandCenter_UpdateSessionsEmptyList ensures SelectedSession is
// nil-safe after an UpdateSessions that empties the list.
func TestCommandCenter_UpdateSessionsEmptyList(t *testing.T) {
	cc := NewCommandCenter()
	cc.Show(makeSessions(), 120, 40)
	cc.selectedIdx = 2

	cc.UpdateSessions(nil, 120, 40)
	assert.Nil(t, cc.SelectedSession())
}

// TestCommandCenter_NewSession_AfterReSort is the regression test for the
// user-visible bug. The user opens the command center, navigates to a
// non-first session, an UpdateSessions tick re-sorts the list, then they
// press 'p'/'b'/'c'. The resulting startSessionMsg must carry the worktree
// of the navigated session, not the worktree of whichever session is
// currently first in the list.
//
// Setup: three running sessions A/B/C. User navigates to B (index 1).
// Then C transitions to idle, which lifts it to index 0 — pushing A to 1
// and B to 2. Without the fix, selectedIdx stays at 1 and silently
// retargets onto A's worktree. With the fix, the cursor follows B by ID.
func TestCommandCenter_NewSession_AfterReSort(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	now := time.Now()
	mkSession := func(id, path, name string, status session.SessionStatus, age time.Duration) session.SessionInfo {
		return session.SessionInfo{
			ID: session.SessionID(id), Status: status,
			WorktreePath: path, WorktreeName: name, RepoName: "test-repo",
			Progress: session.SessionProgressSnapshot{LastActivity: now.Add(-age)},
		}
	}
	sessions := []session.SessionInfo{
		mkSession("sess-a", "/tmp/wt/A", "A", session.StatusRunning, 1*time.Minute),
		mkSession("sess-b", "/tmp/wt/B", "B", session.StatusRunning, 2*time.Minute),
		mkSession("sess-c", "/tmp/wt/C", "C", session.StatusRunning, 3*time.Minute),
	}
	m.commandCenter.Show(sessions, m.width, m.height)
	m.focus = FocusCommandCenter

	// Navigate to sess-b. With LastActivity desc within the running tier the
	// initial sorted order is [A, B, C]; one MoveSelection lands on B.
	m.commandCenter.MoveSelection(1)
	require.Equal(t, session.SessionID("sess-b"), m.commandCenter.SelectedSession().ID)

	// Status-driven re-sort: sess-c flips to idle (highest priority tier),
	// so the new sorted order is [C, A, B]. A naive numeric cursor at
	// index 1 would now point at sess-a — exactly the user-visible bug.
	mutated := []session.SessionInfo{
		mkSession("sess-a", "/tmp/wt/A", "A", session.StatusRunning, 1*time.Minute),
		mkSession("sess-b", "/tmp/wt/B", "B", session.StatusRunning, 2*time.Minute),
		mkSession("sess-c", "/tmp/wt/C", "C", session.StatusIdle, 3*time.Minute),
	}
	m.commandCenter.UpdateSessions(mutated, m.width, m.height)

	require.NotNil(t, m.commandCenter.SelectedSession())
	require.Equal(t, session.SessionID("sess-b"), m.commandCenter.SelectedSession().ID,
		"cursor must follow sess-b across re-sort")

	newModel, _ := m.handleCommandCenter(keyPress('p'))
	m2 := newModel.(Model)
	require.True(t, m2.inputMode)

	_, cmd := m2.Update(promptInputMsg{value: "do thing"})
	require.NotNil(t, cmd)
	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)
	assert.Equal(t, "/tmp/wt/B", startMsg.worktreePath,
		"new session must target the navigated session's worktree, not the first card's")
}

func TestCommandCenter_NewSession_NoSession(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Show with no sessions
	m.commandCenter.Show(nil, m.width, m.height)
	m.focus = FocusCommandCenter

	newModel, _ := m.handleCommandCenter(keyPress('p'))
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No session selected")
	// Command center should stay visible on error
	assert.True(t, m2.commandCenter.IsVisible())
}
