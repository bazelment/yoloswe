package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/fixer/issue"
)

var statusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show tracked issue status",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		tracker, err := issue.NewTracker(resolveTrackerPath(root))
		if err != nil {
			return fmt.Errorf("load tracker: %w", err)
		}

		all := tracker.All()

		if statusJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(all)
		}

		printStatus(all)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")
}

func printStatus(issues []*issue.Issue) {
	if len(issues) == 0 {
		fmt.Println("No tracked issues.")
		return
	}

	// Group by status
	groups := make(map[issue.Status][]*issue.Issue)
	for _, iss := range issues {
		groups[iss.Status] = append(groups[iss.Status], iss)
	}

	fmt.Printf("\n=== Fixer Status (%d issues) ===\n", len(issues))

	statusOrder := []issue.Status{
		issue.StatusNew,
		issue.StatusRecurred,
		issue.StatusInProgress,
		issue.StatusFixPending,
		issue.StatusFixApproved,
		issue.StatusFixMerged,
		issue.StatusVerified,
		issue.StatusWontFix,
	}

	for _, status := range statusOrder {
		group := groups[status]
		if len(group) == 0 {
			continue
		}

		fmt.Printf("\n%s (%d):\n", status, len(group))
		for _, iss := range group {
			fmt.Printf("  [%s] %s — %s", iss.ID, iss.Category, iss.Summary)
			if len(iss.FixAttempts) > 0 {
				last := iss.FixAttempts[len(iss.FixAttempts)-1]
				if last.PRURL != "" {
					fmt.Printf(" (PR: %s)", last.PRURL)
				}
			}
			fmt.Println()
			if iss.File != "" {
				fmt.Printf("         %s", iss.File)
				if iss.Line > 0 {
					fmt.Printf(":%d", iss.Line)
				}
				fmt.Printf(" (seen %dx)\n", iss.SeenCount)
			}
		}
	}
}
