package wt

import (
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
