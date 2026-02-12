package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/medivac/issue"
)

var reopenCmd = &cobra.Command{
	Use:   "reopen <id>",
	Short: "Reopen a dismissed issue",
	Long:  `Set a previously dismissed issue back to "new" status so fix agents will pick it up again.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		tracker, err := issue.NewTracker(resolveTrackerPath(root))
		if err != nil {
			return fmt.Errorf("load tracker: %w", err)
		}

		id := args[0]
		if err := tracker.Reopen(id); err != nil {
			return err
		}

		if err := tracker.Save(); err != nil {
			return fmt.Errorf("save tracker: %w", err)
		}

		iss := tracker.GetByID(id)
		fmt.Printf("Reopened [%s] %s -- %s\n", iss.ID, iss.Category, iss.Summary)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(reopenCmd)
}
