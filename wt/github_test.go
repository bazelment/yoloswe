package wt

import (
	"context"
	"strings"
	"testing"
)

// MockGHRunner implements GHRunner for testing.
type MockGHRunner struct {
	Result  *CmdResult        // Default result (for backward compatibility)
	Err     error             // Default error (for backward compatibility)
	Args    []string          // Last args (for backward compatibility)
	Calls   [][]string        // All calls made
	Results map[string]*CmdResult
	Errors  map[string]error
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
