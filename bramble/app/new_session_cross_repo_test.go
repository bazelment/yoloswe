package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func injectSecondRepo(t *testing.T, m *Model, repoName string) *session.Manager {
	t.Helper()
	mgr := session.NewManagerWithConfig(session.ManagerConfig{
		SessionMode: session.SessionModeTUI,
		RepoName:    repoName,
	})
	t.Cleanup(func() { mgr.Close() })
	m.repos[repoName] = &RepoContext{
		sessionManager:   mgr,
		worktreeDropdown: NewDropdown(nil),
		sessionDropdown:  NewDropdown(nil),
		scrollPositions:  make(map[session.SessionID]int),
	}
	m.openedRepos = append(m.openedRepos, repoName)
	return mgr
}

func addTestSession(t *testing.T, mgr *session.Manager, sess *session.Session) session.SessionInfo {
	t.Helper()
	mgr.AddSession(sess)
	info, ok := mgr.GetSessionInfo(sess.ID)
	require.True(t, ok)
	return info
}

func setRepoWorktrees(m *Model, repoName string, worktrees []wt.Worktree) {
	rc := m.repos[repoName]
	rc.worktrees = worktrees

	items := make([]DropdownItem, 0, len(worktrees))
	for _, wt := range worktrees {
		items = append(items, DropdownItem{ID: wt.Branch, Label: wt.Branch})
	}
	rc.worktreeDropdown.SetItems(items)
	if len(items) > 0 {
		rc.worktreeDropdown.SelectIndex(0)
	}
}

func TestNewSessionFromOverlay_CrossRepo_UsesTargetManager(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/repoA-main"},
	}, "repoA")
	m.worktreeDropdown.SelectIndex(0)

	mgrB := injectSecondRepo(t, &m, "repoB")
	setRepoWorktrees(&m, "repoB", []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/repoB-feature"},
	})

	repoBSess := addTestSession(t, mgrB, &session.Session{
		ID:           "b1",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/repoB-feature",
		WorktreeName: "feature",
		RepoName:     "repoB",
		Title:        "Repo B session",
	})
	m.allSessionsOverlay.Show([]session.SessionInfo{repoBSess}, m.width, m.height)
	m.focus = FocusAllSessions

	newModel, _ := m.handleAllSessionsOverlay(keyPress('b'))
	m2 := newModel.(Model)

	assert.Equal(t, "repoB", m2.repoName, "main view should show the selected session repo")
	assert.True(t, m2.inputMode, "should be in input mode waiting for prompt")
	assert.Equal(t, session.SessionID("b1"), m2.viewingSessionID)
	selected := m2.worktreeDropdown.SelectedItem()
	require.NotNil(t, selected)
	assert.Equal(t, "feature", selected.ID, "prompt view should show the selected session worktree")

	newModel2, cmd := m2.Update(promptInputMsg{value: "do the thing"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd, "promptInputMsg must return a cmd")
	msg := cmd()
	startMsg, ok := msg.(startSessionMsg)
	require.True(t, ok, "expected startSessionMsg, got %T", msg)
	assert.Equal(t, "repoB", startMsg.target.repoName)
	assert.Equal(t, "/tmp/wt/repoB-feature", startMsg.target.worktreePath)

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.Equal(t, "repoB", m4.repoName, "main view should stay on the visible target repo")

	bSessions := mgrB.GetAllSessions()
	require.Len(t, bSessions, 2, "new session should be on repoB's manager")
	assert.Equal(t, "repoB", bSessions[0].RepoName)
	assert.Equal(t, "/tmp/wt/repoB-feature", bSessions[0].WorktreePath)

	aSessions := m4.repos["repoA"].sessionManager.GetAllSessions()
	assert.Empty(t, aSessions, "repoA manager must not receive the new session")
}

func TestNewSessionFromCommandCenter_CrossRepo_UsesTargetManager(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/repoA-main"},
	}, "repoA")
	m.worktreeDropdown.SelectIndex(0)

	mgrB := injectSecondRepo(t, &m, "repoB")
	setRepoWorktrees(&m, "repoB", []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/repoB-feature"},
	})

	repoBSess := addTestSession(t, mgrB, &session.Session{
		ID:           "b1",
		Type:         session.SessionTypePlanner,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/repoB-feature",
		WorktreeName: "feature",
		RepoName:     "repoB",
		Title:        "Repo B session",
	})
	m.commandCenter.Show([]session.SessionInfo{repoBSess}, m.width, m.height)
	m.focus = FocusCommandCenter

	newModel, _ := m.handleCommandCenter(keyPress('p'))
	m2 := newModel.(Model)

	assert.Equal(t, "repoB", m2.repoName)
	assert.True(t, m2.inputMode)
	assert.Equal(t, session.SessionID("b1"), m2.viewingSessionID)
	selected := m2.worktreeDropdown.SelectedItem()
	require.NotNil(t, selected)
	assert.Equal(t, "feature", selected.ID)

	newModel2, cmd := m2.Update(promptInputMsg{value: "plan something"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd)
	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)
	assert.Equal(t, "repoB", startMsg.target.repoName)
	assert.Equal(t, session.SessionTypePlanner, startMsg.sessionType)
	assert.Equal(t, "/tmp/wt/repoB-feature", startMsg.target.worktreePath)

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.Equal(t, "repoB", m4.repoName)
	assert.Len(t, mgrB.GetAllSessions(), 2)
	assert.Empty(t, m4.repos["repoA"].sessionManager.GetAllSessions())
}

func TestNewSessionFromOverlay_SameRepo_SyncsWorktreeDropdown(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0) // start on "main"

	sess := addTestSession(t, m.sessionManager, &session.Session{
		ID:           "s1",
		Type:         session.SessionTypeBuilder,
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature",
		WorktreeName: "feature",
		RepoName:     "test-repo",
		Title:        "Feature session",
	})
	m.allSessionsOverlay.Show([]session.SessionInfo{sess}, m.width, m.height)
	m.focus = FocusAllSessions

	newModel, _ := m.handleAllSessionsOverlay(keyPress('b'))
	m2 := newModel.(Model)

	assert.True(t, m2.inputMode)
	selected := m2.worktreeDropdown.SelectedItem()
	require.NotNil(t, selected)
	assert.Equal(t, "feature", selected.ID, "same-repo overlay must sync worktree dropdown")
	assert.Equal(t, session.SessionID("s1"), m2.viewingSessionID, "prompt view should show selected session output")
	assert.Contains(t, m2.View().Content, "Feature session")
}
