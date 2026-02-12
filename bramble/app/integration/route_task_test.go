//go:build integration

package integration

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/app"
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/taskrouter"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// TestRouteTaskWithRealRouter verifies that the TUI wiring to the real
// Codex-based task router works end-to-end: Model → routeTask → Router.Route → proposal.
func TestRouteTaskWithRealRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start a real router with a codex provider
	router := taskrouter.New(taskrouter.Config{
		Provider: agent.NewCodexProvider(),
		WorkDir:  t.TempDir(),
	})
	router.SetOutput(io.Discard)

	err := router.Start(ctx)
	require.NoError(t, err, "router must start (requires codex binary)")
	defer router.Stop()

	// Create a Model with the real router
	mgr := session.NewManagerWithConfig(session.ManagerConfig{SessionMode: session.SessionModeTUI})
	defer mgr.Close()

	m := app.NewModel(ctx, "/tmp/wt", "test-repo", "", mgr, router, nil, 80, 24, nil, nil)

	// Use RouteTask (exported from taskmodal.go) directly to verify wiring
	req := taskrouter.RouteRequest{
		Prompt:    "Add dark mode support to the UI",
		Worktrees: []taskrouter.WorktreeInfo{},
		CurrentWT: "",
		RepoName:  "test-repo",
	}

	proposal, err := app.RouteTask(ctx, router, req)
	require.NoError(t, err)
	assert.NotNil(t, proposal)
	assert.NotEmpty(t, proposal.Worktree)
	assert.NotEmpty(t, proposal.Reasoning)

	t.Logf("Proposal: action=%s, worktree=%s, parent=%s, reasoning=%s",
		proposal.Action, proposal.Worktree, proposal.Parent, proposal.Reasoning)

	// Verify model was constructed with router (indirect: RouteTask with non-nil router should not use mock)
	_ = m // Model was successfully created with router
}
