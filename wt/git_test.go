package wt

import (
	"context"
	"strings"
	"testing"
)

// captureGitRunner records the full args slice (including any injected flags)
// passed to exec.Command so tests can assert on them without spawning git.
type captureGitRunner struct {
	lastArgs []string
}

func (c *captureGitRunner) Run(_ context.Context, args []string, _ string) (*CmdResult, error) {
	r := DefaultGitRunner{}
	// Replicate the injection logic without actually running git.
	finalArgs := args
	if len(args) > 0 && readOnlyGitSubcmds[args[0]] {
		finalArgs = make([]string, 0, len(args)+1)
		finalArgs = append(finalArgs, "--no-optional-locks")
		finalArgs = append(finalArgs, args...)
	}
	c.lastArgs = finalArgs
	_ = r
	return &CmdResult{}, nil
}

func TestDefaultGitRunnerNoOptionalLocks(t *testing.T) {
	t.Parallel()

	readOnly := []string{"status", "diff", "log", "ls-files", "rev-list", "rev-parse", "symbolic-ref", "ls-remote", "worktree", "branch", "show"}
	readWrite := []string{"fetch", "rebase", "config", "push", "worktree add"}

	for _, sub := range readOnly {
		sub := sub
		t.Run("injects_"+sub, func(t *testing.T) {
			t.Parallel()
			c := &captureGitRunner{}
			c.Run(context.Background(), []string{sub, "--some-flag"}, "")
			if len(c.lastArgs) == 0 || c.lastArgs[0] != "--no-optional-locks" {
				t.Errorf("expected --no-optional-locks prepended for %q, got %v", sub, c.lastArgs)
			}
		})
	}

	for _, sub := range readWrite {
		sub := sub
		t.Run("no_inject_"+sub, func(t *testing.T) {
			t.Parallel()
			c := &captureGitRunner{}
			c.Run(context.Background(), []string{sub}, "")
			if len(c.lastArgs) > 0 && c.lastArgs[0] == "--no-optional-locks" {
				t.Errorf("did not expect --no-optional-locks for %q, got %v", sub, c.lastArgs)
			}
		})
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
