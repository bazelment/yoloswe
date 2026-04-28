package wt

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// initTempRepo creates a minimal git repo in a temp dir suitable for read-only
// git commands (status, log, etc.) in tests.
func initTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", dir},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestDefaultGitRunnerNoOptionalLocks(t *testing.T) {
	t.Parallel()
	dir := initTempRepo(t)
	runner := &DefaultGitRunner{}
	ctx := context.Background()

	// These subcommands must succeed with --no-optional-locks prepended.
	readOnly := [][]string{
		{"status", "--porcelain"},
		{"diff"},
		{"log", "--oneline"},
		{"ls-files"},
		{"rev-parse", "HEAD"},
		{"show", "HEAD"},
	}
	for _, args := range readOnly {
		args := args
		t.Run("injects_"+args[0], func(t *testing.T) {
			t.Parallel()
			if _, err := runner.Run(ctx, args, dir); err != nil {
				t.Errorf("Run(%v) failed: %v — --no-optional-locks may have broken a read-only subcommand", args, err)
			}
		})
	}

	// Write subcommands must NOT get --no-optional-locks. We verify by checking
	// the map directly — these must not be in readOnlyGitSubcmds.
	writeSubcmds := []string{"fetch", "rebase", "config", "push", "worktree", "branch"}
	for _, sub := range writeSubcmds {
		if readOnlyGitSubcmds[sub] {
			t.Errorf("write subcommand %q is in readOnlyGitSubcmds — would inject --no-optional-locks into write operations", sub)
		}
	}
}

func TestGetRepoNameFromURL(t *testing.T) {
	tests := []struct {
		url      string
		expected string
	}{
		{"git@github.com:user/repo.git", "repo"},
		{"git@github.com:user/repo", "repo"},
		{"https://github.com/user/repo.git", "repo"},
		{"https://github.com/user/repo", "repo"},
		{"git@github.com:org/multi-word-repo.git", "multi-word-repo"},
		{"https://github.com/org/multi-word-repo.git", "multi-word-repo"},
		{"git@gitlab.com:group/subgroup/project.git", "project"},
		{"ssh://git@github.com/user/repo.git", "repo"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := GetRepoNameFromURL(tt.url)
			if got != tt.expected {
				t.Errorf("GetRepoNameFromURL(%q) = %q, want %q", tt.url, got, tt.expected)
			}
		})
	}
}

func TestDefaultGitRunnerSetsTerminalPromptEnv(t *testing.T) {
	runner := &DefaultGitRunner{}
	// Run a harmless git command to verify the env is set.
	// "git version" always succeeds and doesn't need a repo.
	result, err := runner.Run(context.Background(), []string{"version"}, "")
	if err != nil {
		t.Fatalf("git version failed: %v", err)
	}
	if !strings.HasPrefix(result.Stdout, "git version") {
		t.Errorf("unexpected output: %s", result.Stdout)
	}
	// The env var is set internally; we verify it works by running a command
	// that would hang without it (tested via integration). Here we just
	// verify the runner executes successfully with the env set.
}
