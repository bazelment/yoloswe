package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/medivac/github"
	"github.com/bazelment/yoloswe/medivac/issue"
	"github.com/bazelment/yoloswe/wt"
)

// mockGHRunner for engine tests.
type mockGHRunner struct {
	responses map[string]*wt.CmdResult
}

func newMockGHRunner() *mockGHRunner {
	return &mockGHRunner{responses: make(map[string]*wt.CmdResult)}
}

func (m *mockGHRunner) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	key := fmt.Sprintf("%v", args)
	if resp, ok := m.responses[key]; ok {
		return resp, nil
	}
	return &wt.CmdResult{}, fmt.Errorf("no mock for: %v", args)
}

func (m *mockGHRunner) set(args []string, stdout string) {
	key := fmt.Sprintf("%v", args)
	m.responses[key] = &wt.CmdResult{Stdout: stdout}
}

// mockTriageQuery returns a mock QueryFn that produces triage results.
func mockTriageQuery(failures []triageItem, cost float64) github.QueryFn {
	data, _ := json.Marshal(failures)
	return func(_ context.Context, _ string, _ ...claude.SessionOption) (*claude.QueryResult, error) {
		return &claude.QueryResult{
			TurnResult: claude.TurnResult{
				Text:    string(data),
				Success: true,
				Usage:   claude.TurnUsage{CostUSD: cost},
			},
		}, nil
	}
}

type triageItem struct {
	Category string `json:"category"`
	File     string `json:"file"`
	Summary  string `json:"summary"`
	Details  string `json:"details"`
	Line     int    `json:"line"`
}

func setupMockForScan(mock *mockGHRunner) {
	// Mock failed runs
	runs := []github.WorkflowRun{
		{
			ID:         100,
			Name:       "CI",
			Conclusion: "failure",
			HeadBranch: "main",
			HeadSHA:    "abc",
			URL:        "https://example.com/runs/100",
			CreatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	runsData, _ := json.Marshal(runs)
	mock.set([]string{
		"run", "list",
		"--branch", "main",
		"--status", "failure",
		"--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt",
		"--limit", "5",
	}, string(runsData))

	// Mock jobs
	jobs := struct {
		Jobs []github.JobResult `json:"jobs"`
	}{
		Jobs: []github.JobResult{
			{ID: 200, Name: "lint", Conclusion: "failure"},
		},
	}
	jobsData, _ := json.Marshal(jobs)
	mock.set([]string{
		"run", "view", "100",
		"--json", "jobs",
	}, string(jobsData))

	// Mock annotations (empty — annotations are just context for LLM now)
	mock.set([]string{
		"api",
		"repos/{owner}/{repo}/check-runs/200/annotations",
	}, "[]")

	// Mock job log
	mock.set([]string{
		"run", "view", "100",
		"--log-failed",
	}, "some log output with errors")
}

func TestScan(t *testing.T) {
	mock := newMockGHRunner()
	setupMockForScan(mock)

	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")

	triageQuery := mockTriageQuery([]triageItem{
		{Category: "lint/go", File: "main.go", Line: 10, Summary: "unused variable x", Details: "main.go:10: unused"},
	}, 0.001)

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: trackerPath,
		Branch:      "main",
		RunLimit:    5,
		TriageQuery: triageQuery,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(result.Runs) != 1 {
		t.Errorf("expected 1 run, got %d", len(result.Runs))
	}
	if len(result.Failures) != 1 {
		t.Errorf("expected 1 failure, got %d", len(result.Failures))
	}
	if result.Failures[0].Category != github.CategoryLintGo {
		t.Errorf("expected lint/go, got %s", result.Failures[0].Category)
	}
	if len(result.Reconciled.New) != 1 {
		t.Errorf("expected 1 new issue, got %d", len(result.Reconciled.New))
	}
	if result.ActionableLen != 1 {
		t.Errorf("expected 1 actionable, got %d", result.ActionableLen)
	}
	if result.TriageCost != 0.001 {
		t.Errorf("expected triage cost 0.001, got %f", result.TriageCost)
	}
}

func TestScan_NoFailures(t *testing.T) {
	mock := newMockGHRunner()

	// No failed runs
	mock.set([]string{
		"run", "list",
		"--branch", "main",
		"--status", "failure",
		"--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt",
		"--limit", "5",
	}, "[]")

	dir := t.TempDir()
	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: filepath.Join(dir, ".medivac", "issues.json"),
		Branch:      "main",
		RunLimit:    5,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(result.Runs) != 0 {
		t.Errorf("expected 0 runs, got %d", len(result.Runs))
	}
	if result.ActionableLen != 0 {
		t.Errorf("expected 0 actionable, got %d", result.ActionableLen)
	}
}

func TestScan_MultipleFailures(t *testing.T) {
	mock := newMockGHRunner()
	setupMockForScan(mock)

	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")

	triageQuery := mockTriageQuery([]triageItem{
		{Category: "lint/go", File: "server.go", Summary: "unused var", Details: "detail1"},
		{Category: "test", File: "api_test.go", Summary: "TestAPI failed", Details: "detail2"},
		{Category: "build/docker", File: "Dockerfile", Summary: "build error", Details: "detail3"},
	}, 0.002)

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: trackerPath,
		Branch:      "main",
		RunLimit:    5,
		TriageQuery: triageQuery,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(result.Failures) != 3 {
		t.Errorf("expected 3 failures, got %d", len(result.Failures))
	}
	if result.ActionableLen != 3 {
		t.Errorf("expected 3 actionable, got %d", result.ActionableLen)
	}
}

func TestFix_DryRun(t *testing.T) {
	mock := newMockGHRunner()
	setupMockForScan(mock)

	dir := t.TempDir()

	triageQuery := mockTriageQuery([]triageItem{
		{Category: "lint/go", File: "main.go", Line: 10, Summary: "error (staticcheck SA1019)", Details: "detail"},
	}, 0.001)

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: filepath.Join(dir, ".medivac", "issues.json"),
		Branch:      "main",
		RunLimit:    5,
		DryRun:      true,
		TriageQuery: triageQuery,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := eng.Fix(context.Background())
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}

	if len(result.Results) != 1 {
		t.Errorf("expected 1 result (dry-run), got %d", len(result.Results))
	}
	if result.TotalCost != 0 {
		t.Errorf("expected 0 cost for dry-run, got %f", result.TotalCost)
	}
}

func TestScan_SkipsReviewedRuns(t *testing.T) {
	mock := newMockGHRunner()
	setupMockForScan(mock)

	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")

	callCount := 0
	countingQuery := func(_ context.Context, _ string, _ ...claude.SessionOption) (*claude.QueryResult, error) {
		callCount++
		data, _ := json.Marshal([]triageItem{
			{Category: "lint/go", File: "main.go", Summary: "err", Details: "detail"},
		})
		return &claude.QueryResult{
			TurnResult: claude.TurnResult{
				Text:    string(data),
				Success: true,
				Usage:   claude.TurnUsage{CostUSD: 0.001},
			},
		}, nil
	}

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: trackerPath,
		Branch:      "main",
		RunLimit:    5,
		TriageQuery: countingQuery,
	})
	if err != nil {
		t.Fatal(err)
	}

	// First scan: run 100 should be triaged.
	_, err = eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 LLM call on first scan, got %d", callCount)
	}

	// Second scan: run 100 is now reviewed, should be skipped.
	callCount = 0
	result, err := eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if callCount != 0 {
		t.Errorf("expected 0 LLM calls on second scan (all reviewed), got %d", callCount)
	}
	if result.TriageCost != 0 {
		t.Errorf("expected 0 triage cost, got %f", result.TriageCost)
	}
}

func setupMockForMultiRunScan(mock *mockGHRunner) {
	// Two failed runs
	runs := []github.WorkflowRun{
		{
			ID: 100, Name: "CI", Conclusion: "failure",
			HeadBranch: "main", HeadSHA: "abc",
			URL:       "https://example.com/runs/100",
			CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			ID: 200, Name: "CI", Conclusion: "failure",
			HeadBranch: "main", HeadSHA: "def",
			URL:       "https://example.com/runs/200",
			CreatedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		},
	}
	runsData, _ := json.Marshal(runs)
	mock.set([]string{
		"run", "list",
		"--branch", "main",
		"--status", "failure",
		"--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt",
		"--limit", "5",
	}, string(runsData))

	for _, runID := range []string{"100", "200"} {
		jobs := struct {
			Jobs []github.JobResult `json:"jobs"`
		}{
			Jobs: []github.JobResult{
				{ID: 300, Name: "lint", Conclusion: "failure"},
			},
		}
		jobsData, _ := json.Marshal(jobs)
		mock.set([]string{"run", "view", runID, "--json", "jobs"}, string(jobsData))
		mock.set([]string{"api", "repos/{owner}/{repo}/check-runs/300/annotations"}, "[]")
		mock.set([]string{"run", "view", runID, "--log-failed"}, "some log output")
	}
}

func TestScan_MultipleRuns_SingleLLMCall(t *testing.T) {
	mock := newMockGHRunner()
	setupMockForMultiRunScan(mock)

	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")

	responseJSON := `[{"category": "lint/go", "job": "lint", "file": "main.go", "line": 10, "summary": "unused var", "details": "x"}]`

	callCount := 0
	query := func(_ context.Context, _ string, _ ...claude.SessionOption) (*claude.QueryResult, error) {
		callCount++
		return &claude.QueryResult{
			TurnResult: claude.TurnResult{
				Text:    responseJSON,
				Success: true,
				Usage:   claude.TurnUsage{CostUSD: 0.005},
			},
		}, nil
	}

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: trackerPath,
		Branch:      "main",
		RunLimit:    5,
		TriageQuery: query,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Should make exactly 1 LLM call for both runs combined.
	if callCount != 1 {
		t.Errorf("expected 1 LLM call for batch triage, got %d", callCount)
	}

	if len(result.Failures) != 1 {
		t.Errorf("expected 1 failure, got %d", len(result.Failures))
	}

	if result.ActionableLen != 1 {
		t.Errorf("expected 1 actionable, got %d", result.ActionableLen)
	}

	// Both runs should now be reviewed.
	if !eng.tracker.IsRunReviewed(100) {
		t.Error("run 100 should be reviewed")
	}
	if !eng.tracker.IsRunReviewed(200) {
		t.Error("run 200 should be reviewed")
	}
}

func TestScan_SecondScan_OnlyNewRuns(t *testing.T) {
	mock := newMockGHRunner()

	dir := t.TempDir()
	trackerPath := filepath.Join(dir, ".medivac", "issues.json")

	// First scan: 1 run.
	firstRunOnly := []github.WorkflowRun{
		{
			ID: 100, Name: "CI", Conclusion: "failure",
			HeadBranch: "main", HeadSHA: "abc",
			URL:       "https://example.com/runs/100",
			CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	firstRunsData, _ := json.Marshal(firstRunOnly)
	mock.set([]string{
		"run", "list",
		"--branch", "main",
		"--status", "failure",
		"--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt",
		"--limit", "5",
	}, string(firstRunsData))

	jobs := struct {
		Jobs []github.JobResult `json:"jobs"`
	}{
		Jobs: []github.JobResult{
			{ID: 200, Name: "lint", Conclusion: "failure"},
		},
	}
	jobsData, _ := json.Marshal(jobs)
	mock.set([]string{"run", "view", "100", "--json", "jobs"}, string(jobsData))
	mock.set([]string{"api", "repos/{owner}/{repo}/check-runs/200/annotations"}, "[]")
	mock.set([]string{"run", "view", "100", "--log-failed"}, "some log")

	extractionResp := `[{"category": "lint/go", "job": "lint", "file": "main.go", "line": 10, "summary": "unused var", "details": "x"}]`
	callCount := 0
	query := func(_ context.Context, _ string, _ ...claude.SessionOption) (*claude.QueryResult, error) {
		callCount++
		return &claude.QueryResult{
			TurnResult: claude.TurnResult{
				Text:    extractionResp,
				Success: true,
				Usage:   claude.TurnUsage{CostUSD: 0.001},
			},
		}, nil
	}

	eng, err := New(Config{
		GHRunner:    mock,
		RepoDir:     dir,
		TrackerPath: trackerPath,
		Branch:      "main",
		RunLimit:    5,
		TriageQuery: query,
	})
	if err != nil {
		t.Fatal(err)
	}

	// First scan.
	_, err = eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 call on first scan, got %d", callCount)
	}

	// Second scan: add a new run 300.
	callCount = 0
	twoRuns := []github.WorkflowRun{
		{
			ID: 100, Name: "CI", Conclusion: "failure",
			HeadBranch: "main", HeadSHA: "abc",
			URL:       "https://example.com/runs/100",
			CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			ID: 300, Name: "CI", Conclusion: "failure",
			HeadBranch: "main", HeadSHA: "ghi",
			URL:       "https://example.com/runs/300",
			CreatedAt: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC),
		},
	}
	twoRunsData, _ := json.Marshal(twoRuns)
	mock.set([]string{
		"run", "list",
		"--branch", "main",
		"--status", "failure",
		"--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt",
		"--limit", "5",
	}, string(twoRunsData))

	mock.set([]string{"run", "view", "300", "--json", "jobs"}, string(jobsData))
	mock.set([]string{"api", "repos/{owner}/{repo}/check-runs/200/annotations"}, "[]")
	mock.set([]string{"run", "view", "300", "--log-failed"}, "some other log")

	_, err = eng.Scan(context.Background())
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	// Only 1 extraction call for run 300 (run 100 is reviewed), no dedup call (single run).
	if callCount != 1 {
		t.Errorf("expected 1 LLM call on second scan (only new run), got %d", callCount)
	}
}

func TestFixBranchName(t *testing.T) {
	tests := []struct {
		category github.FailureCategory
		id       string
		want     string
	}{
		{github.CategoryLintGo, "abc123", "fix/lint-go/abc123"},
		{github.CategoryTest, "def456", "fix/test/def456"},
		{github.CategoryBuild, "xyz", "fix/build/xyz"},
	}

	for _, tt := range tests {
		iss := &issue.Issue{
			Category: tt.category,
			ID:       tt.id,
		}
		got := fixBranchName(iss)
		if got != tt.want {
			t.Errorf("fixBranchName(%s, %s) = %s, want %s", tt.category, tt.id, got, tt.want)
		}
	}
}
