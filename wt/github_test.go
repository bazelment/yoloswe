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
	mock := NewMockGHRunner()
	// gh api repos/{owner}/{repo}/pulls returns JSON directly
	mock.Results["api repos/{owner}/{repo}/pulls -f title=Test PR -f body=Description -f head=feature -f base=main"] = &CmdResult{
		Stdout: `{"number": 123, "html_url": "https://github.com/org/repo/pull/123", "head": {"ref": "feature"}, "base": {"ref": "main"}, "draft": false}`,
	}

	info, err := CreatePR(context.Background(), mock, "Test PR", "Description", "main", "feature", false, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.Number != 123 {
		t.Errorf("Number = %d, want 123", info.Number)
	}
	if info.URL != "https://github.com/org/repo/pull/123" {
		t.Errorf("URL = %q, want expected URL", info.URL)
	}
	if info.HeadRefName != "feature" {
		t.Errorf("HeadRefName = %q, want %q", info.HeadRefName, "feature")
	}
	if info.BaseRefName != "main" {
		t.Errorf("BaseRefName = %q, want %q", info.BaseRefName, "main")
	}

	// Verify the API call
	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.Calls))
	}
	argsStr := strings.Join(mock.Calls[0], " ")
	if !strings.Contains(argsStr, "api repos/{owner}/{repo}/pulls") {
		t.Errorf("expected api call, got %s", argsStr)
	}
	if !strings.Contains(argsStr, "base=main") {
		t.Errorf("expected base=main in args: %s", argsStr)
	}
	if !strings.Contains(argsStr, "head=feature") {
		t.Errorf("expected head=feature in args: %s", argsStr)
	}
}

func TestListOpenPRs(t *testing.T) {
	mock := &MockGHRunner{
		Result: &CmdResult{
			Stdout: `[
				{"number": 10, "headRefName": "feature-a", "baseRefName": "main", "state": "OPEN", "isDraft": false, "reviewDecision": "APPROVED", "url": "https://github.com/org/repo/pull/10"},
				{"number": 20, "headRefName": "feature-b", "baseRefName": "main", "state": "OPEN", "isDraft": true, "reviewDecision": "", "url": "https://github.com/org/repo/pull/20"}
			]`,
		},
	}

	prs, err := ListOpenPRs(context.Background(), mock, "/tmp")
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
	if !strings.Contains(argsStr, "--limit 1000") {
		t.Errorf("expected '--limit 1000' in args: %s", argsStr)
	}
}

func TestListOpenPRsEmpty(t *testing.T) {
	mock := &MockGHRunner{
		Result: &CmdResult{
			Stdout: `[]`,
		},
	}

	prs, err := ListOpenPRs(context.Background(), mock, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(prs) != 0 {
		t.Fatalf("got %d PRs, want 0", len(prs))
	}
}

func TestCreatePRDraft(t *testing.T) {
	mock := NewMockGHRunner()
	mock.Results["api repos/{owner}/{repo}/pulls -f title= -f body= -f head=feat -f base=main -F draft=true"] = &CmdResult{
		Stdout: `{"number": 456, "html_url": "https://github.com/org/repo/pull/456", "head": {"ref": "feat"}, "base": {"ref": "main"}, "draft": true}`,
	}

	info, err := CreatePR(context.Background(), mock, "", "", "main", "feat", true, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.Number != 456 {
		t.Errorf("Number = %d, want 456", info.Number)
	}
	if !info.IsDraft {
		t.Error("expected IsDraft = true")
	}

	argsStr := strings.Join(mock.Calls[0], " ")
	if !strings.Contains(argsStr, "draft=true") {
		t.Errorf("expected draft=true in args: %s", argsStr)
	}
}
