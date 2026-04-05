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
	output1, sessionID1, err := jiradozer.RunStepAgent(ctx, "plan", data, stepCfg, workDir, "", "", logger)
	require.NoError(t, err, "first plan execution should succeed")
	require.NotEmpty(t, output1, "first plan output should not be empty")
	require.NotEmpty(t, sessionID1, "first execution should return a session ID")
	t.Logf("Session ID from step 1: %s", sessionID1)
	t.Logf("Plan output (first 200 chars): %.200s", output1)

	// --- Step 2: Resume with feedback (same session ID) ---
	t.Log("Step 2: Resume with feedback (redo)")
	feedback := "Please also add validation for required fields and return typed errors instead of generic errors"
	output2, sessionID2, err := jiradozer.RunStepAgent(ctx, "plan", data, stepCfg, workDir, feedback, sessionID1, logger)
	require.NoError(t, err, "resumed plan execution should succeed")
	require.NotEmpty(t, output2, "resumed plan output should not be empty")
	require.NotEmpty(t, sessionID2, "resumed execution should return a session ID")
	t.Logf("Session ID from step 2: %s", sessionID2)
	t.Logf("Plan output (first 200 chars): %.200s", output2)

	// --- Assertions ---
	// The resumed output should be different from the first (feedback was incorporated).
	assert.NotEqual(t, output1, output2, "resumed output should differ from first execution")

	// The session IDs should be related (same or evolved).
	// Claude's session resume keeps the same session ID.
	t.Logf("Session IDs: first=%s, second=%s", sessionID1, sessionID2)
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
	planOutput, planSessionID, err := jiradozer.RunStepAgent(ctx, "plan", data, planCfg, workDir, "", "", logger)
	require.NoError(t, err)
	require.NotEmpty(t, planOutput)
	require.NotEmpty(t, planSessionID)
	t.Logf("Plan session ID: %s", planSessionID)

	// Build step — uses the plan output.
	buildCfg := jiradozer.StepConfig{
		Model:          "haiku",
		PermissionMode: "bypass",
		MaxTurns:       3,
		MaxBudgetUSD:   1.0,
	}
	data.Plan = planOutput

	t.Log("Running build step with plan output")
	buildOutput, buildSessionID, err := jiradozer.RunStepAgent(ctx, "build", data, buildCfg, workDir, "", "", logger)
	require.NoError(t, err)
	require.NotEmpty(t, buildOutput)
	require.NotEmpty(t, buildSessionID)
	t.Logf("Build session ID: %s", buildSessionID)

	// Build and plan should have different session IDs (different steps).
	assert.NotEqual(t, planSessionID, buildSessionID, "plan and build should have different sessions")
}
