package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initRepo creates a bare-bones git repo at dir with a single remote origin.
func initRepo(t *testing.T, dir, remoteURL string) {
	t.Helper()
	for _, cmd := range [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "remote", "add", "origin", remoteURL},
	} {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git: %s: %s", cmd, out)
	}
}

func TestRepoNameFromGitRemote_HTTPS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir, "https://github.com/bazelment/yoloswe.git")
	assert.Equal(t, "yoloswe", repoNameFromGitRemote(dir))
}

func TestRepoNameFromGitRemote_SSH(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir, "git@github.com:bazelment/yoloswe.git")
	assert.Equal(t, "yoloswe", repoNameFromGitRemote(dir))
}

func TestRepoNameFromGitRemote_NoDotGitSuffix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	initRepo(t, dir, "https://example.com/acme/widget")
	assert.Equal(t, "widget", repoNameFromGitRemote(dir))
}

func TestRepoNameFromGitRemote_NoRemoteReturnsEmpty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, repoNameFromGitRemote(t.TempDir()),
		"non-git cwd should return empty so caller can fall back")
}

// TestRepoNameFromCwd_WorktreeLayout guards against the bug where the old
// implementation returned "feature" for paths like
// /home/.../worktrees/<repo>/feature/<branch>.
func TestRepoNameFromCwd_WorktreeLayout(t *testing.T) {
	t.Parallel()
	// Simulate: <tmp>/worktrees/yoloswe/feature/pr-watcher with the real repo
	// at <tmp>/worktrees/yoloswe (holds .git). The cwd is the nested branch dir.
	root := t.TempDir()
	repoDir := filepath.Join(root, "worktrees", "yoloswe")
	branchDir := filepath.Join(repoDir, "feature", "pr-watcher")
	require.NoError(t, os.MkdirAll(branchDir, 0o755))
	initRepo(t, repoDir, "https://github.com/bazelment/yoloswe.git")

	got := repoNameFromCwd(branchDir)
	assert.Equal(t, "yoloswe", got, "must not pick up intermediate 'feature' directory")
}

func TestRepoNameFromGitDir_WalksUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repoDir := filepath.Join(root, "proj")
	sub := filepath.Join(repoDir, "a", "b", "c")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(repoDir, ".git"), 0o755))
	assert.Equal(t, "proj", repoNameFromGitDir(sub))
}

func TestRepoNameFromGitDir_NoGitReturnsEmpty(t *testing.T) {
	t.Parallel()
	// Use a nested tempdir whose path never contains ".git" up to /tmp.
	assert.Empty(t, repoNameFromGitDir(t.TempDir()))
}
