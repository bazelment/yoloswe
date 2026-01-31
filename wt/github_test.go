package wt

import (
	"context"
	"testing"
)

// MockGHRunner implements GHRunner for testing.
type MockGHRunner struct {
	Result *CmdResult
	Err    error
	Args   []string
}

func (m *MockGHRunner) Run(ctx context.Context, args []string, dir string) (*CmdResult, error) {
	m.Args = args
	return m.Result, m.Err
}

func TestGetPRInfo(t *testing.T) {
	tests := []struct {
		name     string
		prNumber int
		wantArg  string
	}{
		{"single digit", 5, "5"},
		{"double digit", 42, "42"},
		{"triple digit", 123, "123"},
		{"large number", 9999, "9999"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockGHRunner{
				Result: &CmdResult{
					Stdout: `{"number": 123, "url": "https://github.com/org/repo/pull/123", "headRefName": "feature"}`,
				},
			}

			_, _ = GetPRInfo(context.Background(), mock, tt.prNumber, "/tmp")

			// Check that the PR number was passed correctly
			if len(mock.Args) < 3 {
				t.Fatalf("expected at least 3 args, got %d", len(mock.Args))
			}
			if mock.Args[2] != tt.wantArg {
				t.Errorf("PR number arg = %q, want %q", mock.Args[2], tt.wantArg)
			}
		})
	}
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
