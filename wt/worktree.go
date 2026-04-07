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
	# Legacy names
	post_create:
	  - npm install
	post_remove:
	  - echo "cleaned up"
	# Bramble names
	on_worktree_create:
	  - npm install
	on_worktree_delete:
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

// GitRunner returns the git runner used by this manager.
func (m *Manager) GitRunner() GitRunner {
	return m.git
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
	config, err := LoadRepoConfig(mainPath)
	if err != nil {
		m.output.Warn(fmt.Sprintf("Failed to load repo config, skipping hooks: %v", err))
	} else {
		createCommands := config.WorktreeCreateCommands()
		if len(createCommands) > 0 {
			if err := RunHooks(createCommands, mainPath, defaultBranch, m.output); err != nil {
				m.output.Warn(fmt.Sprintf("Post-create hook failed: %v", err))
			}
		}
	}

	return mainPath, nil
}

// FetchOrigin fetches the default branch from origin for this repo's bare clone.
// Call this before parallel New calls to avoid concurrent git-fetch conflicts.
func (m *Manager) FetchOrigin(ctx context.Context) error {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return ErrRepoNotInitialized
	}
	if err := CheckGitHubAuth(ctx, m.gh); err != nil {
		return err
	}
	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)
	m.output.Info(fmt.Sprintf("Fetching %s from origin...", defaultBranch))
	result, err := m.git.Run(ctx, []string{"fetch", "origin", defaultBranch}, bareDir)
	if err != nil {
		return fmt.Errorf("failed to fetch from origin: %w", wrapAuthError(err, result))
	}
	return nil
}

// fetchBaseBranchIfStacked fetches baseBranch from origin when it differs from
// the default branch. This is needed for stacked/dependent worktrees whose
// parent is not the repo's default branch (e.g. feature-a → feature-b).
// It is a no-op when baseBranch is already the default branch (already fetched
// by FetchOrigin).
func (m *Manager) fetchBaseBranchIfStacked(ctx context.Context, baseBranch string) error {
	bareDir := m.BareDir()
	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)
	if baseBranch == defaultBranch {
		return nil
	}
	m.output.Info(fmt.Sprintf("Fetching base branch %s...", baseBranch))
	result, err := m.git.Run(ctx, []string{"fetch", "origin", baseBranch}, bareDir)
	if err != nil {
		return fmt.Errorf("failed to fetch base branch %s: %w", baseBranch, wrapAuthError(err, result))
	}
	return nil
}

// SyncOptions configures optional behavior for Sync.
type SyncOptions struct {
	FetchAll bool // fetch all remote branches instead of only the default branch
}

// NewOptions configures optional behavior for New.
type NewOptions struct {
	SkipFetch bool // skip git-fetch (caller already fetched)
}

// SyncDefaultBranch fast-forwards the local default branch to match origin.
// This keeps the main worktree current when creating new worktrees.
// It's safe to call even if the main worktree doesn't exist (no-op in that case).
// All errors are handled internally; the function is intentionally best-effort.
func (m *Manager) SyncDefaultBranch(ctx context.Context) {
	bareDir := m.BareDir()
	defaultBranch, err := GetDefaultBranch(ctx, m.git, bareDir)
	if err != nil {
		m.output.Warn(fmt.Sprintf("Skipping default branch sync: could not determine default branch: %v", err))
		return
	}

	mainPath := filepath.Join(m.RepoDir(), defaultBranch)
	if _, err := os.Stat(mainPath); os.IsNotExist(err) {
		return // main worktree doesn't exist, nothing to sync
	}

	// Check if currently on the default branch in that worktree
	result, err := m.git.Run(ctx, []string{"branch", "--show-current"}, mainPath)
	if err != nil || strings.TrimSpace(result.Stdout) != defaultBranch {
		return // not on default branch (detached HEAD, etc.), skip
	}

	// Check for uncommitted changes that would block a pull
	statusResult, err := m.git.Run(ctx, []string{"status", "--porcelain"}, mainPath)
	if err != nil {
		return // can't check status, skip silently
	}
	if strings.TrimSpace(statusResult.Stdout) != "" {
		m.output.Warn(fmt.Sprintf("Skipping %s sync: worktree has uncommitted changes", defaultBranch))
		return
	}

	m.output.Info(fmt.Sprintf("Fast-forwarding %s to origin/%s...", defaultBranch, defaultBranch))
	if _, err := m.git.Run(ctx, []string{"merge", "--ff-only", "origin/" + defaultBranch}, mainPath); err != nil {
		m.output.Warn(fmt.Sprintf("Could not fast-forward %s (may have local commits)", defaultBranch))
		return // non-fatal: don't block worktree creation
	}
	m.output.Success(fmt.Sprintf("Updated %s to latest", defaultBranch))
}

// New creates a new worktree with a new branch.
func (m *Manager) New(ctx context.Context, branch, baseBranch, goal string, opts ...NewOptions) (string, error) {
	var o NewOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return "", ErrRepoNotInitialized
	}

	worktreePath := filepath.Join(m.RepoDir(), branch)
	if _, err := os.Stat(worktreePath); err == nil {
		// If the existing worktree already has the requested branch, reuse it.
		result, gitErr := m.git.Run(ctx, []string{"branch", "--show-current"}, worktreePath)
		if gitErr == nil && strings.TrimSpace(result.Stdout) == branch {
			m.output.Info(fmt.Sprintf("Reusing existing worktree for %s", branch))
			if goal != "" {
				if err := SetBranchGoal(ctx, m.git, branch, goal, worktreePath); err != nil {
					m.output.Warn(fmt.Sprintf("Failed to set goal: %v", err))
				}
			}
			return worktreePath, nil
		}
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

	if !o.SkipFetch {
		if err := m.FetchOrigin(ctx); err != nil {
			return "", err
		}
		if err := m.fetchBaseBranchIfStacked(ctx, baseBranch); err != nil {
			return "", err
		}
	}

	// Keep the main worktree current
	m.SyncDefaultBranch(ctx)

	// Prune stale worktree metadata (prevents exit 128 from deleted worktrees).
	m.git.Run(ctx, []string{"worktree", "prune"}, bareDir)

	m.output.Info(fmt.Sprintf("Creating worktree %s from %s...", branch, baseBranch))
	if result, err := m.git.Run(ctx, []string{
		"worktree", "add", "-b", branch, worktreePath, "origin/" + baseBranch,
	}, bareDir); err != nil {
		if result != nil {
			if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
				return "", fmt.Errorf("failed to create worktree: %s: %w", stderr, err)
			}
		}
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

	m.output.Info(fmt.Sprintf("Fetching %s from origin...", branch))
	if _, fetchErr := m.git.Run(ctx, []string{"fetch", "origin", branch}, bareDir); fetchErr != nil {
		// If the fetch failed, check whether the branch actually exists on the remote.
		// A missing branch is the most common cause of fetch failure for a specific ref.
		if _, revErr := m.git.Run(ctx, []string{
			"ls-remote", "--exit-code", "origin", "refs/heads/" + branch,
		}, bareDir); revErr != nil {
			return "", ErrBranchNotFound
		}
		return "", fmt.Errorf("failed to fetch %s from origin: %w", branch, fetchErr)
	}

	// Confirm the ref landed locally after a successful fetch
	if _, err := m.git.Run(ctx, []string{
		"rev-parse", "refs/remotes/origin/" + branch,
	}, bareDir); err != nil {
		return "", ErrBranchNotFound
	}

	m.output.Info(fmt.Sprintf("Creating worktree for %s...", branch))
	if result, err := m.git.Run(ctx, []string{
		"worktree", "add", worktreePath, branch,
	}, bareDir); err != nil {
		if result != nil {
			if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
				return "", fmt.Errorf("failed to create worktree: %s: %w", stderr, err)
			}
		}
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
// dir must be a valid Git worktree path (not the bare repo parent)
// because gh requires a Git repository context.
func (m *Manager) FetchAllPRInfo(ctx context.Context, dir string) ([]PRInfo, error) {
	return ListOpenPRs(ctx, m.gh, dir)
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
func (m *Manager) Remove(ctx context.Context, nameOrBranch string, deleteBranch bool, force bool) error {
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
	config, err := LoadRepoConfig(worktreePath)
	if err != nil {
		m.output.Warn(fmt.Sprintf("Failed to load repo config, skipping hooks: %v", err))
	} else {
		deleteCommands := config.WorktreeDeleteCommands()
		if len(deleteCommands) > 0 {
			if err := RunHooks(deleteCommands, worktreePath, branchName, m.output); err != nil {
				m.output.Warn(fmt.Sprintf("Post-remove hook failed: %v", err))
			}
		}
	}

	m.output.Info(fmt.Sprintf("Removing worktree %s...", branchName))
	removeArgs := []string{"worktree", "remove"}
	if force {
		removeArgs = append(removeArgs, "--force")
	}
	removeArgs = append(removeArgs, worktreePath)
	if result, err := m.git.Run(ctx, removeArgs, bareDir); err != nil {
		stderr := ""
		if result != nil {
			stderr = strings.TrimSpace(result.Stderr)
		}
		if stderr != "" {
			return fmt.Errorf("failed to remove worktree: %s", stderr)
		}
		return fmt.Errorf("failed to remove worktree: %w", err)
	}
	m.output.Success(fmt.Sprintf("Removed worktree %s", branchName))

	if deleteBranch {
		// Prune stale worktree metadata so git doesn't think the branch
		// is still checked out (this prevents exit code 128 errors).
		m.git.Run(ctx, []string{"worktree", "prune"}, bareDir)

		m.output.Info(fmt.Sprintf("Deleting local branch %s...", branchName))
		if result, err := m.git.Run(ctx, []string{"branch", "-D", branchName}, bareDir); err != nil {
			stderr := ""
			if result != nil {
				stderr = strings.TrimSpace(result.Stderr)
			}
			if stderr != "" {
				m.output.Error(fmt.Sprintf("Failed to delete local branch %s: %s", branchName, stderr))
			} else {
				m.output.Error(fmt.Sprintf("Failed to delete local branch %s: %v", branchName, err))
			}
		} else {
			m.output.Success(fmt.Sprintf("Deleted local branch %s", branchName))
		}

		m.output.Info(fmt.Sprintf("Deleting remote branch %s...", branchName))
		if result, err := m.git.Run(ctx, []string{"push", "origin", "--delete", branchName}, bareDir); err != nil {
			if result != nil && IsAuthError(result.Stderr) {
				m.output.Error(fmt.Sprintf("Failed to delete remote branch %s: %v", branchName, ErrGitHubAuthRequired))
			} else {
				stderr := ""
				if result != nil {
					stderr = strings.TrimSpace(result.Stderr)
				}
				if stderr != "" {
					m.output.Warn(fmt.Sprintf("Remote branch %s may not exist: %s", branchName, stderr))
				} else {
					m.output.Warn(fmt.Sprintf("Remote branch %s may not exist: %v", branchName, err))
				}
			}
		} else {
			m.output.Success(fmt.Sprintf("Deleted remote branch %s", branchName))
		}
	}

	return nil
}

// Sync fetches the latest changes and rebases worktrees.
// If branch is non-empty, only that worktree is synced.
// If branch is empty, all worktrees in the repo are synced.
func (m *Manager) Sync(ctx context.Context, branch string, opts ...SyncOptions) error {
	var o SyncOptions
	if len(opts) > 0 {
		o = opts[0]
	}

	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return ErrRepoNotInitialized
	}

	if err := CheckGitHubAuth(ctx, m.gh); err != nil {
		return err
	}

	worktrees, err := m.List(ctx)
	if err != nil {
		return err
	}

	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)

	if o.FetchAll {
		m.output.Info("Fetching all branches from origin...")
		result, err := m.git.Run(ctx, []string{"fetch", "--all", "--prune"}, bareDir)
		if err != nil {
			return fmt.Errorf("failed to fetch: %w", wrapAuthError(err, result))
		}
	} else {
		// Fetch only the default branch and any non-merged parent branches needed for stacked worktrees
		m.output.Info(fmt.Sprintf("Fetching %s from origin...", defaultBranch))
		result, err := m.git.Run(ctx, []string{"fetch", "origin", defaultBranch}, bareDir)
		if err != nil {
			return fmt.Errorf("failed to fetch %s: %w", defaultBranch, wrapAuthError(err, result))
		}

		// Collect unique parent branches that need fetching (non-default, non-local-only).
		// When syncing a single branch, only fetch that branch's parent chain.
		// When syncing all branches, fetch parents for all worktrees.
		var worktreesToCheck []Worktree
		if branch != "" {
			for _, wt := range worktrees {
				if wt.Branch == branch {
					worktreesToCheck = []Worktree{wt}
					break
				}
			}
		} else {
			worktreesToCheck = worktrees
		}
		fetched := map[string]bool{defaultBranch: true}
		for _, wt := range worktreesToCheck {
			if wt.IsDetached {
				continue
			}
			parent, _ := m.GetParentBranch(ctx, wt.Branch, wt.Path)
			if parent != "" && parent != defaultBranch && !fetched[parent] {
				fetched[parent] = true
				m.output.Info(fmt.Sprintf("Fetching parent branch %s...", parent))
				if result, err := m.git.Run(ctx, []string{"fetch", "origin", parent}, bareDir); err != nil {
					// Check if the branch was deleted/merged on remote (non-fatal) vs a real
					// network/auth error (fatal: continuing would rebase onto stale refs).
					exists, existsErr := RemoteBranchExists(ctx, m.git, parent, bareDir)
					if existsErr == nil && !exists {
						m.output.Warn(fmt.Sprintf("Skipping %s: branch no longer exists on remote (merged?)", parent))
						continue
					}
					return fmt.Errorf("failed to fetch parent branch %s: %w", parent, wrapAuthError(err, result))
				}
			}
		}
	}
	m.output.Success("Fetched latest changes")

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

	prNumber, err := m.mergePR(ctx, currentBranch, cwd, opts)
	if err != nil {
		return err
	}

	bareDir := m.BareDir()
	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)

	// Cleanup unless --keep
	if !opts.Keep {
		// Navigate away from current worktree before removing it
		m.output.Info("Navigating to default branch worktree...")
		fmt.Printf("__WT_CD__:%s\n", filepath.Join(m.RepoDir(), defaultBranch))

		if err := m.Remove(ctx, currentBranch, true, false); err != nil {
			m.output.Warn(fmt.Sprintf("Failed to cleanup worktree: %v", err))
		}
	}

	_ = prNumber
	return nil
}

// MergePRForBranch merges the PR for the given branch. Unlike MergePR, it does
// not rely on os.Getwd() and always keeps the worktree (caller handles cleanup).
func (m *Manager) MergePRForBranch(ctx context.Context, branch string, opts MergeOptions) (int, error) {
	return m.mergePR(ctx, branch, m.BareDir(), opts)
}

// mergePR is the shared implementation for MergePR and MergePRForBranch.
// It looks up the PR, merges it, fetches, and handles child branches.
func (m *Manager) mergePR(ctx context.Context, branch, dir string, opts MergeOptions) (int, error) {
	bareDir := m.BareDir()
	defaultBranch, _ := GetDefaultBranch(ctx, m.git, bareDir)

	if branch == defaultBranch {
		return 0, fmt.Errorf("cannot merge the default branch (%s)", defaultBranch)
	}

	// Get PR info for the branch
	prInfo, err := GetPRByBranch(ctx, m.gh, branch, dir)
	if err != nil {
		return 0, fmt.Errorf("no PR found for branch %s: %w", branch, err)
	}

	if prInfo.ReviewDecision != "" && prInfo.ReviewDecision != "APPROVED" {
		m.output.Warn(fmt.Sprintf("PR #%d review status: %s", prInfo.Number, prInfo.ReviewDecision))
	}

	m.output.Info(fmt.Sprintf("Merging PR #%d for branch %s...", prInfo.Number, branch))

	// Find child branches BEFORE merging
	childDeps, err := m.findChildBranches(ctx, branch, dir)
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

	if _, err := m.gh.Run(ctx, mergeArgs, dir); err != nil {
		return 0, fmt.Errorf("failed to merge PR: %w", err)
	}
	m.output.Success(fmt.Sprintf("Merged PR #%d", prInfo.Number))

	// Fetch to get updated remote state
	m.git.Run(ctx, []string{"fetch", "--prune"}, bareDir)

	// Handle child branches
	if len(childDeps) > 0 {
		m.output.Info(fmt.Sprintf("Found %d child branches depending on %s", len(childDeps), branch))
		m.handleChildBranches(ctx, childDeps, defaultBranch)
	}

	return prInfo.Number, nil
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
			if result, err := m.git.Run(ctx, []string{"push", "--force-with-lease"}, child.WorktreePath); err != nil {
				m.output.Error(fmt.Sprintf("Failed to push %s: %v", child.Branch, wrapAuthError(err, result)))
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

// protectedBranches returns the set of branches that should never be deleted.
func protectedBranches(ctx context.Context, git GitRunner, bareDir string) map[string]bool {
	defaultBranch, _ := GetDefaultBranch(ctx, git, bareDir)
	return map[string]bool{
		defaultBranch: true,
		"main":        true,
		"master":      true,
	}
}

// PruneOptions configures prune behavior.
type PruneOptions struct {
	DryRun    bool // Preview only, no changes
	MergedPRs bool // Also remove worktrees whose GitHub PRs are merged
}

// PruneResult contains the results of pruning.
type PruneResult struct {
	StaleWorktrees  []string // Lines from git worktree prune
	MergedWorktrees []string // Worktrees removed because their PR was merged
}

// Prune cleans stale worktree metadata and optionally removes worktrees
// whose GitHub PRs have been merged.
func (m *Manager) Prune(ctx context.Context, opts PruneOptions) (*PruneResult, error) {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return nil, ErrRepoNotInitialized
	}

	result := &PruneResult{}

	args := []string{"worktree", "prune"}
	if opts.DryRun {
		args = append(args, "--dry-run", "-v")
	}

	gitResult, err := m.git.Run(ctx, args, bareDir)
	if err != nil {
		return nil, err
	}

	if gitResult.Stdout != "" {
		result.StaleWorktrees = strings.Split(strings.TrimSpace(gitResult.Stdout), "\n")
	} else {
		m.output.Success("No stale worktrees to prune")
	}

	if opts.MergedPRs {
		merged, err := m.pruneMergedPRs(ctx, bareDir, opts.DryRun)
		if err != nil {
			m.output.Warn(fmt.Sprintf("Merged PR check failed: %v", err))
		} else {
			result.MergedWorktrees = merged
		}
	}

	return result, nil
}

// pruneMergedPRs finds and removes worktrees whose PRs are merged.
func (m *Manager) pruneMergedPRs(ctx context.Context, bareDir string, dryRun bool) ([]string, error) {
	protected := protectedBranches(ctx, m.git, bareDir)

	worktrees, err := m.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	mergedPRs, err := ListMergedPRs(ctx, m.gh, bareDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list merged PRs: %w", err)
	}

	if len(mergedPRs) >= 200 {
		m.output.Warn("GitHub returned 200 merged PRs (limit reached); older merged-PR worktrees may not be detected")
	}

	mergedByBranch := make(map[string]*PRInfo, len(mergedPRs))
	for i := range mergedPRs {
		mergedByBranch[mergedPRs[i].HeadRefName] = &mergedPRs[i]
	}

	removed := []string{}
	for _, wt := range worktrees {
		if wt.IsDetached || protected[wt.Branch] {
			continue
		}

		pr, ok := mergedByBranch[wt.Branch]
		if !ok {
			continue
		}

		if dryRun {
			m.output.Info(fmt.Sprintf("[dry-run] Would remove %s (PR #%d merged)", wt.Branch, pr.Number))
			removed = append(removed, wt.Branch)
			continue
		}

		m.output.Info(fmt.Sprintf("Removing %s (PR #%d merged)...", wt.Branch, pr.Number))
		if err := m.Remove(ctx, wt.Branch, true, true); err != nil {
			m.output.Error(fmt.Sprintf("Failed to remove %s: %v", wt.Branch, err))
			continue
		}
		removed = append(removed, wt.Branch)
	}

	if len(removed) == 0 {
		m.output.Success("No worktrees with merged PRs to remove")
	}

	return removed, nil
}

// GCOptions configures garbage collection behavior.
type GCOptions struct {
	DryRun         bool // Preview only, no changes
	DeleteBranches bool // Delete orphaned local branches
	DeleteRemote   bool // Also delete remote branches (requires DeleteBranches)
	MergedPRs      bool // Also remove worktrees whose GitHub PRs are merged
}

// GCResult contains the results of garbage collection.
type GCResult struct {
	PrunedWorktrees  []string // Lines from git worktree prune
	MergedWorktrees  []string // Worktrees removed because their PR was merged
	OrphanedBranches []string // Local branches with no worktree
	DeletedBranches  []string // Actually deleted local branches
	DeletedRemote    []string // Actually deleted remote branches
	FetchPruned      bool     // Whether fetch --prune ran
	GCRan            bool     // Whether git gc ran
}

// GC performs comprehensive garbage collection: prunes stale worktree metadata,
// fetches and prunes remote refs, detects and optionally deletes orphaned branches,
// and runs git gc.
func (m *Manager) GC(ctx context.Context, opts GCOptions) (*GCResult, error) {
	bareDir := m.BareDir()
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		return nil, ErrRepoNotInitialized
	}

	result := &GCResult{}

	// Step 1: git worktree prune — delegate to m.Prune to avoid duplicating logic.
	pruned, err := m.Prune(ctx, PruneOptions{
		DryRun:    opts.DryRun,
		MergedPRs: opts.MergedPRs,
	})
	if err != nil {
		return nil, fmt.Errorf("worktree prune failed: %w", err)
	}
	result.PrunedWorktrees = pruned.StaleWorktrees
	result.MergedWorktrees = pruned.MergedWorktrees
	for _, line := range pruned.StaleWorktrees {
		m.output.Info(fmt.Sprintf("Pruned: %s", line))
	}

	// Step 2: git fetch --prune
	if !opts.DryRun {
		m.output.Info("Fetching and pruning remote refs...")
		if _, err := m.git.Run(ctx, []string{"fetch", "--prune"}, bareDir); err != nil {
			m.output.Warn(fmt.Sprintf("fetch --prune failed: %v", err))
		} else {
			result.FetchPruned = true
			m.output.Success("Fetched and pruned remote refs")
		}
	} else {
		m.output.Info("[dry-run] Would run: git fetch --prune")
	}

	// Step 3: Detect orphaned branches
	protected := protectedBranches(ctx, m.git, bareDir)

	branchResult, err := m.git.Run(ctx, []string{"branch", "--list", "--format=%(refname:short)"}, bareDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	var allBranches []string
	for _, line := range strings.Split(strings.TrimSpace(branchResult.Stdout), "\n") {
		branch := strings.TrimSpace(line)
		if branch != "" {
			allBranches = append(allBranches, branch)
		}
	}

	worktrees, err := m.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}
	activeBranches := make(map[string]bool)
	for _, wt := range worktrees {
		activeBranches[wt.Branch] = true
	}

	for _, branch := range allBranches {
		if !protected[branch] && !activeBranches[branch] {
			result.OrphanedBranches = append(result.OrphanedBranches, branch)
		}
	}

	if len(result.OrphanedBranches) > 0 {
		m.output.Info(fmt.Sprintf("Found %d orphaned branch(es):", len(result.OrphanedBranches)))
		for _, branch := range result.OrphanedBranches {
			m.output.Info(fmt.Sprintf("  %s", branch))
		}
	} else {
		m.output.Success("No orphaned branches found")
	}

	// Step 4: Delete orphaned local branches
	if opts.DeleteBranches && len(result.OrphanedBranches) > 0 {
		for _, branch := range result.OrphanedBranches {
			if opts.DryRun {
				m.output.Info(fmt.Sprintf("[dry-run] Would delete local branch: %s", branch))
				continue
			}
			if delResult, err := m.git.Run(ctx, []string{"branch", "-D", branch}, bareDir); err != nil {
				stderr := ""
				if delResult != nil {
					stderr = strings.TrimSpace(delResult.Stderr)
				}
				if stderr != "" {
					m.output.Error(fmt.Sprintf("Failed to delete %s: %s", branch, stderr))
				} else {
					m.output.Error(fmt.Sprintf("Failed to delete %s: %v", branch, err))
				}
			} else {
				result.DeletedBranches = append(result.DeletedBranches, branch)
				m.output.Success(fmt.Sprintf("Deleted local branch %s", branch))
			}
		}
	}

	// Step 5: Delete remote branches (only for branches whose local deletion succeeded)
	if opts.DeleteBranches && opts.DeleteRemote {
		remoteBranches := result.DeletedBranches
		if opts.DryRun {
			// In dry-run mode, no local deletions have happened yet; use orphaned list
			remoteBranches = result.OrphanedBranches
		}
		for _, branch := range remoteBranches {
			if opts.DryRun {
				m.output.Info(fmt.Sprintf("[dry-run] Would delete remote branch: %s", branch))
				continue
			}
			if delResult, err := m.git.Run(ctx, []string{"push", "origin", "--delete", branch}, bareDir); err != nil {
				stderr := ""
				if delResult != nil {
					stderr = strings.TrimSpace(delResult.Stderr)
				}
				if stderr != "" {
					m.output.Warn(fmt.Sprintf("Remote branch %s may not exist: %s", branch, stderr))
				} else {
					m.output.Warn(fmt.Sprintf("Remote branch %s may not exist: %v", branch, err))
				}
			} else {
				result.DeletedRemote = append(result.DeletedRemote, branch)
				m.output.Success(fmt.Sprintf("Deleted remote branch %s", branch))
			}
		}
	}

	// Step 6: git gc
	if !opts.DryRun {
		m.output.Info("Running git gc...")
		if _, err := m.git.Run(ctx, []string{"gc"}, bareDir); err != nil {
			m.output.Warn(fmt.Sprintf("git gc failed: %v", err))
		} else {
			result.GCRan = true
			m.output.Success("Git garbage collection complete")
		}
	} else {
		m.output.Info("[dry-run] Would run: git gc")
	}

	return result, nil
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
		result, err := m.git.Run(ctx, []string{"push", "-u", "origin", currentBranch}, cwd)
		if err != nil {
			return nil, fmt.Errorf("failed to push branch: %w", wrapAuthError(err, result))
		}
	}

	// Create PR
	m.output.Info(fmt.Sprintf("Creating PR: %s -> %s", currentBranch, baseBranch))
	prInfo, err := CreatePR(ctx, m.gh, opts.Title, opts.Body, baseBranch, currentBranch, opts.Draft, cwd)
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
