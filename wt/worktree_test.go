package wt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		// Return the matching Result (if any) alongside the error so callers
		// can inspect Stderr.  Fall back to a bare CmdResult.
		if result, ok := m.Results[key]; ok {
			return result, err
		}
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
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

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
	mockGit.Errors["fetch origin main"] = os.ErrPermission

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

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
	mockGH.Results["pr list --json number,headRefName,baseRefName,state,isDraft,reviewDecision,url --state open --limit 1000"] = &CmdResult{
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
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

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
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

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

func TestManagerRemoveContinuesOnWorktreeDeleteCommandFailure(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := `
on_worktree_delete:
  - false
`
	if err := os.WriteFile(filepath.Join(wtPath, ".wt.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	err := m.Remove(context.Background(), "feature", false, false)
	if err != nil {
		t.Fatalf("Remove() should continue when delete command fails, got error: %v", err)
	}

	removeFound := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "worktree" && call[1] == "remove" && call[2] == wtPath {
			removeFound = true
			break
		}
	}
	if !removeFound {
		t.Fatalf("Expected worktree remove call even when delete command fails, got calls: %v", mockGit.Calls)
	}
}

func TestManagerRemoveRunsWorktreeDeleteCommands(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := `
on_worktree_delete:
  - true
`
	if err := os.WriteFile(filepath.Join(wtPath, ".wt.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	if err := m.Remove(context.Background(), "feature", false, false); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	removeFound := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "worktree" && call[1] == "remove" && call[2] == wtPath {
			removeFound = true
			break
		}
	}
	if !removeFound {
		t.Fatalf("Expected worktree remove call, got calls: %v", mockGit.Calls)
	}
}

func TestSyncDefaultBranch(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	mainPath := filepath.Join(repoDir, "main")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mainPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --show-current"] = &CmdResult{Stdout: "main\n"}
	mockGit.Results["status --porcelain"] = &CmdResult{Stdout: ""}
	mockGit.Results["merge --ff-only origin/main"] = &CmdResult{}

	var buf bytes.Buffer
	output := NewOutput(&buf, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	m.SyncDefaultBranch(context.Background())

	ffCalled := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "merge" && call[1] == "--ff-only" && call[2] == "origin/main" {
			ffCalled = true
			break
		}
	}
	if !ffCalled {
		t.Errorf("Expected merge --ff-only origin/main call, got calls: %v", mockGit.Calls)
	}
}

func TestSyncDefaultBranchSkipsWithUncommittedChanges(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	mainPath := filepath.Join(repoDir, "main")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mainPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --show-current"] = &CmdResult{Stdout: "main\n"}
	mockGit.Results["status --porcelain"] = &CmdResult{Stdout: " M dirty-file.go\n"}

	var buf bytes.Buffer
	output := NewOutput(&buf, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	m.SyncDefaultBranch(context.Background())

	// Should NOT call merge
	for _, call := range mockGit.Calls {
		if len(call) >= 2 && call[0] == "merge" {
			t.Error("Should not call merge when worktree has uncommitted changes")
		}
	}

	if !strings.Contains(buf.String(), "uncommitted changes") {
		t.Error("Expected warning about uncommitted changes")
	}
}

func TestSyncDefaultBranchNoMainWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	// No main worktree directory exists

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}

	var buf bytes.Buffer
	output := NewOutput(&buf, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	m.SyncDefaultBranch(context.Background())

	// Should not call any branch or merge commands
	for _, call := range mockGit.Calls {
		if len(call) >= 1 && (call[0] == "merge" || call[0] == "branch") {
			t.Errorf("Should not call %s when main worktree doesn't exist", call[0])
		}
	}
}

func TestRemoveIncludesWorktreePrune(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree remove "+wtPath] = &CmdResult{}
	mockGit.Results["worktree prune"] = &CmdResult{}
	mockGit.Results["branch -D feature"] = &CmdResult{}
	mockGit.Results["push origin --delete feature"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	err := m.Remove(context.Background(), "feature", true, false)
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	// Verify worktree prune was called before branch -D
	pruneIdx := -1
	branchDeleteIdx := -1
	for i, call := range mockGit.Calls {
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "prune" {
			pruneIdx = i
		}
		if len(call) >= 2 && call[0] == "branch" && call[1] == "-D" {
			branchDeleteIdx = i
		}
	}
	if pruneIdx == -1 {
		t.Fatal("Expected worktree prune call")
	}
	if branchDeleteIdx == -1 {
		t.Fatal("Expected branch -D call")
	}
	if pruneIdx >= branchDeleteIdx {
		t.Errorf("worktree prune (idx=%d) should be called before branch -D (idx=%d)", pruneIdx, branchDeleteIdx)
	}
}

// newMockGHRunnerWithPRError returns a MockGHRunner configured to return an error
// for all PR-related commands, so Sync() tests don't panic on nil JSON unmarshal.
// auth status succeeds so CheckGitHubAuth passes; everything else returns an error.
func newMockGHRunnerWithPRError() *MockGHRunner {
	gh := NewMockGHRunner()
	gh.Results["auth status"] = &CmdResult{Stdout: "Logged in", ExitCode: 0}
	gh.Err = os.ErrNotExist // default for all other calls (e.g. pr view)
	return gh
}

// TestSyncFetchesOnlyDefaultBranch verifies that Sync() without FetchAll fetches
// only the default branch (not all remotes).
func TestSyncFetchesOnlyDefaultBranch(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	mainPath := filepath.Join(repoDir, "main")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + mainPath + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	// fetch origin main should succeed
	mockGit.Results["fetch origin main"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(newMockGHRunnerWithPRError()), WithOutput(output))

	ctx := context.Background()
	// Sync may fail later (e.g., no commits to rebase), but what matters here is fetch behavior.
	_ = m.Sync(ctx, "")

	// Verify fetch origin main was called, but NOT fetch --all --prune
	fetchMainCalled := false
	fetchAllCalled := false
	for _, call := range mockGit.Calls {
		key := strings.Join(call, " ")
		if key == "fetch origin main" {
			fetchMainCalled = true
		}
		if key == "fetch --all --prune" {
			fetchAllCalled = true
		}
	}
	if !fetchMainCalled {
		t.Error("Expected 'fetch origin main' to be called in narrow fetch mode")
	}
	if fetchAllCalled {
		t.Error("Expected 'fetch --all --prune' NOT to be called in narrow fetch mode")
	}
}

// TestSyncFetchAllFlagUsesWideScope verifies that Sync() with FetchAll uses fetch --all --prune.
func TestSyncFetchAllFlagUsesWideScope(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	mainPath := filepath.Join(repoDir, "main")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + mainPath + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	mockGit.Results["fetch --all --prune"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(newMockGHRunnerWithPRError()), WithOutput(output))

	ctx := context.Background()
	_ = m.Sync(ctx, "", SyncOptions{FetchAll: true})

	fetchAllCalled := false
	for _, call := range mockGit.Calls {
		if strings.Join(call, " ") == "fetch --all --prune" {
			fetchAllCalled = true
		}
	}
	if !fetchAllCalled {
		t.Error("Expected 'fetch --all --prune' to be called when FetchAll=true")
	}
}

// TestSyncFetchesParentBranchForStackedWorktrees verifies that Sync() fetches
// non-default parent branches for stacked worktrees.
func TestSyncFetchesParentBranchForStackedWorktrees(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	mainPath := filepath.Join(repoDir, "main")
	featureAPath := filepath.Join(repoDir, "feature-a")
	featureBPath := filepath.Join(repoDir, "feature-b")

	for _, dir := range []string{bareDir, mainPath, featureAPath, featureBPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + mainPath + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n" +
			"worktree " + featureAPath + "\nHEAD bcd2345678901\nbranch refs/heads/feature-a\n\n" +
			"worktree " + featureBPath + "\nHEAD cde3456789012\nbranch refs/heads/feature-b\n\n",
	}
	// feature-b tracks feature-a as parent
	mockGit.Results["config branch.feature-b.description"] = &CmdResult{Stdout: "parent:feature-a\n"}
	mockGit.Results["fetch origin main"] = &CmdResult{}
	mockGit.Results["fetch origin feature-a"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(newMockGHRunnerWithPRError()), WithOutput(output))

	ctx := context.Background()
	_ = m.Sync(ctx, "")

	fetchFeatureACalled := false
	for _, call := range mockGit.Calls {
		if strings.Join(call, " ") == "fetch origin feature-a" {
			fetchFeatureACalled = true
		}
	}
	if !fetchFeatureACalled {
		t.Error("Expected 'fetch origin feature-a' to be called for stacked parent branch")
	}
}

// TestSyncParentFetchFailureFatal verifies that Sync() returns an error when
// fetching a parent branch fails but the branch still exists on remote.
func TestSyncParentFetchFailureFatal(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	mainPath := filepath.Join(repoDir, "main")
	featureBPath := filepath.Join(repoDir, "feature-b")

	for _, dir := range []string{bareDir, mainPath, featureBPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + mainPath + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n" +
			"worktree " + featureBPath + "\nHEAD cde3456789012\nbranch refs/heads/feature-b\n\n",
	}
	mockGit.Results["config branch.feature-b.description"] = &CmdResult{Stdout: "parent:feature-a\n"}
	mockGit.Results["fetch origin main"] = &CmdResult{}
	// fetch for parent branch fails (simulating network/auth error)
	mockGit.Errors["fetch origin feature-a"] = os.ErrPermission
	// ls-remote says branch still exists on remote
	mockGit.Results["ls-remote --heads origin feature-a"] = &CmdResult{Stdout: "abc123 refs/heads/feature-a\n"}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(newMockGHRunnerWithPRError()), WithOutput(output))

	ctx := context.Background()
	err := m.Sync(ctx, "")
	if err == nil {
		t.Fatal("Expected Sync() to return error when parent branch fetch fails and branch still exists on remote")
	}
	if !strings.Contains(err.Error(), "failed to fetch parent branch") {
		t.Errorf("Error = %q, want to contain 'failed to fetch parent branch'", err.Error())
	}
}

// TestSyncParentFetchFailureNonFatalWhenDeleted verifies that Sync() does not
// return an error when a parent branch no longer exists on remote (merged/deleted).
func TestSyncParentFetchFailureNonFatalWhenDeleted(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	mainPath := filepath.Join(repoDir, "main")
	featureBPath := filepath.Join(repoDir, "feature-b")

	for _, dir := range []string{bareDir, mainPath, featureBPath} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + mainPath + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n" +
			"worktree " + featureBPath + "\nHEAD cde3456789012\nbranch refs/heads/feature-b\n\n",
	}
	mockGit.Results["config branch.feature-b.description"] = &CmdResult{Stdout: "parent:feature-a\n"}
	mockGit.Results["fetch origin main"] = &CmdResult{}
	// fetch for parent branch fails
	mockGit.Errors["fetch origin feature-a"] = os.ErrPermission
	// ls-remote returns empty: branch no longer exists on remote
	mockGit.Results["ls-remote --heads origin feature-a"] = &CmdResult{Stdout: ""}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(newMockGHRunnerWithPRError()), WithOutput(output))

	ctx := context.Background()
	// Sync should not return an error for the fetch; it may fail later for other reasons.
	// We verify by checking the error does NOT mention the parent fetch.
	err := m.Sync(ctx, "")
	if err != nil && strings.Contains(err.Error(), "failed to fetch parent branch") {
		t.Errorf("Sync() should not return parent fetch error when branch is gone from remote, got: %v", err)
	}
}

func TestManagerNewGitErrorIncludesStderr(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	wtPath := filepath.Join(repoDir, "feature")
	mockGit := NewMockGitRunner()
	mockGit.Results["fetch origin"] = &CmdResult{Stdout: ""}
	// Set both a Result (with Stderr) and an Error for the worktree add command.
	addKey := "worktree add -b feature " + wtPath + " origin/main"
	mockGit.Results[addKey] = &CmdResult{ExitCode: 128, Stderr: "fatal: 'feature' is already checked out at '/other/path'\n"}
	mockGit.Errors[addKey] = os.ErrPermission

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	_, err := m.New(context.Background(), "feature", "main", "")
	if err == nil {
		t.Fatal("Expected error from New()")
	}
	if !strings.Contains(err.Error(), "already checked out") {
		t.Errorf("Error should contain git stderr, got: %q", err.Error())
	}
	// Original error must remain in chain so callers can use errors.Is/As.
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("Error chain should wrap original error, got: %q", err.Error())
	}
}

func TestManagerNewPrunesBeforeCreate(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["fetch origin"] = &CmdResult{Stdout: ""}
	mockGit.Results["worktree add -b feature "+filepath.Join(repoDir, "feature")+" origin/main"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	_, err := m.New(context.Background(), "feature", "main", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Verify worktree prune was called before worktree add.
	pruneIdx := -1
	addIdx := -1
	for i, call := range mockGit.Calls {
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "prune" {
			pruneIdx = i
		}
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "add" {
			addIdx = i
		}
	}
	if pruneIdx == -1 {
		t.Fatal("Expected worktree prune to be called")
	}
	if addIdx == -1 {
		t.Fatal("Expected worktree add to be called")
	}
	if pruneIdx >= addIdx {
		t.Errorf("worktree prune (call %d) should come before worktree add (call %d)", pruneIdx, addIdx)
	}
}

func TestManagerNewReusesExistingWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create worktree directory so it "already exists"
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	// branch --show-current returns matching branch
	mockGit.Results["branch --show-current"] = &CmdResult{Stdout: "feature\n"}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	path, err := m.New(context.Background(), "feature", "main", "")
	if err != nil {
		t.Fatalf("New() should reuse existing worktree, got error: %v", err)
	}
	if path != wtPath {
		t.Errorf("New() path = %q, want %q", path, wtPath)
	}
}

func TestManagerNewReusesExistingWorktreeSetsGoal(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	mockGit.Results["branch --show-current"] = &CmdResult{Stdout: "feature\n"}
	goalKey := "config branch.feature.goal my goal"
	mockGit.Results[goalKey] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	_, err := m.New(context.Background(), "feature", "main", "my goal")
	if err != nil {
		t.Fatalf("New() should reuse existing worktree, got error: %v", err)
	}

	// Verify goal was set on reuse.
	found := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "config" && call[1] == "branch.feature.goal" {
			found = true
			if call[2] != "my goal" {
				t.Errorf("goal = %q, want %q", call[2], "my goal")
			}
		}
	}
	if !found {
		t.Error("Expected git config branch.feature.goal to be called on worktree reuse")
	}
}

func TestManagerNewRejectsExistingWorktreeWrongBranch(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	// branch --show-current returns a different branch
	mockGit.Results["branch --show-current"] = &CmdResult{Stdout: "other-branch\n"}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	_, err := m.New(context.Background(), "feature", "main", "")
	if err != ErrWorktreeExists {
		t.Errorf("New() error = %v, want ErrWorktreeExists", err)
	}
}

func TestGCPrunesWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + filepath.Join(repoDir, "main") + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	mockGit.Results["gc"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if !result.FetchPruned {
		t.Error("expected FetchPruned to be true")
	}
	if !result.GCRan {
		t.Error("expected GCRan to be true")
	}
	if len(result.OrphanedBranches) != 0 {
		t.Errorf("expected no orphaned branches, got %v", result.OrphanedBranches)
	}
}

func TestGCDetectsOrphanedBranches(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nfeature-a\nfeature-b\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + filepath.Join(repoDir, "main") + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	mockGit.Results["gc"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if len(result.OrphanedBranches) != 2 {
		t.Fatalf("expected 2 orphaned branches, got %v", result.OrphanedBranches)
	}
	if result.OrphanedBranches[0] != "feature-a" || result.OrphanedBranches[1] != "feature-b" {
		t.Errorf("unexpected orphaned branches: %v", result.OrphanedBranches)
	}
	if len(result.DeletedBranches) != 0 {
		t.Error("expected no branches deleted without DeleteBranches option")
	}
}

func TestGCDeletesOrphanedBranches(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nfeature-a\nfeature-b\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + filepath.Join(repoDir, "main") + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	mockGit.Results["branch -D feature-a"] = &CmdResult{}
	mockGit.Results["branch -D feature-b"] = &CmdResult{}
	mockGit.Results["gc"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{DeleteBranches: true})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if len(result.DeletedBranches) != 2 {
		t.Fatalf("expected 2 deleted branches, got %v", result.DeletedBranches)
	}
}

func TestGCDeletesRemoteBranches(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nfeature-a\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + filepath.Join(repoDir, "main") + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	mockGit.Results["branch -D feature-a"] = &CmdResult{}
	mockGit.Results["push origin --delete feature-a"] = &CmdResult{}
	mockGit.Results["gc"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{DeleteBranches: true, DeleteRemote: true})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if len(result.DeletedRemote) != 1 || result.DeletedRemote[0] != "feature-a" {
		t.Errorf("expected deleted remote [feature-a], got %v", result.DeletedRemote)
	}
}

func TestGCProtectsDefaultBranch(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	// main and master have no worktrees but should still be protected
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nmaster\nfeature-a\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n",
	}
	mockGit.Results["branch -D feature-a"] = &CmdResult{}
	mockGit.Results["gc"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{DeleteBranches: true})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if len(result.OrphanedBranches) != 1 || result.OrphanedBranches[0] != "feature-a" {
		t.Errorf("expected only feature-a as orphan, got %v", result.OrphanedBranches)
	}
}

func TestGCDryRun(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune --dry-run -v"] = &CmdResult{Stdout: "Removing worktrees/stale\n"}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nfeature-a\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + filepath.Join(repoDir, "main") + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{DryRun: true, DeleteBranches: true, DeleteRemote: true})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}

	// Verify no destructive calls were made
	for _, call := range mockGit.Calls {
		key := strings.Join(call, " ")
		if key == "fetch --prune" || key == "gc" || strings.HasPrefix(key, "branch -D") || strings.HasPrefix(key, "push origin --delete") {
			t.Errorf("dry-run should not call %q", key)
		}
	}
	if result.FetchPruned || result.GCRan {
		t.Error("dry-run should not set FetchPruned or GCRan")
	}
	if len(result.DeletedBranches) != 0 || len(result.DeletedRemote) != 0 {
		t.Error("dry-run should not delete any branches")
	}
	if len(result.PrunedWorktrees) != 1 {
		t.Errorf("expected 1 pruned worktree line, got %v", result.PrunedWorktrees)
	}
}

func TestGCRepoNotInitialized(t *testing.T) {
	tmpDir := t.TempDir()

	mockGit := NewMockGitRunner()
	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	_, err := m.GC(context.Background(), GCOptions{})
	if !errors.Is(err, ErrRepoNotInitialized) {
		t.Errorf("GC() error = %v, want ErrRepoNotInitialized", err)
	}
}

func TestGCContinuesOnPartialFailure(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nfeature-a\nfeature-b\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + filepath.Join(repoDir, "main") + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	// feature-a fails, feature-b succeeds
	mockGit.Errors["branch -D feature-a"] = errors.New("branch not fully merged")
	mockGit.Results["branch -D feature-b"] = &CmdResult{}
	mockGit.Results["gc"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{DeleteBranches: true})
	if err != nil {
		t.Fatalf("GC() should not return error on partial failure, got %v", err)
	}
	if len(result.DeletedBranches) != 1 || result.DeletedBranches[0] != "feature-b" {
		t.Errorf("expected only feature-b deleted, got %v", result.DeletedBranches)
	}
}

func TestGCRemoteOnlyDeletesLocallyDeletedBranches(t *testing.T) {
	// Regression test: remote deletion must only target branches whose local
	// deletion succeeded. Previously it iterated OrphanedBranches, which could
	// cause a remote branch to be deleted even when the local deletion failed.
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nfeature-a\nfeature-b\n"}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\nworktree " + filepath.Join(repoDir, "main") + "\nHEAD abc1234567890\nbranch refs/heads/main\n\n",
	}
	// feature-a local deletion fails; feature-b succeeds
	mockGit.Errors["branch -D feature-a"] = errors.New("branch not fully merged")
	mockGit.Results["branch -D feature-b"] = &CmdResult{}
	mockGit.Results["push origin --delete feature-b"] = &CmdResult{}
	mockGit.Results["gc"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{DeleteBranches: true, DeleteRemote: true})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	// Only feature-b should be remotely deleted; feature-a's local deletion failed
	if len(result.DeletedRemote) != 1 || result.DeletedRemote[0] != "feature-b" {
		t.Errorf("expected only feature-b in DeletedRemote, got %v", result.DeletedRemote)
	}
	// Verify push for feature-a was never called
	for _, call := range mockGit.Calls {
		key := strings.Join(call, " ")
		if key == "push origin --delete feature-a" {
			t.Error("should not have attempted to delete remote feature-a since local deletion failed")
		}
	}
}

func TestPruneMergedPRs_RemovesWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	featurePath := filepath.Join(repoDir, "feature-voice")
	os.MkdirAll(bareDir, 0755)
	os.MkdirAll(featurePath, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + filepath.Join(repoDir, "main") + "\nHEAD abc123\nbranch refs/heads/main\n\n" +
			"worktree " + featurePath + "\nHEAD def456\nbranch refs/heads/feature/voice\n\n",
	}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	// Remove calls
	mockGit.Results["branch --show-current"] = &CmdResult{Stdout: "feature/voice\n"}
	mockGit.Results["worktree remove --force "+featurePath] = &CmdResult{}
	mockGit.Results["worktree prune"] = &CmdResult{}
	mockGit.Results["branch -D feature/voice"] = &CmdResult{}
	mockGit.Results["push origin --delete feature/voice"] = &CmdResult{}

	mockGH := NewMockGHRunner()
	mockGH.Results["pr list --json number,headRefName,baseRefName,state,url --state merged --limit 200"] = &CmdResult{
		Stdout: `[{"number":42,"headRefName":"feature/voice","baseRefName":"main","state":"MERGED","url":"https://github.com/org/repo/pull/42"}]`,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(mockGH), WithOutput(output))

	result, err := m.Prune(context.Background(), PruneOptions{MergedPRs: true})
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if len(result.MergedWorktrees) != 1 || result.MergedWorktrees[0] != "feature/voice" {
		t.Errorf("expected [feature/voice] in MergedWorktrees, got %v", result.MergedWorktrees)
	}
}

func TestPruneMergedPRs_SkipsProtected(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + filepath.Join(repoDir, "main") + "\nHEAD abc123\nbranch refs/heads/main\n\n",
	}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}

	mockGH := NewMockGHRunner()
	mockGH.Results["pr list --json number,headRefName,baseRefName,state,url --state merged --limit 200"] = &CmdResult{
		Stdout: `[{"number":1,"headRefName":"main","state":"MERGED"}]`,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(mockGH), WithOutput(output))

	result, err := m.Prune(context.Background(), PruneOptions{MergedPRs: true})
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if len(result.MergedWorktrees) != 0 {
		t.Errorf("expected no merged worktrees (main is protected), got %v", result.MergedWorktrees)
	}
}

func TestPruneMergedPRs_SkipsNoPR(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)
	os.MkdirAll(filepath.Join(repoDir, "feature-x"), 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + filepath.Join(repoDir, "main") + "\nHEAD abc123\nbranch refs/heads/main\n\n" +
			"worktree " + filepath.Join(repoDir, "feature-x") + "\nHEAD def456\nbranch refs/heads/feature-x\n\n",
	}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}

	mockGH := NewMockGHRunner()
	// No merged PRs matching feature-x
	mockGH.Results["pr list --json number,headRefName,baseRefName,state,url --state merged --limit 200"] = &CmdResult{
		Stdout: `[]`,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(mockGH), WithOutput(output))

	result, err := m.Prune(context.Background(), PruneOptions{MergedPRs: true})
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if len(result.MergedWorktrees) != 0 {
		t.Errorf("expected no merged worktrees, got %v", result.MergedWorktrees)
	}
}

func TestPruneMergedPRs_DryRun(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)
	os.MkdirAll(filepath.Join(repoDir, "feature-voice"), 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune --dry-run -v"] = &CmdResult{Stdout: ""}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + filepath.Join(repoDir, "main") + "\nHEAD abc123\nbranch refs/heads/main\n\n" +
			"worktree " + filepath.Join(repoDir, "feature-voice") + "\nHEAD def456\nbranch refs/heads/feature/voice\n\n",
	}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}

	mockGH := NewMockGHRunner()
	mockGH.Results["pr list --json number,headRefName,baseRefName,state,url --state merged --limit 200"] = &CmdResult{
		Stdout: `[{"number":42,"headRefName":"feature/voice","baseRefName":"main","state":"MERGED","url":"https://github.com/org/repo/pull/42"}]`,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(mockGH), WithOutput(output))

	result, err := m.Prune(context.Background(), PruneOptions{DryRun: true, MergedPRs: true})
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if len(result.MergedWorktrees) != 1 {
		t.Fatalf("expected 1 merged worktree candidate, got %v", result.MergedWorktrees)
	}
	// Verify Remove was NOT called (no worktree remove in git calls)
	for _, call := range mockGit.Calls {
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "remove" {
			t.Error("Remove should not be called in dry-run mode")
		}
	}
}

func TestPruneMergedPRs_GHFailure(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}

	mockGH := NewMockGHRunner()
	mockGH.Errors["pr list --json number,headRefName,baseRefName,state,url --state merged --limit 200"] = errors.New("auth required")

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(mockGH), WithOutput(output))

	result, err := m.Prune(context.Background(), PruneOptions{MergedPRs: true})
	if err != nil {
		t.Fatalf("Prune() should not fail on GH error, got %v", err)
	}
	// Stale worktree prune still ran
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// Merged worktrees should be nil since GH failed
	if result.MergedWorktrees != nil {
		t.Errorf("expected nil MergedWorktrees on GH failure, got %v", result.MergedWorktrees)
	}
}

func TestPruneWithoutMergedFlag(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	os.MkdirAll(bareDir, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(NewMockGHRunner()), WithOutput(output))

	result, err := m.Prune(context.Background(), PruneOptions{})
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	// Should only do git worktree prune, no GH calls
	if result.MergedWorktrees != nil {
		t.Errorf("expected nil MergedWorktrees without --merged, got %v", result.MergedWorktrees)
	}
}

func TestGCMergedPRsPassthrough(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	featurePath := filepath.Join(repoDir, "feature-done")
	os.MkdirAll(bareDir, 0755)
	os.MkdirAll(featurePath, 0755)

	mockGit := NewMockGitRunner()
	mockGit.Results["worktree prune"] = &CmdResult{Stdout: ""}
	mockGit.Results["worktree list --porcelain"] = &CmdResult{
		Stdout: "worktree " + bareDir + "\nbare\n\n" +
			"worktree " + filepath.Join(repoDir, "main") + "\nHEAD abc123\nbranch refs/heads/main\n\n" +
			"worktree " + featurePath + "\nHEAD def456\nbranch refs/heads/feature/done\n\n",
	}
	mockGit.Results["symbolic-ref refs/remotes/origin/HEAD"] = &CmdResult{Stdout: "refs/remotes/origin/main\n"}
	mockGit.Results["fetch --prune"] = &CmdResult{}
	mockGit.Results["branch --list --format=%(refname:short)"] = &CmdResult{Stdout: "main\nfeature/done\n"}
	mockGit.Results["gc"] = &CmdResult{}
	// Remove calls for feature/done
	mockGit.Results["branch --show-current"] = &CmdResult{Stdout: "feature/done\n"}
	mockGit.Results["worktree remove --force "+featurePath] = &CmdResult{}
	mockGit.Results["branch -D feature/done"] = &CmdResult{}
	mockGit.Results["push origin --delete feature/done"] = &CmdResult{}

	mockGH := NewMockGHRunner()
	mockGH.Results["pr list --json number,headRefName,baseRefName,state,url --state merged --limit 200"] = &CmdResult{
		Stdout: `[{"number":99,"headRefName":"feature/done","baseRefName":"main","state":"MERGED","url":"https://github.com/org/repo/pull/99"}]`,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithGHRunner(mockGH), WithOutput(output))

	result, err := m.GC(context.Background(), GCOptions{MergedPRs: true})
	if err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	if len(result.MergedWorktrees) != 1 || result.MergedWorktrees[0] != "feature/done" {
		t.Errorf("expected [feature/done] in MergedWorktrees, got %v", result.MergedWorktrees)
	}
}

func TestRemoveForce(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	if err := m.Remove(context.Background(), "feature", false, true); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	forceFound := false
	for _, call := range mockGit.Calls {
		if len(call) >= 4 && call[0] == "worktree" && call[1] == "remove" && call[2] == "--force" && call[3] == wtPath {
			forceFound = true
			break
		}
	}
	if !forceFound {
		t.Fatalf("Expected 'worktree remove --force' call, got calls: %v", mockGit.Calls)
	}
}

func TestRemoveNoForce(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	if err := m.Remove(context.Background(), "feature", false, false); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	for _, call := range mockGit.Calls {
		if call[0] == "worktree" && call[1] == "remove" {
			for _, arg := range call[2:] {
				if arg == "--force" {
					t.Fatalf("Expected no '--force' flag, got calls: %v", mockGit.Calls)
				}
			}
		}
	}
}

func TestRemoveIncludesStderr(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	wtPath := filepath.Join(repoDir, "feature")

	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	removeKey := "worktree remove " + wtPath
	injectedErr := fmt.Errorf("exit status 128")
	mockGit.Errors[removeKey] = injectedErr
	mockGit.Results[removeKey] = &CmdResult{
		Stderr:   "fatal: 'feature' contains modified or untracked files, use --force to delete\n",
		ExitCode: 128,
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	err := m.Remove(context.Background(), "feature", false, false)
	if err == nil {
		t.Fatal("Expected error from Remove()")
	}

	if !strings.Contains(err.Error(), "contains modified or untracked files") {
		t.Errorf("Expected stderr in error message, got: %v", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("Expected original error to be wrapped, got: %v", err)
	}
}
