// Command fixer provides automated CI failure remediation.
package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var (
	repoRoot    string
	repoName    string
	trackerPath string
	sessionDir  string
	dryRun      bool
	verbose     bool
)

var rootCmd = &cobra.Command{
	Use:   "fixer",
	Short: "Automated CI failure remediation",
	Long: `Fixer scans GitHub Actions failures, categorizes them, launches
Claude agents to investigate and fix each problem, creates PRs,
and tracks the lifecycle through merge and verification.`,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&repoRoot, "repo-root", "", "Repository worktree root (auto-detected if unset)")
	rootCmd.PersistentFlags().StringVar(&repoName, "repo-name", "", "Repository name for GitHub API (e.g. owner/repo)")
	rootCmd.PersistentFlags().StringVar(&trackerPath, "tracker", "", "Path to issues.json (default: <repo-root>/.fixer/issues.json)")
	rootCmd.PersistentFlags().StringVar(&sessionDir, "session-dir", "", "Session recording directory (default: <repo-root>/.fixer/sessions)")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Show what would be done without making changes")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// resolveRepoRoot finds the repo root from flags or git.
func resolveRepoRoot() (string, error) {
	if repoRoot != "" {
		return repoRoot, nil
	}
	// Default to current directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return cwd, nil
}

// resolveTrackerPath returns the tracker file path.
func resolveTrackerPath(root string) string {
	if trackerPath != "" {
		return trackerPath
	}
	return filepath.Join(root, ".fixer", "issues.json")
}

// resolveSessionDir returns the session directory.
func resolveSessionDir(root string) string {
	if sessionDir != "" {
		return sessionDir
	}
	return filepath.Join(root, ".fixer", "sessions")
}

// newLogger creates a structured logger with the configured verbosity.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
