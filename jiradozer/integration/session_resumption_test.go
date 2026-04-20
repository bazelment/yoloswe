//go:build integration

package integration

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// TestSessionResumption_PlanFeedbackLoop verifies that agent session
// resumption works across a feedback loop. This is the critical mechanism
// that lets jiradozer send feedback to an agent in the context of its
// previous session, rather than starting fresh.
//
// Sequence:
//  1. Run planning step with an issue prompt → agent returns a plan + session ID.
//  2. Simulate a "redo" with feedback text.
//  3. Resume the session with the same session ID + feedback → agent should
//     acknowledge the feedback and produce a revised plan.
func TestSessionResumption_PlanFeedbackLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()

	desc := "Add a JSON config parser that reads from a file path and returns a typed struct"
	issue := &tracker.Issue{
		ID:          "test-issue-1",
		Identifier:  "TEST-1",
		Title:       "Add JSON config parser",
		Description: &desc,
	}
	data := jiradozer.NewPromptData(issue, "main")

	stepCfg := jiradozer.StepConfig{
		Model:          "haiku",
		PermissionMode: "plan",
		MaxTurns:       3,
		MaxBudgetUSD:   1.0,
	}

	// --- Step 1: First execution (no session ID, no feedback) ---
	t.Log("Step 1: First planning execution")
	res1, err := jiradozer.RunStepAgent(ctx, "plan", data, stepCfg, workDir, "", "", nil, logger)
	require.NoError(t, err, "first plan execution should succeed")
	require.NotEmpty(t, res1.Output, "first plan output should not be empty")
	require.NotEmpty(t, res1.SessionID, "first execution should return a session ID")
	t.Logf("Session ID from step 1: %s", res1.SessionID)
	t.Logf("Plan output (first 200 chars): %.200s", res1.Output)

	// --- Step 2: Resume with feedback (same session ID) ---
	t.Log("Step 2: Resume with feedback (redo)")
	feedback := "Please also add validation for required fields and return typed errors instead of generic errors"
	res2, err := jiradozer.RunStepAgent(ctx, "plan", data, stepCfg, workDir, feedback, res1.SessionID, nil, logger)
	require.NoError(t, err, "resumed plan execution should succeed")
	require.NotEmpty(t, res2.Output, "resumed plan output should not be empty")
	require.NotEmpty(t, res2.SessionID, "resumed execution should return a session ID")
	t.Logf("Session ID from step 2: %s", res2.SessionID)
	t.Logf("Plan output (first 200 chars): %.200s", res2.Output)

	// --- Assertions ---
	// The resumed output should be different from the first (feedback was incorporated).
	assert.NotEqual(t, res1.Output, res2.Output, "resumed output should differ from first execution")

	// The session IDs should be related (same or evolved).
	// Claude's session resume keeps the same session ID.
	t.Logf("Session IDs: first=%s, second=%s", res1.SessionID, res2.SessionID)
}

// TestSessionResumption_BuildAfterPlan verifies that the plan output from
// step 1 is passed downstream to the build step as PromptData.Plan.
func TestSessionResumption_BuildAfterPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()

	desc := "Add a function that reverses a string"
	issue := &tracker.Issue{
		ID:          "test-issue-2",
		Identifier:  "TEST-2",
		Title:       "Add string reverse function",
		Description: &desc,
	}

	planCfg := jiradozer.StepConfig{
		Model:          "haiku",
		PermissionMode: "plan",
		MaxTurns:       3,
		MaxBudgetUSD:   1.0,
	}

	// Plan step.
	t.Log("Running plan step")
	data := jiradozer.NewPromptData(issue, "main")
	planRes, err := jiradozer.RunStepAgent(ctx, "plan", data, planCfg, workDir, "", "", nil, logger)
	require.NoError(t, err)
	require.NotEmpty(t, planRes.Output)
	require.NotEmpty(t, planRes.SessionID)
	t.Logf("Plan session ID: %s", planRes.SessionID)

	// Build step — uses the plan output.
	buildCfg := jiradozer.StepConfig{
		Model:          "haiku",
		PermissionMode: "bypass",
		MaxTurns:       3,
		MaxBudgetUSD:   1.0,
	}
	data.Plan = planRes.Output

	t.Log("Running build step with plan output")
	buildRes, err := jiradozer.RunStepAgent(ctx, "build", data, buildCfg, workDir, "", "", nil, logger)
	require.NoError(t, err)
	require.NotEmpty(t, buildRes.Output)
	require.NotEmpty(t, buildRes.SessionID)
	t.Logf("Build session ID: %s", buildRes.SessionID)

	// Build and plan should have different session IDs (different steps).
	assert.NotEqual(t, planRes.SessionID, buildRes.SessionID, "plan and build should have different sessions")
}
