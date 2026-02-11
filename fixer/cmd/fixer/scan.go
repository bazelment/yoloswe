package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/fixer/engine"
	"github.com/bazelment/yoloswe/wt"
)

var (
	scanBranch       string
	scanLimit        int
	scanTriageModel  string
	scanTriageBudget float64
)

var scanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Scan CI failures and reconcile with known issues",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		log := newLogger()
		eng, err := engine.New(engine.Config{
			GHRunner:     &wt.DefaultGHRunner{},
			RepoDir:      root,
			TrackerPath:  resolveTrackerPath(root),
			Branch:       scanBranch,
			RunLimit:     scanLimit,
			TriageModel:  scanTriageModel,
			TriageBudget: scanTriageBudget,
			DryRun:       dryRun,
			Logger:       log,
		})
		if err != nil {
			return fmt.Errorf("create engine: %w", err)
		}

		result, err := eng.Scan(cmd.Context())
		if err != nil {
			return err
		}

		printScanResult(result)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(scanCmd)
	scanCmd.Flags().StringVar(&scanBranch, "branch", "main", "Branch to scan for failures")
	scanCmd.Flags().IntVar(&scanLimit, "limit", 5, "Number of recent failed runs to check")
	scanCmd.Flags().StringVar(&scanTriageModel, "triage-model", "haiku", "Claude model for LLM triage")
	scanCmd.Flags().Float64Var(&scanTriageBudget, "triage-budget", 0.50, "Max spend on LLM triage per scan (USD)")
}

func printScanResult(r *engine.ScanResult) {
	fmt.Printf("\n=== Scan Results ===\n")
	fmt.Printf("Failed runs:       %d\n", len(r.Runs))
	fmt.Printf("Parsed failures:   %d\n", len(r.Failures))

	if r.TriageCost > 0 {
		fmt.Printf("Triage cost:       $%.4f\n", r.TriageCost)
	}

	// Per-run summary
	if len(r.Runs) > 0 {
		fmt.Printf("\nRuns:\n")
		for i := range r.Runs {
			run := &r.Runs[i]
			failCount := 0
			for j := range r.Failures {
				if r.Failures[j].RunID == run.ID {
					failCount++
				}
			}
			fmt.Printf("  [%d] %s — %d failure(s)\n", run.ID, run.Name, failCount)
			fmt.Printf("       %s\n", run.URL)
		}
	}

	// All parsed failures
	if len(r.Failures) > 0 {
		fmt.Printf("\nFailures:\n")
		for i := range r.Failures {
			f := &r.Failures[i]
			fmt.Printf("  [%s] %s — %s\n", f.Category, f.JobName, f.Summary)
			if f.File != "" {
				fmt.Printf("         %s", f.File)
				if f.Line > 0 {
					fmt.Printf(":%d", f.Line)
				}
				fmt.Println()
			}
		}
	}

	fmt.Printf("\nNew issues:        %d\n", len(r.Reconciled.New))
	fmt.Printf("Updated issues:    %d\n", len(r.Reconciled.Updated))
	fmt.Printf("Resolved issues:   %d\n", len(r.Reconciled.Resolved))
	fmt.Printf("Total tracked:     %d\n", r.TotalIssues)
	fmt.Printf("Actionable (need fix): %d\n", r.ActionableLen)

	if len(r.Reconciled.New) > 0 {
		fmt.Printf("\nNew issues:\n")
		for _, iss := range r.Reconciled.New {
			fmt.Printf("  [%s] %s — %s\n", iss.ID, iss.Category, iss.Summary)
			if iss.File != "" {
				fmt.Printf("         %s", iss.File)
				if iss.Line > 0 {
					fmt.Printf(":%d", iss.Line)
				}
				fmt.Println()
			}
		}
	}

	if len(r.Reconciled.Updated) > 0 {
		fmt.Printf("\nUpdated issues (seen again):\n")
		for _, iss := range r.Reconciled.Updated {
			fmt.Printf("  [%s] %s — %s (seen %dx)\n", iss.ID, iss.Category, iss.Summary, iss.SeenCount)
		}
	}

	if len(r.Reconciled.Resolved) > 0 {
		fmt.Printf("\nResolved issues:\n")
		for _, iss := range r.Reconciled.Resolved {
			fmt.Printf("  [%s] %s — %s\n", iss.ID, iss.Category, iss.Summary)
		}
	}
}
