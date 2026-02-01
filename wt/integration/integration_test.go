package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/wt"
	"github.com/stretchr/testify/require"
)

// testRepo sets up a wt-managed repository with a local "remote" for integration testing.
type testRepo struct {
	t         *testing.T
	ctx       context.Context
	git       *wt.DefaultGitRunner
	remoteDir string // Bare remote repository
	root      string // wt root directory (e.g., ~/worktrees)
	repoName  string // Repository name
	manager   *wt.Manager
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
	mainPath, err := r.manager.Init(r.ctx, r.remoteDir)
	require.NoError(r.t, err, "Manager.Init failed")

	r.repoName = wt.GetRepoNameFromURL(r.remoteDir)
	output := wt.NewOutput(&bytes.Buffer{}, false)
	r.manager = wt.NewManager(r.root, r.repoName, wt.WithOutput(output))
	return mainPath
}

func (r *testRepo) pushBranch(branch string) {
	r.t.Helper()
	tmpDir := r.t.TempDir()
	_, err := r.git.Run(r.ctx, []string{"clone", r.remoteDir, tmpDir}, "")
	require.NoError(r.t, err)

	r.git.Run(r.ctx, []string{"config", "user.email", "test@test.com"}, tmpDir)
	r.git.Run(r.ctx, []string{"config", "user.name", "Test"}, tmpDir)
	r.git.Run(r.ctx, []string{"checkout", "-b", branch}, tmpDir)
	require.NoError(r.t, os.WriteFile(filepath.Join(tmpDir, branch+".txt"), []byte(branch+" content\n"), 0644))
	r.git.Run(r.ctx, []string{"add", "."}, tmpDir)
	r.git.Run(r.ctx, []string{"commit", "-m", "add "+branch}, tmpDir)
	r.git.Run(r.ctx, []string{"push", "-u", "origin", branch}, tmpDir)
}

func (r *testRepo) pushWorktree(wtPath, branch string) {
	r.t.Helper()
	r.git.Run(r.ctx, []string{"config", "user.email", "test@test.com"}, wtPath)
	r.git.Run(r.ctx, []string{"config", "user.name", "Test"}, wtPath)
	_, err := r.git.Run(r.ctx, []string{"push", "-u", "origin", branch}, wtPath)
	require.NoError(r.t, err, "git push failed for "+branch)
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
