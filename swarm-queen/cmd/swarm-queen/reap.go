package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
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
	sessions, sessionErr := liveSessions(ctx, cmd)
	var safe, refused, failed int
	var applier *lifecycle.Applier

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

		probe := lifecycle.UnknownSessions()
		if sessionErr == nil {
			probe = lifecycle.KnownSessions(sessionsForLane(sessions, lane.Worktree))
		}
		plan := lifecycle.PlanReap(ctx, git, reapRepoDir, lane, wt, st.Config.Target, probe)
		if !plan.Safe {
			refused++
			fmt.Printf("REFUSE %s: %v\n", lane.ID, plan.Blockers)
			continue
		}
		if len(plan.Steps) == 0 {
			continue
		}
		safe++
		if !reapApply {
			fmt.Printf("WOULD REAP %s: %v\n", lane.ID, plan.Steps)
			continue
		}
		// --apply means apply. Printing a different verb and doing nothing left
		// worktrees, branches, sessions and the ledger untouched while reporting
		// a teardown, so route every safe plan through the same transactional
		// Applier that `tick --apply` uses.
		if applier == nil {
			if applier, err = newReapApplier(ctx, cmd, args[0]); err != nil {
				return err
			}
		}
		out := applier.Apply(ctx, []decide.Decision{{
			Lane: lane.ID, Kind: decide.KindReap, Source: decide.SourceRule,
			Reason: "reap --apply",
		}})
		for i := range out {
			fmt.Println(out[i])
			if !out[i].OK() {
				failed++
			}
		}
	}

	fmt.Printf("\nreap: %d reapable, %d refused, %d lane(s)", safe, refused, len(st.Lanes))
	if !reapApply {
		fmt.Print(" (dry run; pass --apply to act)")
	}
	if sessionErr != nil {
		fmt.Print(" (session checks skipped)")
	}
	fmt.Println()
	if sessionErr != nil {
		fmt.Printf("\nsession checks SKIPPED, not passed: %v\n", sessionErr)
	}
	switch {
	case failed > 0:
		return fmt.Errorf("%d lane(s) failed to close", failed)
	case sessionErr != nil:
		// An unmeasured probe must not read as success via the exit code. Every
		// lane is refused in this state, so `failed` stays 0 and a caller gating
		// on the exit status would treat an unmeasured run as a clean one --
		// the same false green doctor already refuses to print.
		return fmt.Errorf("session probe could not run, so no lane could be "+
			"proven safe to reap: %w", sessionErr)
	}
	return nil
}

// newReapApplier builds the same Applier tick uses, so the two commands share
// one teardown path rather than growing a second, less-checked one.
//
// Built lazily: a dry run and a run with nothing reapable both need no bramble
// TUI, and requiring one would make `reap` unusable for the reporting it exists
// to do.
func newReapApplier(ctx context.Context, cmd *cobra.Command, runDir string) (*lifecycle.Applier, error) {
	client, err := bramble.New()
	if err != nil {
		return nil, fmt.Errorf("--apply needs a reachable bramble TUI: %w", err)
	}
	tmux := lifecycle.ExecTmux{}
	self, serr := lifecycle.ResolveSelf(ctx, tmux)
	if serr != nil {
		// Fail-closed rather than fatal: an unresolvable self only means window
		// kills are refused, which is the safe direction.
		cmd.PrintErrf("warning: cannot resolve own tmux window (%v); "+
			"window kills will be refused\n", serr)
	}
	sessions, sessionErr := liveSessions(ctx, cmd)
	return &lifecycle.Applier{
		Git: reconcile.ExecGit{}, Tmux: tmux, Spawner: client,
		Store: state.NewStore(runDir), RunDir: runDir, RepoDir: reapRepoDir,
		SelfWindow: self,
		LiveSessions: func(l *state.Lane) lifecycle.SessionProbe {
			if sessionErr != nil {
				return lifecycle.UnknownSessions()
			}
			return lifecycle.KnownSessions(sessionsForLane(sessions, l.Worktree))
		},
	}, nil
}
