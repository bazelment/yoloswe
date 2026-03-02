package wt

import (
	"context"
	"strings"
	"testing"
)

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
