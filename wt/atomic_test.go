package wt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicOpCommit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithOutput(output))

	op := m.NewAtomicOp()

	undoCalled := false
	op.AddUndo(func(ctx context.Context) error {
		undoCalled = true
		return nil
	})

	op.Commit()

	// Rollback after commit should be a no-op
	if err := op.Rollback(context.Background()); err != nil {
		t.Errorf("Rollback after Commit should return nil, got %v", err)
	}
	if undoCalled {
		t.Error("Undo function should not be called after Commit")
	}
}

func TestAtomicOpRollback(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithOutput(output))

	op := m.NewAtomicOp()

	// Track the order of undo operations
	var order []int
	op.AddUndo(func(ctx context.Context) error {
		order = append(order, 1)
		return nil
	})
	op.AddUndo(func(ctx context.Context) error {
		order = append(order, 2)
		return nil
	})
	op.AddUndo(func(ctx context.Context) error {
		order = append(order, 3)
		return nil
	})

	if err := op.Rollback(context.Background()); err != nil {
		t.Errorf("Rollback error = %v", err)
	}

	// Should execute in reverse order
	if len(order) != 3 {
		t.Fatalf("Expected 3 undo operations, got %d", len(order))
	}
	if order[0] != 3 || order[1] != 2 || order[2] != 1 {
		t.Errorf("Undo order = %v, want [3, 2, 1]", order)
	}
}

func TestAtomicOpRollbackReturnsFirstError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithOutput(output))

	op := m.NewAtomicOp()

	errFirst := errors.New("first undo error")
	errSecond := errors.New("second undo error")

	allCalled := 0
	op.AddUndo(func(ctx context.Context) error {
		allCalled++
		return nil
	})
	op.AddUndo(func(ctx context.Context) error {
		allCalled++
		return errSecond
	})
	op.AddUndo(func(ctx context.Context) error {
		allCalled++
		return errFirst
	})

	err := op.Rollback(context.Background())
	// Should return first error encountered (errFirst from step 3, executed first in reverse)
	if !errors.Is(err, errFirst) {
		t.Errorf("Rollback error = %v, want %v", err, errFirst)
	}
	// All undo functions should still be called
	if allCalled != 3 {
		t.Errorf("Expected 3 undo calls, got %d", allCalled)
	}
}

func TestNewAtomicRepoNotInitialized(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "nonexistent", WithOutput(output))

	_, err := m.NewAtomic(context.Background(), "feature", "main", "")
	if err != ErrRepoNotInitialized {
		t.Errorf("NewAtomic() error = %v, want ErrRepoNotInitialized", err)
	}
}

func TestNewAtomicWorktreeExists(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Pre-create the worktree directory
	wtPath := filepath.Join(repoDir, "feature")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithOutput(output))

	_, err := m.NewAtomic(context.Background(), "feature", "main", "")
	if err != ErrWorktreeExists {
		t.Errorf("NewAtomic() error = %v, want ErrWorktreeExists", err)
	}
}

func TestNewAtomicSuccess(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	featurePath := filepath.Join(repoDir, "feature")

	mockGit.Results["fetch origin"] = &CmdResult{}
	mockGit.Results["worktree add -b feature "+featurePath+" origin/main"] = &CmdResult{}
	mockGit.Results["config branch.feature.description parent:main"] = &CmdResult{}
	mockGit.Results["config branch.feature.goal Test goal"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	path, err := m.NewAtomic(context.Background(), "feature", "main", "Test goal")
	if err != nil {
		t.Fatalf("NewAtomic() error = %v", err)
	}
	if path != featurePath {
		t.Errorf("NewAtomic() path = %q, want %q", path, featurePath)
	}

	// Verify all expected commands were called
	commands := make(map[string]bool)
	for _, call := range mockGit.Calls {
		commands[call[0]] = true
	}
	if !commands["fetch"] {
		t.Error("Expected fetch to be called")
	}
	if !commands["worktree"] {
		t.Error("Expected worktree add to be called")
	}
	if !commands["config"] {
		t.Error("Expected config to be called")
	}
}

func TestNewAtomicRollbackOnWorktreeAddFailure(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	featurePath := filepath.Join(repoDir, "feature")

	mockGit.Results["fetch origin"] = &CmdResult{}
	mockGit.Errors["worktree add -b feature "+featurePath+" origin/main"] = errors.New("worktree add failed")

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	_, err := m.NewAtomic(context.Background(), "feature", "main", "")
	if err == nil {
		t.Fatal("Expected error when worktree add fails")
	}

	// No rollback steps should have been registered (failure was at step 2, before any undo)
	// Verify that no cleanup commands were issued (worktree remove, branch -D)
	for _, call := range mockGit.Calls {
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "remove" {
			t.Error("Should not call worktree remove when worktree add itself failed")
		}
	}
}

func TestNewAtomicRollbackOnDescriptionFailure(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	featurePath := filepath.Join(repoDir, "feature")

	mockGit.Results["fetch origin"] = &CmdResult{}
	mockGit.Results["worktree add -b feature "+featurePath+" origin/main"] = &CmdResult{}
	mockGit.Errors["config branch.feature.description parent:main"] = errors.New("config failed")

	// Rollback calls
	mockGit.Results["worktree remove --force "+featurePath] = &CmdResult{}
	mockGit.Results["branch -D feature"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	_, err := m.NewAtomic(context.Background(), "feature", "main", "")
	if err == nil {
		t.Fatal("Expected error when config fails")
	}

	// Verify rollback happened: worktree remove + branch delete
	removeFound := false
	branchDeleteFound := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "worktree" && call[1] == "remove" && call[2] == "--force" {
			removeFound = true
		}
		if len(call) >= 2 && call[0] == "branch" && call[1] == "-D" {
			branchDeleteFound = true
		}
	}
	if !removeFound {
		t.Error("Rollback should call worktree remove")
	}
	if !branchDeleteFound {
		t.Error("Rollback should call branch -D")
	}
}

func TestNewAtomicRollbackOnGoalFailure(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "test-repo")
	bareDir := filepath.Join(repoDir, ".bare")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}

	mockGit := NewMockGitRunner()
	featurePath := filepath.Join(repoDir, "feature")

	mockGit.Results["fetch origin"] = &CmdResult{}
	mockGit.Results["worktree add -b feature "+featurePath+" origin/main"] = &CmdResult{}
	mockGit.Results["config branch.feature.description parent:main"] = &CmdResult{}
	mockGit.Errors["config branch.feature.goal Test goal"] = errors.New("config goal failed")

	// Rollback calls
	mockGit.Results["worktree remove --force "+featurePath] = &CmdResult{}
	mockGit.Results["branch -D feature"] = &CmdResult{}
	mockGit.Results["config --unset branch.feature.description"] = &CmdResult{}

	output := NewOutput(&bytes.Buffer{}, false)
	m := NewManager(tmpDir, "test-repo", WithGitRunner(mockGit), WithOutput(output))

	_, err := m.NewAtomic(context.Background(), "feature", "main", "Test goal")
	if err == nil {
		t.Fatal("Expected error when goal config fails")
	}

	// Verify rollback: should unset description AND remove worktree
	unsetDesc := false
	removeWT := false
	for _, call := range mockGit.Calls {
		if len(call) >= 3 && call[0] == "config" && call[1] == "--unset" && call[2] == "branch.feature.description" {
			unsetDesc = true
		}
		if len(call) >= 3 && call[0] == "worktree" && call[1] == "remove" && call[2] == "--force" {
			removeWT = true
		}
	}
	if !unsetDesc {
		t.Error("Rollback should unset branch description")
	}
	if !removeWT {
		t.Error("Rollback should remove worktree")
	}
}
