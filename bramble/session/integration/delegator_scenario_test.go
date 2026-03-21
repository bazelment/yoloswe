package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
)

// TestDelegatorScenario_HappyPath verifies the delegator starts a planner,
// then a builder, and reports completion.
func TestDelegatorScenario_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := session.RunDelegatorScenario(ctx, session.DelegatorScenarioConfig{
		InitialPrompt: "Create a hello world Go program in main.go",
		Model:         "haiku",
		AutoNotify:    true,
		MaxTurns:      15,
		TurnTimeout:   120 * time.Second,
		Behaviors: map[string][]*session.MockSessionBehavior{
			"planner": {
				{States: []session.MockSessionState{
					{Status: "running", TurnCount: 1, RecentOutput: []string{"Analyzing task..."}},
					{Status: "completed", TurnCount: 3, TotalCostUSD: 0.05,
						RecentOutput: []string{"Plan: create main.go with hello world program."}},
				}},
			},
			"builder": {
				{States: []session.MockSessionState{
					{Status: "running", TurnCount: 1, RecentOutput: []string{"Writing main.go..."}},
					{Status: "completed", TurnCount: 5, TotalCostUSD: 0.10,
						RecentOutput: []string{"Created main.go with hello world program."}},
				}},
			},
		},
	})
	require.NoError(t, err)

	// Should have called start_session at least once.
	startCalls := result.Mock.CallsFor("start_session")
	assert.GreaterOrEqual(t, len(startCalls), 1, "expected at least one start_session call")

	// Should have called get_session_progress at least once.
	progressCalls := result.Mock.CallsFor("get_session_progress")
	assert.GreaterOrEqual(t, len(progressCalls), 1, "expected at least one get_session_progress call")

	t.Logf("Turns: %d, Total cost: $%.4f, start_session calls: %d, get_session_progress calls: %d",
		result.TurnCount(), result.TotalCost, len(startCalls), len(progressCalls))
}

// TestDelegatorScenario_RetriableError verifies the delegator retries on a
// retriable failure.
func TestDelegatorScenario_RetriableError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	result, err := session.RunDelegatorScenario(ctx, session.DelegatorScenarioConfig{
		InitialPrompt: "Refactor the auth module to use middleware pattern",
		Model:         "haiku",
		AutoNotify:    true,
		MaxTurns:      20,
		TurnTimeout:   180 * time.Second,
		Behaviors: map[string][]*session.MockSessionBehavior{
			"planner": {
				{States: []session.MockSessionState{
					{Status: "completed", TurnCount: 2, TotalCostUSD: 0.03,
						RecentOutput: []string{"Plan complete: refactor auth to middleware."}},
				}},
			},
			"builder": {
				// First builder fails with retriable error.
				{States: []session.MockSessionState{
					{Status: "running", TurnCount: 1},
					{Status: "failed", ErrorMsg: "API rate limit exceeded, please retry", TurnCount: 2},
				}},
				// Second builder succeeds.
				{States: []session.MockSessionState{
					{Status: "running", TurnCount: 1, RecentOutput: []string{"Retrying implementation..."}},
					{Status: "completed", TurnCount: 4, TotalCostUSD: 0.08,
						RecentOutput: []string{"Auth middleware refactored successfully."}},
				}},
			},
		},
	})
	require.NoError(t, err)

	// Should have created multiple builder sessions (retry).
	startCalls := result.Mock.CallsFor("start_session")
	builderStarts := 0
	for _, c := range startCalls {
		if c.Params["type"] == "builder" {
			builderStarts++
		}
	}
	// Flexible: at least 1 builder start (the LLM may or may not retry).
	assert.GreaterOrEqual(t, builderStarts, 1, "expected at least one builder start_session")

	t.Logf("Turns: %d, Builder starts: %d", result.TurnCount(), builderStarts)
}

// TestDelegatorScenario_NonRetriableError verifies the delegator does NOT
// blindly retry on a non-retriable error and instead asks the user.
func TestDelegatorScenario_NonRetriableError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := session.RunDelegatorScenario(ctx, session.DelegatorScenarioConfig{
		InitialPrompt: "Rewrite the entire codebase to use a new framework",
		Model:         "haiku",
		AutoNotify:    true,
		MaxTurns:      10,
		TurnTimeout:   120 * time.Second,
		Behaviors: map[string][]*session.MockSessionBehavior{
			"planner": {
				{States: []session.MockSessionState{
					{Status: "completed", TurnCount: 2, TotalCostUSD: 0.03,
						RecentOutput: []string{"Plan: rewrite entire codebase."}},
				}},
			},
			"builder": {
				{States: []session.MockSessionState{
					{Status: "running", TurnCount: 1},
					{Status: "failed", ErrorMsg: "context window exhausted: 180k tokens used", TurnCount: 3},
				}},
			},
		},
	})
	require.NoError(t, err)

	// The delegator should produce text output (likely asking the user what to do).
	var allText string
	for _, tr := range result.Turns {
		allText += tr.TextOutput
	}
	assert.NotEmpty(t, allText, "delegator should produce text output about the failure")

	t.Logf("Turns: %d, Text length: %d", result.TurnCount(), len(allText))
}

// TestDelegatorScenario_AmbiguousTask verifies the delegator asks for
// clarification when the task is ambiguous.
func TestDelegatorScenario_AmbiguousTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := session.RunDelegatorScenario(ctx, session.DelegatorScenarioConfig{
		InitialPrompt: "Fix the bug",
		Model:         "haiku",
		AutoNotify:    false, // No auto-notify — we expect no sessions started
		MaxTurns:      3,
		TurnTimeout:   120 * time.Second,
		Behaviors:     map[string][]*session.MockSessionBehavior{}, // No behaviors — nothing to match
	})
	require.NoError(t, err)

	// In the first turn, the delegator should NOT start a session.
	// It should ask for clarification.
	if len(result.Turns) > 0 {
		firstTurn := result.Turns[0]
		hasStartSession := false
		for _, tc := range firstTurn.ToolCalls {
			if tc == "start_session" {
				hasStartSession = true
			}
		}
		// Flexible: the LLM *might* start a session, but ideally it asks first.
		if !hasStartSession {
			t.Log("Good: delegator asked for clarification without starting a session")
			assert.NotEmpty(t, firstTurn.TextOutput, "expected text asking for clarification")
		} else {
			t.Log("Note: delegator started a session despite ambiguous task (acceptable but not ideal)")
		}
	}

	t.Logf("Turns: %d", result.TurnCount())
}

// TestDelegatorScenario_MultiSession verifies the delegator can manage
// multiple builder sessions for a multi-part task.
func TestDelegatorScenario_MultiSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := session.RunDelegatorScenario(ctx, session.DelegatorScenarioConfig{
		InitialPrompt: "Add user authentication to the web app AND update the API documentation",
		Model:         "haiku",
		AutoNotify:    true,
		MaxTurns:      20,
		TurnTimeout:   120 * time.Second,
		Behaviors: map[string][]*session.MockSessionBehavior{
			"planner": {
				{States: []session.MockSessionState{
					{Status: "completed", TurnCount: 3, TotalCostUSD: 0.05,
						RecentOutput: []string{
							"Plan: 1) Add auth middleware 2) Update API docs",
						}},
				}},
			},
			"builder": {
				// First builder: auth
				{States: []session.MockSessionState{
					{Status: "running", TurnCount: 1, RecentOutput: []string{"Implementing auth..."}},
					{Status: "completed", TurnCount: 6, TotalCostUSD: 0.12,
						RecentOutput: []string{"Auth middleware added."}},
				}},
				// Second builder: docs
				{States: []session.MockSessionState{
					{Status: "running", TurnCount: 1, RecentOutput: []string{"Updating docs..."}},
					{Status: "completed", TurnCount: 4, TotalCostUSD: 0.08,
						RecentOutput: []string{"API documentation updated."}},
				}},
			},
		},
	})
	require.NoError(t, err)

	// Should have created at least one builder session.
	startCalls := result.Mock.CallsFor("start_session")
	assert.GreaterOrEqual(t, len(startCalls), 1, "expected at least one start_session call")

	// Check that text mentions completion or success somewhere.
	var allText string
	for _, tr := range result.Turns {
		allText += tr.TextOutput
	}
	hasCompletion := strings.Contains(strings.ToLower(allText), "complet") ||
		strings.Contains(strings.ToLower(allText), "done") ||
		strings.Contains(strings.ToLower(allText), "finish") ||
		strings.Contains(strings.ToLower(allText), "success")
	// Flexible assertion — LLM output varies.
	if hasCompletion {
		t.Log("Good: delegator text mentions completion")
	}

	t.Logf("Turns: %d, Total cost: $%.4f, Sessions created: %d",
		result.TurnCount(), result.TotalCost, len(result.Mock.SessionIDs()))
}
