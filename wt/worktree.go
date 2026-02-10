/*
Package wt provides Git worktree management for concurrent branch development.

# Overview

wt manages multiple Git worktrees using a bare clone structure:

	~/worktrees/
	└── repo-name/
	    ├── .bare/          # Bare clone (shared Git objects)
	    ├── main/           # Worktree for main branch
	    └── feature-x/      # Worktree for feature-x branch

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

4. Day-to-day navigation:

	wt ls                         # List worktrees in current repo
	wt ls -a                      # List all repos
	wt cd feature-x               # Navigate to worktree
	wt status                     # Show sync/dirty status
	wt sync                       # Fetch all branches

5. Cleanup:

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
init, new, open, and rm operations with no confirmation prompt.
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
// WorktreeStatus holds extended status for a worktree.
type WorktreeStatus struct {
	LastCommitTime time.Time
	LastCommitMsg  string
	PRURL          string
	PRState        string // OPEN, MERGED, CLOSED
	PRReviewStatus string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, etc.
	Worktree       Worktree
	Ahead          int
	Behind         int
	PRNumber       int
	IsDirty        bool
	PRIsDraft      bool
}

// Manager handles worktree operations for a repository.
type Manager struct {
	git      GitRunner
	gh       GHRunner
	output   *Output
	root     string
	repoName string
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
func (m *Manager) New(ctx context.Context, branch, baseBranch, goal string) (string, error) {
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

	// Always track parent branch for proper sync behavior
	description := "parent:" + baseBranch
	if err := SetBranchDescription(ctx, m.git, branch, description, worktreePath); err != nil {
		m.output.Warn(fmt.Sprintf("Failed to track parent branch: %v", err))
	} else {
		defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)
		if baseBranch != defaultBranch {
			m.output.Info(fmt.Sprintf("Tracking parent branch: %s", baseBranch))
		}
	}

	// Set goal if provided
	if goal != "" {
		if err := SetBranchGoal(ctx, m.git, branch, goal, worktreePath); err != nil {
			m.output.Warn(fmt.Sprintf("Failed to set goal: %v", err))
		}
	}

	// Run post-create hooks
	config, _ := LoadRepoConfig(worktreePath)
	if len(config.PostCreate) > 0 {
		RunHooks(config.PostCreate, worktreePath, branch, m.output)
	}

	return worktreePath, nil
}

// Open creates a worktree for an existing remote branch.
func (m *Manager) Open(ctx context.Context, branch, goal string) (string, error) {
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
		"worktree", "add", worktreePath, branch,
	}, bareDir); err != nil {
		return "", fmt.Errorf("failed to create worktree: %w", err)
	}

	m.output.Success(fmt.Sprintf("Created worktree at %s", worktreePath))

	// Track default branch as parent for proper sync behavior
	// (opened branches are assumed to be based on the default branch)
	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)
	description := "parent:" + defaultBranch
	if err := SetBranchDescription(ctx, m.git, branch, description, worktreePath); err != nil {
		m.output.Warn(fmt.Sprintf("Failed to track parent branch: %v", err))
	}

	// Set goal if provided
	if goal != "" {
		if err := SetBranchGoal(ctx, m.git, branch, goal, worktreePath); err != nil {
			m.output.Warn(fmt.Sprintf("Failed to set goal: %v", err))
		}
	}

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

// GetGitStatus returns local git status for a worktree (no network calls).
// This is fast and suitable for UI that needs immediate feedback.
func (m *Manager) GetGitStatus(ctx context.Context, wt Worktree) (*WorktreeStatus, error) {
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

	return status, nil
}

// FetchPRInfo fetches PR information for a worktree via the GitHub CLI.
// This makes a network call and may be slow.
func (m *Manager) FetchPRInfo(ctx context.Context, wt Worktree) (*PRInfo, error) {
	if wt.IsDetached {
		return nil, nil
	}

	result, err := m.gh.Run(ctx, []string{"pr", "view", "--json", "number,url,state,isDraft,reviewDecision"}, wt.Path)
	if err != nil || result.Stdout == "" {
		return nil, err
	}

	var prData struct {
		URL            string `json:"url"`
		State          string `json:"state"`
		ReviewDecision string `json:"reviewDecision"`
		Number         int    `json:"number"`
		IsDraft        bool   `json:"isDraft"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &prData); err != nil {
		return nil, err
	}

	return &PRInfo{
		Number:         prData.Number,
		URL:            prData.URL,
		State:          prData.State,
		IsDraft:        prData.IsDraft,
		ReviewDecision: prData.ReviewDecision,
	}, nil
}

// FetchAllPRInfo fetches all open PRs in a single API call.
// Any worktree directory can be used as the working directory for the gh CLI.
func (m *Manager) FetchAllPRInfo(ctx context.Context) ([]PRInfo, error) {
	return ListAllPRInfo(ctx, m.gh, m.RepoDir())
}

// GetStatus returns extended status for a worktree including PR info.
// This makes a network call for PR info; use GetGitStatus for local-only status.
func (m *Manager) GetStatus(ctx context.Context, wt Worktree) (*WorktreeStatus, error) {
	status, err := m.GetGitStatus(ctx, wt)
	if err != nil {
		return nil, err
	}

	pr, _ := m.FetchPRInfo(ctx, wt)
	if pr != nil {
		status.PRNumber = pr.Number
		status.PRURL = pr.URL
		status.PRState = pr.State
		status.PRIsDraft = pr.IsDraft
		status.PRReviewStatus = pr.ReviewDecision
	}

	return status, nil
}

// Remove removes a worktree by name (directory) or branch name.
func (m *Manager) Remove(ctx context.Context, nameOrBranch string, deleteBranch bool) error {
	bareDir := m.BareDir()

	// First try as directory name
	worktreePath := filepath.Join(m.RepoDir(), nameOrBranch)
	branchName := nameOrBranch

	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		// Not found by directory name, try to find by branch name
		worktrees, listErr := m.List(ctx)
		if listErr != nil {
			return ErrWorktreeNotFound
		}

		found := false
		for _, wt := range worktrees {
			if wt.Branch == nameOrBranch {
				worktreePath = wt.Path
				branchName = wt.Branch
				found = true
				break
			}
		}
		if !found {
			return ErrWorktreeNotFound
		}
	} else {
		// Found by directory name, get the actual branch name
		result, err := m.git.Run(ctx, []string{"branch", "--show-current"}, worktreePath)
		if err == nil && strings.TrimSpace(result.Stdout) != "" {
			branchName = strings.TrimSpace(result.Stdout)
		}
	}

	// Run post-remove hooks first
	config, _ := LoadRepoConfig(worktreePath)
	if len(config.PostRemove) > 0 {
		RunHooks(config.PostRemove, worktreePath, branchName, m.output)
	}

	m.output.Info(fmt.Sprintf("Removing worktree %s...", branchName))
	if _, err := m.git.Run(ctx, []string{"worktree", "remove", worktreePath}, bareDir); err != nil {
		return fmt.Errorf("failed to remove worktree: %w", err)
	}
	m.output.Success(fmt.Sprintf("Removed worktree %s", branchName))

	if deleteBranch {
		m.output.Info(fmt.Sprintf("Deleting local branch %s...", branchName))
		if _, err := m.git.Run(ctx, []string{"branch", "-D", branchName}, bareDir); err != nil {
			m.output.Error(fmt.Sprintf("Failed to delete local branch %s", branchName))
		} else {
			m.output.Success(fmt.Sprintf("Deleted local branch %s", branchName))
		}

		m.output.Info(fmt.Sprintf("Deleting remote branch %s...", branchName))
		if _, err := m.git.Run(ctx, []string{"push", "origin", "--delete", branchName}, bareDir); err != nil {
			m.output.Warn(fmt.Sprintf("Remote branch %s may not exist", branchName))
		} else {
			m.output.Success(fmt.Sprintf("Deleted remote branch %s", branchName))
		}
	}

	return nil
}

// Sync fetches the latest changes and rebases worktrees.
// If branch is non-empty, only that worktree is synced.
// If branch is empty, all worktrees in the repo are synced.
func (m *Manager) Sync(ctx context.Context, branch string) error {
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

	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)

	// Find a worktree to run gh commands from
	var ghDir string
	for _, wt := range worktrees {
		if !wt.IsDetached {
			ghDir = wt.Path
			break
		}
	}
	if ghDir == "" {
		ghDir = bareDir
	}

	// Build dependency graph and sort topologically
	orderedWorktrees := m.buildDependencyOrder(ctx, worktrees)

	// If syncing a single branch, filter to just that worktree
	if branch != "" {
		var filtered []Worktree
		for _, wt := range orderedWorktrees {
			if wt.Branch == branch {
				filtered = append(filtered, wt)
				break
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("worktree for branch %q not found", branch)
		}
		orderedWorktrees = filtered
	}

	// Track failed branches to skip their children
	failedBranches := make(map[string]bool)

	for _, wt := range orderedWorktrees {
		if wt.IsDetached {
			m.output.Info(fmt.Sprintf("Skipping detached worktree %s", wt.Name()))
			continue
		}

		// Check if any ancestor failed
		parentBranch, _ := m.GetParentBranch(ctx, wt.Branch, wt.Path)
		if parentBranch != "" && failedBranches[parentBranch] {
			m.output.Warn(fmt.Sprintf("Skipping %s - ancestor branch %s failed to rebase", wt.Branch, parentBranch))
			failedBranches[wt.Branch] = true
			continue
		}

		// Determine rebase target based on parent branch
		var rebaseTarget string

		if parentBranch == "" || parentBranch == defaultBranch {
			// No parent or parent is default branch: rebase onto default branch
			rebaseTarget = "origin/" + defaultBranch
		} else {
			// Cascading branch: check if parent was merged
			if m.isParentBranchMerged(ctx, parentBranch, ghDir) {
				m.output.Info(fmt.Sprintf("Parent branch %s was merged, rebasing %s onto %s...",
					parentBranch, wt.Branch, defaultBranch))
				rebaseTarget = "origin/" + defaultBranch

				// Update PR base branch if PR exists
				prInfo, err := GetPRByBranch(ctx, m.gh, wt.Branch, ghDir)
				if err == nil && prInfo != nil && prInfo.Number > 0 {
					m.output.Info(fmt.Sprintf("Updating PR #%d base to %s...", prInfo.Number, defaultBranch))
					if err := UpdatePRBase(ctx, m.gh, prInfo.Number, defaultBranch, ghDir); err != nil {
						m.output.Warn(fmt.Sprintf("Failed to update PR base: %v", err))
					}
				}

				// Update branch description
				if err := SetBranchDescription(ctx, m.git, wt.Branch, "parent:"+defaultBranch, wt.Path); err != nil {
					m.output.Warn(fmt.Sprintf("Failed to update branch description: %v", err))
				}
			} else {
				// Parent not merged: rebase onto remote parent branch
				rebaseTarget = "origin/" + parentBranch
			}
		}

		m.output.Info(fmt.Sprintf("Rebasing %s onto %s...", wt.Branch, rebaseTarget))
		if _, err := m.git.Run(ctx, []string{"rebase", "--autostash", rebaseTarget}, wt.Path); err != nil {
			m.output.Error(fmt.Sprintf("Failed to rebase %s - resolve conflicts manually:\n  cd %s\n  git rebase --continue  # after fixing conflicts\n  git rebase --abort      # to cancel",
				wt.Branch, wt.Path))
			failedBranches[wt.Branch] = true
		} else {
			m.output.Success(fmt.Sprintf("Rebased %s", wt.Branch))
		}
	}

	return nil
}

// isParentBranchMerged checks if a parent branch has been merged to default.
func (m *Manager) isParentBranchMerged(ctx context.Context, parentBranch, ghDir string) bool {
	// Method 1: Check if parent branch's PR is merged
	prInfo, err := GetPRByBranch(ctx, m.gh, parentBranch, ghDir)
	if err == nil && prInfo != nil {
		if prInfo.State == "MERGED" {
			return true
		}
	}

	// Method 2: Check if branch no longer exists on remote
	exists, err := RemoteBranchExists(ctx, m.git, parentBranch, m.BareDir())
	if err == nil && !exists {
		return true
	}

	return false
}

// buildDependencyOrder sorts worktrees topologically so parents come before children.
func (m *Manager) buildDependencyOrder(ctx context.Context, worktrees []Worktree) []Worktree {
	// Build parent map
	parentMap := make(map[string]string)
	wtMap := make(map[string]Worktree)

	for _, wt := range worktrees {
		wtMap[wt.Branch] = wt
		if !wt.IsDetached {
			parent, _ := m.GetParentBranch(ctx, wt.Branch, wt.Path)
			if parent != "" {
				parentMap[wt.Branch] = parent
			}
		}
	}

	// Topological sort using Kahn's algorithm
	// Count incoming edges (children count for each parent)
	childCount := make(map[string]int)
	for branch, parent := range parentMap {
		if _, exists := wtMap[parent]; exists {
			childCount[branch]++
		}
	}

	// Start with branches that have no parent in our worktree set
	var result []Worktree
	var queue []Worktree

	for _, wt := range worktrees {
		if childCount[wt.Branch] == 0 {
			queue = append(queue, wt)
		}
	}

	processed := make(map[string]bool)
	for len(queue) > 0 {
		wt := queue[0]
		queue = queue[1:]

		if processed[wt.Branch] {
			continue
		}
		processed[wt.Branch] = true
		result = append(result, wt)

		// Find children and add them to queue
		for branch, parent := range parentMap {
			if parent == wt.Branch {
				if child, exists := wtMap[branch]; exists && !processed[branch] {
					queue = append(queue, child)
				}
			}
		}
	}

	// Add any remaining worktrees (cycles or orphans)
	for _, wt := range worktrees {
		if !processed[wt.Branch] {
			result = append(result, wt)
		}
	}

	return result
}

// MergeOptions configures the merge operation.
type MergeOptions struct {
	MergeMethod string
	Keep        bool
}

// BranchDependency represents a branch that depends on another.
type BranchDependency struct {
	Branch       string
	BaseBranch   string
	WorktreePath string
	PRNumber     int
	HasWorktree  bool
}

// MergePR merges the PR for the current worktree and handles cleanup.
func (m *Manager) MergePR(ctx context.Context, opts MergeOptions) error {
	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// Get current branch
	result, err := m.git.Run(ctx, []string{"branch", "--show-current"}, cwd)
	if err != nil {
		return fmt.Errorf("failed to get current branch: %w", err)
	}
	currentBranch := strings.TrimSpace(result.Stdout)
	if currentBranch == "" {
		return fmt.Errorf("not on a branch (detached HEAD?)")
	}

	bareDir := m.BareDir()
	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)

	// Check if trying to merge default branch
	if currentBranch == defaultBranch {
		return fmt.Errorf("cannot merge the default branch (%s)", defaultBranch)
	}

	// Get PR info for current branch
	prInfo, err := GetPRByBranch(ctx, m.gh, currentBranch, cwd)
	if err != nil {
		return fmt.Errorf("no PR found for branch %s: %w", currentBranch, err)
	}

	// Check if PR is mergeable
	if prInfo.ReviewDecision != "" && prInfo.ReviewDecision != "APPROVED" {
		m.output.Warn(fmt.Sprintf("PR #%d review status: %s", prInfo.Number, prInfo.ReviewDecision))
	}

	m.output.Info(fmt.Sprintf("Merging PR #%d for branch %s...", prInfo.Number, currentBranch))

	// Find child branches BEFORE merging
	childDeps, err := m.findChildBranches(ctx, currentBranch, cwd)
	if err != nil {
		m.output.Warn(fmt.Sprintf("Failed to find child branches: %v", err))
	}

	// Merge the PR
	mergeArgs := []string{"pr", "merge", strconv.Itoa(prInfo.Number), "--delete-branch"}
	switch opts.MergeMethod {
	case "squash":
		mergeArgs = append(mergeArgs, "--squash")
	case "rebase":
		mergeArgs = append(mergeArgs, "--rebase")
	case "merge":
		mergeArgs = append(mergeArgs, "--merge")
	}

	if _, err := m.gh.Run(ctx, mergeArgs, cwd); err != nil {
		return fmt.Errorf("failed to merge PR: %w", err)
	}
	m.output.Success(fmt.Sprintf("Merged PR #%d", prInfo.Number))

	// Fetch to get updated remote state
	m.git.Run(ctx, []string{"fetch", "--prune"}, bareDir)

	// Cleanup unless --keep
	if !opts.Keep {
		// Navigate away from current worktree before removing it
		m.output.Info("Navigating to default branch worktree...")
		fmt.Printf("__WT_CD__:%s\n", filepath.Join(m.RepoDir(), defaultBranch))

		if err := m.Remove(ctx, currentBranch, true); err != nil {
			m.output.Warn(fmt.Sprintf("Failed to cleanup worktree: %v", err))
		}
	}

	// Handle child branches
	if len(childDeps) > 0 {
		m.output.Info(fmt.Sprintf("Found %d child branches depending on %s", len(childDeps), currentBranch))
		m.handleChildBranches(ctx, childDeps, defaultBranch)
	}

	return nil
}

// findChildBranches finds all branches that have PRs targeting the given branch.
func (m *Manager) findChildBranches(ctx context.Context, parentBranch, dir string) ([]BranchDependency, error) {
	// Get all open PRs
	prs, err := ListOpenPRs(ctx, m.gh, dir)
	if err != nil {
		return nil, err
	}

	// Get local worktrees
	worktrees, _ := m.List(ctx)
	wtMap := make(map[string]string) // branch -> path
	for _, wt := range worktrees {
		wtMap[wt.Branch] = wt.Path
	}

	// Filter PRs targeting our parent branch
	var children []BranchDependency
	for _, pr := range prs {
		if pr.BaseRefName == parentBranch {
			child := BranchDependency{
				Branch:     pr.HeadRefName,
				PRNumber:   pr.Number,
				BaseBranch: parentBranch,
			}
			if path, ok := wtMap[pr.HeadRefName]; ok {
				child.HasWorktree = true
				child.WorktreePath = path
			}
			children = append(children, child)
		}
	}

	return children, nil
}

// handleChildBranches rebases child branches onto the new base and updates their PRs.
func (m *Manager) handleChildBranches(ctx context.Context, children []BranchDependency, newBase string) {
	failedBranches := make(map[string]bool)

	for _, child := range children {
		// Check if ancestor failed
		if failedBranches[child.BaseBranch] {
			m.output.Warn(fmt.Sprintf("Skipping %s - ancestor branch failed to rebase", child.Branch))
			failedBranches[child.Branch] = true
			continue
		}

		if child.HasWorktree {
			// Fetch latest
			m.git.Run(ctx, []string{"fetch", "origin"}, child.WorktreePath)

			// Rebase onto new base
			m.output.Info(fmt.Sprintf("Rebasing %s onto %s...", child.Branch, newBase))
			if _, err := m.git.Run(ctx, []string{"rebase", "origin/" + newBase}, child.WorktreePath); err != nil {
				m.output.Error(fmt.Sprintf("Failed to rebase %s - resolve conflicts manually:\n  cd %s\n  git rebase --continue\n  git rebase --abort",
					child.Branch, child.WorktreePath))
				failedBranches[child.Branch] = true
				continue
			}

			// Force push
			if _, err := m.git.Run(ctx, []string{"push", "--force-with-lease"}, child.WorktreePath); err != nil {
				m.output.Error(fmt.Sprintf("Failed to push %s: %v", child.Branch, err))
				failedBranches[child.Branch] = true
				continue
			}

			// Update branch description
			SetBranchDescription(ctx, m.git, child.Branch, "parent:"+newBase, child.WorktreePath)

			m.output.Success(fmt.Sprintf("Rebased %s onto %s", child.Branch, newBase))
		} else {
			m.output.Warn(fmt.Sprintf("Branch %s has no worktree, updating PR base only (rebase manually later)", child.Branch))
		}

		// Update PR base branch
		if child.PRNumber > 0 {
			if err := UpdatePRBase(ctx, m.gh, child.PRNumber, newBase, m.BareDir()); err != nil {
				m.output.Warn(fmt.Sprintf("Failed to update PR #%d base: %v", child.PRNumber, err))
			} else {
				m.output.Success(fmt.Sprintf("Updated PR #%d base to %s", child.PRNumber, newBase))
			}
		}
	}
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

// GetParentBranch returns the parent branch for a given branch if tracked.
// If the current branch has no parent config, it falls back to checking the
// directory name (the original branch the worktree was created for), which
// handles the case where the user ran `git checkout -b` inside a worktree.
func (m *Manager) GetParentBranch(ctx context.Context, branch, dir string) (string, error) {
	desc, err := GetBranchDescription(ctx, m.git, branch, dir)
	if err == nil {
		if parent, ok := strings.CutPrefix(desc, "parent:"); ok {
			return parent, nil
		}
	}
	// Fallback: if the worktree directory name differs from the branch,
	// check the original branch's config (worktree may have been checked out to a different branch)
	dirName := filepath.Base(dir)
	if dirName != branch {
		desc, err = GetBranchDescription(ctx, m.git, dirName, dir)
		if err == nil {
			if parent, ok := strings.CutPrefix(desc, "parent:"); ok {
				return parent, nil
			}
		}
	}
	return "", nil
}

// SetGoal sets the goal for a branch in a worktree.
func (m *Manager) SetGoal(ctx context.Context, branch, goal, dir string) error {
	return SetBranchGoal(ctx, m.git, branch, goal, dir)
}

// GetGoal returns the goal for a branch in a worktree.
// If the current branch has no goal config, it falls back to checking the
// directory name, which handles the case where the user ran `git checkout -b`
// inside a worktree.
func (m *Manager) GetGoal(ctx context.Context, branch, dir string) (string, error) {
	goal, err := GetBranchGoal(ctx, m.git, branch, dir)
	if err == nil && goal != "" {
		return goal, nil
	}
	dirName := filepath.Base(dir)
	if dirName != branch {
		return GetBranchGoal(ctx, m.git, dirName, dir)
	}
	return "", nil
}

// PROptions configures PR creation.
type PROptions struct {
	Title  string
	Body   string
	Base   string // Override auto-detected base
	Draft  bool
	NoPush bool
}

// PRResult contains the result of PR creation.
type PRResult struct {
	URL     string
	Branch  string
	Base    string
	Number  int
	Existed bool
}

// CreatePR pushes the current branch and creates a GitHub PR.
// Base branch is auto-detected: parent branch for cascading, otherwise default.
func (m *Manager) CreatePR(ctx context.Context, opts PROptions) (*PRResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current directory: %w", err)
	}

	// Get current branch
	result, err := m.git.Run(ctx, []string{"branch", "--show-current"}, cwd)
	if err != nil {
		return nil, fmt.Errorf("failed to get current branch: %w", err)
	}
	currentBranch := strings.TrimSpace(result.Stdout)
	if currentBranch == "" {
		return nil, fmt.Errorf("not on a branch (detached HEAD?)")
	}

	bareDir := m.BareDir()
	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)

	// Prevent PR for default branch
	if currentBranch == defaultBranch {
		return nil, fmt.Errorf("cannot create PR for the default branch (%s)", defaultBranch)
	}

	// Check if PR already exists
	existingPR, err := GetPRByBranch(ctx, m.gh, currentBranch, cwd)
	if err == nil && existingPR != nil && existingPR.Number > 0 && existingPR.State == "OPEN" {
		m.output.Info(fmt.Sprintf("PR #%d already exists for branch %s", existingPR.Number, currentBranch))
		return &PRResult{
			Number:  existingPR.Number,
			URL:     existingPR.URL,
			Branch:  currentBranch,
			Base:    existingPR.BaseRefName,
			Existed: true,
		}, nil
	}

	// Determine base branch
	baseBranch := opts.Base
	if baseBranch == "" {
		parentBranch, _ := m.GetParentBranch(ctx, currentBranch, cwd)
		if parentBranch != "" {
			baseBranch = parentBranch
			m.output.Info(fmt.Sprintf("Using parent branch: %s", baseBranch))
		} else {
			baseBranch = defaultBranch
		}
	}

	// Push branch to remote
	if !opts.NoPush {
		m.output.Info(fmt.Sprintf("Pushing %s to origin...", currentBranch))
		_, err := m.git.Run(ctx, []string{"push", "-u", "origin", currentBranch}, cwd)
		if err != nil {
			return nil, fmt.Errorf("failed to push branch: %w", err)
		}
	}

	// Create PR
	m.output.Info(fmt.Sprintf("Creating PR: %s -> %s", currentBranch, baseBranch))
	prInfo, err := CreatePR(ctx, m.gh, opts.Title, opts.Body, baseBranch, opts.Draft, cwd)
	if err != nil {
		return nil, fmt.Errorf("failed to create PR: %w", err)
	}

	m.output.Success(fmt.Sprintf("Created PR #%d: %s", prInfo.Number, prInfo.URL))

	return &PRResult{
		Number: prInfo.Number,
		URL:    prInfo.URL,
		Branch: currentBranch,
		Base:   baseBranch,
	}, nil
}

// WorktreeInfo contains extended information about a worktree.
// This combines Worktree data with branch metadata like goals and parent.
type WorktreeInfo struct {
	LastCommitTime time.Time
	Goal           string
	Parent         string
	PRState        string
	PRURL          string
	LastCommitMsg  string
	Worktree       Worktree
	Ahead          int
	Behind         int
	PRNumber       int
	IsMerged       bool
	IsAhead        bool
	IsDirty        bool
}

// GetWorktreeByBranch returns a Worktree by branch name.
func (m *Manager) GetWorktreeByBranch(ctx context.Context, branch string) (*Worktree, error) {
	worktrees, err := m.List(ctx)
	if err != nil {
		return nil, err
	}

	for i := range worktrees {
		if worktrees[i].Branch == branch {
			return &worktrees[i], nil
		}
	}

	return nil, ErrWorktreeNotFound
}

// GetWorktreeInfo returns extended information about a worktree.
func (m *Manager) GetWorktreeInfo(ctx context.Context, branch string) (*WorktreeInfo, error) {
	wt, err := m.GetWorktreeByBranch(ctx, branch)
	if err != nil {
		return nil, err
	}

	info := &WorktreeInfo{
		Worktree: *wt,
	}

	// Get goal
	goal, _ := m.GetGoal(ctx, branch, wt.Path)
	info.Goal = goal

	// Get parent
	parent, _ := m.GetParentBranch(ctx, branch, wt.Path)
	info.Parent = parent

	// Get status
	status, err := m.GetStatus(ctx, *wt)
	if err == nil {
		info.IsDirty = status.IsDirty
		info.Ahead = status.Ahead
		info.Behind = status.Behind
		info.IsAhead = status.Ahead > 0
		info.PRState = status.PRState
		info.PRURL = status.PRURL
		info.PRNumber = status.PRNumber
		info.IsMerged = status.PRState == "MERGED"
		info.LastCommitTime = status.LastCommitTime
		info.LastCommitMsg = status.LastCommitMsg
	}

	return info, nil
}

// GetAllWorktreeInfo returns extended information for all worktrees.
func (m *Manager) GetAllWorktreeInfo(ctx context.Context) ([]*WorktreeInfo, error) {
	worktrees, err := m.List(ctx)
	if err != nil {
		return nil, err
	}

	infos := make([]*WorktreeInfo, 0, len(worktrees))
	for _, wt := range worktrees {
		info, err := m.GetWorktreeInfo(ctx, wt.Branch)
		if err != nil {
			continue // Skip worktrees we can't get info for
		}
		infos = append(infos, info)
	}

	return infos, nil
}
