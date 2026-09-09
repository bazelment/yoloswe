package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

var (
	reapRepoDir string
	reapApply   bool
)

var reapCmd = &cobra.Command{
	Use:   "reap <run-dir>",
	Short: "Plan (or apply) the teardown of terminal lanes",
	Long: `reap reports what it would take to fully close each terminal lane, and why
any lane is not yet safe to close.

It is a DRY RUN by default. Nothing is removed until --apply is passed, and even
then a lane with unsnapshotted work, an unmerged PR, or a non-terminal status is
refused rather than forced.`,
	Args: cobra.ExactArgs(1),
	RunE: runReap,
}

func init() {
	reapCmd.Flags().StringVar(&reapRepoDir, "repo", ".",
		"repository directory for branch and ref checks")
	reapCmd.Flags().BoolVar(&reapApply, "apply", false,
		"actually perform the teardown (default: dry run)")
	rootCmd.AddCommand(reapCmd)
}

func runReap(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	st, err := state.NewStore(args[0]).Read()
	if err != nil {
		return err
	}

	git := reconcile.ExecGit{}
	// Sessions come from bramble, never from the ledger: window_id decayed to
	// 1-of-12 populated in a real run, so a lane with a live agent can carry an
	// empty one and would otherwise have its worktree pulled out from under it.
	sessions := liveSessions(ctx, cmd)
	var safe, refused int

	for _, lane := range st.Lanes {
		wt, err := reconcile.ProbeWorktree(ctx, git, lane.Worktree, lane.ForkSHA)
		if err != nil && wt.Path == "" {
			wt.Path = lane.Worktree
		}
		// Snapshot before planning: work at risk should be protected whether or
		// not the lane turns out to be reapable.
		if wt.Exists && wt.DirtyCount > 0 {
			res, err := lifecycle.SnapshotAtRisk(ctx, git, lane.ID, lane.Worktree)
			switch {
			case err != nil:
				cmd.PrintErrf("warning: snapshot %s: %v\n", lane.ID, err)
			case !res.Skipped:
				fmt.Printf("SNAPSHOT %s -> %s (%d file(s))\n", lane.ID, res.Ref, res.Files)
			}
		}

		plan := lifecycle.PlanReap(ctx, git, reapRepoDir, lane, wt, st.Config.Target,
			sessionsForLane(sessions, lane.Worktree))
		if !plan.Safe {
			refused++
			fmt.Printf("REFUSE %s: %v\n", lane.ID, plan.Blockers)
			continue
		}
		if len(plan.Steps) == 0 {
			continue
		}
		safe++
		verb := "WOULD REAP"
		if reapApply {
			verb = "REAP"
		}
		fmt.Printf("%s %s: %v\n", verb, lane.ID, plan.Steps)
	}

	fmt.Printf("\nreap: %d reapable, %d refused, %d lane(s)", safe, refused, len(st.Lanes))
	if !reapApply {
		fmt.Print(" (dry run; pass --apply to act)")
	}
	fmt.Println()
	return nil
}
