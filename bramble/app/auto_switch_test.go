package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
)

func TestWorktreeOpResult_SetsAutoSwitch(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Simulate a successful worktree create result
	newModel, _ := m.Update(worktreeOpResultMsg{
		branch:   "feature/new",
		messages: []string{"Created worktree feature/new"},
	})
	m2 := newModel.(Model)

	assert.Equal(t, "feature/new", m2.pendingWorktreeSelect)
	assert.Empty(t, m2.pendingPlannerPrompt)
}

func TestWorktreeOpResult_ErrorDoesNotSetAutoSwitch(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Simulate a failed worktree create result
	newModel, _ := m.Update(worktreeOpResultMsg{
		branch:   "feature/bad",
		messages: []string{"Failed"},
		err:      assert.AnError,
	})
	m2 := newModel.(Model)

	assert.Empty(t, m2.pendingWorktreeSelect)
}

func TestWorktreeOpResult_NoBranchDoesNotSetAutoSwitch(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Simulate a delete/sync result (no branch)
	newModel, _ := m.Update(worktreeOpResultMsg{
		messages: []string{"Deleted worktree"},
	})
	m2 := newModel.(Model)

	assert.Empty(t, m2.pendingWorktreeSelect)
}

func TestWorktreesMsg_SelectsNewWorktreeWithoutPlanner(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Set pending selection (as createWorktree would)
	m.pendingWorktreeSelect = "feature/new"
	// No planner prompt (manual 'n' key)

	// Simulate worktrees refresh arriving with the new worktree
	newModel, _ := m.Update(worktreesMsg{worktrees: []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
		{Branch: "feature/new", Path: "/tmp/wt/feature-new"},
	}})
	m2 := newModel.(Model)

	// Should have selected the new worktree
	selected := m2.worktreeDropdown.SelectedItem()
	require.NotNil(t, selected)
	assert.Equal(t, "feature/new", selected.ID)
	// Pending state should be cleared
	assert.Empty(t, m2.pendingWorktreeSelect)
	assert.Empty(t, m2.pendingPlannerPrompt)
}

func TestWorktreeOpResult_ClearsStalePlannerPrompt(t *testing.T) {
	m := setupModel(t, session.SessionModeTUI, []wt.Worktree{
		{Branch: "main", Path: "/tmp/wt/main"},
	}, "test-repo")
	m.worktreeDropdown.SelectIndex(0)

	// Simulate stale state from a task flow
	m.pendingPlannerPrompt = "stale task prompt"

	// Manual worktree create succeeds
	newModel, _ := m.Update(worktreeOpResultMsg{
		branch:   "feature/manual",
		messages: []string{"Created worktree feature/manual"},
	})
	m2 := newModel.(Model)

	assert.Equal(t, "feature/manual", m2.pendingWorktreeSelect)
	// Stale prompt should be cleared
	assert.Empty(t, m2.pendingPlannerPrompt)
}
