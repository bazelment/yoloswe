package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func pressKey(m Model, key rune) Model {
	newModel, _ := m.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
	return newModel.(Model)
}

func TestMergeKey_NoWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, nil, "test-repo")

	m2 := pressKey(m, 'm')

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Select a worktree first")
}

func TestMergeKey_StatusNotLoaded(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	m2 := pressKey(m, 'm')

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "status not loaded")
}

func TestMergeKey_NoPR(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 0},
	}

	m2 := pressKey(m, 'm')

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "No PR found")
}

func TestMergeKey_PRNotOpen(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 42, PRState: "MERGED"},
	}

	m2 := pressKey(m, 'm')

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "PR #42 is MERGED")
}

func TestMergeKey_DirtyWorktree(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 42, PRState: "OPEN", IsDirty: true},
	}

	m2 := pressKey(m, 'm')

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Uncommitted changes")
}

func TestMergeKey_UnpushedCommits(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 42, PRState: "OPEN", Ahead: 3},
	}

	m2 := pressKey(m, 'm')

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "3 unpushed commits")
}

func TestMergeKey_ActiveSession(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 42, PRState: "OPEN"},
	}

	// Start a running session for this worktree
	_, err := m.sessionManager.StartSession(session.SessionTypePlanner, "/tmp/wt/feature", "test")
	require.NoError(t, err)

	m2 := pressKey(m, 'm')

	assert.True(t, m2.toasts.HasToasts())
	assert.Contains(t, m2.toasts.toasts[0].Message, "Stop active sessions")
}

func TestMergeKey_ShowsConfirmPrompt(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 42, PRState: "OPEN", PRReviewStatus: "APPROVED"},
	}

	m2 := pressKey(m, 'm')

	assert.Equal(t, FocusConfirm, m2.focus)
	assert.NotNil(t, m2.confirmPrompt)
	assert.Contains(t, m2.confirmPrompt.message, "Merge PR #42")
	assert.Contains(t, m2.confirmPrompt.message, "APPROVED")
}

func TestMergeKey_DraftPRShownInPrompt(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 42, PRState: "OPEN", PRIsDraft: true},
	}

	m2 := pressKey(m, 'm')

	assert.Equal(t, FocusConfirm, m2.focus)
	assert.Contains(t, m2.confirmPrompt.message, "[Draft PR]")
}

func TestMergeKey_IdleSessionAllowed(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)
	m.worktreeStatuses = map[string]*wt.WorktreeStatus{
		"feature": {PRNumber: 42, PRState: "OPEN"},
	}

	// Inject an idle session directly to avoid async races
	m.sessions = []session.SessionInfo{
		{ID: "s1", Status: session.StatusIdle, WorktreePath: "/tmp/wt/feature"},
	}

	m2 := pressKey(m, 'm')

	// Idle sessions should NOT block merge - confirm prompt should appear
	assert.Equal(t, FocusConfirm, m2.focus)
	assert.NotNil(t, m2.confirmPrompt)
}

func TestMergePRDone_Error(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")

	msg := mergePRDoneMsg{
		branch:   "feature",
		prNumber: 0,
		messages: []string{"some output"},
		err:      assert.AnError,
	}

	newModel, _ := m.Update(msg)
	m2 := newModel.(Model)

	assert.True(t, m2.toasts.HasToasts())
	assert.Equal(t, ToastError, m2.toasts.toasts[0].Level)
}

func TestMergePRDone_Success_ShowsPostMergePrompt(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")

	msg := mergePRDoneMsg{
		branch:   "feature",
		prNumber: 42,
		messages: []string{"Merged PR #42"},
	}

	newModel, _ := m.Update(msg)
	m2 := newModel.(Model)

	assert.Equal(t, FocusConfirm, m2.focus)
	assert.NotNil(t, m2.confirmPrompt)
	assert.Contains(t, m2.confirmPrompt.message, "PR #42 merged!")
	assert.Contains(t, m2.confirmPrompt.message, "feature")
}

func TestPostMergeAction_Keep(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "feature", Path: "/tmp/wt/feature"},
	}, "test-repo")

	msg := postMergeActionMsg{branch: "feature", action: "keep"}

	newModel, cmd := m.Update(msg)
	m2 := newModel.(Model)

	// "keep" should refresh worktrees (cmd should be non-nil batch)
	assert.NotNil(t, cmd)
	// No toasts - just a refresh
	assert.False(t, m2.toasts.HasToasts())
}
