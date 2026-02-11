package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/medivac/issue"
)

var (
	statusJSON    bool
	statusVerbose bool
)

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
	statusCmd.Flags().BoolVarP(&statusVerbose, "verbose", "V", false, "Show analysis details for issues with fix attempts")
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

	fmt.Printf("\n=== Medivac Status (%d issues) ===\n", len(issues))

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

		// Sort by SeenCount descending, then Category ascending
		sort.Slice(group, func(i, j int) bool {
			if group[i].SeenCount != group[j].SeenCount {
				return group[i].SeenCount > group[j].SeenCount
			}
			return group[i].Category < group[j].Category
		})

		fmt.Printf("\n%s (%d):\n", status, len(group))
		for _, iss := range group {
			fmt.Printf("  [%s] %s — %s", iss.ID, iss.Category, iss.Summary)
			if iss.DismissReason != "" {
				fmt.Printf(" (reason: %s)", iss.DismissReason)
			}
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

			// Show analysis for verbose mode or always for wont_fix issues.
			if len(iss.FixAttempts) > 0 && (statusVerbose || status == issue.StatusWontFix) {
				last := iss.FixAttempts[len(iss.FixAttempts)-1]
				if last.RootCause != "" {
					fmt.Printf("         Root cause: %s\n", last.RootCause)
				}
				if len(last.FixOptions) > 0 {
					labels := make([]string, len(last.FixOptions))
					for i, opt := range last.FixOptions {
						labels[i] = opt.Label
					}
					fmt.Printf("         Options: %s\n", strings.Join(labels, " | "))
				}
				if last.LogFile != "" {
					fmt.Printf("         Log: %s\n", last.LogFile)
				}
			}
		}
	}
}
