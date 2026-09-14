package main

import (
	"context"
	"fmt"
	"strings"

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
	var unmeasured []string
	var applier *lifecycle.Applier

	for _, lane := range st.Lanes {
		wt, err := reconcile.ProbeWorktree(ctx, git, lane.Worktree, lane.ForkSHA)
		if err != nil && wt.Path == "" {
			wt.Path = lane.Worktree
		}
		if wt.Unknown() {
			unmeasured = append(unmeasured, lane.ID)
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

		// A lane this harness already closed has nothing left to reap. Skip it
		// on measured evidence -- terminal status, worktree gone from disk,
		// branch gone from the repo -- rather than by erasing the ledger fields
		// the five-zeros audit reads. Re-planning it produced a REFUSE with
		// "cannot verify integration" and an unknown-session blocker on every
		// run, which buried the real refusals.
		if closed, why := laneFullyClosed(ctx, git, lane, wt); closed {
			fmt.Printf("CLOSED %s: %s\n", lane.ID, why)
			continue
		}

		probe := laneProbe(sessions, sessionErr == nil, lane.Worktree)
		plan := lifecycle.PlanReap(ctx, git, reapRepoDir, lane, wt, st.Config.Target, probe)
		if !plan.Safe {
			refused++
			fmt.Printf("REFUSE %s: %v\n", lane.ID, plan.Blockers)
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
	var skipped []string
	if sessionErr != nil {
		skipped = append(skipped, "session")
	}
	if len(unmeasured) > 0 {
		skipped = append(skipped, "worktree")
	}
	if len(skipped) > 0 {
		fmt.Printf(" (%s checks skipped)", strings.Join(skipped, ", "))
	}
	fmt.Println()
	if sessionErr != nil {
		fmt.Printf("\nsession checks SKIPPED, not passed: %v\n", sessionErr)
	}
	if len(unmeasured) > 0 {
		fmt.Printf("\nworktree checks SKIPPED for %d lane(s), not passed: %s\n",
			len(unmeasured), strings.Join(unmeasured, ", "))
	}
	return reapExit(failed, unmeasured, sessionErr)
}

// reapExit decides the command's exit status.
//
// Separate from runReap because the rule it encodes is the one this whole class
// of fix exists to remove: an unmeasured run must not read as a pass. Every lane
// is REFUSED when a probe could not run, so `failed` stays 0 and a caller gating
// on the exit code would see a clean sweep -- which is exactly the false green
// doctor already refuses to print.
func reapExit(failed int, unmeasured []string, sessionErr error) error {
	switch {
	case failed > 0:
		return fmt.Errorf("%d lane(s) failed to close", failed)
	case len(unmeasured) > 0:
		return fmt.Errorf("%d lane(s) could not be measured: %s",
			len(unmeasured), strings.Join(unmeasured, ", "))
	case sessionErr != nil:
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
	return &lifecycle.Applier{
		Git: reconcile.ExecGit{}, Tmux: tmux, Spawner: client,
		Store: state.NewStore(runDir), RunDir: runDir, RepoDir: reapRepoDir,
		SelfWindow:   self,
		LiveSessions: freshLaneProbe(ctx, cmd),
	}, nil
}

// freshLaneProbe resolves a lane's sessions by asking bramble AT THE MOMENT OF
// THE CALL, which is what makes it safe to use for a destructive decision.
//
// Capturing one snapshot and reusing it for every lane meant a session that
// started after the snapshot -- or after an earlier lane's kill in the same run
// -- was invisible, so its worktree could be removed with the agent still live.
// tick re-probes per lane for exactly this reason; reap shared lifecycle.Applier
// but not the wiring, so the two drifted while reap's doc claimed one teardown
// path. Both commands now build the probe here, so there is one implementation
// to keep correct rather than two to keep in step.
//
// A query that FAILS at apply time yields UnknownSessions, which refuses the
// reap, rather than an empty answer, which would permit it.
func freshLaneProbe(ctx context.Context, cmd *cobra.Command) func(*state.Lane) lifecycle.SessionProbe {
	return func(l *state.Lane) lifecycle.SessionProbe {
		fresh, ferr := reapSessions(ctx, cmd)
		return laneProbe(fresh, ferr == nil, l.Worktree)
	}
}

// reapSessions probes the live fleet. A variable so a test can supply a
// measurement without a live bramble TUI, matching doctorSessions.
var reapSessions = liveSessions

// laneFullyClosed reports whether a terminal lane has no resources left, on
// MEASURED evidence rather than on the absence of a ledger field.
//
// Absence of a field is not evidence of closure -- that conflation is what made
// clearing Worktree and Branch look like a fix while it disarmed the audit. The
// worktree must be measured and gone, and the branch must be absent from the
// repo; an unmeasured probe returns false so the lane is planned and refused in
// the ordinary way.
func laneFullyClosed(
	ctx context.Context,
	g reconcile.GitRunner,
	lane *state.Lane,
	wt reconcile.WorktreeState,
) (bool, string) {
	if !lane.Status.Terminal() || wt.Unknown() || wt.Exists {
		return false, ""
	}
	if lane.Branch != "" {
		out, err := g.Run(ctx, reapRepoDir, "branch", "--list", lane.Branch)
		if err != nil || strings.TrimSpace(out) != "" {
			return false, ""
		}
	}
	// A RETAINED backup does not make a lane unclosed.
	//
	// applyReap deliberately keeps the snapshot of a lane reaped while dirty --
	// that work exists nowhere else. Treating the ref as "not closed" sent the
	// lane back through PlanReap, whose branch was already deleted, so
	// BranchMerged failed and it printed "cannot verify integration of <branch>"
	// on every run: the misleading REFUSE noise this skip exists to remove,
	// reappearing on exactly the lanes whose work was most worth protecting.
	if lifecycle.HasBackup(ctx, g, reapRepoDir, lane.ID) {
		return true, "terminal, worktree gone, branch gone; backup retained at " +
			lifecycle.BackupRef(lane.ID)
	}
	return true, "terminal, worktree gone, branch gone, no backup ref"
}
