package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

// testRepo sets up a wt-managed repository with a local "remote" for integration testing.
type testRepo struct {
	ctx       context.Context
	t         *testing.T
	git       *wt.DefaultGitRunner
	manager   *wt.Manager
	remoteDir string
	root      string
	repoName  string
}

// newTestRepo creates a test repository setup with a local remote.
func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	ctx := context.Background()
	git := &wt.DefaultGitRunner{}

	// Create a bare "remote" repository
	remoteDir := t.TempDir()
	_, err := git.Run(ctx, []string{"init", "--bare"}, remoteDir)
	require.NoError(t, err, "git init --bare failed")

	// Create a temporary clone to set up initial content
	setupDir := t.TempDir()
	_, err = git.Run(ctx, []string{"clone", remoteDir, setupDir}, "")
	require.NoError(t, err, "git clone failed")

	// Configure git user and create initial commit
	git.Run(ctx, []string{"config", "user.email", "test@test.com"}, setupDir)
	git.Run(ctx, []string{"config", "user.name", "Test"}, setupDir)
	require.NoError(t, os.WriteFile(filepath.Join(setupDir, "README.md"), []byte("# Test Repo\n"), 0644))
	git.Run(ctx, []string{"add", "."}, setupDir)
	git.Run(ctx, []string{"commit", "-m", "initial commit"}, setupDir)
	git.Run(ctx, []string{"branch", "-M", "main"}, setupDir)
	_, err = git.Run(ctx, []string{"push", "-u", "origin", "main"}, setupDir)
	require.NoError(t, err, "git push failed")
	git.Run(ctx, []string{"symbolic-ref", "HEAD", "refs/heads/main"}, remoteDir)

	root := t.TempDir()
	output := wt.NewOutput(&bytes.Buffer{}, false)
	manager := wt.NewManager(root, "test-repo", wt.WithOutput(output))

	return &testRepo{
		t:         t,
		ctx:       ctx,
		git:       git,
		remoteDir: remoteDir,
		root:      root,
		repoName:  "test-repo",
		manager:   manager,
	}
}

func (r *testRepo) init() string {
	r.t.Helper()
	r.t.Logf("Initializing wt repo from remote: %s", r.remoteDir)
	mainPath, err := r.manager.Init(r.ctx, r.remoteDir)
	require.NoError(r.t, err, "Manager.Init failed")

	r.repoName = wt.GetRepoNameFromURL(r.remoteDir)
	output := wt.NewOutput(&bytes.Buffer{}, false)
	r.manager = wt.NewManager(r.root, r.repoName, wt.WithOutput(output))
	r.t.Logf("  -> main worktree at: %s", mainPath)
	return mainPath
}

func (r *testRepo) pushBranch(branch string) {
	r.t.Helper()
	r.t.Logf("Creating and pushing branch %q to remote", branch)
	tmpDir := r.t.TempDir()
	_, err := r.git.Run(r.ctx, []string{"clone", r.remoteDir, tmpDir}, "")
	require.NoError(r.t, err)

	r.git.Run(r.ctx, []string{"config", "user.email", "test@test.com"}, tmpDir)
	r.git.Run(r.ctx, []string{"config", "user.name", "Test"}, tmpDir)
	r.git.Run(r.ctx, []string{"checkout", "-b", branch}, tmpDir)
	require.NoError(r.t, os.WriteFile(filepath.Join(tmpDir, branch+".txt"), []byte(branch+" content\n"), 0644))
	r.git.Run(r.ctx, []string{"add", "."}, tmpDir)
	r.git.Run(r.ctx, []string{"commit", "-m", "add " + branch}, tmpDir)
	r.git.Run(r.ctx, []string{"push", "-u", "origin", branch}, tmpDir)
}

func (r *testRepo) pushWorktree(wtPath, branch string) {
	r.t.Helper()
	r.t.Logf("Pushing worktree branch %q to remote", branch)
	r.git.Run(r.ctx, []string{"config", "user.email", "test@test.com"}, wtPath)
	r.git.Run(r.ctx, []string{"config", "user.name", "Test"}, wtPath)
	_, err := r.git.Run(r.ctx, []string{"push", "-u", "origin", branch}, wtPath)
	require.NoError(r.t, err, "git push failed for "+branch)
}

// commitInWorktree creates a commit in a worktree with a new file.
func (r *testRepo) commitInWorktree(wtPath, filename, content, message string) {
	r.t.Helper()
	r.t.Logf("Committing in worktree: %s (file: %s)", filepath.Base(wtPath), filename)
	r.git.Run(r.ctx, []string{"config", "user.email", "test@test.com"}, wtPath)
	r.git.Run(r.ctx, []string{"config", "user.name", "Test"}, wtPath)
	require.NoError(r.t, os.WriteFile(filepath.Join(wtPath, filename), []byte(content), 0644))
	r.git.Run(r.ctx, []string{"add", "."}, wtPath)
	_, err := r.git.Run(r.ctx, []string{"commit", "-m", message}, wtPath)
	require.NoError(r.t, err, "commit failed in "+wtPath)
}

// addRemoteCommit adds a commit to the remote repository on the specified branch.
func (r *testRepo) addRemoteCommit(branch, filename, content, message string) {
	r.t.Helper()
	r.t.Logf("Adding remote commit to %q: %s (file: %s)", branch, message, filename)
	tmpDir := r.t.TempDir()
	_, err := r.git.Run(r.ctx, []string{"clone", r.remoteDir, tmpDir}, "")
	require.NoError(r.t, err)

	r.git.Run(r.ctx, []string{"config", "user.email", "test@test.com"}, tmpDir)
	r.git.Run(r.ctx, []string{"config", "user.name", "Test"}, tmpDir)
	r.git.Run(r.ctx, []string{"checkout", branch}, tmpDir)
	require.NoError(r.t, os.WriteFile(filepath.Join(tmpDir, filename), []byte(content), 0644))
	r.git.Run(r.ctx, []string{"add", "."}, tmpDir)
	r.git.Run(r.ctx, []string{"commit", "-m", message}, tmpDir)
	_, err = r.git.Run(r.ctx, []string{"push", "origin", branch}, tmpDir)
	require.NoError(r.t, err, "push failed for "+branch)
}

// deleteRemoteBranch deletes a branch from the remote repository.
func (r *testRepo) deleteRemoteBranch(branch string) {
	r.t.Helper()
	r.t.Logf("Deleting remote branch %q", branch)
	tmpDir := r.t.TempDir()
	_, err := r.git.Run(r.ctx, []string{"clone", r.remoteDir, tmpDir}, "")
	require.NoError(r.t, err)

	_, err = r.git.Run(r.ctx, []string{"push", "origin", "--delete", branch}, tmpDir)
	require.NoError(r.t, err, "delete remote branch failed for "+branch)
}

// remoteBranchExists checks if a branch exists on the remote.
func (r *testRepo) remoteBranchExists(branch string) bool {
	r.t.Helper()
	exists, err := wt.RemoteBranchExists(r.ctx, r.git, branch, r.manager.BareDir())
	require.NoError(r.t, err)
	r.t.Logf("Checking remote branch %q exists: %v", branch, exists)
	return exists
}

// getWorktreeCommit returns the HEAD commit hash of a worktree.
func (r *testRepo) getWorktreeCommit(wtPath string) string {
	r.t.Helper()
	result, err := r.git.Run(r.ctx, []string{"rev-parse", "HEAD"}, wtPath)
	require.NoError(r.t, err)
	hash := strings.TrimSpace(result.Stdout)
	r.t.Logf("Worktree %s HEAD: %s", filepath.Base(wtPath), hash[:8])
	return hash
}

// isRebaseInProgress checks if a worktree is in the middle of a rebase.
func (r *testRepo) isRebaseInProgress(branch string) bool {
	r.t.Helper()
	bareDir := r.manager.BareDir()
	rebaseMerge := filepath.Join(bareDir, "worktrees", branch, "rebase-merge")
	rebaseApply := filepath.Join(bareDir, "worktrees", branch, "rebase-apply")
	inProgress := fileExists(rebaseMerge) || fileExists(rebaseApply)
	r.t.Logf("Checking rebase in progress for %q: %v", branch, inProgress)
	return inProgress
}

// abortRebase aborts any in-progress rebase in the worktree.
func (r *testRepo) abortRebase(wtPath string) {
	r.t.Helper()
	r.t.Logf("Aborting rebase in %s", filepath.Base(wtPath))
	r.git.Run(r.ctx, []string{"rebase", "--abort"}, wtPath)
}

// fileExists checks if a file exists in a directory.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestWorktreeLifecycle tests the full lifecycle: init, new, status, remove.
func TestWorktreeLifecycle(t *testing.T) {
	repo := newTestRepo(t)

	// Init
	mainPath := repo.init()
	require.DirExists(t, mainPath)
	require.DirExists(t, repo.manager.BareDir())

	// New
	featurePath, err := repo.manager.New(repo.ctx, "feature-a", "main")
	require.NoError(t, err)
	require.DirExists(t, featurePath)

	// List
	worktrees, err := repo.manager.List(repo.ctx)
	require.NoError(t, err)
	require.Len(t, worktrees, 2)

	// Find main worktree for status tests
	var mainWt wt.Worktree
	for _, w := range worktrees {
		if w.Branch == "main" {
			mainWt = w
			break
		}
	}

	// Status (clean)
	status, err := repo.manager.GetStatus(repo.ctx, mainWt)
	require.NoError(t, err)
	require.False(t, status.IsDirty)

	// Status (dirty)
	require.NoError(t, os.WriteFile(filepath.Join(mainPath, "dirty.txt"), []byte("dirty"), 0644))
	status, err = repo.manager.GetStatus(repo.ctx, mainWt)
	require.NoError(t, err)
	require.True(t, status.IsDirty)

	// Remove
	err = repo.manager.Remove(repo.ctx, "feature-a", false)
	require.NoError(t, err)
	require.NoDirExists(t, featurePath)

	worktrees, err = repo.manager.List(repo.ctx)
	require.NoError(t, err)
	require.Len(t, worktrees, 1)
}

// TestCascadingBranches tests cascading branch chain with parent tracking.
func TestCascadingBranches(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create chain: main -> feature-a -> feature-b -> feature-c
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main")
	require.NoError(t, err)
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a")
	require.NoError(t, err)
	repo.pushWorktree(featureBPath, "feature-b")

	featureCPath, err := repo.manager.New(repo.ctx, "feature-c", "feature-b")
	require.NoError(t, err)

	// Verify parent tracking
	parentA, _ := repo.manager.GetParentBranch(repo.ctx, "feature-a", featureAPath)
	parentB, _ := repo.manager.GetParentBranch(repo.ctx, "feature-b", featureBPath)
	parentC, _ := repo.manager.GetParentBranch(repo.ctx, "feature-c", featureCPath)

	require.Empty(t, parentA, "feature-a should not have parent (based on default)")
	require.Equal(t, "feature-a", parentB)
	require.Equal(t, "feature-b", parentC)

	worktrees, err := repo.manager.List(repo.ctx)
	require.NoError(t, err)
	require.Len(t, worktrees, 4)
}

// TestOpenExistingBranch tests opening an existing remote branch.
func TestOpenExistingBranch(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Push branch after init
	repo.pushBranch("existing-feature")

	wtPath, err := repo.manager.Open(repo.ctx, "existing-feature")
	require.NoError(t, err)
	require.DirExists(t, wtPath)
	require.FileExists(t, filepath.Join(wtPath, "existing-feature.txt"))
}

// TestErrorCases tests various error conditions.
func TestErrorCases(t *testing.T) {
	t.Run("repo not initialized", func(t *testing.T) {
		root := t.TempDir()
		output := wt.NewOutput(&bytes.Buffer{}, false)
		manager := wt.NewManager(root, "not-initialized", wt.WithOutput(output))
		ctx := context.Background()

		_, err := manager.New(ctx, "feature", "main")
		require.ErrorIs(t, err, wt.ErrRepoNotInitialized)

		_, err = manager.Open(ctx, "feature")
		require.ErrorIs(t, err, wt.ErrRepoNotInitialized)
	})

	t.Run("worktree already exists", func(t *testing.T) {
		repo := newTestRepo(t)
		repo.init()

		_, err := repo.manager.New(repo.ctx, "feature-a", "main")
		require.NoError(t, err)

		_, err = repo.manager.New(repo.ctx, "feature-a", "main")
		require.ErrorIs(t, err, wt.ErrWorktreeExists)
	})

	t.Run("branch not found", func(t *testing.T) {
		repo := newTestRepo(t)
		repo.init()

		_, err := repo.manager.Open(repo.ctx, "nonexistent-branch")
		require.ErrorIs(t, err, wt.ErrBranchNotFound)
	})
}

// TestSyncRebasesWorktrees tests that Sync() fetches and rebases worktrees onto their remote tracking branch.
func TestSyncRebasesWorktrees(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a from main with its own commit
	t.Log("Creating worktree feature-a from main")
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	// Add another commit to origin/feature-a remotely (simulating teammate push)
	repo.addRemoteCommit("feature-a", "remote-update.txt", "remote update content\n", "teammate added remote update")

	// Before sync: local feature-a should NOT have the remote update
	t.Log("Verifying local worktree does not have remote update yet")
	require.False(t, fileExists(filepath.Join(featureAPath, "remote-update.txt")))

	// Sync - should rebase local onto origin/feature-a
	t.Log("Calling Sync() to rebase worktrees")
	err = repo.manager.Sync(repo.ctx)
	require.NoError(t, err)

	// After sync: feature-a should have the remote update (rebased)
	t.Log("Verifying feature-a has remote update after sync")
	require.True(t, fileExists(filepath.Join(featureAPath, "remote-update.txt")),
		"feature-a should contain remote-update.txt after rebase")
	// And feature-a's own work should be preserved
	require.True(t, fileExists(filepath.Join(featureAPath, "feature-a.txt")),
		"feature-a should still have its own work")
}

// TestSyncMultipleWorktrees tests that Sync() handles multiple worktrees with remote updates.
func TestSyncMultipleWorktrees(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create chain: main -> feature-a -> feature-b
	t.Log("Creating worktree chain: main -> feature-a -> feature-b")
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a")
	require.NoError(t, err)
	repo.commitInWorktree(featureBPath, "feature-b.txt", "feature-b content\n", "add feature-b work")
	repo.pushWorktree(featureBPath, "feature-b")

	// Add remote commits to both branches (simulating teammate pushes)
	t.Log("Simulating teammate pushes to both branches")
	repo.addRemoteCommit("feature-a", "remote-a.txt", "remote a content\n", "teammate update to feature-a")
	repo.addRemoteCommit("feature-b", "remote-b.txt", "remote b content\n", "teammate update to feature-b")

	// Before sync: neither branch should have the remote updates
	t.Log("Verifying local worktrees do not have remote updates yet")
	require.False(t, fileExists(filepath.Join(featureAPath, "remote-a.txt")))
	require.False(t, fileExists(filepath.Join(featureBPath, "remote-b.txt")))

	// Sync - should rebase both branches onto their respective remotes
	t.Log("Calling Sync() to rebase all worktrees")
	err = repo.manager.Sync(repo.ctx)
	require.NoError(t, err)

	// After sync:
	t.Log("Verifying both branches have their remote updates")
	// - feature-a should have remote-a.txt
	require.True(t, fileExists(filepath.Join(featureAPath, "remote-a.txt")),
		"feature-a should contain remote-a.txt after rebase")
	require.True(t, fileExists(filepath.Join(featureAPath, "feature-a.txt")),
		"feature-a should still have its own work")

	// - feature-b should have remote-b.txt
	require.True(t, fileExists(filepath.Join(featureBPath, "remote-b.txt")),
		"feature-b should contain remote-b.txt after rebase")
	require.True(t, fileExists(filepath.Join(featureBPath, "feature-b.txt")),
		"feature-b should still have its own work")
}

// TestSyncConflictHandling tests that rebase conflicts are handled gracefully.
func TestSyncConflictHandling(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a and add conflict.txt with one content
	t.Log("Creating worktree feature-a with conflict.txt")
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "conflict.txt", "local version\n", "add conflict.txt locally")
	repo.pushWorktree(featureAPath, "feature-a")

	// Create divergent history on remote by force pushing a conflicting change
	t.Log("Force-pushing conflicting change to remote (simulating teammate)")
	tmpDir := repo.t.TempDir()
	_, err = repo.git.Run(repo.ctx, []string{"clone", repo.remoteDir, tmpDir}, "")
	require.NoError(t, err)
	repo.git.Run(repo.ctx, []string{"config", "user.email", "test@test.com"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"config", "user.name", "Test"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"checkout", "feature-a"}, tmpDir)

	// Reset to before our local commit, then create conflicting change
	repo.git.Run(repo.ctx, []string{"reset", "--hard", "HEAD~1"}, tmpDir)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "conflict.txt"), []byte("remote version\n"), 0644))
	repo.git.Run(repo.ctx, []string{"add", "."}, tmpDir)
	repo.git.Run(repo.ctx, []string{"commit", "-m", "add conflict.txt on remote"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"push", "--force", "origin", "feature-a"}, tmpDir)

	// Sync should not panic - it handles the conflict gracefully
	t.Log("Calling Sync() - expecting conflict during rebase")
	_ = repo.manager.Sync(repo.ctx)

	// Worktree should be in rebase state (conflict needs manual resolution)
	t.Log("Verifying feature-a is in rebase state due to conflict")
	require.True(t, repo.isRebaseInProgress("feature-a"),
		"sync should leave worktree in rebase state on conflict")

	// Clean up the rebase state for test isolation
	repo.abortRebase(featureAPath)
}

// TestSyncSkipsChildrenOfFailedBranch tests that children are skipped when parent rebase fails.
func TestSyncSkipsChildrenOfFailedBranch(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create chain: main -> feature-a -> feature-b
	t.Log("Creating worktree chain: main -> feature-a -> feature-b")
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "conflict.txt", "local version\n", "add conflict.txt locally")
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a")
	require.NoError(t, err)
	repo.commitInWorktree(featureBPath, "feature-b.txt", "feature-b content\n", "add feature-b work")
	repo.pushWorktree(featureBPath, "feature-b")

	// Record feature-b's commit hash before sync
	t.Log("Recording feature-b commit hash before sync")
	hashBefore := repo.getWorktreeCommit(featureBPath)

	// Create conflict on origin/feature-a by force pushing a different change
	t.Log("Force-pushing conflicting change to feature-a on remote")
	tmpDir := repo.t.TempDir()
	_, err = repo.git.Run(repo.ctx, []string{"clone", repo.remoteDir, tmpDir}, "")
	require.NoError(t, err)
	repo.git.Run(repo.ctx, []string{"config", "user.email", "test@test.com"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"config", "user.name", "Test"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"checkout", "feature-a"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"reset", "--hard", "HEAD~1"}, tmpDir)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "conflict.txt"), []byte("remote version\n"), 0644))
	repo.git.Run(repo.ctx, []string{"add", "."}, tmpDir)
	repo.git.Run(repo.ctx, []string{"commit", "-m", "conflicting change on remote"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"push", "--force", "origin", "feature-a"}, tmpDir)

	// Sync - feature-a will fail, feature-b should be skipped
	t.Log("Calling Sync() - feature-a will conflict, feature-b should be skipped")
	_ = repo.manager.Sync(repo.ctx)

	// Feature-a should be in rebase state (conflict)
	t.Log("Verifying feature-a is in rebase state")
	require.True(t, repo.isRebaseInProgress("feature-a"),
		"feature-a should be in rebase state due to conflict")

	// Feature-b should NOT have been rebased (same commit hash)
	t.Log("Verifying feature-b was skipped (unchanged)")
	hashAfter := repo.getWorktreeCommit(featureBPath)
	require.Equal(t, hashBefore, hashAfter,
		"feature-b should not have been rebased because parent feature-a failed")

	// Feature-b's own work should be unchanged
	require.True(t, fileExists(filepath.Join(featureBPath, "feature-b.txt")),
		"feature-b should still have its own work unchanged")

	// Clean up rebase state
	repo.abortRebase(featureAPath)
}

// TestSyncParentBranchDeleted tests that when parent is deleted, child rebases onto default branch.
func TestSyncParentBranchDeleted(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create chain: main -> feature-a -> feature-b
	t.Log("Creating worktree chain: main -> feature-a -> feature-b")
	featureAPath, err := repo.manager.New(repo.ctx, "feature-a", "main")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a")
	require.NoError(t, err)
	repo.commitInWorktree(featureBPath, "feature-b.txt", "feature-b content\n", "add feature-b work")
	repo.pushWorktree(featureBPath, "feature-b")

	// Verify feature-b's parent is feature-a
	t.Log("Verifying feature-b parent is feature-a")
	parentB, _ := repo.manager.GetParentBranch(repo.ctx, "feature-b", featureBPath)
	require.Equal(t, "feature-a", parentB)

	// Simulate parent branch merged: delete from remote and remove local worktree
	t.Log("Simulating parent branch merged: delete remote and remove local worktree")
	repo.deleteRemoteBranch("feature-a")
	require.False(t, repo.remoteBranchExists("feature-a"), "feature-a should be deleted from remote")

	t.Log("Removing local feature-a worktree")
	err = repo.manager.Remove(repo.ctx, "feature-a", false)
	require.NoError(t, err)

	// Add a new commit to main
	repo.addRemoteCommit("main", "main-update.txt", "main update content\n", "update main after parent deleted")

	// Sync - feature-b should detect parent is gone and rebase onto main
	t.Log("Calling Sync() - feature-b should rebase onto main (parent gone)")
	err = repo.manager.Sync(repo.ctx)
	require.NoError(t, err)

	// Feature-b should have the main update (rebased onto main, not feature-a)
	t.Log("Verifying feature-b rebased onto main")
	require.True(t, fileExists(filepath.Join(featureBPath, "main-update.txt")),
		"feature-b should contain main-update.txt after rebase onto main")

	// Feature-b's own work should be preserved
	require.True(t, fileExists(filepath.Join(featureBPath, "feature-b.txt")),
		"feature-b should still have its own work")

	// Verify parent tracking was updated to main
	t.Log("Verifying feature-b parent updated to main")
	newParent, _ := repo.manager.GetParentBranch(repo.ctx, "feature-b", featureBPath)
	require.Equal(t, "main", newParent, "feature-b should now track main as parent")
}
