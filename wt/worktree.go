/*
Package wt provides Git worktree management for concurrent branch development.

# Overview

wt manages multiple Git worktrees using a bare clone structure:

	~/worktrees/
	└── repo-name/
	    ├── .bare/          # Bare clone (shared Git objects)
	    ├── main/           # Worktree for main branch
	    ├── feature-x/      # Worktree for feature-x branch
	    └── pr-123/         # Worktree for PR #123

This structure allows working on multiple branches simultaneously without
stashing or switching contexts. Each worktree is a full checkout with its
own working directory.

# User Journeys

1. Starting with a new repository:

	wt init git@github.com:user/repo.git
	cd ~/worktrees/repo/main

2. Creating and maintaining a feature branch:

	wt new feature-x              # Creates from default base (main)
	wt new feature-y --from dev   # Creates from specific branch

   Staying in sync with upstream:

	wt cd feature-x
	wt sync                       # Fetch latest (shows ahead/behind)
	wt sync --rebase              # Fetch and rebase onto origin

   Cascading branches (feature-b depends on feature-a):

	wt new feature-a              # First feature from main
	wt new feature-b --from feature-a  # Second feature from first

   When feature-a is updated, rebase feature-b:

	wt cd feature-b
	git rebase feature-a          # Rebase onto updated parent

   After PR is merged:

	wt cd main
	git pull                      # Update main with merged changes
	wt rm feature-x -D            # Remove worktree + delete branch

   If you have cascading branches after parent merges:

	wt cd feature-b
	git rebase main               # Rebase onto main (parent is now in main)
	git push --force-with-lease   # Update remote

3. Opening an existing remote branch:

	wt open existing-branch

4. Reviewing a GitHub PR:

	wt pr 123                     # Creates pr-123/ worktree

5. Day-to-day navigation:

	wt ls                         # List worktrees in current repo
	wt ls -a                      # List all repos
	wt cd feature-x               # Navigate to worktree
	wt status                     # Show sync/dirty status
	wt sync                       # Fetch all branches

6. Cleanup:

	wt rm feature-x               # Remove worktree only
	wt rm feature-x -D            # Remove worktree + delete branch

# Shell Integration

Add to ~/.bashrc or ~/.zshrc:

	eval "$(wt shellenv)"

This enables `wt cd` to change your shell's working directory.

# Configuration

Create .wt.yaml in your repository root:

	default_base: main
	post_create:
	  - npm install
	post_remove:
	  - echo "cleaned up"

SECURITY WARNING: Hooks in .wt.yaml are executed automatically during
init, new, open, pr, and rm operations with no confirmation prompt.
Only use wt with repositories you trust. A malicious .wt.yaml can
execute arbitrary shell commands on your machine.

# Library Usage

	m := wt.NewManager("~/worktrees", "repo-name")

	// Initialize a new repo
	mainPath, err := m.Init(ctx, "git@github.com:user/repo.git")

	// Create a new branch worktree
	path, err := m.New(ctx, "feature-x", "main")

	// List and check status
	worktrees, _ := m.List(ctx)
	for _, w := range worktrees {
	    status, _ := m.GetStatus(ctx, w)
	    fmt.Printf("%s: dirty=%v ahead=%d\n", w.Branch, status.IsDirty, status.Ahead)
	}
*/
package wt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Common errors.
var (
	ErrRepoNotInitialized = errors.New("repository not initialized")
	ErrWorktreeExists     = errors.New("worktree already exists")
	ErrWorktreeNotFound   = errors.New("worktree not found")
	ErrBranchNotFound     = errors.New("branch not found on remote")
)

// Worktree represents a Git worktree.
type Worktree struct {
	Path       string
	Branch     string
	Commit     string
	IsDetached bool
}

// Name returns the worktree name (directory name).
func (w *Worktree) Name() string {
	return filepath.Base(w.Path)
}

// WorktreeStatus contains extended status information.
type WorktreeStatus struct {
	Worktree       Worktree
	Ahead          int
	Behind         int
	IsDirty        bool
	LastCommitTime time.Time
	LastCommitMsg  string
	PRNumber       int
	PRURL          string
}

// Manager handles worktree operations for a repository.
type Manager struct {
	root     string
	repoName string
	git      GitRunner
	gh       GHRunner
	output   *Output
}

// Option configures a Manager.
type Option func(*Manager)

// WithGitRunner sets a custom git runner.
func WithGitRunner(r GitRunner) Option {
	return func(m *Manager) { m.git = r }
}

// WithGHRunner sets a custom gh runner.
func WithGHRunner(r GHRunner) Option {
	return func(m *Manager) { m.gh = r }
}

// WithOutput sets a custom output writer.
func WithOutput(o *Output) Option {
	return func(m *Manager) { m.output = o }
}

// NewManager creates a Manager for the given repository.
func NewManager(root, repoName string, opts ...Option) *Manager {
	m := &Manager{
		root:     root,
		repoName: repoName,
		git:      &DefaultGitRunner{},
		gh:       &DefaultGHRunner{},
		output:   DefaultOutput(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// RepoDir returns the path to the repository root.
func (m *Manager) RepoDir() string {
	return filepath.Join(m.root, m.repoName)
}

// BareDir returns the path to the bare repository.
func (m *Manager) BareDir() string {
	return filepath.Join(m.RepoDir(), ".bare")
}

// Init initializes a new repository with a bare clone.
func (m *Manager) Init(ctx context.Context, url string) (string, error) {
	repoName := GetRepoNameFromURL(url)
	m.repoName = repoName

	repoDir := m.RepoDir()
	bareDir := m.BareDir()

	if _, err := os.Stat(bareDir); err == nil {
		return "", fmt.Errorf("repository already initialized at %s", repoDir)
	}

	if err := os.MkdirAll(repoDir, 0755); err != nil {
		return "", err
	}

	m.output.Info(fmt.Sprintf("Cloning %s as bare repository...", url))
	if _, err := m.git.Run(ctx, []string{"clone", "--bare", url, bareDir}, ""); err != nil {
		return "", fmt.Errorf("failed to clone: %w", err)
	}

	// Configure fetch refspec
	if _, err := m.git.Run(ctx, []string{
		"config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*",
	}, bareDir); err != nil {
		return "", err
	}

	if _, err := m.git.Run(ctx, []string{"fetch", "origin"}, bareDir); err != nil {
		return "", err
	}

	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)
	mainPath := filepath.Join(repoDir, defaultBranch)

	m.output.Info(fmt.Sprintf("Creating main worktree at %s...", mainPath))
	if _, err := m.git.Run(ctx, []string{
		"worktree", "add", mainPath, defaultBranch,
	}, bareDir); err != nil {
		return "", fmt.Errorf("failed to create main worktree: %w", err)
	}

	m.output.Success(fmt.Sprintf("Initialized %s at %s", repoName, repoDir))
	m.output.Success(fmt.Sprintf("Main worktree: %s", mainPath))

	// Run post-create hooks
	config, _ := LoadRepoConfig(mainPath)
	if len(config.PostCreate) > 0 {
		RunHooks(config.PostCreate, mainPath, defaultBranch, m.output)
	}

	return mainPath, nil
}

// New creates a new worktree with a new branch.
func (m *Manager) New(ctx context.Context, branch, baseBranch string) (string, error) {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return "", ErrRepoNotInitialized
	}

	worktreePath := filepath.Join(m.RepoDir(), branch)
	if _, err := os.Stat(worktreePath); err == nil {
		return "", ErrWorktreeExists
	}

	// Determine base branch
	if baseBranch == "" {
		// Try to get from config in any existing worktree
		entries, _ := os.ReadDir(m.RepoDir())
		for _, entry := range entries {
			if entry.IsDir() {
				wtPath := filepath.Join(m.RepoDir(), entry.Name())
				if _, err := os.Stat(filepath.Join(wtPath, ".git")); err == nil {
					config, _ := LoadRepoConfig(wtPath)
					baseBranch = config.DefaultBase
					break
				}
			}
		}
		if baseBranch == "" {
			baseBranch, _ = GetDefaultBranch(ctx, m.git, bareDir)
		}
	}

	m.output.Info("Fetching latest from origin...")
	if _, err := m.git.Run(ctx, []string{"fetch", "origin"}, bareDir); err != nil {
		return "", fmt.Errorf("failed to fetch from origin: %w", err)
	}

	m.output.Info(fmt.Sprintf("Creating worktree %s from %s...", branch, baseBranch))
	if _, err := m.git.Run(ctx, []string{
		"worktree", "add", "-b", branch, worktreePath, "origin/" + baseBranch,
	}, bareDir); err != nil {
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}

	m.output.Success(fmt.Sprintf("Created worktree at %s", worktreePath))

	// Run post-create hooks
	config, _ := LoadRepoConfig(worktreePath)
	if len(config.PostCreate) > 0 {
		RunHooks(config.PostCreate, worktreePath, branch, m.output)
	}

	return worktreePath, nil
}

// Open creates a worktree for an existing remote branch.
func (m *Manager) Open(ctx context.Context, branch string) (string, error) {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return "", ErrRepoNotInitialized
	}

	worktreePath := filepath.Join(m.RepoDir(), branch)
	if _, err := os.Stat(worktreePath); err == nil {
		return "", ErrWorktreeExists
	}

	m.output.Info("Fetching latest from origin...")
	if _, err := m.git.Run(ctx, []string{"fetch", "origin"}, bareDir); err != nil {
		return "", fmt.Errorf("failed to fetch from origin: %w", err)
	}

	// Check if branch exists on remote
	if _, err := m.git.Run(ctx, []string{
		"rev-parse", "refs/remotes/origin/" + branch,
	}, bareDir); err != nil {
		return "", ErrBranchNotFound
	}

	m.output.Info(fmt.Sprintf("Creating worktree for %s...", branch))
	if _, err := m.git.Run(ctx, []string{
		"worktree", "add", "--track", "-b", branch, worktreePath, "origin/" + branch,
	}, bareDir); err != nil {
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}

	m.output.Success(fmt.Sprintf("Created worktree at %s", worktreePath))

	// Run post-create hooks
	config, _ := LoadRepoConfig(worktreePath)
	if len(config.PostCreate) > 0 {
		RunHooks(config.PostCreate, worktreePath, branch, m.output)
	}

	return worktreePath, nil
}

// OpenPR creates a worktree for a GitHub PR.
func (m *Manager) OpenPR(ctx context.Context, prNumber int) (string, error) {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return "", ErrRepoNotInitialized
	}

	// Find an existing worktree to run gh commands from
	var ghDir string
	entries, _ := os.ReadDir(m.RepoDir())
	for _, entry := range entries {
		if entry.IsDir() {
			wtPath := filepath.Join(m.RepoDir(), entry.Name())
			if _, err := os.Stat(filepath.Join(wtPath, ".git")); err == nil {
				ghDir = wtPath
				break
			}
		}
	}
	if ghDir == "" {
		ghDir = bareDir
	}

	m.output.Info(fmt.Sprintf("Fetching PR #%d info...", prNumber))
	result, err := m.gh.Run(ctx, []string{
		"pr", "view", strconv.Itoa(prNumber), "--json", "headRefName",
	}, ghDir)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR info: %w", err)
	}

	var prInfo struct {
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &prInfo); err != nil {
		return "", fmt.Errorf("failed to parse PR info: %w", err)
	}
	branch := prInfo.HeadRefName

	worktreeName := fmt.Sprintf("pr-%d", prNumber)
	worktreePath := filepath.Join(m.RepoDir(), worktreeName)
	if _, err := os.Stat(worktreePath); err == nil {
		return "", ErrWorktreeExists
	}

	m.output.Info(fmt.Sprintf("Fetching PR #%d (%s)...", prNumber, branch))
	if _, err := m.git.Run(ctx, []string{
		"fetch", "origin", fmt.Sprintf("pull/%d/head:%s", prNumber, branch),
	}, bareDir); err != nil {
		return "", fmt.Errorf("failed to fetch PR: %w", err)
	}

	m.output.Info(fmt.Sprintf("Creating worktree for PR #%d...", prNumber))
	if _, err := m.git.Run(ctx, []string{
		"worktree", "add", worktreePath, branch,
	}, bareDir); err != nil {
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}

	m.output.Success(fmt.Sprintf("Created worktree at %s", worktreePath))

	// Run post-create hooks
	config, _ := LoadRepoConfig(worktreePath)
	if len(config.PostCreate) > 0 {
		RunHooks(config.PostCreate, worktreePath, branch, m.output)
	}

	return worktreePath, nil
}

// List returns all worktrees for the repository.
func (m *Manager) List(ctx context.Context) ([]Worktree, error) {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return nil, nil
	}

	result, err := m.git.Run(ctx, []string{"worktree", "list", "--porcelain"}, bareDir)
	if err != nil {
		return nil, err
	}

	return parseWorktreeList(result.Stdout), nil
}

// parseWorktreeList parses git worktree list --porcelain output.
func parseWorktreeList(output string) []Worktree {
	var worktrees []Worktree
	current := make(map[string]string)

	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			if _, isBare := current["bare"]; !isBare && current["worktree"] != "" {
				branch := strings.TrimPrefix(current["branch"], "refs/heads/")
				if branch == "" {
					branch = "(detached)"
				}
				commit := current["HEAD"]
				if len(commit) > 8 {
					commit = commit[:8]
				}
				worktrees = append(worktrees, Worktree{
					Path:       current["worktree"],
					Branch:     branch,
					Commit:     commit,
					IsDetached: current["detached"] == "true",
				})
			}
			current = make(map[string]string)
		} else if strings.HasPrefix(line, "worktree ") {
			current["worktree"] = line[9:]
		} else if strings.HasPrefix(line, "HEAD ") {
			current["HEAD"] = line[5:]
		} else if strings.HasPrefix(line, "branch ") {
			current["branch"] = line[7:]
		} else if line == "bare" {
			current["bare"] = "true"
		} else if line == "detached" {
			current["detached"] = "true"
		}
	}

	// Handle last entry
	if _, isBare := current["bare"]; !isBare && current["worktree"] != "" {
		branch := strings.TrimPrefix(current["branch"], "refs/heads/")
		if branch == "" {
			branch = "(detached)"
		}
		commit := current["HEAD"]
		if len(commit) > 8 {
			commit = commit[:8]
		}
		worktrees = append(worktrees, Worktree{
			Path:       current["worktree"],
			Branch:     branch,
			Commit:     commit,
			IsDetached: current["detached"] == "true",
		})
	}

	return worktrees
}

// GetStatus returns extended status for a worktree.
func (m *Manager) GetStatus(ctx context.Context, wt Worktree) (*WorktreeStatus, error) {
	status := &WorktreeStatus{Worktree: wt}

	// Check dirty status
	result, _ := m.git.Run(ctx, []string{"status", "--porcelain"}, wt.Path)
	status.IsDirty = strings.TrimSpace(result.Stdout) != ""

	// Check ahead/behind
	if !wt.IsDetached {
		result, err := m.git.Run(ctx, []string{
			"rev-list", "--left-right", "--count",
			"origin/" + wt.Branch + "...HEAD",
		}, wt.Path)
		if err == nil {
			parts := strings.Split(strings.TrimSpace(result.Stdout), "\t")
			if len(parts) == 2 {
				status.Behind, _ = strconv.Atoi(parts[0])
				status.Ahead, _ = strconv.Atoi(parts[1])
			}
		}
	}

	// Get last commit info
	result, _ = m.git.Run(ctx, []string{"log", "-1", "--format=%ct|%s"}, wt.Path)
	if result != nil && result.Stdout != "" {
		parts := strings.SplitN(strings.TrimSpace(result.Stdout), "|", 2)
		if len(parts) == 2 {
			if ts, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				status.LastCommitTime = time.Unix(ts, 0)
			}
			status.LastCommitMsg = parts[1]
			if len(status.LastCommitMsg) > 50 {
				status.LastCommitMsg = status.LastCommitMsg[:50]
			}
		}
	}

	// Get PR info
	if !wt.IsDetached {
		result, err := m.gh.Run(ctx, []string{"pr", "view", "--json", "number,url"}, wt.Path)
		if err == nil && result.Stdout != "" {
			var prData struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			}
			if json.Unmarshal([]byte(result.Stdout), &prData) == nil {
				status.PRNumber = prData.Number
				status.PRURL = prData.URL
			}
		}
	}

	return status, nil
}

// Remove removes a worktree.
func (m *Manager) Remove(ctx context.Context, branch string, deleteBranch bool) error {
	bareDir := m.BareDir()
	worktreePath := filepath.Join(m.RepoDir(), branch)

	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		return ErrWorktreeNotFound
	}

	// Run post-remove hooks first
	config, _ := LoadRepoConfig(worktreePath)
	if len(config.PostRemove) > 0 {
		RunHooks(config.PostRemove, worktreePath, branch, m.output)
	}

	m.output.Info(fmt.Sprintf("Removing worktree %s...", branch))
	if _, err := m.git.Run(ctx, []string{"worktree", "remove", worktreePath}, bareDir); err != nil {
		return fmt.Errorf("failed to remove worktree: %w", err)
	}
	m.output.Success(fmt.Sprintf("Removed worktree %s", branch))

	if deleteBranch {
		m.output.Info(fmt.Sprintf("Deleting local branch %s...", branch))
		if _, err := m.git.Run(ctx, []string{"branch", "-D", branch}, bareDir); err != nil {
			m.output.Error(fmt.Sprintf("Failed to delete local branch %s", branch))
		} else {
			m.output.Success(fmt.Sprintf("Deleted local branch %s", branch))
		}

		m.output.Info(fmt.Sprintf("Deleting remote branch %s...", branch))
		if _, err := m.git.Run(ctx, []string{"push", "origin", "--delete", branch}, bareDir); err != nil {
			m.output.Warn(fmt.Sprintf("Remote branch %s may not exist", branch))
		} else {
			m.output.Success(fmt.Sprintf("Deleted remote branch %s", branch))
		}
	}

	return nil
}

// Sync fetches and optionally rebases all worktrees.
func (m *Manager) Sync(ctx context.Context, rebase bool) error {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return ErrRepoNotInitialized
	}

	m.output.Info("Fetching from origin...")
	if _, err := m.git.Run(ctx, []string{"fetch", "--all", "--prune"}, bareDir); err != nil {
		return fmt.Errorf("failed to fetch: %w", err)
	}
	m.output.Success("Fetched latest changes")

	worktrees, err := m.List(ctx)
	if err != nil {
		return err
	}

	for _, wt := range worktrees {
		if wt.IsDetached {
			m.output.Info(fmt.Sprintf("Skipping detached worktree %s", wt.Name()))
			continue
		}

		if rebase {
			m.output.Info(fmt.Sprintf("Rebasing %s...", wt.Branch))
			if _, err := m.git.Run(ctx, []string{"rebase", "origin/" + wt.Branch}, wt.Path); err != nil {
				m.output.Error(fmt.Sprintf("Failed to rebase %s - resolve conflicts manually", wt.Branch))
			} else {
				m.output.Success(fmt.Sprintf("Rebased %s", wt.Branch))
			}
		} else {
			status, _ := m.GetStatus(ctx, wt)
			if status.Behind > 0 {
				m.output.Info(fmt.Sprintf("%s: %d commits behind", wt.Branch, status.Behind))
			} else if status.Ahead > 0 {
				m.output.Info(fmt.Sprintf("%s: %d commits ahead", wt.Branch, status.Ahead))
			} else {
				m.output.Info(fmt.Sprintf("%s: up to date", wt.Branch))
			}
		}
	}

	return nil
}

// Prune cleans stale worktree metadata.
func (m *Manager) Prune(ctx context.Context, dryRun bool) ([]string, error) {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return nil, ErrRepoNotInitialized
	}

	args := []string{"worktree", "prune"}
	if dryRun {
		args = append(args, "--dry-run", "-v")
	}

	result, err := m.git.Run(ctx, args, bareDir)
	if err != nil {
		return nil, err
	}

	if result.Stdout != "" {
		return strings.Split(strings.TrimSpace(result.Stdout), "\n"), nil
	}

	m.output.Success("No stale worktrees to prune")
	return nil, nil
}

// GetWorktreePath returns the path to a worktree by branch name.
func (m *Manager) GetWorktreePath(branch string) (string, error) {
	if branch == "" {
		return m.RepoDir(), nil
	}

	path := filepath.Join(m.RepoDir(), branch)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", ErrWorktreeNotFound
	}
	return path, nil
}
