package wt

import (
	"context"
	"strings"
	"testing"
)

// MockGHRunner implements GHRunner for testing.
type MockGHRunner struct {
	Err     error
	Result  *CmdResult
	Results map[string]*CmdResult
	Errors  map[string]error
	Args    []string
	Calls   [][]string
}

func NewMockGHRunner() *MockGHRunner {
	return &MockGHRunner{
		Results: make(map[string]*CmdResult),
		Errors:  make(map[string]error),
	}
}

func (m *MockGHRunner) Run(ctx context.Context, args []string, dir string) (*CmdResult, error) {
	m.Args = args
	m.Calls = append(m.Calls, args)
	key := strings.Join(args, " ")

	// Check for specific result/error first
	if m.Results != nil {
		if err, ok := m.Errors[key]; ok {
			return &CmdResult{ExitCode: 1, Stderr: err.Error()}, err
		}
		if result, ok := m.Results[key]; ok {
			return result, nil
		}
	}

	// Fall back to default Result/Err
	return m.Result, m.Err
}

func TestGetPRForBranch(t *testing.T) {
	mock := &MockGHRunner{
		Result: &CmdResult{
			Stdout: `{"number": 456, "url": "https://github.com/org/repo/pull/456"}`,
		},
	}

	info, err := GetPRForBranch(context.Background(), mock, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.Number != 456 {
		t.Errorf("Number = %d, want 456", info.Number)
	}
	if info.URL != "https://github.com/org/repo/pull/456" {
		t.Errorf("URL = %q, want %q", info.URL, "https://github.com/org/repo/pull/456")
	}
}

func TestCreatePR(t *testing.T) {
	mock := &MockGHRunner{
		Result: &CmdResult{
			Stdout: `{"number": 123, "url": "https://github.com/org/repo/pull/123", "headRefName": "feature", "baseRefName": "main"}`,
		},
	}

	info, err := CreatePR(context.Background(), mock, "Test PR", "Description", "main", false, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.Number != 123 {
		t.Errorf("Number = %d, want 123", info.Number)
	}
	if info.URL != "https://github.com/org/repo/pull/123" {
		t.Errorf("URL = %q, want expected URL", info.URL)
	}

	// Verify args
	args := mock.Args
	if len(args) < 2 || args[0] != "pr" || args[1] != "create" {
		t.Errorf("expected pr create command, got %v", args)
	}

	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, "--base main") {
		t.Errorf("expected --base main in args: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--title") {
		t.Errorf("expected --title in args: %s", argsStr)
	}
}

func TestListAllPRInfo(t *testing.T) {
	mock := &MockGHRunner{
		Result: &CmdResult{
			Stdout: `[
				{"number": 10, "headRefName": "feature-a", "baseRefName": "main", "state": "OPEN", "isDraft": false, "reviewDecision": "APPROVED", "url": "https://github.com/org/repo/pull/10"},
				{"number": 20, "headRefName": "feature-b", "baseRefName": "main", "state": "OPEN", "isDraft": true, "reviewDecision": "", "url": "https://github.com/org/repo/pull/20"}
			]`,
		},
	}

	prs, err := ListAllPRInfo(context.Background(), mock, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(prs) != 2 {
		t.Fatalf("got %d PRs, want 2", len(prs))
	}

	// Verify first PR
	if prs[0].Number != 10 {
		t.Errorf("prs[0].Number = %d, want 10", prs[0].Number)
	}
	if prs[0].HeadRefName != "feature-a" {
		t.Errorf("prs[0].HeadRefName = %q, want %q", prs[0].HeadRefName, "feature-a")
	}
	if prs[0].ReviewDecision != "APPROVED" {
		t.Errorf("prs[0].ReviewDecision = %q, want %q", prs[0].ReviewDecision, "APPROVED")
	}
	if prs[0].IsDraft {
		t.Error("prs[0].IsDraft = true, want false")
	}
	if prs[0].URL != "https://github.com/org/repo/pull/10" {
		t.Errorf("prs[0].URL = %q, want expected URL", prs[0].URL)
	}

	// Verify second PR
	if prs[1].Number != 20 {
		t.Errorf("prs[1].Number = %d, want 20", prs[1].Number)
	}
	if prs[1].HeadRefName != "feature-b" {
		t.Errorf("prs[1].HeadRefName = %q, want %q", prs[1].HeadRefName, "feature-b")
	}
	if !prs[1].IsDraft {
		t.Error("prs[1].IsDraft = false, want true")
	}

	// Verify correct args were passed
	argsStr := strings.Join(mock.Args, " ")
	if !strings.Contains(argsStr, "pr list") {
		t.Errorf("expected 'pr list' in args: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--state open") {
		t.Errorf("expected '--state open' in args: %s", argsStr)
	}
	if !strings.Contains(argsStr, "isDraft") {
		t.Errorf("expected 'isDraft' in JSON fields: %s", argsStr)
	}
	if !strings.Contains(argsStr, "reviewDecision") {
		t.Errorf("expected 'reviewDecision' in JSON fields: %s", argsStr)
	}
	if !strings.Contains(argsStr, "url") {
		t.Errorf("expected 'url' in JSON fields: %s", argsStr)
	}
}

func TestListAllPRInfoEmpty(t *testing.T) {
	mock := &MockGHRunner{
		Result: &CmdResult{
			Stdout: `[]`,
		},
	}

	prs, err := ListAllPRInfo(context.Background(), mock, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(prs) != 0 {
		t.Fatalf("got %d PRs, want 0", len(prs))
	}
}

func TestCreatePRDraft(t *testing.T) {
	mock := &MockGHRunner{
		Result: &CmdResult{
			Stdout: `{"number": 456, "url": "https://github.com/org/repo/pull/456", "headRefName": "feat", "baseRefName": "main"}`,
		},
	}

	_, err := CreatePR(context.Background(), mock, "", "", "main", true, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	argsStr := strings.Join(mock.Args, " ")
	if !strings.Contains(argsStr, "--draft") {
		t.Errorf("expected --draft in args: %s", argsStr)
	}
}
