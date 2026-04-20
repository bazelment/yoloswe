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

func e2eStepConfig(autoApprove bool) jiradozer.StepConfig {
	return jiradozer.StepConfig{
		Model:        "haiku",
		MaxTurns:     3,
		MaxBudgetUSD: 2.0,
		AutoApprove:  autoApprove,
	}
}

func e2eConfig(t *testing.T, workDir string) *jiradozer.Config {
	t.Helper()
	cfg := jiradozer.DefaultConfig()
	cfg.Agent.Model = "haiku"
	cfg.WorkDir = workDir
	cfg.BaseBranch = "main"
	cfg.MaxBudgetUSD = 5.0
	cfg.PollInterval = 50 * time.Millisecond

	cfg.Plan = e2eStepConfig(true)
	cfg.Plan.PermissionMode = "plan"
	cfg.Build = e2eStepConfig(true)
	cfg.Build.PermissionMode = "bypass"

	cfg.Validate = e2eStepConfig(true)
	cfg.Validate.PermissionMode = "bypass"
	cfg.Validate.Prompt = `Issue: {{.Identifier}} — {{.Title}}

Check if hello.txt exists in the current directory and contains "hello world".
Report what you find. Do not run any test frameworks or linters.`

	cfg.CreatePR = e2eStepConfig(true)
	cfg.CreatePR.PermissionMode = "bypass"
	cfg.CreatePR.Prompt = `Report that the PR creation step ran successfully. Do not create a pull request or run any git commands.`

	cfg.Ship = e2eStepConfig(true)
	cfg.Ship.PermissionMode = "bypass"
	cfg.Ship.Prompt = `Issue: {{.Identifier}} — {{.Title}}

Write a file named SHIP_SUMMARY.md summarizing what was built and how it would be shipped.
Do not attempt to create a pull request or run any git commands.`

	return cfg
}

var stepCompleteHeadings = []string{"## Plan Complete", "## Build Complete", "## Create_pr Complete", "## Validate Complete", "## Ship Complete"}

func postCommentBodies(ft *FakeTracker) []string {
	var bodies []string
	for _, c := range ft.CallsFor("PostComment") {
		bodies = append(bodies, c.Args[1])
	}
	return bodies
}

// assertBodyContains checks that at least one body string contains the given substring.
func assertBodyContains(t *testing.T, bodies []string, sub string) {
	t.Helper()
	for _, b := range bodies {
		if strings.Contains(b, sub) {
			return
		}
	}
	t.Errorf("no comment body contains %q", sub)
}

// countBodies returns how many body strings contain the given substring.
func countBodies(bodies []string, sub string) int {
	n := 0
	for _, b := range bodies {
		if strings.Contains(b, sub) {
			n++
		}
	}
	return n
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

	assert.Equal(t, "state-done", ft.IssueStateID(issue.ID), "final tracker state should be done")

	mu.Lock()
	got := transitions
	mu.Unlock()

	expected := []jiradozer.WorkflowStep{
		jiradozer.StepPlanning,
		jiradozer.StepPlanReview,
		jiradozer.StepBuilding,
		jiradozer.StepCreatingPR,
		jiradozer.StepBuildReview,
		jiradozer.StepValidating,
		jiradozer.StepValidateReview,
		jiradozer.StepShipping,
		jiradozer.StepShipReview,
		jiradozer.StepDone,
	}
	assert.Equal(t, expected, got, "transitions should follow happy path order")

	assert.Len(t, ft.CallsFor("FetchWorkflowStates"), 1)

	updateCalls := ft.CallsFor("UpdateIssueState")
	require.GreaterOrEqual(t, len(updateCalls), 2, "at least in_progress + done")
	assert.Equal(t, "state-ip", updateCalls[0].Args[1])
	assert.Equal(t, "state-done", updateCalls[len(updateCalls)-1].Args[1])

	postBodies := postCommentBodies(ft)
	assert.GreaterOrEqual(t, len(postBodies), 4, "at least 4 PostComment calls (one per step)")
	for _, heading := range stepCompleteHeadings {
		assertBodyContains(t, postBodies, heading)
	}

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

	res, err := jiradozer.RunStepAgent(ctx, "plan", data, stepCfg, workDir, "", "", nil, logger)
	require.NoError(t, err, "plan step should succeed")
	assert.NotEmpty(t, res.Output, "plan output should not be empty")
	assert.NotEmpty(t, res.SessionID, "should return a session ID")
	t.Logf("Plan output (first 300 chars): %.300s", res.Output)
}

// TestE2E_HumanFeedback runs the full workflow with human feedback injected
// at each review step. The plan step gets a "redo" with feedback first, then
// "approve" on the second review. All other steps get "approve" directly.
// This exercises the feedback polling, redo loop, and comment injection paths.
func TestE2E_HumanFeedback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	workDir := t.TempDir()

	issue := e2eIssue()
	ft := NewFakeTracker(e2eWorkflowStates())
	ft.AddIssue(*issue)

	cfg := e2eConfig(t, workDir)
	// Disable auto-approve for all steps — feedback will be injected.
	cfg.Plan.AutoApprove = false
	cfg.Build.AutoApprove = false
	cfg.Validate.AutoApprove = false
	cfg.Ship.AutoApprove = false

	wf := jiradozer.NewWorkflow(ft, issue, cfg, logger)

	var transitions []jiradozer.WorkflowStep
	var mu sync.Mutex
	var wg sync.WaitGroup
	planReviewCount := 0

	// injectAfterDelay spawns a goroutine that waits briefly then injects a
	// human comment. The delay ensures lastCommentAt is set before the
	// injected comment's CreatedAt (the workflow sets lastCommentAt
	// synchronously after the OnTransition callback returns).
	injectAfterDelay := func(body string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(200 * time.Millisecond)
			ft.InjectHumanComment(issue.ID, body)
		}()
	}

	wf.OnTransition = func(step jiradozer.WorkflowStep) {
		mu.Lock()
		transitions = append(transitions, step)
		mu.Unlock()
		t.Logf("transition → %s", step)

		switch step {
		case jiradozer.StepPlanReview:
			mu.Lock()
			planReviewCount++
			visit := planReviewCount
			mu.Unlock()
			if visit == 1 {
				injectAfterDelay("Please also consider edge cases and error handling in the plan")
			} else {
				injectAfterDelay("lgtm")
			}
		case jiradozer.StepBuildReview:
			injectAfterDelay("approve")
		case jiradozer.StepValidateReview:
			injectAfterDelay("ship it")
		case jiradozer.StepShipReview:
			injectAfterDelay("approved")
		}
	}

	err := wf.Run(ctx)
	wg.Wait() // Ensure all inject goroutines finish before test returns.
	require.NoError(t, err, "workflow should complete successfully")

	assert.Equal(t, "state-done", ft.IssueStateID(issue.ID), "final tracker state should be done")

	mu.Lock()
	got := transitions
	mu.Unlock()

	expected := []jiradozer.WorkflowStep{
		jiradozer.StepPlanning,
		jiradozer.StepPlanReview,
		jiradozer.StepPlanning,   // redo: back to planning
		jiradozer.StepPlanReview, // second review
		jiradozer.StepBuilding,
		jiradozer.StepCreatingPR,
		jiradozer.StepBuildReview,
		jiradozer.StepValidating,
		jiradozer.StepValidateReview,
		jiradozer.StepShipping,
		jiradozer.StepShipReview,
		jiradozer.StepDone,
	}
	assert.Equal(t, expected, got, "transitions should include plan redo loop")

	fetchCalls := ft.CallsFor("FetchComments")
	assert.GreaterOrEqual(t, len(fetchCalls), 5, "FetchComments called at least 5 times (2 plan reviews + 3 other reviews)")

	postBodies := postCommentBodies(ft)

	// Waiting comments posted for each review (not auto-approved).
	assert.GreaterOrEqual(t, countBodies(postBodies, "Waiting for review"), 5,
		"should have 5 waiting comments (2 plan + build + validate + ship)")

	for _, heading := range stepCompleteHeadings {
		assertBodyContains(t, postBodies, heading)
	}

	// Plan Complete appears twice (original + redo).
	assert.Equal(t, 2, countBodies(postBodies, "## Plan Complete"),
		"Plan Complete should appear twice (original + redo)")
}
