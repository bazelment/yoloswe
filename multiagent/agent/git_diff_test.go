package agent

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initGitRepo creates a temporary git repo with an initial commit.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		require.NoError(t, cmd.Run(), "failed to run: %v", args)
	}

	// Create initial file and commit
	require.NoError(t, os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("hello"), 0644))
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
	cmd = exec.Command("git", "commit", "-m", "initial")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())

	return dir
}

func TestDetectFileChangesGit_ModifiedFile(t *testing.T) {
	dir := initGitRepo(t)

	// Modify a tracked file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("modified"), 0644))

	result := detectFileChangesGit(dir, "HEAD", slog.Default())
	assert.Empty(t, result.FilesCreated)
	assert.Equal(t, []string{"initial.txt"}, result.FilesModified)
}

func TestDetectFileChangesGit_StagedNewFile(t *testing.T) {
	dir := initGitRepo(t)

	// Create and stage a new file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0644))
	cmd := exec.Command("git", "add", "new.txt")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())

	result := detectFileChangesGit(dir, "HEAD", slog.Default())
	assert.Equal(t, []string{"new.txt"}, result.FilesCreated)
	assert.Empty(t, result.FilesModified)
}

func TestDetectFileChangesGit_UntrackedNewFile(t *testing.T) {
	dir := initGitRepo(t)

	// Create a new file WITHOUT staging it
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("data"), 0644))

	result := detectFileChangesGit(dir, "HEAD", slog.Default())
	assert.Equal(t, []string{"untracked.txt"}, result.FilesCreated)
	assert.Empty(t, result.FilesModified)
}

func TestDetectFileChangesGit_Mixed(t *testing.T) {
	dir := initGitRepo(t)

	// Modify existing file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("changed"), 0644))
	// Create staged new file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("s"), 0644))
	cmd := exec.Command("git", "add", "staged.txt")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
	// Create untracked new file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u"), 0644))

	result := detectFileChangesGit(dir, "HEAD", slog.Default())
	sort.Strings(result.FilesCreated)
	assert.Equal(t, []string{"staged.txt", "untracked.txt"}, result.FilesCreated)
	assert.Equal(t, []string{"initial.txt"}, result.FilesModified)
}

func TestDetectFileChangesGit_NoChanges(t *testing.T) {
	dir := initGitRepo(t)

	result := detectFileChangesGit(dir, "HEAD", slog.Default())
	assert.Empty(t, result.FilesCreated)
	assert.Empty(t, result.FilesModified)
}

func TestDetectFileChangesGit_CommittedChanges(t *testing.T) {
	dir := initGitRepo(t)

	// Snapshot HEAD before the "agent" runs.
	baseRef := gitHeadRef(dir)

	// Simulate an agent that creates a file and commits it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fix.go"), []byte("package fix"), 0644))
	for _, args := range [][]string{
		{"git", "add", "fix.go"},
		{"git", "commit", "-m", "agent fix"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		require.NoError(t, cmd.Run())
	}

	// Diffing against the pre-run snapshot should still detect the new file.
	result := detectFileChangesGit(dir, baseRef, slog.Default())
	assert.Equal(t, []string{"fix.go"}, result.FilesCreated)

	// Diffing against HEAD (post-commit) would miss it.
	resultHead := detectFileChangesGit(dir, "HEAD", slog.Default())
	assert.Empty(t, resultHead.FilesCreated)
	assert.Empty(t, resultHead.FilesModified)
}

func TestDetectFileChangesGit_Rename(t *testing.T) {
	dir := initGitRepo(t)

	// Rename a file via git mv
	cmd := exec.Command("git", "mv", "initial.txt", "renamed.txt")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())

	result := detectFileChangesGit(dir, "HEAD", slog.Default())
	assert.Contains(t, result.FilesCreated, "renamed.txt")
	assert.Contains(t, result.FilesDeleted, "initial.txt")
}

func TestDetectFileChangesGit_Delete(t *testing.T) {
	dir := initGitRepo(t)

	// Delete a tracked file
	cmd := exec.Command("git", "rm", "initial.txt")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())

	result := detectFileChangesGit(dir, "HEAD", slog.Default())
	assert.Equal(t, []string{"initial.txt"}, result.FilesDeleted)
	assert.Empty(t, result.FilesCreated)
	assert.Empty(t, result.FilesModified)
}
