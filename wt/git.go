package wt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CmdResult holds command execution results.
type CmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// GitRunner executes git commands.
type GitRunner interface {
	Run(ctx context.Context, args []string, dir string) (*CmdResult, error)
}

// DefaultGitRunner implements GitRunner using os/exec.
type DefaultGitRunner struct{}

// Run executes a git command.
func (r *DefaultGitRunner) Run(ctx context.Context, args []string, dir string) (*CmdResult, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}

	stdout, err := cmd.Output()
	result := &CmdResult{
		Stdout: string(stdout),
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		result.Stderr = string(exitErr.Stderr)
		result.ExitCode = exitErr.ExitCode()
		return result, err
	}

	return result, err
}

// GetRepoNameFromURL extracts the repository name from a Git URL.
func GetRepoNameFromURL(url string) string {
	// Handle SSH URLs: git@github.com:user/repo.git
	if strings.HasPrefix(url, "git@") {
		parts := strings.Split(url, ":")
		if len(parts) >= 2 {
			path := parts[len(parts)-1]
			return strings.TrimSuffix(filepath.Base(path), ".git")
		}
	}

	// Handle HTTPS URLs: https://github.com/user/repo.git
	path := url
	if idx := strings.LastIndex(url, "/"); idx >= 0 {
		path = url[idx+1:]
	}
	return strings.TrimSuffix(path, ".git")
}

// GetDefaultBranch returns the default branch for a repository.
func GetDefaultBranch(ctx context.Context, runner GitRunner, repoPath string) (string, error) {
	// Try to get from symbolic ref
	result, err := runner.Run(ctx, []string{"symbolic-ref", "refs/remotes/origin/HEAD"}, repoPath)
	if err == nil {
		branch := strings.TrimSpace(result.Stdout)
		branch = strings.TrimPrefix(branch, "refs/remotes/origin/")
		return branch, nil
	}

	// Fall back to checking main/master
	for _, branch := range []string{"main", "master"} {
		_, err := runner.Run(ctx, []string{"rev-parse", "refs/heads/" + branch}, repoPath)
		if err == nil {
			return branch, nil
		}
	}

	return "main", nil
}

// GetCurrentRepoName determines the repository name from the current directory.
// It first checks if we're in a wt-managed worktree, then falls back to git remote.
func GetCurrentRepoName(ctx context.Context, runner GitRunner, root string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	// Check if in a wt-managed worktree using filepath.Rel for correctness
	// (strings.HasPrefix would incorrectly match /worktrees-old as inside /worktrees)
	relative, err := filepath.Rel(root, cwd)
	if err == nil && !strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative) {
		// Walk up the path to find the directory containing .bare
		checkPath := root
		parts := strings.Split(relative, string(filepath.Separator))
		for i, part := range parts {
			checkPath = filepath.Join(checkPath, part)
			if _, err := os.Stat(filepath.Join(checkPath, ".bare")); err == nil {
				// Return the path relative to root as the repo name
				return filepath.Join(parts[:i+1]...), nil
			}
		}
	}

	// Fall back to git remote
	result, err := runner.Run(ctx, []string{"remote", "get-url", "origin"}, cwd)
	if err != nil {
		return "", err
	}
	return GetRepoNameFromURL(strings.TrimSpace(result.Stdout)), nil
}

// ListAllRepos lists all wt-managed repository names under the given root.
func ListAllRepos(root string) ([]string, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}

	var repos []string
	var findRepos func(path string, prefix string) error

	findRepos = func(path string, prefix string) error {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			repoName := entry.Name()
			if prefix != "" {
				repoName = prefix + "/" + entry.Name()
			}

			childPath := filepath.Join(path, entry.Name())
			if _, err := os.Stat(filepath.Join(childPath, ".bare")); err == nil {
				repos = append(repos, repoName)
			} else if strings.Count(prefix, "/") < 3 {
				// Recurse into subdirectories (max 4 levels deep)
				findRepos(childPath, repoName)
			}
		}
		return nil
	}

	err := findRepos(root, "")
	return repos, err
}
