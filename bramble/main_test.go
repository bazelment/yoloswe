package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkBareRepo creates a fake wt-managed repo under wtRoot:
//
//	<wtRoot>/<repoName>/.bare/
//	<wtRoot>/<repoName>/<branchName>/  (a worktree directory)
func mkBareRepo(t *testing.T, wtRoot, repoName, branchName string) string {
	t.Helper()
	repoDir := filepath.Join(wtRoot, repoName)
	bareDir := filepath.Join(repoDir, ".bare")
	require.NoError(t, os.MkdirAll(bareDir, 0o755))
	worktreeDir := filepath.Join(repoDir, branchName)
	require.NoError(t, os.MkdirAll(worktreeDir, 0o755))
	return worktreeDir
}

func TestDetectRepoFromPath_FromWorktreeDir(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()

	worktreeDir := mkBareRepo(t, wtRoot, "myrepo", "feature-branch")

	repo, err := detectRepoFromPath(worktreeDir, wtRoot)
	require.NoError(t, err)
	assert.Equal(t, "myrepo", repo)
}

func TestDetectRepoFromPath_FromSubdir(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()

	worktreeDir := mkBareRepo(t, wtRoot, "myrepo", "feature-branch")
	subDir := filepath.Join(worktreeDir, "pkg", "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	repo, err := detectRepoFromPath(subDir, wtRoot)
	require.NoError(t, err)
	assert.Equal(t, "myrepo", repo)
}

func TestDetectRepoFromPath_FromRepoRoot(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()

	mkBareRepo(t, wtRoot, "myrepo", "main")
	repoRoot := filepath.Join(wtRoot, "myrepo")

	repo, err := detectRepoFromPath(repoRoot, wtRoot)
	require.NoError(t, err)
	assert.Equal(t, "myrepo", repo)
}

func TestDetectRepoFromPath_OutsideWtRoot(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()
	otherDir := t.TempDir()

	// Create a repo but look from a completely different directory.
	mkBareRepo(t, wtRoot, "myrepo", "main")

	_, err := detectRepoFromPath(otherDir, wtRoot)
	assert.Error(t, err)
}

func TestDetectRepoFromPath_NoBareDir(t *testing.T) {
	t.Parallel()
	wtRoot := t.TempDir()
	cwd := filepath.Join(wtRoot, "somerepo", "worktree")
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	_, err := detectRepoFromPath(cwd, wtRoot)
	assert.Error(t, err)
}
