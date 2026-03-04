package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateTmuxWindowName_WithRepoAndWorktree(t *testing.T) {
	// Outside tmux, ListTmuxWindows returns empty so index 0 is always available.
	name := GenerateTmuxWindowName("yoloswe", "resume-tmux")
	assert.Equal(t, "yoloswe/resume-tmux:0", name)
}

func TestGenerateTmuxWindowName_FallbackWithoutRepoInfo(t *testing.T) {
	// Empty repo/worktree should fall back to random two-word name.
	name := GenerateTmuxWindowName("", "")
	assert.NotEmpty(t, name)
	assert.NotContains(t, name, "/")
	assert.NotContains(t, name, ":")
}

func TestGenerateTmuxWindowName_FallbackEmptyRepo(t *testing.T) {
	name := GenerateTmuxWindowName("", "worktree")
	assert.NotContains(t, name, "/")
}

func TestGenerateTmuxWindowName_FallbackEmptyWorktree(t *testing.T) {
	name := GenerateTmuxWindowName("repo", "")
	assert.NotContains(t, name, "/")
}

func TestGenerateRandomTmuxWindowName(t *testing.T) {
	name := generateRandomTmuxWindowName()
	assert.NotEmpty(t, name)
	// Should be in "adjective-noun" format
	assert.Contains(t, name, "-")
}
