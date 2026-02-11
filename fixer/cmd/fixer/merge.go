package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/fixer/engine"
	"github.com/bazelment/yoloswe/wt"
)

var mergeCmd = &cobra.Command{
	Use:   "merge",
	Short: "Merge approved fix PRs and refresh status",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := resolveRepoRoot()
		if err != nil {
			return err
		}

		log := newLogger()

		wtRoot := filepath.Dir(root)
		wtRepoName := filepath.Base(root)

		var wtManager *wt.Manager
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
			DryRun:      dryRun,
			Logger:      log,
		})
		if err != nil {
			return fmt.Errorf("create engine: %w", err)
		}

		results, err := eng.MergeApproved(cmd.Context())
		if err != nil {
			return err
		}

		if len(results) == 0 {
			fmt.Println("No approved PRs to merge.")
			return nil
		}

		fmt.Printf("\n=== Merge Results ===\n")
		for _, r := range results {
			if r.Error != nil {
				fmt.Printf("  [FAIL] PR #%d — %s\n", r.PRNumber, r.Error)
			} else {
				fmt.Printf("  [OK]   PR #%d merged\n", r.PRNumber)
			}
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(mergeCmd)
}
