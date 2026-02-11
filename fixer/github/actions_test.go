package github

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/wt"
)

// mockGHRunner records calls and returns canned responses.
type mockGHRunner struct {
	responses map[string]*wt.CmdResult
	calls     [][]string
}

func newMockGHRunner() *mockGHRunner {
	return &mockGHRunner{
		responses: make(map[string]*wt.CmdResult),
	}
}

func (m *mockGHRunner) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	m.calls = append(m.calls, args)
	key := fmt.Sprintf("%v", args)
	if resp, ok := m.responses[key]; ok {
		return resp, nil
	}
	return &wt.CmdResult{}, fmt.Errorf("no mock for args: %v", args)
}

func (m *mockGHRunner) set(args []string, stdout string) {
	key := fmt.Sprintf("%v", args)
	m.responses[key] = &wt.CmdResult{Stdout: stdout}
}

func TestListFailedRuns(t *testing.T) {
	mock := newMockGHRunner()

	runs := []WorkflowRun{
		{
			ID:         123,
			Name:       "CI",
			Status:     "completed",
			Conclusion: "failure",
			HeadBranch: "main",
			HeadSHA:    "abc123",
			URL:        "https://github.com/owner/repo/actions/runs/123",
			CreatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	data, _ := json.Marshal(runs)
	mock.set([]string{
		"run", "list",
		"--branch", "main",
		"--status", "failure",
		"--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt",
		"--limit", "5",
	}, string(data))

	client := NewClient(mock, "/repo")
	result, err := client.ListFailedRuns(context.Background(), "main", 5)
	if err != nil {
		t.Fatalf("ListFailedRuns: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 run, got %d", len(result))
	}
	if result[0].ID != 123 {
		t.Errorf("expected run ID 123, got %d", result[0].ID)
	}
	if result[0].HeadBranch != "main" {
		t.Errorf("expected branch main, got %s", result[0].HeadBranch)
	}
}

func TestGetJobsForRun(t *testing.T) {
	mock := newMockGHRunner()

	resp := jobsResponse{
		Jobs: []JobResult{
			{ID: 456, Name: "lint", Conclusion: "failure"},
			{ID: 789, Name: "test", Conclusion: "success"},
		},
	}
	data, _ := json.Marshal(resp)
	mock.set([]string{
		"run", "view", "123",
		"--json", "jobs",
	}, string(data))

	client := NewClient(mock, "/repo")
	jobs, err := client.GetJobsForRun(context.Background(), 123)
	if err != nil {
		t.Fatalf("GetJobsForRun: %v", err)
	}

	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	if jobs[0].RunID != 123 {
		t.Errorf("expected RunID 123, got %d", jobs[0].RunID)
	}
	if jobs[0].Name != "lint" {
		t.Errorf("expected job name lint, got %s", jobs[0].Name)
	}
}

func TestGetAnnotations(t *testing.T) {
	mock := newMockGHRunner()

	// Set up jobs response
	resp := jobsResponse{
		Jobs: []JobResult{
			{ID: 456, Name: "lint", Conclusion: "failure"},
		},
	}
	jobsData, _ := json.Marshal(resp)
	mock.set([]string{
		"run", "view", "123",
		"--json", "jobs",
	}, string(jobsData))

	// Set up annotations response
	anns := []Annotation{
		{Path: "main.go", StartLine: 10, Level: "failure", Message: "unused variable", Title: "staticcheck SA1019"},
	}
	annsData, _ := json.Marshal(anns)
	mock.set([]string{
		"api",
		"repos/{owner}/{repo}/check-runs/456/annotations",
	}, string(annsData))

	client := NewClient(mock, "/repo")
	result, err := client.GetAnnotations(context.Background(), 123)
	if err != nil {
		t.Fatalf("GetAnnotations: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(result))
	}
	if result[0].JobName != "lint" {
		t.Errorf("expected job name lint, got %s", result[0].JobName)
	}
}

func TestGetJobLog(t *testing.T) {
	mock := newMockGHRunner()
	mock.set([]string{
		"run", "view", "123",
		"--log-failed",
	}, "some log output here")

	client := NewClient(mock, "/repo")
	log, err := client.GetJobLog(context.Background(), 123)
	if err != nil {
		t.Fatalf("GetJobLog: %v", err)
	}

	if log != "some log output here" {
		t.Errorf("unexpected log: %s", log)
	}
}
