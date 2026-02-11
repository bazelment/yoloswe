package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/fixer/engine"
	"github.com/bazelment/yoloswe/wt"
)

var (
	fixBranch      string
	fixMaxParallel int
	fixModel       string
	fixBudget      float64
)

var fixCmd = &cobra.Command{
	Use:   "fix",
	Short: "Scan CI failures and launch fix agents",
	Long: `Full workflow: scan GitHub Actions for failures, reconcile with
known issues, then launch parallel Claude agents to fix each actionable issue.
Each agent creates a worktree, investigates the failure, applies a fix,
and creates a PR.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		log := newLogger()

		// Determine worktree root (parent of repo dir for wt.Manager)
		wtRoot := filepath.Dir(root)
		wtRepoName := filepath.Base(root)

		var wtManager *wt.Manager
		// Only create wt.Manager if not dry-run
		if !dryRun {
			wtManager = wt.NewManager(wtRoot, wtRepoName)
		}

		sessDir := resolveSessionDir(root)
		if err := os.MkdirAll(sessDir, 0755); err != nil {
			return fmt.Errorf("create session dir: %w", err)
		}

		eng, err := engine.New(engine.Config{
			WTManager:   wtManager,
			GHRunner:    &wt.DefaultGHRunner{},
			RepoDir:     root,
			TrackerPath: resolveTrackerPath(root),
			SessionDir:  sessDir,
			MaxParallel: fixMaxParallel,
			AgentModel:  fixModel,
			AgentBudget: fixBudget,
			DryRun:      dryRun,
			Branch:      fixBranch,
			Logger:      log,
		})
		if err != nil {
			return fmt.Errorf("create engine: %w", err)
		}

		result, err := eng.Fix(cmd.Context())
		if err != nil {
			return err
		}

		printFixResult(result)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(fixCmd)
	fixCmd.Flags().StringVar(&fixBranch, "branch", "main", "Branch to scan for failures")
	fixCmd.Flags().IntVar(&fixMaxParallel, "max-parallel", 3, "Maximum parallel fix agents")
	fixCmd.Flags().StringVar(&fixModel, "model", "sonnet", "Claude model for fix agents")
	fixCmd.Flags().Float64Var(&fixBudget, "budget", 1.0, "Cost budget per agent in USD")
}

func printFixResult(r *engine.FixResult) {
	printScanResult(r.ScanResult)

	if len(r.Results) == 0 {
		fmt.Println("\nNo fix agents were launched.")
		return
	}

	fmt.Printf("\n=== Fix Results ===\n")
	fmt.Printf("Agents launched: %d\n", len(r.Results))
	fmt.Printf("Total cost:      $%.4f\n", r.TotalCost)

	var succeeded, failed int
	for _, res := range r.Results {
		if res.Success {
			succeeded++
			fmt.Printf("  [OK]   %s — PR: %s\n", res.Issue.ID, res.PRURL)
		} else if res.Error != nil {
			failed++
			fmt.Printf("  [FAIL] %s — %s\n", res.Issue.ID, res.Error)
		} else {
			fmt.Printf("  [SKIP] %s (dry-run)\n", res.Issue.ID)
		}
	}

	fmt.Printf("\nSucceeded: %d  Failed: %d\n", succeeded, failed)
}
