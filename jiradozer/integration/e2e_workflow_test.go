//go:build integration

package integration

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func e2eIssue() *tracker.Issue {
	desc := "Create a file named hello.txt that contains the text 'hello world'"
	return &tracker.Issue{
		ID:          "fake-issue-1",
		Identifier:  "TEST-1",
		Title:       "Create hello.txt with hello world",
		Description: &desc,
		TeamID:      "team-fake",
	}
}

func e2eWorkflowStates() []tracker.WorkflowState {
	return []tracker.WorkflowState{
		{ID: "state-ip", Name: "In Progress", Type: "started"},
		{ID: "state-ir", Name: "In Review", Type: "started"},
		{ID: "state-done", Name: "Done", Type: "completed"},
	}
}

func e2eConfig(t *testing.T, workDir string) *jiradozer.Config {
	t.Helper()
	cfg := jiradozer.DefaultConfigForTest()
	cfg.Agent.Model = "haiku"
	cfg.WorkDir = workDir
	cfg.BaseBranch = "main"
	cfg.MaxBudgetUSD = 5.0
	cfg.PollInterval = 50 * time.Millisecond

	// All steps use haiku, low turns, auto-approve.
	cfg.Plan.Model = "haiku"
	cfg.Plan.MaxTurns = 3
	cfg.Plan.MaxBudgetUSD = 2.0
	cfg.Plan.AutoApprove = true

	cfg.Build.Model = "haiku"
	cfg.Build.MaxTurns = 3
	cfg.Build.MaxBudgetUSD = 2.0
	cfg.Build.AutoApprove = true

	// Custom validate prompt: avoids running test frameworks.
	cfg.Validate.Model = "haiku"
	cfg.Validate.MaxTurns = 3
	cfg.Validate.MaxBudgetUSD = 2.0
	cfg.Validate.AutoApprove = true
	cfg.Validate.Prompt = `Issue: {{.Identifier}} — {{.Title}}

Check if hello.txt exists in the current directory and contains "hello world".
Report what you find. Do not run any test frameworks or linters.`

	// Custom ship prompt: avoids gh pr create which needs a real git remote.
	cfg.Ship.Model = "haiku"
	cfg.Ship.MaxTurns = 3
	cfg.Ship.MaxBudgetUSD = 2.0
	cfg.Ship.AutoApprove = true
	cfg.Ship.Prompt = `Issue: {{.Identifier}} — {{.Title}}

Write a file named SHIP_SUMMARY.md summarizing what was built and how it would be shipped.
Do not attempt to create a pull request or run any git commands.`

	return cfg
}

// TestE2E_HappyPath_AllAutoApprove runs the full plan→build→validate→ship
// workflow with real Claude execution (haiku) and auto-approve at every review step.
func TestE2E_HappyPath_AllAutoApprove(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	workDir := t.TempDir()

	issue := e2eIssue()
	ft := NewFakeTracker(e2eWorkflowStates())
	ft.AddIssue(*issue)

	cfg := e2eConfig(t, workDir)
	wf := jiradozer.NewWorkflow(ft, issue, cfg, logger)

	var transitions []jiradozer.WorkflowStep
	var mu sync.Mutex
	wf.OnTransition = func(step jiradozer.WorkflowStep) {
		mu.Lock()
		transitions = append(transitions, step)
		mu.Unlock()
		t.Logf("transition → %s", step)
	}

	err := wf.Run(ctx)
	require.NoError(t, err, "workflow should complete successfully")

	// Final tracker state should be "done".
	assert.Equal(t, "state-done", ft.IssueStateID(issue.ID), "final tracker state should be done")

	// Transition sequence should follow the happy path.
	mu.Lock()
	got := transitions
	mu.Unlock()

	expected := []jiradozer.WorkflowStep{
		jiradozer.StepPlanning,
		jiradozer.StepPlanReview,
		jiradozer.StepBuilding,
		jiradozer.StepBuildReview,
		jiradozer.StepValidating,
		jiradozer.StepValidateReview,
		jiradozer.StepShipping,
		jiradozer.StepShipReview,
		jiradozer.StepDone,
	}
	assert.Equal(t, expected, got, "transitions should follow happy path order")

	// FetchWorkflowStates called once at start.
	assert.Len(t, ft.CallsFor("FetchWorkflowStates"), 1)

	// UpdateIssueState: first is in_progress, last is done.
	updateCalls := ft.CallsFor("UpdateIssueState")
	require.GreaterOrEqual(t, len(updateCalls), 2, "at least in_progress + done")
	assert.Equal(t, "state-ip", updateCalls[0].Args[1], "first state update should be in_progress")
	assert.Equal(t, "state-done", updateCalls[len(updateCalls)-1].Args[1], "last state update should be done")

	// PostComment should include step-complete headings.
	postCalls := ft.CallsFor("PostComment")
	assert.GreaterOrEqual(t, len(postCalls), 4, "at least 4 PostComment calls (one per step)")

	var bodies []string
	for _, c := range postCalls {
		bodies = append(bodies, c.Args[1])
	}
	for _, heading := range []string{"## Plan Complete", "## Build Complete", "## Validate Complete", "## Ship Complete"} {
		found := false
		for _, b := range bodies {
			if strings.Contains(b, heading) {
				found = true
				break
			}
		}
		assert.True(t, found, "should have comment containing %q", heading)
	}

	// FetchComments should NOT be called since all steps are auto-approved.
	assert.Empty(t, ft.CallsFor("FetchComments"), "FetchComments should not be called with auto-approve")
}

// TestE2E_PlanStep_Smoke is a fast smoke test that runs only the plan step
// with real Claude execution. Verifies the agent setup works before running
// the full 10-minute E2E test.
func TestE2E_PlanStep_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()

	issue := e2eIssue()
	data := jiradozer.NewPromptData(issue, "main")

	stepCfg := jiradozer.StepConfig{
		Model:          "haiku",
		PermissionMode: "plan",
		MaxTurns:       3,
		MaxBudgetUSD:   1.0,
	}

	output, sessionID, err := jiradozer.RunStepAgent(ctx, "plan", data, stepCfg, workDir, "", "", logger)
	require.NoError(t, err, "plan step should succeed")
	assert.NotEmpty(t, output, "plan output should not be empty")
	assert.NotEmpty(t, sessionID, "should return a session ID")
	t.Logf("Plan output (first 300 chars): %.300s", output)
}
