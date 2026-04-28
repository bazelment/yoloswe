// Command medivac provides automated CI failure remediation.
package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/medivac/engine"
)

var (
	repoRoot    string
	trackerPath string
	sessionDir  string
	dryRun      bool
)

var rootOpts = cliapp.Options{
	ToolName: "medivac",
	// Capture LevelTrace and LevelDump in the log file so prompts, LLM
	// responses, and raw CI logs land alongside Info events for postmortem.
	LogFileLevel: engine.LevelDump,
}

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

	cliapp.RegisterStandardFlags(rootCmd, &rootOpts)
}

func main() {
	os.Exit(cliapp.Run(&rootOpts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
}

// resolveRepoRoot finds the repo root from flags or git.
func resolveRepoRoot() (string, error) {
	if repoRoot != "" {
		return repoRoot, nil
	}
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
func resolveWTRoot(repoRoot string) (string, string, error) {
	dir := repoRoot
	for {
		bare := filepath.Join(dir, ".bare")
		if info, err := os.Stat(bare); err == nil && info.IsDir() {
			return filepath.Dir(dir), filepath.Base(dir), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Dir(repoRoot), filepath.Base(repoRoot), nil
		}
		dir = parent
	}
}
