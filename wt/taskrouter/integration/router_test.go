package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt/taskrouter"
)

// Integration tests for the task router using real Codex.
// These tests are tagged with "manual" and "integration" in BUILD.bazel
// and should be run manually: bazel test //wt/taskrouter:integration_test --test_timeout=60

func TestRouteEmptyRepo(t *testing.T) {
	// Test: Empty repo with only main branch
	// Expected: create_new with a reasonable branch name

	r := taskrouter.New(taskrouter.Config{
		Model:   "gpt-5.2-codex",
		WorkDir: t.TempDir(),
		Verbose: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := r.Start(ctx)
	require.NoError(t, err)
	defer r.Stop()

	req := taskrouter.RouteRequest{
		Prompt:    "Add user authentication with OAuth2",
		Worktrees: []taskrouter.WorktreeInfo{}, // Empty - only main exists
		CurrentWT: "",
		RepoName:  "my-app",
	}

	proposal, err := r.Route(ctx, req)
	require.NoError(t, err)

	// Should propose creating a new branch
	assert.Equal(t, taskrouter.ActionCreateNew, proposal.Action)
	assert.NotEmpty(t, proposal.Worktree)
	assert.NotEmpty(t, proposal.Reasoning)

	t.Logf("Proposal: action=%s, worktree=%s, parent=%s, reasoning=%s",
		proposal.Action, proposal.Worktree, proposal.Parent, proposal.Reasoning)
}

func TestRouteMatchingBranch(t *testing.T) {
	// Test: Task that clearly relates to an existing branch
	// Expected: use_existing with the matching branch

	r := taskrouter.New(taskrouter.Config{
		Model:   "gpt-5.2-codex",
		WorkDir: t.TempDir(),
		Verbose: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := r.Start(ctx)
	require.NoError(t, err)
	defer r.Stop()

	req := taskrouter.RouteRequest{
		Prompt: "Add Google OAuth provider to the authentication system",
		Worktrees: []taskrouter.WorktreeInfo{
			{
				Name:    "feature-auth",
				Goal:    "Implement user authentication with OAuth2",
				Parent:  "main",
				PRState: "open",
			},
			{
				Name:   "fix-nav-menu",
				Goal:   "Fix navigation menu dropdown",
				Parent: "main",
			},
		},
		CurrentWT: "feature-auth",
		RepoName:  "my-app",
	}

	proposal, err := r.Route(ctx, req)
	require.NoError(t, err)

	// Should use existing feature-auth branch
	assert.Equal(t, taskrouter.ActionUseExisting, proposal.Action)
	assert.Equal(t, "feature-auth", proposal.Worktree)
	assert.NotEmpty(t, proposal.Reasoning)

	t.Logf("Proposal: action=%s, worktree=%s, reasoning=%s",
		proposal.Action, proposal.Worktree, proposal.Reasoning)
}

func TestRouteDependentTask(t *testing.T) {
	// Test: Task that depends on unmerged work in another branch
	// Expected: create_new with parent set to the dependency branch

	r := taskrouter.New(taskrouter.Config{
		Model:   "gpt-5.2-codex",
		WorkDir: t.TempDir(),
		Verbose: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := r.Start(ctx)
	require.NoError(t, err)
	defer r.Stop()

	req := taskrouter.RouteRequest{
		Prompt: "Add password reset functionality that uses the new OAuth authentication flow",
		Worktrees: []taskrouter.WorktreeInfo{
			{
				Name:    "feature-auth",
				Goal:    "Implement OAuth2 authentication (not yet merged)",
				Parent:  "main",
				PRState: "open",
				IsAhead: true,
			},
			{
				Name:   "feature-dashboard",
				Goal:   "New user dashboard",
				Parent: "main",
			},
		},
		CurrentWT: "feature-auth",
		RepoName:  "my-app",
	}

	proposal, err := r.Route(ctx, req)
	require.NoError(t, err)

	// Should create new branch based on feature-auth since password reset depends on OAuth
	assert.Equal(t, taskrouter.ActionCreateNew, proposal.Action)
	assert.NotEmpty(t, proposal.Worktree)
	// Parent should be feature-auth since the task depends on that unmerged work
	assert.Equal(t, "feature-auth", proposal.Parent, "Should base on feature-auth due to dependency")
	assert.NotEmpty(t, proposal.Reasoning)

	t.Logf("Proposal: action=%s, worktree=%s, parent=%s, reasoning=%s",
		proposal.Action, proposal.Worktree, proposal.Parent, proposal.Reasoning)
}

func TestRouteUnrelatedTask(t *testing.T) {
	// Test: Task completely unrelated to existing branches
	// Expected: create_new from main

	r := taskrouter.New(taskrouter.Config{
		Model:   "gpt-5.2-codex",
		WorkDir: t.TempDir(),
		Verbose: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := r.Start(ctx)
	require.NoError(t, err)
	defer r.Stop()

	req := taskrouter.RouteRequest{
		Prompt: "Add dark mode support to the UI",
		Worktrees: []taskrouter.WorktreeInfo{
			{
				Name:    "feature-auth",
				Goal:    "Implement OAuth2 authentication",
				Parent:  "main",
				PRState: "open",
			},
			{
				Name:    "fix-performance",
				Goal:    "Optimize database queries",
				Parent:  "main",
				PRState: "open",
			},
		},
		CurrentWT: "feature-auth",
		RepoName:  "my-app",
	}

	proposal, err := r.Route(ctx, req)
	require.NoError(t, err)

	// Should create new branch from main since dark mode is unrelated
	assert.Equal(t, taskrouter.ActionCreateNew, proposal.Action)
	assert.NotEmpty(t, proposal.Worktree)
	assert.Equal(t, "main", proposal.Parent)
	assert.NotEmpty(t, proposal.Reasoning)

	t.Logf("Proposal: action=%s, worktree=%s, parent=%s, reasoning=%s",
		proposal.Action, proposal.Worktree, proposal.Parent, proposal.Reasoning)
}
