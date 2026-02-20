package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/medivac/issue"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// mockQuery returns a fake QueryResult with the given text and cost.
func mockQuery(text string, cost float64) QueryFn {
	return func(_ context.Context, _, _ string) (*agent.QueryResult, error) {
		return &agent.QueryResult{
			Text:  text,
			Usage: agent.AgentUsage{CostUSD: cost},
		}, nil
	}
}

func mockQueryError(err error) QueryFn {
	return func(_ context.Context, _, _ string) (*agent.QueryResult, error) {
		return nil, err
	}
}

var testRun = WorkflowRun{
	ID:         100,
	Name:       "CI",
	HeadBranch: "main",
	HeadSHA:    "abc123",
	URL:        "https://example.com/runs/100",
	CreatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
}

var testJobs = []JobResult{
	{ID: 200, Name: "lint", Conclusion: "failure"},
	{ID: 201, Name: "build", Conclusion: "failure"},
}

func TestTriageRun_BasicJSON(t *testing.T) {
	responseJSON := `[
		{
			"category": "lint/go",
			"job": "lint",
			"file": "pkg/server.go",
			"line": 42,
			"summary": "unused variable 'ctx'",
			"details": "pkg/server.go:42:10: ctx declared and not used"
		},
		{
			"category": "lint/ts",
			"job": "build",
			"file": "src/app.ts",
			"line": 15,
			"summary": "no-unused-vars: 'x' is declared but never used",
			"details": "src/app.ts:15:7 error TS6133"
		}
	]`

	cfg := TriageConfig{
		Model: "haiku",
		Query: mockQuery(responseJSON, 0.001),
	}

	failures, cost, err := triageRun(context.Background(), testRun, testJobs, nil, "some log", cfg)
	if err != nil {
		t.Fatalf("triageRun: %v", err)
	}
	if cost != 0.001 {
		t.Errorf("expected cost 0.001, got %f", cost)
	}
	if len(failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(failures))
	}

	f0 := failures[0]
	if f0.Category != issue.CategoryLintGo {
		t.Errorf("expected lint/go, got %s", f0.Category)
	}
	if f0.File != "pkg/server.go" {
		t.Errorf("expected pkg/server.go, got %s", f0.File)
	}
	if f0.Line != 42 {
		t.Errorf("expected line 42, got %d", f0.Line)
	}
	if f0.RunID != 100 {
		t.Errorf("expected runID 100, got %d", f0.RunID)
	}
	if f0.JobName != "lint" {
		t.Errorf("expected jobName lint, got %s", f0.JobName)
	}
	if f0.Signature == "" {
		t.Error("expected non-empty signature")
	}

	f1 := failures[1]
	if f1.Category != issue.CategoryLintTS {
		t.Errorf("expected lint/ts, got %s", f1.Category)
	}
	if f1.JobName != "build" {
		t.Errorf("expected jobName build, got %s", f1.JobName)
	}
}

func TestTriageRun_MarkdownFencedJSON(t *testing.T) {
	responseJSON := "```json\n" + `[{"category": "test", "job": "lint", "file": "", "line": 0, "summary": "TestFoo failed", "details": "assertion error"}]` + "\n```"

	cfg := TriageConfig{
		Query: mockQuery(responseJSON, 0.0005),
	}

	failures, _, err := triageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("triageRun: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].Category != issue.CategoryTest {
		t.Errorf("expected test, got %s", failures[0].Category)
	}
}

func TestTriageRun_InvalidCategory(t *testing.T) {
	responseJSON := `[{"category": "deploy/k8s", "job": "lint", "file": "", "line": 0, "summary": "deploy failed", "details": "timeout"}]`

	cfg := TriageConfig{
		Query: mockQuery(responseJSON, 0.001),
	}

	failures, _, err := triageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("triageRun: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].Category != issue.CategoryUnknown {
		t.Errorf("invalid category should map to unknown, got %s", failures[0].Category)
	}
}

func TestTriageRun_InvalidJobFallback(t *testing.T) {
	// LLM returns a job name not in the list — should fall back to first job.
	responseJSON := `[{"category": "test", "job": "nonexistent-job", "file": "", "line": 0, "summary": "fail", "details": "err"}]`

	cfg := TriageConfig{
		Query: mockQuery(responseJSON, 0.001),
	}

	failures, _, err := triageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("triageRun: %v", err)
	}
	if failures[0].JobName != "lint" {
		t.Errorf("expected fallback to first job 'lint', got %s", failures[0].JobName)
	}
}

func TestTriageRun_EmptyArray(t *testing.T) {
	cfg := TriageConfig{
		Query: mockQuery("[]", 0.0001),
	}

	failures, _, err := triageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("triageRun: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(failures))
	}
}

func TestTriageRun_QueryError(t *testing.T) {
	cfg := TriageConfig{
		Query: mockQueryError(fmt.Errorf("network error")),
	}

	_, _, err := triageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "network error") {
		t.Errorf("expected network error in message, got: %s", err.Error())
	}
}

func TestTriageRun_Dedup(t *testing.T) {
	// Same failure twice (across jobs) should be deduped.
	responseJSON := `[
		{"category": "lint/go", "job": "lint", "file": "main.go", "line": 10, "summary": "unused var", "details": "x"},
		{"category": "lint/go", "job": "build", "file": "main.go", "line": 20, "summary": "unused var", "details": "x"}
	]`

	cfg := TriageConfig{
		Query: mockQuery(responseJSON, 0.001),
	}

	failures, _, err := triageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("triageRun: %v", err)
	}
	if len(failures) != 1 {
		t.Errorf("expected 1 deduped failure, got %d", len(failures))
	}
}

func TestBuildTriagePrompt(t *testing.T) {
	anns := []Annotation{
		{Path: "main.go", StartLine: 10, Level: "error", Message: "unused variable"},
	}

	prompt := buildTriagePrompt(testRun, testJobs, anns, "some log output")

	checks := []string{
		"CI",
		"main",
		"abc123",
		"lint",
		"build",
		"Failed jobs (2)",
		"unused variable",
		"main.go:10",
		"some log output",
		"lint/go",
		"lint/ts",
		"build/docker",
		"DEDUPLICATE",
	}
	for _, check := range checks {
		if !strings.Contains(prompt, check) {
			t.Errorf("prompt should contain %q", check)
		}
	}
}

func TestTriageBatch_Empty(t *testing.T) {
	result, err := TriageBatch(context.Background(), nil, TriageConfig{})
	if err != nil {
		t.Fatalf("TriageBatch: %v", err)
	}
	if len(result.Failures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(result.Failures))
	}
	if result.Cost != 0 {
		t.Errorf("expected 0 cost, got %f", result.Cost)
	}
}

func TestTriageBatch_SingleRun(t *testing.T) {
	responseJSON := `[
		{"category": "lint/go", "job": "lint", "file": "main.go", "line": 10, "summary": "unused var", "details": "x"},
		{"category": "lint/ts", "job": "build", "file": "app.ts", "line": 5, "summary": "no-unused-vars", "details": "y"}
	]`

	cfg := TriageConfig{
		Query: mockQuery(responseJSON, 0.003),
	}

	runs := []RunData{
		{Run: testRun, FailedJobs: testJobs, Log: "some log"},
	}

	result, err := TriageBatch(context.Background(), runs, cfg)
	if err != nil {
		t.Fatalf("TriageBatch: %v", err)
	}
	if len(result.Failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(result.Failures))
	}
	if result.Cost != 0.003 {
		t.Errorf("expected cost 0.003, got %f", result.Cost)
	}
	for _, f := range result.Failures {
		if f.Signature == "" {
			t.Error("expected non-empty signature")
		}
	}
}

func TestTriageBatch_MultiRun(t *testing.T) {
	responseJSON := `[
		{"category": "lint/go", "job": "lint", "file": "main.go", "line": 10, "summary": "unused var", "details": "x"},
		{"category": "build/docker", "job": "build", "file": "", "line": 0, "summary": "docker build failed", "details": "exit code 1"}
	]`

	run2 := WorkflowRun{
		ID: 200, Name: "Docker Publish", HeadBranch: "main", HeadSHA: "def456",
		URL: "https://example.com/runs/200", CreatedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
	}

	callCount := 0
	countingQuery := func(_ context.Context, _, _ string) (*agent.QueryResult, error) {
		callCount++
		return &agent.QueryResult{
			Text:  responseJSON,
			Usage: agent.AgentUsage{CostUSD: 0.005},
		}, nil
	}

	runs := []RunData{
		{Run: testRun, FailedJobs: testJobs, Log: "log from CI run"},
		{Run: run2, FailedJobs: testJobs, Log: "log from Docker run"},
	}

	result, err := TriageBatch(context.Background(), runs, TriageConfig{Query: countingQuery})
	if err != nil {
		t.Fatalf("TriageBatch: %v", err)
	}

	// Should make exactly 1 LLM call for both runs combined.
	if callCount != 1 {
		t.Errorf("expected 1 LLM call for batch triage, got %d", callCount)
	}
	if len(result.Failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(result.Failures))
	}
}

func TestTriageBatch_MultiRun_RunAttribution(t *testing.T) {
	// LLM returns failures with run_id matching the correct run.
	responseJSON := fmt.Sprintf(`[
		{"run_id": %d, "category": "lint/go", "job": "lint", "file": "main.go", "line": 10, "summary": "unused var", "details": "x"},
		{"run_id": 200, "category": "build/docker", "job": "build", "file": "", "line": 0, "summary": "docker build failed", "details": "exit code 1"}
	]`, testRun.ID)

	run2 := WorkflowRun{
		ID: 200, Name: "Docker Publish", HeadBranch: "main", HeadSHA: "def456",
		URL: "https://example.com/runs/200", CreatedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
	}

	runs := []RunData{
		{Run: testRun, FailedJobs: testJobs, Log: "ci log"},
		{Run: run2, FailedJobs: testJobs, Log: "docker log"},
	}

	result, err := TriageBatch(context.Background(), runs, TriageConfig{Query: mockQuery(responseJSON, 0.005)})
	if err != nil {
		t.Fatalf("TriageBatch: %v", err)
	}
	if len(result.Failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(result.Failures))
	}

	// First failure should have testRun metadata.
	f0 := result.Failures[0]
	if f0.RunID != testRun.ID {
		t.Errorf("failure 0: expected RunID %d, got %d", testRun.ID, f0.RunID)
	}
	if f0.RunURL != testRun.URL {
		t.Errorf("failure 0: expected RunURL %q, got %q", testRun.URL, f0.RunURL)
	}

	// Second failure should have run2 metadata.
	f1 := result.Failures[1]
	if f1.RunID != 200 {
		t.Errorf("failure 1: expected RunID 200, got %d", f1.RunID)
	}
	if f1.RunURL != "https://example.com/runs/200" {
		t.Errorf("failure 1: expected RunURL for run2, got %q", f1.RunURL)
	}
}

func TestBuildBatchTriagePrompt(t *testing.T) {
	run2 := WorkflowRun{ID: 200, Name: "Docker Publish", HeadBranch: "main", HeadSHA: "def456"}
	runs := []RunData{
		{Run: testRun, FailedJobs: testJobs, Log: "ci log output"},
		{Run: run2, FailedJobs: []JobResult{{Name: "docker-build", Conclusion: "failure"}}, Log: "docker log output"},
	}

	prompt := buildBatchTriagePrompt(runs)

	checks := []string{
		"triage system",
		"Run 1: CI",
		"Run 2: Docker Publish",
		"ci log output",
		"docker log output",
		"run_id",
		"ENUMERATE",
		"DEDUPLICATE",
		"ONLY ONCE",
		"lint",
		"docker-build",
	}
	for _, check := range checks {
		if !strings.Contains(prompt, check) {
			t.Errorf("batch triage prompt should contain %q", check)
		}
	}
}

func TestParseTriageResponse_NoJSON(t *testing.T) {
	_, err := parseTriageResponse("This response has no JSON at all")
	if err == nil {
		t.Fatal("expected error for non-JSON response")
	}
}

func TestParseTriageResponse_InvalidJSON(t *testing.T) {
	_, err := parseTriageResponse("[{invalid json}]")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseTriageResponse_TextAroundJSON(t *testing.T) {
	input := "Here are the results:\n\n[{\"category\":\"test\",\"job\":\"lint\",\"file\":\"\",\"line\":0,\"summary\":\"test failed\",\"details\":\"err\"}]\n\nHope this helps!"
	items, err := parseTriageResponse(input)
	if err != nil {
		t.Fatalf("parseTriageResponse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Category != "test" {
		t.Errorf("expected test, got %s", items[0].Category)
	}
}

func TestTriageResponse_WithErrorCode(t *testing.T) {
	response := `[{
		"category": "lint/ts",
		"job": "lint",
		"file": "services/typescript/forge-v2/src/foo.tsx",
		"line": 42,
		"error_code": "TS7006",
		"summary": "Parameter 'e' implicitly has an 'any' type",
		"details": "error TS7006: Parameter 'e' implicitly has an 'any' type."
	}]`

	items, err := parseTriageResponse(response)
	if err != nil {
		t.Fatalf("parseTriageResponse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ErrorCode != "TS7006" {
		t.Errorf("expected error_code TS7006, got %q", items[0].ErrorCode)
	}
}
