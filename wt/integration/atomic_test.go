package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

func TestNewAtomicSuccess(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// Create a worktree atomically
	path, err := repo.manager.NewAtomic(repo.ctx, "feature-atomic", "main", "test goal")
	require.NoError(t, err)

	// Verify worktree exists on disk
	require.DirExists(t, path)
	require.FileExists(t, filepath.Join(path, ".git"))
	require.FileExists(t, filepath.Join(path, "README.md"))

	// Verify the branch exists
	worktrees, err := repo.manager.List(repo.ctx)
	require.NoError(t, err)

	found := false
	for _, w := range worktrees {
		if w.Branch == "feature-atomic" {
			found = true
			require.Equal(t, path, w.Path)
			break
		}
	}
	require.True(t, found, "feature-atomic worktree should exist in list")

	// Verify parent tracking was set
	parent, err := repo.manager.GetParentBranch(repo.ctx, "feature-atomic", path)
	require.NoError(t, err)
	require.Equal(t, "main", parent)
}

func TestNewAtomicDefaultBase(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// Create with empty baseBranch -- should default to main
	path, err := repo.manager.NewAtomic(repo.ctx, "feature-default-base", "", "")
	require.NoError(t, err)
	require.DirExists(t, path)

	parent, err := repo.manager.GetParentBranch(repo.ctx, "feature-default-base", path)
	require.NoError(t, err)
	require.Equal(t, "main", parent)
}

func TestNewAtomicDuplicate(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// Create first worktree
	_, err := repo.manager.NewAtomic(repo.ctx, "feature-dup", "main", "")
	require.NoError(t, err)

	// Attempt duplicate -- should fail with ErrWorktreeExists
	_, err = repo.manager.NewAtomic(repo.ctx, "feature-dup", "main", "")
	require.ErrorIs(t, err, wt.ErrWorktreeExists)
}

func TestNewAtomicNotInitialized(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	// DO NOT call repo.init() -- bare dir doesn't exist

	_, err := repo.manager.NewAtomic(repo.ctx, "feature-noinit", "main", "")
	require.ErrorIs(t, err, wt.ErrRepoNotInitialized)
}

func TestNewAtomicRollbackOnBadBase(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// Attempt to create with a non-existent base branch
	_, err := repo.manager.NewAtomic(repo.ctx, "feature-badbase", "nonexistent-branch", "")
	require.Error(t, err)

	// Verify no orphaned worktree directory
	worktreePath := filepath.Join(repo.manager.RepoDir(), "feature-badbase")
	require.NoDirExists(t, worktreePath, "worktree directory should be cleaned up after failure")

	// Verify no orphaned branch
	worktrees, err := repo.manager.List(repo.ctx)
	require.NoError(t, err)
	for _, w := range worktrees {
		require.NotEqual(t, "feature-badbase", w.Branch, "orphaned branch should not exist")
	}
}

func TestAtomicOpRollbackOrder(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// Test AtomicOp directly to verify LIFO undo order
	op := repo.manager.NewAtomicOp()

	var order []int
	op.AddUndo(func(_ context.Context) error {
		order = append(order, 1)
		return nil
	})
	op.AddUndo(func(_ context.Context) error {
		order = append(order, 2)
		return nil
	})
	op.AddUndo(func(_ context.Context) error {
		order = append(order, 3)
		return nil
	})

	err := op.Rollback(repo.ctx)
	require.NoError(t, err)
	require.Equal(t, []int{3, 2, 1}, order, "undo steps should execute in reverse order")
}

func TestAtomicOpCommitPreventsRollback(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	op := repo.manager.NewAtomicOp()
	called := false
	op.AddUndo(func(_ context.Context) error {
		called = true
		return nil
	})

	op.Commit()
	err := op.Rollback(repo.ctx)
	require.NoError(t, err)
	require.False(t, called, "undo should not execute after Commit()")
}

func TestNewAtomicMultipleWorktrees(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// Create multiple atomic worktrees in sequence
	pathA, err := repo.manager.NewAtomic(repo.ctx, "feature-a", "main", "goal A")
	require.NoError(t, err)

	pathB, err := repo.manager.NewAtomic(repo.ctx, "feature-b", "main", "goal B")
	require.NoError(t, err)

	// Both should exist
	require.DirExists(t, pathA)
	require.DirExists(t, pathB)

	worktrees, err := repo.manager.List(repo.ctx)
	require.NoError(t, err)

	branches := make(map[string]bool)
	for _, w := range worktrees {
		branches[w.Branch] = true
	}
	require.True(t, branches["feature-a"])
	require.True(t, branches["feature-b"])
}

func TestNewAtomicCascadingBranches(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a from main
	pathA, err := repo.manager.NewAtomic(repo.ctx, "feature-a", "main", "")
	require.NoError(t, err)

	// Push feature-a to remote so feature-b can be based on it
	repo.commitInWorktree(pathA, "feature-a.txt", "a content", "add feature-a file")
	repo.pushWorktree(pathA, "feature-a")

	// Create feature-b from feature-a
	pathB, err := repo.manager.NewAtomic(repo.ctx, "feature-b", "feature-a", "")
	require.NoError(t, err)
	require.DirExists(t, pathB)

	// Verify parent tracking
	parentB, err := repo.manager.GetParentBranch(repo.ctx, "feature-b", pathB)
	require.NoError(t, err)
	require.Equal(t, "feature-a", parentB)

	// Verify feature-a's file is present in feature-b
	require.FileExists(t, filepath.Join(pathB, "feature-a.txt"))
}

func TestNewAtomicCleanupAfterPartialFailure(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	repo.init()

	// First create a worktree normally
	path, err := repo.manager.NewAtomic(repo.ctx, "feature-partial", "main", "")
	require.NoError(t, err)
	require.DirExists(t, path)

	// Remove it so we can test cleanup
	err = repo.manager.Remove(repo.ctx, "feature-partial", true)
	require.NoError(t, err)

	// Verify clean removal
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "worktree directory should be removed")
}
