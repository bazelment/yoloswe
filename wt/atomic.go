package wt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// AtomicOp represents a worktree operation that can be rolled back.
// It accumulates undo steps as sub-operations succeed, and executes them
// in reverse order on failure.
type AtomicOp struct {
	manager   *Manager
	undoSteps []func(ctx context.Context) error
	committed bool
}

// NewAtomicOp creates a new atomic operation context.
func (m *Manager) NewAtomicOp() *AtomicOp {
	return &AtomicOp{manager: m}
}

// AddUndo registers a rollback step. Steps are executed in reverse order on Rollback.
func (op *AtomicOp) AddUndo(fn func(ctx context.Context) error) {
	op.undoSteps = append(op.undoSteps, fn)
}

// Commit marks the operation as successful. Rollback becomes a no-op after this.
func (op *AtomicOp) Commit() {
	op.committed = true
}

// Rollback executes all undo steps in reverse order.
// Returns the first error encountered but continues rolling back all steps.
// If the operation was committed, Rollback is a no-op.
func (op *AtomicOp) Rollback(ctx context.Context) error {
	if op.committed {
		return nil
	}
	var firstErr error
	for i := len(op.undoSteps) - 1; i >= 0; i-- {
		if err := op.undoSteps[i](ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NewAtomic creates a worktree with rollback on failure.
// If any step fails, all previously completed steps are undone, leaving no
// orphaned worktrees or branches.
//
// Note: post_create hook failures are non-fatal. The worktree remains created;
// hooks are side-effectful by nature and may not be safely reversible.
func (m *Manager) NewAtomic(ctx context.Context, branch, baseBranch, goal string, opts ...NewOptions) (string, error) {
	var o NewOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	op := m.NewAtomicOp()
	defer func() {
		if !op.committed {
			op.Rollback(ctx)
		}
	}()

	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return "", ErrRepoNotInitialized
	}

	worktreePath := filepath.Join(m.RepoDir(), branch)
	if _, err := os.Stat(worktreePath); err == nil {
		return "", ErrWorktreeExists
	}

	// Determine base branch (same logic as New)
	if baseBranch == "" {
		entries, _ := os.ReadDir(m.RepoDir())
		for _, entry := range entries {
			if entry.IsDir() {
				wtPath := filepath.Join(m.RepoDir(), entry.Name())
				if _, err := os.Stat(filepath.Join(wtPath, ".git")); err == nil {
					config, err := LoadRepoConfig(wtPath)
					if err != nil {
						// Config load failed, try next worktree
						continue
					}
					baseBranch = config.DefaultBase
					break
				}
			}
		}
		if baseBranch == "" {
			baseBranch, _ = GetDefaultBranch(ctx, m.git, bareDir)
		}
	}

	// Step 1: Fetch (unless caller already fetched)
	if !o.SkipFetch {
		if err := m.FetchOrigin(ctx); err != nil {
			return "", err
		}
	}

	// Step 2: Create worktree + branch
	m.output.Info(fmt.Sprintf("Creating worktree %s from %s...", branch, baseBranch))
	if _, err := m.git.Run(ctx, []string{
		"worktree", "add", "-b", branch, worktreePath, "origin/" + baseBranch,
	}, bareDir); err != nil {
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}
	op.AddUndo(func(ctx context.Context) error {
		m.output.Info(fmt.Sprintf("Rolling back: removing worktree %s...", branch))
		m.git.Run(ctx, []string{"worktree", "remove", "--force", worktreePath}, bareDir)
		m.git.Run(ctx, []string{"branch", "-D", branch}, bareDir)
		return nil
	})

	m.output.Success(fmt.Sprintf("Created worktree at %s", worktreePath))

	// Step 3: Set branch description (parent tracking)
	description := "parent:" + baseBranch
	if err := SetBranchDescription(ctx, m.git, branch, description, worktreePath); err != nil {
		return "", fmt.Errorf("failed to set branch description: %w", err)
	}
	op.AddUndo(func(ctx context.Context) error {
		m.git.Run(ctx, []string{"config", "--unset", "branch." + branch + ".description"}, worktreePath)
		return nil
	})

	// Step 4: Set goal
	if goal != "" {
		if err := SetBranchGoal(ctx, m.git, branch, goal, worktreePath); err != nil {
			return "", fmt.Errorf("failed to set goal: %w", err)
		}
		op.AddUndo(func(ctx context.Context) error {
			m.git.Run(ctx, []string{"config", "--unset", "branch." + branch + ".goal"}, worktreePath)
			return nil
		})
	}

	// Step 5: Run post-create hooks (not reversed on failure -- hooks are side-effectful)
	config, err := LoadRepoConfig(worktreePath)
	if err != nil {
		m.output.Warn(fmt.Sprintf("Failed to load repo config, skipping hooks: %v", err))
	} else {
		createCommands := config.WorktreeCreateCommands()
		if len(createCommands) > 0 {
			if err := RunHooks(createCommands, worktreePath, branch, m.output); err != nil {
				m.output.Warn(fmt.Sprintf("Post-create hook failed: %v", err))
			}
		}
	}

	op.Commit()
	return worktreePath, nil
}
