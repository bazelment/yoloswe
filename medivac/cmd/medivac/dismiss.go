package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/medivac/issue"
)

var dismissReason string

var dismissCmd = &cobra.Command{
	Use:   "dismiss <id>",
	Short: "Dismiss an issue (mark as wont_fix)",
	Long:  `Mark an issue as dismissed so it will not be picked up by fix agents. Use "medivac reopen <id>" to undo.`,
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
		if err := tracker.Dismiss(id, dismissReason); err != nil {
			return err
		}

		if err := tracker.Save(); err != nil {
			return fmt.Errorf("save tracker: %w", err)
		}

		iss := tracker.GetByID(id)
		fmt.Printf("Dismissed [%s] %s -- %s\n", iss.ID, iss.Category, iss.Summary)
		if dismissReason != "" {
			fmt.Printf("  Reason: %s\n", dismissReason)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(dismissCmd)
	dismissCmd.Flags().StringVar(&dismissReason, "reason", "", "Reason for dismissing the issue")
}
