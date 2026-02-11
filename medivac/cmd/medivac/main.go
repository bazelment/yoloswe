// Command medivac provides automated CI failure remediation.
package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var (
	repoRoot    string
	trackerPath string
	sessionDir  string
	dryRun      bool
	verbose     bool
)

var rootCmd = &cobra.Command{
	Use:   "medivac",
	Short: "Automated CI failure remediation",
	Long: `Medivac scans GitHub Actions failures, categorizes them, launches
Claude agents to investigate and fix each problem, creates PRs,
and tracks the lifecycle through merge and verification.`,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&repoRoot, "repo-root", "", "Repository worktree root (auto-detected if unset)")
	rootCmd.PersistentFlags().StringVar(&trackerPath, "tracker", "", "Path to issues.json (default: <repo-root>/.medivac/issues.json)")
	rootCmd.PersistentFlags().StringVar(&sessionDir, "session-dir", "", "Session recording directory (default: <repo-root>/.medivac/sessions)")
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
	return filepath.Join(root, ".medivac", "issues.json")
}

// resolveSessionDir returns the session directory.
func resolveSessionDir(root string) string {
	if sessionDir != "" {
		return sessionDir
	}
	return filepath.Join(root, ".medivac", "sessions")
}

// resolveWTRoot walks up from the repo root to find the wt-managed repository
// root (the directory containing .bare). Returns (wtRoot, repoName) where
// wtRoot is the parent of the repo dir and repoName is the repo directory name.
// For example, given /Users/x/worktrees/org/kernel/feature/scanner where
// /Users/x/worktrees/org/kernel/.bare exists, returns ("/Users/x/worktrees/org", "kernel").
func resolveWTRoot(repoRoot string) (string, string, error) {
	dir := repoRoot
	for {
		bare := filepath.Join(dir, ".bare")
		if info, err := os.Stat(bare); err == nil && info.IsDir() {
			return filepath.Dir(dir), filepath.Base(dir), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding .bare
			// Fall back to simple parent/base split
			return filepath.Dir(repoRoot), filepath.Base(repoRoot), nil
		}
		dir = parent
	}
}

// newLogger creates a structured logger that writes to stderr and to a
// timestamped log file in <root>/.medivac/logs/ so runs can be revisited later.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// newFileLogger creates a logger that writes to both stderr and a persistent
// log file under <root>/.medivac/logs/. Returns the logger, the log file path,
// and a cleanup function to close the log file.
func newFileLogger(root string) (*slog.Logger, string, func()) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	logDir := filepath.Join(root, ".medivac", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		// Fall back to stderr-only.
		return newLogger(), "", func() {}
	}

	logFile := filepath.Join(logDir, time.Now().Format("2006-01-02T15-04-05")+".log")
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return newLogger(), "", func() {}
	}

	w := io.MultiWriter(os.Stderr, f)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})), logFile, func() { f.Close() }
}
