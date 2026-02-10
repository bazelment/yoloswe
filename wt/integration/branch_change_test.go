package integration

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBranchCheckoutInWorktree verifies that after checking out a new branch
// in a worktree, List() returns the new branch and Name() returns the old directory name.
func TestBranchCheckoutInWorktree(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create worktree for feature-a
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main", "")
	require.NoError(t, err)

	// Checkout a new branch inside the worktree
	repo.checkoutBranch(featureAPath, "new-branch", true)

	// List should show new-branch (git reports the current branch)
	worktrees, err := repo.manager.List(repo.ctx)
	require.NoError(t, err)

	var foundNewBranch bool
	for _, wt := range worktrees {
		if wt.Branch == "new-branch" {
			foundNewBranch = true
			// Name() should return directory name (feature-a), not branch name
			require.Equal(t, "feature-a", wt.Name(),
				"Name() should return directory name, not checked-out branch")
		}
	}
	require.True(t, foundNewBranch, "List() should show the new checked-out branch")
}

// TestParentTrackingSurvivesBranchChange verifies that GetParentBranch
// still returns the correct parent via directory-name fallback after
// checking out a different branch inside the worktree.
func TestParentTrackingSurvivesBranchChange(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a from main (parent:main is stored under branch.feature-a.description)
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main", "")
	require.NoError(t, err)

	// Verify parent before branch change
	parent, err := repo.manager.GetParentBranch(repo.ctx, "feature-a", featureAPath)
	require.NoError(t, err)
	require.Equal(t, "main", parent)

	// Checkout a new branch
	repo.checkoutBranch(featureAPath, "diverged-branch", true)

	// GetParentBranch("diverged-branch", ...) should fall back to directory name "feature-a"
	parent, err = repo.manager.GetParentBranch(repo.ctx, "diverged-branch", featureAPath)
	require.NoError(t, err)
	require.Equal(t, "main", parent,
		"GetParentBranch should fall back to directory name and find parent:main")
}

// TestGoalSurvivesBranchChange verifies that GetGoal still returns
// the goal via directory-name fallback after checking out a different branch.
func TestGoalSurvivesBranchChange(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create worktree with a goal
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main", "implement auth system")
	require.NoError(t, err)

	// Verify goal before branch change
	goal, err := repo.manager.GetGoal(repo.ctx, "feature-a", featureAPath)
	require.NoError(t, err)
	require.Equal(t, "implement auth system", goal)

	// Checkout a new branch
	repo.checkoutBranch(featureAPath, "diverged-branch", true)

	// GetGoal("diverged-branch", ...) should fall back to directory name "feature-a"
	goal, err = repo.manager.GetGoal(repo.ctx, "diverged-branch", featureAPath)
	require.NoError(t, err)
	require.Equal(t, "implement auth system", goal,
		"GetGoal should fall back to directory name and find the original goal")
}

// TestSyncAfterBranchChange verifies that Sync still correctly rebases
// a worktree after the user checked out a different branch inside it.
// The parent tracking via directory-name fallback is critical for sync.
func TestSyncAfterBranchChange(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a from main
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	// Checkout new branch inside the worktree
	repo.checkoutBranch(featureAPath, "new-branch", true)
	repo.commitInWorktree(featureAPath, "new-branch.txt", "new-branch content\n", "add new-branch work")

	// Push the new branch so origin/new-branch exists for ahead/behind tracking
	repo.pushWorktree(featureAPath, "new-branch")

	// Add a commit to origin/main
	repo.addRemoteCommit("main", "main-update.txt", "main update content\n", "update main")

	// Before sync: should not have the main update
	require.False(t, fileExists(filepath.Join(featureAPath, "main-update.txt")))

	// Sync all worktrees — the new-branch worktree should still be rebased
	// because GetParentBranch falls back to directory name "feature-a" -> parent:main
	err = repo.manager.Sync(repo.ctx, "")
	require.NoError(t, err)

	// After sync: the worktree should have the main update
	require.True(t, fileExists(filepath.Join(featureAPath, "main-update.txt")),
		"worktree should contain main-update.txt after sync (parent found via dir name fallback)")
	// Own work preserved
	require.True(t, fileExists(filepath.Join(featureAPath, "new-branch.txt")),
		"worktree should still have its own work")
}

// TestSyncSingleBranchAfterChange verifies that Sync(ctx, branchName) uses
// the branch name (not directory name) to find the worktree.
func TestSyncSingleBranchAfterChange(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a from main
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	// Checkout new branch
	repo.checkoutBranch(featureAPath, "new-branch", true)
	repo.pushWorktree(featureAPath, "new-branch")

	// Add a commit to origin/main
	repo.addRemoteCommit("main", "main-update.txt", "main update\n", "update main")

	// Sync with old directory name should fail (no worktree has branch "feature-a" anymore)
	err = repo.manager.Sync(repo.ctx, "feature-a")
	require.Error(t, err, "Sync with old branch name should fail")

	// Sync with new branch name should succeed
	err = repo.manager.Sync(repo.ctx, "new-branch")
	require.NoError(t, err, "Sync with new branch name should succeed")

	require.True(t, fileExists(filepath.Join(featureAPath, "main-update.txt")),
		"worktree should contain main-update.txt after sync with new branch name")
}
