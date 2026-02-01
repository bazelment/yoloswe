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
