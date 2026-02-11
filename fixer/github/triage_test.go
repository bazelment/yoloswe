package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// mockQuery returns a fake QueryResult with the given text and cost.
func mockQuery(text string, cost float64) QueryFn {
	return func(_ context.Context, _ string, _ ...claude.SessionOption) (*claude.QueryResult, error) {
		return &claude.QueryResult{
			TurnResult: claude.TurnResult{
				Text:    text,
				Success: true,
				Usage:   claude.TurnUsage{CostUSD: cost},
			},
		}, nil
	}
}

func mockQueryError(err error) QueryFn {
	return func(_ context.Context, _ string, _ ...claude.SessionOption) (*claude.QueryResult, error) {
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

	failures, cost, err := TriageRun(context.Background(), testRun, testJobs, nil, "some log", cfg)
	if err != nil {
		t.Fatalf("TriageRun: %v", err)
	}
	if cost != 0.001 {
		t.Errorf("expected cost 0.001, got %f", cost)
	}
	if len(failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(failures))
	}

	f0 := failures[0]
	if f0.Category != CategoryLintGo {
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
	if f1.Category != CategoryLintTS {
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

	failures, _, err := TriageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("TriageRun: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].Category != CategoryTest {
		t.Errorf("expected test, got %s", failures[0].Category)
	}
}

func TestTriageRun_InvalidCategory(t *testing.T) {
	responseJSON := `[{"category": "deploy/k8s", "job": "lint", "file": "", "line": 0, "summary": "deploy failed", "details": "timeout"}]`

	cfg := TriageConfig{
		Query: mockQuery(responseJSON, 0.001),
	}

	failures, _, err := TriageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("TriageRun: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].Category != CategoryUnknown {
		t.Errorf("invalid category should map to unknown, got %s", failures[0].Category)
	}
}

func TestTriageRun_InvalidJobFallback(t *testing.T) {
	// LLM returns a job name not in the list — should fall back to first job.
	responseJSON := `[{"category": "test", "job": "nonexistent-job", "file": "", "line": 0, "summary": "fail", "details": "err"}]`

	cfg := TriageConfig{
		Query: mockQuery(responseJSON, 0.001),
	}

	failures, _, err := TriageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("TriageRun: %v", err)
	}
	if failures[0].JobName != "lint" {
		t.Errorf("expected fallback to first job 'lint', got %s", failures[0].JobName)
	}
}

func TestTriageRun_EmptyArray(t *testing.T) {
	cfg := TriageConfig{
		Query: mockQuery("[]", 0.0001),
	}

	failures, _, err := TriageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("TriageRun: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("expected 0 failures, got %d", len(failures))
	}
}

func TestTriageRun_QueryError(t *testing.T) {
	cfg := TriageConfig{
		Query: mockQueryError(fmt.Errorf("network error")),
	}

	_, _, err := TriageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
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

	failures, _, err := TriageRun(context.Background(), testRun, testJobs, nil, "log", cfg)
	if err != nil {
		t.Fatalf("TriageRun: %v", err)
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
