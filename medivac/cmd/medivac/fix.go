package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/medivac/engine"
	"github.com/bazelment/yoloswe/wt"
)

var (
	fixBranch      string
	fixMaxParallel int
	fixModel       string
	fixBudget      float64
	fixSkipScan    bool
)

var fixCmd = &cobra.Command{
	Use:   "fix",
	Short: "Scan CI failures and launch fix agents",
	Long: `Full workflow: scan GitHub Actions for failures, reconcile with
known issues, then launch parallel agents to fix each actionable issue.
Each agent creates a worktree, investigates the failure, applies a fix,
and creates a PR. Supports Claude, Gemini, and Codex providers.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		log, logFile, closeLog := newFileLogger(root)
		defer closeLog()

		// Determine worktree root (find .bare directory up the tree)
		wtRoot, wtRepoName, err := resolveWTRoot(root)
		if err != nil {
			return fmt.Errorf("resolve wt root: %w", err)
		}

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
			LogFile:     logFile,
			Logger:      log,
		})
		if err != nil {
			return fmt.Errorf("create engine: %w", err)
		}

		var result *engine.FixResult
		if fixSkipScan {
			result, err = eng.FixFromTracker(cmd.Context())
		} else {
			result, err = eng.Fix(cmd.Context())
		}
		if err != nil {
			return err
		}

		printFixResult(result)
		return nil
	},
}

func truncateSummary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 3 {
		return "..."
	}
	return s[:max-3] + "..."
}

func printAnalysis(a *engine.AgentAnalysis) {
	if a == nil {
		return
	}
	if a.RootCause != "" {
		fmt.Printf("         Root cause: %s\n", a.RootCause)
	}
	if len(a.FixOptions) > 0 {
		fmt.Printf("         Options:\n")
		for i, opt := range a.FixOptions {
			if opt.Description != "" {
				fmt.Printf("           %d. %s: %s\n", i+1, opt.Label, opt.Description)
			} else {
				fmt.Printf("           %d. %s\n", i+1, opt.Label)
			}
		}
	}
}

func init() {
	rootCmd.AddCommand(fixCmd)
	fixCmd.Flags().StringVar(&fixBranch, "branch", "main", "Branch to scan for failures")
	fixCmd.Flags().IntVar(&fixMaxParallel, "max-parallel", 3, "Maximum parallel fix agents")
	fixCmd.Flags().StringVar(&fixModel, "model", "sonnet", "Model for fix agents (e.g. sonnet, gemini-2.5-pro)")
	fixCmd.Flags().Float64Var(&fixBudget, "budget", 1.0, "Cost budget per agent in USD")
	fixCmd.Flags().BoolVar(&fixSkipScan, "skip-scan", false, "Skip scanning; fix issues from existing tracker state")
}

func printFixResult(r *engine.FixResult) {
	if r.ScanResult != nil {
		printScanResult(r.ScanResult)
	}

	totalAgents := len(r.Results) + len(r.GroupResults)
	if totalAgents == 0 {
		fmt.Println("\nNo fix agents were launched.")
		return
	}

	fmt.Printf("\n=== Fix Results ===\n")
	fmt.Printf("Agents launched: %d (%d single, %d grouped)\n",
		totalAgents, len(r.Results), len(r.GroupResults))

	// Calculate total issues across all results
	totalIssues := len(r.Results) // each singleton result = 1 issue
	for _, gr := range r.GroupResults {
		totalIssues += len(gr.Group.Issues)
	}
	fmt.Printf("Total issues:    %d (in %d groups)\n", totalIssues, totalAgents)

	fmt.Printf("Total cost:      $%.4f\n", r.TotalCost)

	var succeeded, failed int

	// Print single-issue results
	for _, res := range r.Results {
		iss := res.Issue
		issueDesc := fmt.Sprintf("%s %s -- %s", iss.ID, iss.Category, truncateSummary(iss.Summary, 60))
		if iss.File != "" {
			issueDesc += fmt.Sprintf(" (%s)", iss.File)
		}

		if res.Success && res.PRURL != "" {
			succeeded++
			fmt.Printf("  [OK]   %s\n", issueDesc)
			fmt.Printf("         PR: %s\n", res.PRURL)
		} else if res.Success {
			// analysis_only
			fmt.Printf("  [INFO] %s\n", issueDesc)
		} else if res.Error != nil {
			failed++
			fmt.Printf("  [FAIL] %s\n", issueDesc)
			fmt.Printf("         %s\n", res.Error)
		} else {
			fmt.Printf("  [SKIP] %s (dry-run)\n", issueDesc)
		}
		printAnalysis(res.Analysis)
	}

	// Print group results
	for _, res := range r.GroupResults {
		groupDesc := fmt.Sprintf("GROUP %s (%d issues)", res.Group.Key, len(res.Group.Issues))
		if res.Success && res.PRURL != "" {
			succeeded++
			fmt.Printf("  [OK]   %s\n", groupDesc)
			fmt.Printf("         PR: %s\n", res.PRURL)
		} else if res.Success {
			fmt.Printf("  [INFO] %s\n", groupDesc)
		} else if res.Error != nil {
			failed++
			fmt.Printf("  [FAIL] %s\n", groupDesc)
			fmt.Printf("         %s\n", res.Error)
		} else {
			fmt.Printf("  [SKIP] %s (dry-run)\n", groupDesc)
			for _, iss := range res.Group.Issues {
				fmt.Printf("         - %s %s -- %s\n", iss.ID, iss.Category,
					truncateSummary(iss.Summary, 60))
			}
		}
		printAnalysis(res.Analysis)
	}

	fmt.Printf("\nSucceeded: %d  Failed: %d\n", succeeded, failed)
}
