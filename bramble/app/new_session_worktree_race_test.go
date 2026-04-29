package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestMainViewNewSession_CapturesWorktreeBeforePrompt(t *testing.T) {
	for _, key := range []rune{'p', 'b', 'c'} {
		t.Run(string(key), func(t *testing.T) {
			sessionType := sessionTypeFromKey(string(key))
			m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
				{Branch: "A", Path: "/tmp/wt/A"},
				{Branch: "B", Path: "/tmp/wt/B"},
			}, "repoA")
			require.True(t, m.worktreeDropdown.SelectByID("A"))

			newModel, _ := m.handleKeyPress(keyPress(key))
			m2 := newModel.(Model)
			require.True(t, m2.inputMode)

			require.True(t, m2.worktreeDropdown.SelectByID("B"))

			newModel2, cmd := m2.Update(promptInputMsg{value: "do the thing"})
			m3 := newModel2.(Model)
			require.NotNil(t, cmd)

			startMsg, ok := cmd().(startSessionMsg)
			require.True(t, ok)
			assert.Equal(t, sessionType, startMsg.sessionType)
			assert.Equal(t, "repoA", startMsg.target.repoName)
			assert.Equal(t, "/tmp/wt/A", startMsg.target.worktreePath)

			newModel3, _ := m3.Update(startMsg)
			m4 := newModel3.(Model)

			sessions := m4.sessionManager.GetAllSessions()
			require.Len(t, sessions, 1)
			assert.Equal(t, sessionType, sessions[0].Type)
			assert.Equal(t, "/tmp/wt/A", sessions[0].WorktreePath)
		})
	}
}

func TestMainViewNewSession_CapturedRepoSurvivesRepoSwitchBeforePromptSubmit(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/repoA-main"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("main"))

	mgrB := injectSecondRepo(t, &m, "repoB")
	setRepoWorktrees(&m, "repoB", []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/repoB-main"},
	})

	newModel, _ := m.handleKeyPress(keyPress('p'))
	m2 := newModel.(Model)
	require.True(t, m2.inputMode)

	switchedModel, _ := m2.switchRepo("repoB")
	mOnB := switchedModel.(Model)
	require.Equal(t, "repoB", mOnB.repoName)

	newModel2, cmd := mOnB.Update(promptInputMsg{value: "plan from original repo"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd)

	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)
	assert.Equal(t, "repoA", startMsg.target.repoName)
	assert.Equal(t, "/tmp/wt/repoA-main", startMsg.target.worktreePath)

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.Equal(t, "repoB", m4.repoName, "active repo should not be switched by dispatch")
	assert.Empty(t, mgrB.GetAllSessions(), "current repo manager must not receive the captured repo session")

	repoASessions := m4.repos["repoA"].sessionManager.GetAllSessions()
	require.Len(t, repoASessions, 1)
	assert.Equal(t, "/tmp/wt/repoA-main", repoASessions[0].WorktreePath)
}

func TestStartSessionMsg_CapturedWorktreeRemovedBeforeDispatch(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
		{Branch: "B", Path: "/tmp/wt/B"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	newModel, _ := m.handleKeyPress(keyPress('p'))
	m2 := newModel.(Model)
	require.True(t, m2.inputMode)

	newModel2, cmd := m2.Update(promptInputMsg{value: "plan removed worktree"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd)

	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)
	require.Equal(t, "/tmp/wt/A", startMsg.target.worktreePath)

	m3.worktrees = nil
	m3.updateWorktreeDropdown()

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.True(t, m4.toasts.HasToasts())
	assert.Contains(t, m4.toasts.toasts[0].Message, "Target worktree no longer available")
	assert.Empty(t, m4.sessionManager.GetAllSessions())
}

func TestAllSessionsOverlayNewSession_UsesCapturedSessionWorktreeAfterSelectionChanges(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
		{Branch: "B", Path: "/tmp/wt/B"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	sessions := []session.SessionInfo{
		{
			ID:           "s1",
			Status:       session.StatusRunning,
			WorktreePath: "/tmp/wt/B",
			WorktreeName: "B",
			RepoName:     "repoA",
		},
	}
	m.allSessionsOverlay.Show(sessions, m.width, m.height)
	m.focus = FocusAllSessions

	newModel, _ := m.handleAllSessionsOverlay(keyPress('b'))
	m2 := newModel.(Model)
	require.True(t, m2.inputMode)

	require.True(t, m2.worktreeDropdown.SelectByID("A"))

	newModel2, cmd := m2.Update(promptInputMsg{value: "build on B"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd)

	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)
	assert.Equal(t, session.SessionTypeBuilder, startMsg.sessionType)
	assert.Equal(t, "repoA", startMsg.target.repoName)
	assert.Equal(t, "/tmp/wt/B", startMsg.target.worktreePath)

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	sessionsAfter := m4.sessionManager.GetAllSessions()
	require.Len(t, sessionsAfter, 1)
	assert.Equal(t, "/tmp/wt/B", sessionsAfter[0].WorktreePath)
}

func TestCommandCenterNewSession_ShowsSelectedSessionWorktreeWhilePromptOpen(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
		{Branch: "B", Path: "/tmp/wt/B"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	sessionA := addTestSession(t, m.sessionManager, &session.Session{
		ID:           "sA",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/A",
		WorktreeName: "A",
		RepoName:     "repoA",
		Title:        "Session A",
	})
	sessionB := addTestSession(t, m.sessionManager, &session.Session{
		ID:           "sB",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/B",
		WorktreeName: "B",
		RepoName:     "repoA",
		Title:        "Session B",
	})
	m.switchViewingSession(sessionA.ID)

	m.commandCenter.Show([]session.SessionInfo{sessionA, sessionB}, m.width, m.height)
	m.focus = FocusCommandCenter
	selectedB := false
	for i, sess := range m.commandCenter.Sessions() {
		if sess.ID == session.SessionID("sB") {
			selectedB = m.commandCenter.SelectByNumber(i + 1)
			break
		}
	}
	require.True(t, selectedB)

	newModel, _ := m.handleCommandCenter(keyPress('b'))
	m2 := newModel.(Model)

	require.True(t, m2.inputMode)
	assert.Equal(t, session.SessionID("sB"), m2.viewingSessionID)
	selected := m2.worktreeDropdown.SelectedItem()
	require.NotNil(t, selected)
	assert.Equal(t, "B", selected.ID)
	view := m2.View().Content
	assert.Contains(t, view, "Session B")
	assert.NotContains(t, view, "Session A")
}

func TestCommandCenterNewSession_SelectsByWorktreePathWhenNameIsStale(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("main"))

	sess := addTestSession(t, m.sessionManager, &session.Session{
		ID:           "s1",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature",
		WorktreeName: "stale-name",
		RepoName:     "repoA",
		Title:        "Feature session",
	})
	m.commandCenter.Show([]session.SessionInfo{sess}, m.width, m.height)
	m.focus = FocusCommandCenter

	newModel, _ := m.handleCommandCenter(keyPress('p'))
	m2 := newModel.(Model)

	require.True(t, m2.inputMode)
	selected := m2.worktreeDropdown.SelectedItem()
	require.NotNil(t, selected)
	assert.Equal(t, "feature", selected.ID)
	assert.Equal(t, session.SessionID("s1"), m2.viewingSessionID)
	assert.Contains(t, m2.View().Content, "Feature session")
}

func TestStartSessionMsg_ExistingSessionWorktreeRemovedBeforeDispatch(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
		{Branch: "B", Path: "/tmp/wt/B"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	sess := addTestSession(t, m.sessionManager, &session.Session{
		ID:           "sB",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/B",
		WorktreeName: "B",
		RepoName:     "repoA",
		Title:        "Session B",
	})
	m.commandCenter.Show([]session.SessionInfo{sess}, m.width, m.height)
	m.focus = FocusCommandCenter

	newModel, _ := m.handleCommandCenter(keyPress('b'))
	m2 := newModel.(Model)
	require.True(t, m2.inputMode)

	newModel2, cmd := m2.Update(promptInputMsg{value: "build on B"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd)
	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)
	require.Equal(t, "/tmp/wt/B", startMsg.target.worktreePath)

	m3.worktrees = []wt.Worktree{{Branch: "A", Path: "/tmp/wt/A"}}
	m3.updateWorktreeDropdown()

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.True(t, m4.toasts.HasToasts())
	assert.Contains(t, m4.toasts.toasts[0].Message, "Target worktree no longer available")
	assert.Len(t, m4.sessionManager.GetAllSessions(), 1, "removed target must not receive a new session")
}

func TestConfirmTask_UnknownExistingWorktreeDoesNotFallBackToCurrentSelection(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
		{Branch: "B", Path: "/tmp/wt/B"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("B"))

	newModel, _ := m.confirmTask(taskConfirmMsg{
		worktree: "missing",
		prompt:   "plan missing worktree",
		isNew:    false,
	})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Target worktree no longer available")
	assert.Empty(t, m2.sessionManager.GetAllSessions())
}

// A worktreesMsg that strips the pending target must both cancel the prompt
// and run the rest of the refresh flow (auto-select + deferred refresh) so
// that pendingWorktreeSelect, the session dropdown, and downstream refreshes
// stay coherent in the same update cycle.
func TestWorktreesMsg_PendingTargetGoneStillRunsRefreshFlow(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	newModel, _ := m.handleKeyPress(keyPress('p'))
	m2 := newModel.(Model)
	require.True(t, m2.inputMode)
	require.Equal(t, "/tmp/wt/A", m2.pendingSessionTarget.worktreePath)

	// Simulate a refresh that drops "A" and offers "B" instead.
	refreshedModel, cmd := m2.Update(worktreesMsg{
		repoName:  "repoA",
		worktrees: []wt.Worktree{{Branch: "B", Path: "/tmp/wt/B"}},
	})
	m3 := refreshedModel.(Model)

	assert.False(t, m3.inputMode, "pending prompt must be cancelled")
	assert.Equal(t, sessionTarget{}, m3.pendingSessionTarget)
	assert.True(t, m3.toasts.HasToasts())
	assert.Contains(t, m3.toasts.toasts[0].Message, "Target worktree no longer available")
	require.NotNil(t, m3.worktreeDropdown.SelectedItem(),
		"refresh flow must auto-select the surviving worktree")
	assert.Equal(t, "B", m3.worktreeDropdown.SelectedItem().ID)
	require.NotNil(t, cmd, "deferred refresh must still fire after cancellation")
}

// Cold target repo: when the captured target points at a repo whose worktree
// snapshot has not finished loading, sessionTargetAvailability returns
// Unknown. Submit must still reject if the path is gone on disk.
func TestStartSessionMsg_RejectsColdTargetWhenPathMissingOnDisk(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	// Register repoB but leave its worktrees unloaded — simulates the cold
	// cross-repo path added by this branch.
	injectSecondRepo(t, &m, "repoB")
	rcB := m.repos["repoB"]
	rcB.worktreesLoaded = false
	rcB.worktrees = nil

	startMsg := startSessionMsg{
		sessionType: session.SessionTypePlanner,
		prompt:      "plan on cold repo",
		model:       "claude-opus-4-7",
		target: sessionTarget{
			repoName:     "repoB",
			worktreePath: "/tmp/wt/repoB-gone",
		},
	}

	prev := worktreePathExists
	worktreePathExists = func(p string) bool { return p != "/tmp/wt/repoB-gone" }
	t.Cleanup(func() { worktreePathExists = prev })

	newModel, _ := m.Update(startMsg)
	m2 := newModel.(Model)
	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Target worktree no longer available")
	assert.Empty(t, rcB.sessionManager.GetAllSessions())
}

// confirmTask should reject a worktree that the snapshot still lists but that
// no longer exists on disk — same disk-stat policy as startSessionMsg.
func TestConfirmTask_RejectsWorktreeMissingOnDisk(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("feature"))

	prev := worktreePathExists
	worktreePathExists = func(p string) bool { return p != "/tmp/wt/feature" }
	t.Cleanup(func() { worktreePathExists = prev })

	newModel, _ := m.confirmTask(taskConfirmMsg{
		worktree: "feature",
		prompt:   "plan on removed worktree",
		isNew:    false,
	})
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Target worktree no longer available")
	assert.Empty(t, m2.sessionManager.GetAllSessions())
}

// Snapshot can lag behind reality: the worktree slice still lists the path
// after the directory was removed on disk. Submit must reject in that case.
func TestStartSessionMsg_TargetGoneOnDiskBetweenPromptAndSubmit(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	newModel, _ := m.handleKeyPress(keyPress('p'))
	m2 := newModel.(Model)
	require.True(t, m2.inputMode)

	newModel2, cmd := m2.Update(promptInputMsg{value: "plan"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd)
	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)

	prev := worktreePathExists
	worktreePathExists = func(p string) bool { return p != "/tmp/wt/A" }
	t.Cleanup(func() { worktreePathExists = prev })

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.True(t, m4.toasts.HasToasts())
	assert.Contains(t, m4.toasts.toasts[0].Message, "Target worktree no longer available")
	assert.Empty(t, m4.sessionManager.GetAllSessions())
}

// Happy path for the cold cross-repo case: target repo's worktrees are still
// loading (worktreesLoaded=false), but the path exists on disk. Submit must
// proceed and start the session against the target repo's manager.
func TestStartSessionMsg_AcceptsColdCrossRepoTargetWhenPathExists(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "A", Path: "/tmp/wt/A"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("A"))

	mgrB := injectSecondRepo(t, &m, "repoB")
	rcB := m.repos["repoB"]
	rcB.worktreesLoaded = false
	rcB.worktrees = nil

	prev := worktreePathExists
	worktreePathExists = func(string) bool { return true }
	t.Cleanup(func() { worktreePathExists = prev })

	startMsg := startSessionMsg{
		sessionType: session.SessionTypePlanner,
		prompt:      "plan on cold repo",
		model:       "claude-opus-4-7",
		target: sessionTarget{
			repoName:     "repoB",
			worktreePath: "/tmp/wt/repoB-main",
		},
	}

	newModel, _ := m.Update(startMsg)
	m2 := newModel.(Model)

	assert.Equal(t, "repoA", m2.repoName, "cross-repo dispatch must not switch active repo")
	require.Len(t, mgrB.GetAllSessions(), 1)
	assert.Equal(t, "/tmp/wt/repoB-main", mgrB.GetAllSessions()[0].WorktreePath)
}

// Plan approval (command-center "a") must apply the same disk-stat
// preflight as confirmTask / startSessionMsg. A planner left idle with a
// plan file can become stale if its worktree is removed between idle review
// and approval — the builder must not launch in that case.
func TestCommandCenter_PlanApproval_RejectsRemovedWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("feature"))

	planner := addTestSession(t, m.sessionManager, &session.Session{
		ID:           "sP",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusIdle,
		WorktreePath: "/tmp/wt/feature",
		WorktreeName: "feature",
		RepoName:     "repoA",
		PlanFilePath: "/tmp/wt/feature/PLAN.md",
		Title:        "Planner",
	})
	m.commandCenter.Show([]session.SessionInfo{planner}, m.width, m.height)
	m.focus = FocusCommandCenter

	prev := worktreePathExists
	worktreePathExists = func(p string) bool { return p != "/tmp/wt/feature" }
	t.Cleanup(func() { worktreePathExists = prev })

	newModel, _ := m.handleCommandCenter(keyPress('a'))
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Target worktree no longer available")
	// Original planner session must remain — CompleteSession should not fire.
	require.Len(t, m2.sessionManager.GetAllSessions(), 1)
	assert.Equal(t, session.SessionTypePlanner, m2.sessionManager.GetAllSessions()[0].Type)
}

// Happy path for confirmTask: existing worktree, snapshot is loaded, disk
// stat agrees the path exists. Planner session must start.
func TestConfirmTask_AcceptsExistingWorktreeWhenPathExists(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "repoA")
	require.True(t, m.worktreeDropdown.SelectByID("feature"))

	newModel, _ := m.confirmTask(taskConfirmMsg{
		worktree: "feature",
		prompt:   "plan on feature",
		isNew:    false,
	})
	m2 := newModel.(Model)

	require.Len(t, m2.sessionManager.GetAllSessions(), 1)
	sess := m2.sessionManager.GetAllSessions()[0]
	assert.Equal(t, session.SessionTypePlanner, sess.Type)
	assert.Equal(t, "/tmp/wt/feature", sess.WorktreePath)
}
