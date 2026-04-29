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
			assert.Equal(t, sessionTargetCapturedSelection, startMsg.target.mode)

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
	m.repos["repoB"].worktrees = []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/repoB-main"},
	}

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
	assert.Equal(t, sessionTargetCapturedSelection, startMsg.target.mode)

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
	assert.Equal(t, sessionTargetExistingSession, startMsg.target.mode)

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
	require.Equal(t, sessionTargetExistingSession, startMsg.target.mode)

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
