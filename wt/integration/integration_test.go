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
	featurePath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
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
	featureAPath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
	require.NoError(t, err)
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a", "")
	require.NoError(t, err)
	repo.pushWorktree(featureBPath, "feature-b")

	featureCPath, err := repo.manager.New(repo.ctx,"feature-c", "feature-b", "")
	require.NoError(t, err)

	// Verify parent tracking (all branches now track their parent, including default)
	parentA, _ := repo.manager.GetParentBranch(repo.ctx, "feature-a", featureAPath)
	parentB, _ := repo.manager.GetParentBranch(repo.ctx, "feature-b", featureBPath)
	parentC, _ := repo.manager.GetParentBranch(repo.ctx, "feature-c", featureCPath)

	require.Equal(t, "main", parentA, "feature-a should track main as parent")
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

	wtPath, err := repo.manager.Open(repo.ctx, "existing-feature", "")
	require.NoError(t, err)
	require.DirExists(t, wtPath)
	require.FileExists(t, filepath.Join(wtPath, "existing-feature.txt"))

	// Verify upstream tracking is configured so git push works without arguments
	result, err := repo.git.Run(repo.ctx, []string{
		"config", "--get", "branch.existing-feature.remote",
	}, wtPath)
	require.NoError(t, err)
	require.Equal(t, "origin", strings.TrimSpace(result.Stdout))
}

// TestErrorCases tests various error conditions.
func TestErrorCases(t *testing.T) {
	t.Run("repo not initialized", func(t *testing.T) {
		root := t.TempDir()
		output := wt.NewOutput(&bytes.Buffer{}, false)
		manager := wt.NewManager(root, "not-initialized", wt.WithOutput(output))
		ctx := context.Background()

		_, err := manager.New(ctx, "feature", "main", "")
		require.ErrorIs(t, err, wt.ErrRepoNotInitialized)

		_, err = manager.Open(ctx, "feature", "")
		require.ErrorIs(t, err, wt.ErrRepoNotInitialized)
	})

	t.Run("worktree already exists", func(t *testing.T) {
		repo := newTestRepo(t)
		repo.init()

		_, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
		require.NoError(t, err)

		_, err = repo.manager.New(repo.ctx,"feature-a", "main", "")
		require.ErrorIs(t, err, wt.ErrWorktreeExists)
	})

	t.Run("branch not found", func(t *testing.T) {
		repo := newTestRepo(t)
		repo.init()

		_, err := repo.manager.Open(repo.ctx, "nonexistent-branch", "")
		require.ErrorIs(t, err, wt.ErrBranchNotFound)
	})
}

// TestSyncRebasesWorktrees tests that Sync() fetches and rebases worktrees onto origin/main.
func TestSyncRebasesWorktrees(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a from main with its own commit
	t.Log("Creating worktree feature-a from main")
	featureAPath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	// Add a commit to origin/main (simulating another developer merging to main)
	repo.addRemoteCommit("main", "main-update.txt", "main update content\n", "another developer merged to main")

	// Before sync: local feature-a should NOT have the main update
	t.Log("Verifying local worktree does not have main update yet")
	require.False(t, fileExists(filepath.Join(featureAPath, "main-update.txt")))

	// Sync - should rebase local onto origin/main
	t.Log("Calling Sync() to rebase worktrees onto origin/main")
	err = repo.manager.Sync(repo.ctx)
	require.NoError(t, err)

	// After sync: feature-a should have the main update (rebased onto origin/main)
	t.Log("Verifying feature-a has main update after sync")
	require.True(t, fileExists(filepath.Join(featureAPath, "main-update.txt")),
		"feature-a should contain main-update.txt after rebase onto origin/main")
	// And feature-a's own work should be preserved
	require.True(t, fileExists(filepath.Join(featureAPath, "feature-a.txt")),
		"feature-a should still have its own work")
}

// TestSyncMultipleWorktrees tests that Sync() handles cascading worktrees correctly.
// feature-a (based on main) rebases onto origin/main
// feature-b (based on feature-a) rebases onto feature-a
func TestSyncMultipleWorktrees(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create chain: main -> feature-a -> feature-b
	t.Log("Creating worktree chain: main -> feature-a -> feature-b")
	featureAPath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureBPath, "feature-b.txt", "feature-b content\n", "add feature-b work")
	repo.pushWorktree(featureBPath, "feature-b")

	// Add a commit to origin/main (simulating another developer merging to main)
	t.Log("Simulating commit to main")
	repo.addRemoteCommit("main", "main-update.txt", "main update content\n", "another developer merged to main")

	// Before sync: neither branch should have the main update
	t.Log("Verifying local worktrees do not have main update yet")
	require.False(t, fileExists(filepath.Join(featureAPath, "main-update.txt")))
	require.False(t, fileExists(filepath.Join(featureBPath, "main-update.txt")))

	// Sync - feature-a rebases onto origin/main, feature-b rebases onto feature-a
	t.Log("Calling Sync() to rebase all worktrees")
	err = repo.manager.Sync(repo.ctx)
	require.NoError(t, err)

	// After sync:
	t.Log("Verifying cascading rebase behavior")
	// - feature-a should have main-update.txt (rebased onto origin/main)
	require.True(t, fileExists(filepath.Join(featureAPath, "main-update.txt")),
		"feature-a should contain main-update.txt after rebase onto origin/main")
	require.True(t, fileExists(filepath.Join(featureAPath, "feature-a.txt")),
		"feature-a should still have its own work")

	// - feature-b should have feature-a's work (rebased onto feature-a)
	require.True(t, fileExists(filepath.Join(featureBPath, "feature-a.txt")),
		"feature-b should contain feature-a.txt after rebase onto feature-a")
	require.True(t, fileExists(filepath.Join(featureBPath, "feature-b.txt")),
		"feature-b should still have its own work")
}

// TestSyncConflictHandling tests that rebase conflicts are handled gracefully.
func TestSyncConflictHandling(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create feature-a and add conflict.txt with one content
	t.Log("Creating worktree feature-a with conflict.txt")
	featureAPath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "conflict.txt", "local version\n", "add conflict.txt locally")
	repo.pushWorktree(featureAPath, "feature-a")

	// Create conflicting change on origin/main (sync rebases onto origin/main)
	t.Log("Adding conflicting change to origin/main")
	tmpDir := repo.t.TempDir()
	_, err = repo.git.Run(repo.ctx, []string{"clone", repo.remoteDir, tmpDir}, "")
	require.NoError(t, err)
	repo.git.Run(repo.ctx, []string{"config", "user.email", "test@test.com"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"config", "user.name", "Test"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"checkout", "main"}, tmpDir)

	// Add conflict.txt with different content on main
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "conflict.txt"), []byte("main version\n"), 0644))
	repo.git.Run(repo.ctx, []string{"add", "."}, tmpDir)
	repo.git.Run(repo.ctx, []string{"commit", "-m", "add conflict.txt on main"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"push", "origin", "main"}, tmpDir)

	// Sync should not panic - it handles the conflict gracefully
	t.Log("Calling Sync() - expecting conflict during rebase onto origin/main")
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
	featureAPath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "conflict.txt", "local version\n", "add conflict.txt locally")
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureBPath, "feature-b.txt", "feature-b content\n", "add feature-b work")
	repo.pushWorktree(featureBPath, "feature-b")

	// Record feature-b's commit hash before sync
	t.Log("Recording feature-b commit hash before sync")
	hashBefore := repo.getWorktreeCommit(featureBPath)

	// Create conflict on origin/main (sync rebases feature-a onto origin/main)
	t.Log("Adding conflicting change to origin/main")
	tmpDir := repo.t.TempDir()
	_, err = repo.git.Run(repo.ctx, []string{"clone", repo.remoteDir, tmpDir}, "")
	require.NoError(t, err)
	repo.git.Run(repo.ctx, []string{"config", "user.email", "test@test.com"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"config", "user.name", "Test"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"checkout", "main"}, tmpDir)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "conflict.txt"), []byte("main version\n"), 0644))
	repo.git.Run(repo.ctx, []string{"add", "."}, tmpDir)
	repo.git.Run(repo.ctx, []string{"commit", "-m", "conflicting change on main"}, tmpDir)
	repo.git.Run(repo.ctx, []string{"push", "origin", "main"}, tmpDir)

	// Sync - feature-a will fail due to conflict with main, feature-b should be skipped
	t.Log("Calling Sync() - feature-a will conflict with main, feature-b should be skipped")
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
	featureAPath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a", "")
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

// TestSyncOpenedBranchRebasesOntoMain tests that a branch opened with Open()
// is rebased onto origin/main during sync.
func TestSyncOpenedBranchRebasesOntoMain(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create remote branch 'foo' with a commit (simulating a branch created outside wt)
	t.Log("Creating remote branch 'foo' with its own commit")
	repo.pushBranch("foo")

	// Add a commit to origin/main (main has diverged from foo)
	t.Log("Adding a commit to main (main diverges from foo)")
	repo.addRemoteCommit("main", "main-update.txt", "main update content\n", "main has a new commit")

	// Open the existing remote branch foo (tracks main as parent)
	t.Log("Opening existing remote branch 'foo' with wt.Open()")
	fooPath, err := repo.manager.Open(repo.ctx, "foo", "")
	require.NoError(t, err)

	// Verify foo has its original content but not the main update
	t.Log("Verifying foo has its content but not main update yet")
	require.True(t, fileExists(filepath.Join(fooPath, "foo.txt")),
		"foo should have its original content")
	require.False(t, fileExists(filepath.Join(fooPath, "main-update.txt")),
		"foo should NOT have main-update.txt before sync")

	// Sync - should rebase foo onto origin/main
	t.Log("Calling Sync() to rebase foo onto origin/main")
	err = repo.manager.Sync(repo.ctx)
	require.NoError(t, err)

	// After sync: foo should have the main update (rebased onto origin/main)
	t.Log("Verifying foo has main update after sync")
	require.True(t, fileExists(filepath.Join(fooPath, "main-update.txt")),
		"foo should contain main-update.txt after rebase onto origin/main")
	// And foo's own work should be preserved
	require.True(t, fileExists(filepath.Join(fooPath, "foo.txt")),
		"foo should still have its own work")
}

// TestSyncCascadingWithRemovedParentWorktree tests that cascading branches correctly
// rebase onto origin/parent even when the parent worktree has been removed.
func TestSyncCascadingWithRemovedParentWorktree(t *testing.T) {
	repo := newTestRepo(t)
	repo.init()

	// Create chain: main -> feature-a -> feature-b
	t.Log("Creating worktree chain: main -> feature-a -> feature-b")
	featureAPath, err := repo.manager.New(repo.ctx,"feature-a", "main", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureAPath, "feature-a.txt", "feature-a content\n", "add feature-a work")
	repo.pushWorktree(featureAPath, "feature-a")

	featureBPath, err := repo.manager.New(repo.ctx, "feature-b", "feature-a", "")
	require.NoError(t, err)
	repo.commitInWorktree(featureBPath, "feature-b.txt", "feature-b content\n", "add feature-b work")
	repo.pushWorktree(featureBPath, "feature-b")

	// Remove feature-a worktree (but keep the remote branch)
	t.Log("Removing feature-a worktree")
	err = repo.manager.Remove(repo.ctx, "feature-a", false)
	require.NoError(t, err)

	// Add a commit to origin/feature-a (simulating teammate push to parent)
	t.Log("Adding commit to origin/feature-a (parent branch on remote)")
	repo.addRemoteCommit("feature-a", "parent-update.txt", "parent update content\n", "teammate updated feature-a")

	// Verify feature-b doesn't have the parent update yet
	require.False(t, fileExists(filepath.Join(featureBPath, "parent-update.txt")),
		"feature-b should NOT have parent-update.txt before sync")

	// Sync - feature-b should rebase onto origin/feature-a (not stale local ref)
	t.Log("Calling Sync() - feature-b should rebase onto origin/feature-a")
	err = repo.manager.Sync(repo.ctx)
	require.NoError(t, err)

	// After sync: feature-b should have the parent update from origin/feature-a
	t.Log("Verifying feature-b has parent update after sync")
	require.True(t, fileExists(filepath.Join(featureBPath, "parent-update.txt")),
		"feature-b should contain parent-update.txt after rebase onto origin/feature-a")
	// And feature-b's own work should be preserved
	require.True(t, fileExists(filepath.Join(featureBPath, "feature-b.txt")),
		"feature-b should still have its own work")
}
