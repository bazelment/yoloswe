package wt

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWorktreeList(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected []Worktree
	}{
		{
			name: "single worktree with bare",
			output: `worktree /path/to/.bare
bare

worktree /path/to/main
HEAD abc1234567890
branch refs/heads/main

`,
			expected: []Worktree{
				{Path: "/path/to/main", Branch: "main", Commit: "abc12345", IsDetached: false},
			},
		},
		{
			name: "multiple worktrees",
			output: `worktree /path/to/.bare
bare

worktree /path/to/main
HEAD abc1234567890
branch refs/heads/main

worktree /path/to/feature
HEAD def5678901234
branch refs/heads/feature

`,
			expected: []Worktree{
				{Path: "/path/to/main", Branch: "main", Commit: "abc12345", IsDetached: false},
				{Path: "/path/to/feature", Branch: "feature", Commit: "def56789", IsDetached: false},
			},
		},
		{
			name: "detached head",
			output: `worktree /path/to/.bare
bare

worktree /path/to/pr-123
HEAD abc1234567890
detached

`,
			expected: []Worktree{
				{Path: "/path/to/pr-123", Branch: "(detached)", Commit: "abc12345", IsDetached: true},
			},
		},
		{
			name:     "empty output",
			output:   "",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWorktreeList(tt.output)
			if len(got) != len(tt.expected) {
				t.Fatalf("len(parseWorktreeList()) = %d, want %d", len(got), len(tt.expected))
			}
			for i, w := range got {
				exp := tt.expected[i]
				if w.Path != exp.Path {
					t.Errorf("worktrees[%d].Path = %q, want %q", i, w.Path, exp.Path)
				}
				if w.Branch != exp.Branch {
					t.Errorf("worktrees[%d].Branch = %q, want %q", i, w.Branch, exp.Branch)
				}
				if w.Commit != exp.Commit {
					t.Errorf("worktrees[%d].Commit = %q, want %q", i, w.Commit, exp.Commit)
				}
				if w.IsDetached != exp.IsDetached {
					t.Errorf("worktrees[%d].IsDetached = %v, want %v", i, w.IsDetached, exp.IsDetached)
				}
			}
		})
	}
}

func TestWorktreeName(t *testing.T) {
	wt := Worktree{Path: "/home/user/worktrees/repo/feature-branch"}
	if wt.Name() != "feature-branch" {
		t.Errorf("Name() = %q, want %q", wt.Name(), "feature-branch")
	}
}

// MockGitRunner implements GitRunner for testing Manager.
type MockGitRunner struct {
	Calls   [][]string
	Results map[string]*CmdResult
	Errors  map[string]error
}

func NewMockGitRunner() *MockGitRunner {
	return &MockGitRunner{
		Results: make(map[string]*CmdResult),
		Errors:  make(map[string]error),
	}
}

func (m *MockGitRunner) Run(ctx context.Context, args []string, dir string) (*CmdResult, error) {
	m.Calls = append(m.Calls, args)
	key := strings.Join(args, " ")
	if err, ok := m.Errors[key]; ok {
		return &CmdResult{ExitCode: 1}, err
	}
	if result, ok := m.Results[key]; ok {
		return result, nil
	}
	return &CmdResult{Stdout: "", ExitCode: 0}, nil
}

func TestManagerNew(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	// Create fake bare dir
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["fetch origin"] = &CmdResult{Stdout: ""}
	mockGit.Results["worktree add -b feature "+filepath.Join(repoDir, "feature")+" origin/main"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	ctx := context.Background()
	path, err := m.New(ctx, "feature", "main")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if path != filepath.Join(repoDir, "feature") {
		t.Errorf("New() path = %q, want %q", path, filepath.Join(repoDir, "feature"))
	}

	// Verify fetch was called
	fetchCalled := false
	for _, call := range mockGit.Calls {
		if len(call) >= 2 && call[0] == "fetch" && call[1] == "origin" {
			fetchCalled = true
			break
		}
	}
	if !fetchCalled {
		t.Error("Expected fetch origin to be called")
	}
}

func TestManagerNewFetchError(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Errors["fetch origin"] = os.ErrPermission

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	ctx := context.Background()
	_, err := m.New(ctx, "feature", "main")
	if err == nil {
		t.Fatal("Expected error when fetch fails")
	}
	if !strings.Contains(err.Error(), "failed to fetch") {
		t.Errorf("Error = %q, want to contain 'failed to fetch'", err.Error())
	}
}

func TestManagerList(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: `worktree ` + bareDir + `
bare

worktree ` + filepath.Join(repoDir, "main") + `
HEAD abc1234567890
branch refs/heads/main

`,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	ctx := context.Background()
	worktrees, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(worktrees) != 1 {
		t.Fatalf("List() returned %d worktrees, want 1", len(worktrees))
	}

	if worktrees[0].Branch != "main" {
		t.Errorf("worktrees[0].Branch = %q, want %q", worktrees[0].Branch, "main")
	}
}

func TestManagerRepoNotInitialized(t *testing.T) {
	tmpDir := t.TempDir()

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "nonexistent", WithOutput(output))

	ctx := context.Background()

	_, err := m.New(ctx, "feature", "main")
	if err != ErrRepoNotInitialized {
		t.Errorf("New() error = %v, want ErrRepoNotInitialized", err)
	}

	_, err = m.Open(ctx, "feature")
	if err != ErrRepoNotInitialized {
		t.Errorf("Open() error = %v, want ErrRepoNotInitialized", err)
	}
}
