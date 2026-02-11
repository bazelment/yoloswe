// Package service defines the service interfaces used by the TUI app.
// Both local implementations (wrapping concrete types) and remote gRPC
// proxies implement these interfaces.
package service

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/wt"
)

// WorktreeService abstracts worktree management operations.
type WorktreeService interface {
	List(ctx context.Context) ([]wt.Worktree, error)
	GetGitStatus(ctx context.Context, w wt.Worktree) (*wt.WorktreeStatus, error)
	FetchAllPRInfo(ctx context.Context) ([]wt.PRInfo, error)
	NewAtomic(ctx context.Context, branch, baseBranch, goal string) (string, error)
	Remove(ctx context.Context, nameOrBranch string, deleteBranch bool) error
	Sync(ctx context.Context, branch string) error
	MergePRForBranch(ctx context.Context, branch string, opts wt.MergeOptions) (int, error)
	GatherContext(ctx context.Context, w wt.Worktree, opts wt.ContextOptions) (*wt.WorktreeContext, error)
	ResetToDefault(ctx context.Context, branch string) error

	// Messages returns any captured output messages from the last operation.
	// The slice is reset on each new operation call.
	Messages() []string
}

// LocalWorktreeService implements WorktreeService using the local wt.Manager.
type LocalWorktreeService struct {
	root     string
	repoName string
	messages []string
	mu       sync.Mutex
}

// NewLocalWorktreeService creates a new local worktree service.
func NewLocalWorktreeService(wtRoot, repoName string) *LocalWorktreeService {
	return &LocalWorktreeService{
		root:     wtRoot,
		repoName: repoName,
	}
}

func (s *LocalWorktreeService) manager() *wt.Manager {
	return wt.NewManager(s.root, s.repoName)
}

func (s *LocalWorktreeService) managerWithCapture() (*wt.Manager, *bytes.Buffer) {
	var buf bytes.Buffer
	output := wt.NewOutput(&buf, false)
	return wt.NewManager(s.root, s.repoName, wt.WithOutput(output)), &buf
}

func (s *LocalWorktreeService) captureMessages(buf *bytes.Buffer) {
	s.messages = nil
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			s.messages = append(s.messages, line)
		}
	}
}

// Messages returns captured output messages from the last operation.
func (s *LocalWorktreeService) Messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages
}

// List returns all worktrees for the repo.
func (s *LocalWorktreeService) List(ctx context.Context) ([]wt.Worktree, error) {
	s.mu.Lock()
	s.messages = nil
	s.mu.Unlock()
	return s.manager().List(ctx)
}

// GetGitStatus returns local git status for a worktree.
func (s *LocalWorktreeService) GetGitStatus(ctx context.Context, w wt.Worktree) (*wt.WorktreeStatus, error) {
	s.mu.Lock()
	s.messages = nil
	s.mu.Unlock()
	return s.manager().GetGitStatus(ctx, w)
}

// FetchAllPRInfo fetches all open PRs in a single batch.
func (s *LocalWorktreeService) FetchAllPRInfo(ctx context.Context) ([]wt.PRInfo, error) {
	s.mu.Lock()
	s.messages = nil
	s.mu.Unlock()
	mgr := s.manager()
	// Need a valid git repo directory for gh CLI.
	worktrees, err := mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(worktrees) == 0 {
		return nil, nil
	}
	return mgr.FetchAllPRInfo(ctx, worktrees[0].Path)
}

// NewAtomic creates a worktree with rollback on failure.
func (s *LocalWorktreeService) NewAtomic(ctx context.Context, branch, baseBranch, goal string) (string, error) {
	mgr, buf := s.managerWithCapture()
	result, err := mgr.NewAtomic(ctx, branch, baseBranch, goal)
	s.mu.Lock()
	s.captureMessages(buf)
	s.mu.Unlock()
	return result, err
}

// Remove deletes a worktree.
func (s *LocalWorktreeService) Remove(ctx context.Context, nameOrBranch string, deleteBranch bool) error {
	mgr, buf := s.managerWithCapture()
	err := mgr.Remove(ctx, nameOrBranch, deleteBranch)
	s.mu.Lock()
	s.captureMessages(buf)
	s.mu.Unlock()
	return err
}

// Sync fetches and rebases a worktree.
func (s *LocalWorktreeService) Sync(ctx context.Context, branch string) error {
	mgr, buf := s.managerWithCapture()
	err := mgr.Sync(ctx, branch)
	s.mu.Lock()
	s.captureMessages(buf)
	s.mu.Unlock()
	return err
}

// MergePRForBranch merges the PR for a branch.
func (s *LocalWorktreeService) MergePRForBranch(ctx context.Context, branch string, opts wt.MergeOptions) (int, error) {
	mgr, buf := s.managerWithCapture()
	prNumber, err := mgr.MergePRForBranch(ctx, branch, opts)
	s.mu.Lock()
	s.captureMessages(buf)
	s.mu.Unlock()
	return prNumber, err
}

// GatherContext gathers worktree context for file tree display.
func (s *LocalWorktreeService) GatherContext(ctx context.Context, w wt.Worktree, opts wt.ContextOptions) (*wt.WorktreeContext, error) {
	s.mu.Lock()
	s.messages = nil
	s.mu.Unlock()
	return s.manager().GatherContext(ctx, w, opts)
}

// ResetToDefault resets a worktree to the default branch (e.g. main).
func (s *LocalWorktreeService) ResetToDefault(ctx context.Context, branch string) error {
	mgr, buf := s.managerWithCapture()

	bareDir := mgr.BareDir()
	defaultBranch, _ := wt.GetDefaultBranch(ctx, mgr.GitRunner(), bareDir)
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	worktrees, err := mgr.List(ctx)
	if err != nil {
		s.mu.Lock()
		s.captureMessages(buf)
		s.mu.Unlock()
		return fmt.Errorf("failed to list worktrees: %w", err)
	}

	var wtPath string
	for _, w := range worktrees {
		if w.Branch == branch {
			wtPath = w.Path
			break
		}
	}
	if wtPath == "" {
		s.mu.Lock()
		s.captureMessages(buf)
		s.mu.Unlock()
		return fmt.Errorf("worktree for %s not found", branch)
	}

	if _, err := mgr.GitRunner().Run(ctx, []string{"fetch", "origin"}, wtPath); err != nil {
		s.mu.Lock()
		s.captureMessages(buf)
		s.mu.Unlock()
		return fmt.Errorf("failed to fetch: %w", err)
	}
	if _, err := mgr.GitRunner().Run(ctx, []string{"reset", "--hard", "origin/" + defaultBranch}, wtPath); err != nil {
		s.mu.Lock()
		s.captureMessages(buf)
		s.mu.Unlock()
		return fmt.Errorf("failed to reset: %w", err)
	}

	s.mu.Lock()
	s.captureMessages(buf)
	s.messages = append(s.messages, fmt.Sprintf("Reset %s to %s", branch, defaultBranch))
	s.mu.Unlock()
	return nil
}
