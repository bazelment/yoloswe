package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

// injectSecondRepo adds a second repo to the Model's repos map with its own
// session.Manager, so tests can exercise cross-repo overlay flows without
// going through openRepo's filesystem checks.
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

func TestNewSessionFromOverlay_CrossRepo_UsesTargetManager(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/repoA-main"},
	}, "repoA")
	m.worktreeDropdown.SelectIndex(0)

	mgrB := injectSecondRepo(t, &m, "repoB")

	repoBSess := session.SessionInfo{
		ID:           "b1",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/repoB-feature",
		WorktreeName: "feature",
		RepoName:     "repoB",
	}
	m.allSessionsOverlay.Show([]session.SessionInfo{repoBSess}, m.width, m.height)
	m.focus = FocusAllSessions

	// Capture the active worktree dropdown selection before the keypress so we
	// can assert it was not mutated by the overlay handler.
	dropdownBefore := m.worktreeDropdown.SelectedItem()

	newModel, _ := m.handleAllSessionsOverlay(keyPress('b'))
	m2 := newModel.(Model)

	assert.Equal(t, "repoA", m2.repoName, "main view must stay on repoA")
	assert.True(t, m2.inputMode, "should be in input mode waiting for prompt")
	if dropdownBefore != nil {
		after := m2.worktreeDropdown.SelectedItem()
		require.NotNil(t, after)
		assert.Equal(t, dropdownBefore.ID, after.ID, "worktree dropdown must not change for cross-repo")
	}

	// Drive the prompt submission. The handler returns a tea.Cmd that yields a
	// startSessionMsg — inspect it before dispatching to verify the target repo
	// and worktree were captured correctly.
	newModel2, cmd := m2.Update(promptInputMsg{value: "do the thing"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd, "promptInputMsg must return a cmd")
	msg := cmd()
	startMsg, ok := msg.(startSessionMsg)
	require.True(t, ok, "expected startSessionMsg, got %T", msg)
	assert.Equal(t, "repoB", startMsg.repoName)
	assert.Equal(t, "/tmp/wt/repoB-feature", startMsg.worktreePath)

	// Dispatch the startSessionMsg and assert the new session landed on repoB's
	// manager with the correct metadata, while repoA stays untouched.
	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.Equal(t, "repoA", m4.repoName, "main view must still be on repoA")

	bSessions := mgrB.GetAllSessions()
	require.Len(t, bSessions, 1, "new session should be on repoB's manager")
	assert.Equal(t, "repoB", bSessions[0].RepoName)
	assert.Equal(t, "/tmp/wt/repoB-feature", bSessions[0].WorktreePath)

	aSessions := m4.sessionManager.GetAllSessions()
	assert.Empty(t, aSessions, "repoA manager must not receive the new session")
}

func TestNewSessionFromCommandCenter_CrossRepo_UsesTargetManager(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/repoA-main"},
	}, "repoA")
	m.worktreeDropdown.SelectIndex(0)

	mgrB := injectSecondRepo(t, &m, "repoB")

	repoBSess := session.SessionInfo{
		ID:           "b1",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/repoB-feature",
		WorktreeName: "feature",
		RepoName:     "repoB",
	}
	m.commandCenter.Show([]session.SessionInfo{repoBSess}, m.width, m.height)
	m.focus = FocusCommandCenter

	newModel, _ := m.handleCommandCenter(keyPress('p'))
	m2 := newModel.(Model)

	assert.Equal(t, "repoA", m2.repoName)
	assert.True(t, m2.inputMode)

	newModel2, cmd := m2.Update(promptInputMsg{value: "plan something"})
	m3 := newModel2.(Model)
	require.NotNil(t, cmd)
	startMsg, ok := cmd().(startSessionMsg)
	require.True(t, ok)
	assert.Equal(t, "repoB", startMsg.repoName)
	assert.Equal(t, session.SessionTypePlanner, startMsg.sessionType)
	assert.Equal(t, "/tmp/wt/repoB-feature", startMsg.worktreePath)

	newModel3, _ := m3.Update(startMsg)
	m4 := newModel3.(Model)

	assert.Equal(t, "repoA", m4.repoName)
	assert.Len(t, mgrB.GetAllSessions(), 1)
	assert.Empty(t, m4.sessionManager.GetAllSessions())
}

func TestNewSessionFromOverlay_SameRepo_SyncsWorktreeDropdown(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0) // start on "main"

	// Highlight a session on the "feature" worktree in the same repo.
	sess := session.SessionInfo{
		ID:           "s1",
		Status:       session.StatusRunning,
		WorktreePath: "/tmp/wt/feature",
		WorktreeName: "feature",
		RepoName:     "test-repo",
	}
	m.allSessionsOverlay.Show([]session.SessionInfo{sess}, m.width, m.height)
	m.focus = FocusAllSessions

	newModel, _ := m.handleAllSessionsOverlay(keyPress('b'))
	m2 := newModel.(Model)

	assert.True(t, m2.inputMode)
	selected := m2.worktreeDropdown.SelectedItem()
	require.NotNil(t, selected)
	assert.Equal(t, "feature", selected.ID, "same-repo overlay must sync worktree dropdown")
}
