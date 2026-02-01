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
	Results map[string]*CmdResult
	Errors  map[string]error
	Calls   [][]string
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
	path, err := m.New(ctx, "feature", "main", "")
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
	_, err := m.New(ctx, "feature", "main", "")
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

	_, err := m.New(ctx, "feature", "main", "")
	if err != ErrRepoNotInitialized {
		t.Errorf("New() error = %v, want ErrRepoNotInitialized", err)
	}

	_, err = m.Open(ctx, "feature", "")
	if err != ErrRepoNotInitialized {
		t.Errorf("Open() error = %v, want ErrRepoNotInitialized", err)
	}
}

// TestGetParentBranch tests parsing parent from branch description.
func TestGetParentBranch(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a worktree directory with .git
	wtPath := filepath.Join(repoDir, "feature-b")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, ".git"), []byte("gitdir: "+bareDir), 0644); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["config branch.feature-b.description"] = &CmdResult{
		Stdout: "parent:feature-a\n",
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	ctx := context.Background()
	parent, err := m.GetParentBranch(ctx, "feature-b", wtPath)
	if err != nil {
		t.Fatalf("GetParentBranch() error = %v", err)
	}
	if parent != "feature-a" {
		t.Errorf("GetParentBranch() = %q, want %q", parent, "feature-a")
	}
}

// TestBuildDependencyOrder tests topological sorting of worktrees.
func TestBuildDependencyOrder(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create worktree directories
	for _, branch := range []string{"main", "feature-a", "feature-b", "feature-c"} {
		wtPath := filepath.Join(repoDir, branch)
		if err := os.MkdirAll(wtPath, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wtPath, ".git"), []byte("gitdir: "+bareDir), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mockGit := NewMockGitRunner()
	// main has no parent
	mockGit.Errors["config branch.main.description"] = os.ErrNotExist
	// feature-a depends on main (no description means default parent)
	mockGit.Errors["config branch.feature-a.description"] = os.ErrNotExist
	// feature-b depends on feature-a
	mockGit.Results["config branch.feature-b.description"] = &CmdResult{Stdout: "parent:feature-a\n"}
	// feature-c depends on feature-b
	mockGit.Results["config branch.feature-c.description"] = &CmdResult{Stdout: "parent:feature-b\n"}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	worktrees := []Worktree{
		{Path: filepath.Join(repoDir, "feature-c"), Branch: "feature-c"},
		{Path: filepath.Join(repoDir, "feature-b"), Branch: "feature-b"},
		{Path: filepath.Join(repoDir, "main"), Branch: "main"},
		{Path: filepath.Join(repoDir, "feature-a"), Branch: "feature-a"},
	}

	ctx := context.Background()
	ordered := m.buildDependencyOrder(ctx, worktrees)

	// Verify ordering: parents should come before children
	// main and feature-a can be in any order (both have no tracked parent)
	// feature-b must come after feature-a
	// feature-c must come after feature-b
	indexMap := make(map[string]int)
	for i, wt := range ordered {
		indexMap[wt.Branch] = i
	}

	if indexMap["feature-b"] < indexMap["feature-a"] {
		t.Errorf("feature-b (idx=%d) should come after feature-a (idx=%d)", indexMap["feature-b"], indexMap["feature-a"])
	}
	if indexMap["feature-c"] < indexMap["feature-b"] {
		t.Errorf("feature-c (idx=%d) should come after feature-b (idx=%d)", indexMap["feature-c"], indexMap["feature-b"])
	}
}

// TestIsParentBranchMerged tests detection of merged parent branches.
func TestIsParentBranchMerged(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		parentBranch   string
		prMerged       bool
		branchExists   bool
		expectedMerged bool
	}{
		{
			name:           "PR is merged",
			parentBranch:   "feature-a",
			prMerged:       true,
			branchExists:   false,
			expectedMerged: true,
		},
		{
			name:           "branch deleted (merged without PR)",
			parentBranch:   "feature-a",
			prMerged:       false,
			branchExists:   false,
			expectedMerged: true,
		},
		{
			name:           "branch still exists",
			parentBranch:   "feature-a",
			prMerged:       false,
			branchExists:   true,
			expectedMerged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockGit := NewMockGitRunner()
			mockGH := NewMockGHRunner()

			if tt.prMerged {
				mockGH.Results["pr view feature-a --json number,url,headRefName,baseRefName,state,reviewDecision"] = &CmdResult{
					Stdout: `{"number":1,"state":"MERGED"}`,
				}
			} else {
				mockGH.Errors["pr view feature-a --json number,url,headRefName,baseRefName,state,reviewDecision"] = os.ErrNotExist
			}

			if tt.branchExists {
				mockGit.Results["ls-remote --heads origin feature-a"] = &CmdResult{
					Stdout: "abc123\trefs/heads/feature-a\n",
				}
			} else {
				mockGit.Results["ls-remote --heads origin feature-a"] = &CmdResult{Stdout: ""}
			}

			output := NewOutput(&bytes.Buffer{}, false)
			m := NewManager(tmpDir, "test-repo",
				WithGitRunner(mockGit),
				WithGHRunner(mockGH),
				WithOutput(output))

			ctx := context.Background()
			merged := m.isParentBranchMerged(ctx, tt.parentBranch, repoDir)

			if merged != tt.expectedMerged {
				t.Errorf("isParentBranchMerged() = %v, want %v", merged, tt.expectedMerged)
			}
		})
	}
}

// TestFindChildBranches tests finding branches that depend on a parent.
func TestFindChildBranches(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create worktree for feature-b
	wtPath := filepath.Join(repoDir, "feature-b")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: `worktree ` + bareDir + `
bare

worktree ` + wtPath + `
HEAD abc1234567890
branch refs/heads/feature-b

`,
	}

	mockGH := NewMockGHRunner()
	mockGH.Results["pr list --json number,headRefName,baseRefName,state --state open"] = &CmdResult{
		Stdout: `[
			{"number":2,"headRefName":"feature-b","baseRefName":"feature-a","state":"OPEN"},
			{"number":3,"headRefName":"feature-c","baseRefName":"feature-a","state":"OPEN"},
			{"number":4,"headRefName":"other","baseRefName":"main","state":"OPEN"}
		]`,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo",
		WithGitRunner(mockGit),
		WithGHRunner(mockGH),
		WithOutput(output))

	ctx := context.Background()
	children, err := m.findChildBranches(ctx, "feature-a", repoDir)
	if err != nil {
		t.Fatalf("findChildBranches() error = %v", err)
	}

	if len(children) != 2 {
		t.Fatalf("findChildBranches() returned %d children, want 2", len(children))
	}

	// Check feature-b has worktree
	var foundB, foundC bool
	for _, child := range children {
		if child.Branch == "feature-b" {
			foundB = true
			if !child.HasWorktree {
				t.Error("feature-b should have HasWorktree=true")
			}
			if child.PRNumber != 2 {
				t.Errorf("feature-b PRNumber = %d, want 2", child.PRNumber)
			}
		}
		if child.Branch == "feature-c" {
			foundC = true
			if child.HasWorktree {
				t.Error("feature-c should have HasWorktree=false")
			}
		}
	}

	if !foundB {
		t.Error("feature-b not found in children")
	}
	if !foundC {
		t.Error("feature-c not found in children")
	}
}

// TestNewTracksParentBranch tests that New() sets branch description for cascading branches.
func TestNewTracksParentBranch(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["fetch origin"] = &CmdResult{Stdout: ""}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	featurePath := filepath.Join(repoDir, "feature-b")
	mockGit.Results["worktree add -b feature-b "+featurePath+" origin/feature-a"] = &CmdResult{}
	mockGit.Results["config branch.feature-b.description parent:feature-a"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	ctx := context.Background()
	_, err := m.New(ctx, "feature-b", "feature-a", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Verify branch description was set
	descSet := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "config" && call[1] == "branch.feature-b.description" && call[2] == "parent:feature-a" {
			descSet = true
			break
		}
	}
	if !descSet {
		t.Error("Expected branch description to be set for cascading branch")
	}
}

// TestNewTracksDefaultBranch tests that New() always sets parent tracking, including for default branch.
func TestNewTracksDefaultBranch(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["fetch origin"] = &CmdResult{Stdout: ""}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	featurePath := filepath.Join(repoDir, "feature")
	mockGit.Results["worktree add -b feature "+featurePath+" origin/main"] = &CmdResult{}
	mockGit.Results["config branch.feature.description parent:main"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	ctx := context.Background()
	_, err := m.New(ctx, "feature", "main", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Verify branch description was set to track main
	descSet := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "config" && call[1] == "branch.feature.description" && call[2] == "parent:main" {
			descSet = true
			break
		}
	}
	if !descSet {
		t.Error("Expected branch description to be set to track main as parent")
	}
}
