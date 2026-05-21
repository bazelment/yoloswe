package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCheckCodetalkModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modelID   string
		errSubstr string
		wantErr   bool
	}{
		{
			name:    "opus is Claude — no error",
			modelID: "opus",
			wantErr: false,
		},
		{
			name:    "sonnet is Claude — no error",
			modelID: "sonnet",
			wantErr: false,
		},
		{
			name:    "unknown model ID — no error (not routed to non-Claude)",
			modelID: "some-future-model",
			wantErr: false,
		},
		{
			name:      "gpt-5.5 is Codex — error with TUI hint",
			modelID:   "gpt-5.5",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "gemini-3-pro-preview is Gemini — error with TUI hint",
			modelID:   "gemini-3-pro-preview",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "cursor-default is Cursor — error with TUI hint",
			modelID:   "cursor-default",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "agy-default is agy — error with TUI hint",
			modelID:   "agy-default",
			wantErr:   true,
			errSubstr: "bramble TUI",
		},
		{
			name:      "gpt- prefix triggers Codex routing — error",
			modelID:   "gpt-9-future",
			wantErr:   true,
			errSubstr: "codex",
		},
		{
			name:      "gemini- prefix triggers Gemini routing — error",
			modelID:   "gemini-99",
			wantErr:   true,
			errSubstr: "gemini",
		},
		{
			name:      "agy- prefix triggers agy routing — error",
			modelID:   "agy-future",
			wantErr:   true,
			errSubstr: "agy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkCodetalkModel(tt.modelID)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errSubstr != "" {
					assert.True(t, strings.Contains(err.Error(), tt.errSubstr),
						"expected error to contain %q, got: %v", tt.errSubstr, err)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
