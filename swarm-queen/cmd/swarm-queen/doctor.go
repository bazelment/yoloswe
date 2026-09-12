package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
	"github.com/bazelment/yoloswe/swarm-queen/verify"
)

var doctorRepoDir string

var doctorCmd = &cobra.Command{
	Use:   "doctor <run-dir>",
	Short: "Report drift between the ledger and reality (read-only)",
	Long: `doctor probes git, tmux and bramble and reports where the ledger disagrees
with what is actually on disk and running.

It never writes. Run it against a live swarm.`,
	Args: cobra.ExactArgs(1),
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().StringVar(&doctorRepoDir, "repo", ".",
		"repository directory for branch checks")
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	runDir := args[0]

	st, err := state.NewStore(runDir).Read()
	if err != nil {
		return err
	}

	worktrees := probeWorktrees(ctx, st)
	sessions, sessionErr := liveSessions(ctx, cmd)
	// A lane whose worktree could not be measured was not checked, whatever the
	// other findings say. Naming them is the same rule the branch and session
	// probes already follow: a check that could not run says so in the summary.
	var unmeasured []string
	for _, lane := range st.Lanes {
		if wt, ok := worktrees[lane.ID]; ok && wt.Unknown() && lane.Worktree != "" {
			unmeasured = append(unmeasured, lane.ID)
		}
	}

	branches, branchErr := liveBranches(ctx, st)
	findings := verify.LedgerDriftWithBranches(st, worktrees, sessions, branches)
	for _, f := range findings {
		fmt.Println(f)
	}

	// A check that could not run says so IN THE SUMMARY, not only on stderr. A
	// count that silently omits a whole category reads as a complete answer, and
	// the omission is invisible the moment stderr is redirected -- which is the
	// same clean-bill-of-health-from-a-broken-probe failure the audit script
	// documents.
	if branchErr != nil {
		fmt.Printf("\nbranch checks SKIPPED, not passed: %v\n", branchErr)
	}
	if sessionErr != nil {
		fmt.Printf("\nsession checks SKIPPED, not passed: %v\n", sessionErr)
	}
	if len(unmeasured) > 0 {
		fmt.Printf("\nworktree checks SKIPPED for %d lane(s), not passed: %s\n",
			len(unmeasured), strings.Join(unmeasured, ", "))
	}

	nonTerminal := st.NonTerminal()
	fmt.Printf("\ndoctor: %d finding(s) across %d lane(s); %d non-terminal",
		len(findings), len(st.Lanes), len(nonTerminal))
	var skipped []string
	if branchErr != nil {
		skipped = append(skipped, "branch")
	}
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
	for _, l := range nonTerminal {
		fmt.Printf("  non-terminal: %s (%s/%s)\n", l.ID, l.Status, orDash(l.Phase))
	}

	// Exit non-zero on findings so a tick can gate on doctor, and non-zero when a
	// check could not RUN, because a clean result from a partially-measured run
	// is not a pass. Exiting 0 in either case is the same false green as printing
	// an unqualified total: whatever reads the exit code would treat an
	// unmeasured run as a healthy one.
	switch {
	case branchErr != nil:
		return fmt.Errorf("%d finding(s), and some checks could not run: %w",
			len(findings), branchErr)
	case sessionErr != nil:
		return fmt.Errorf("%d finding(s), and some checks could not run: %w",
			len(findings), sessionErr)
	case len(unmeasured) > 0:
		return fmt.Errorf("%d finding(s), and %d lane(s) could not be measured: %s",
			len(findings), len(unmeasured), strings.Join(unmeasured, ", "))
	case len(findings) > 0:
		return fmt.Errorf("%d drift finding(s) across %d lane(s)", len(findings), len(st.Lanes))
	}
	return nil
}

// probeWorktrees measures each lane's worktree. A probe failure is recorded as
// "does not exist" rather than aborting: one unreadable lane must not hide drift
// in every other lane.
func probeWorktrees(ctx context.Context, st *state.State) map[string]reconcile.WorktreeState {
	out := make(map[string]reconcile.WorktreeState, len(st.Lanes))
	for _, lane := range st.Lanes {
		wt, err := reconcile.ProbeWorktree(ctx, reconcile.ExecGit{}, lane.Worktree, lane.ForkSHA)
		if err != nil && wt.Path == "" {
			wt.Path = lane.Worktree
		}
		out[lane.ID] = wt
	}
	return out
}

// liveSessions asks bramble what is running.
//
// Returns (nil, err) when the probe could not run, so "unknown" reaches the
// caller as a distinct answer from "no sessions". They are the same empty slice
// otherwise, and the difference decides whether a worktree may be removed: an
// empty fleet is safe to reap, an unmeasured one is not. The stderr warning is
// kept, but a warning is not a mechanism -- it vanishes the moment stderr is
// redirected, which is how a broken probe writes a clean bill of health.
func liveSessions(ctx context.Context, cmd *cobra.Command) ([]bramble.Session, error) {
	client, err := bramble.New()
	if err != nil {
		cmd.PrintErrf("warning: bramble unreachable (%v); session drift not checked\n", err)
		return nil, fmt.Errorf("bramble unreachable: %w", err)
	}
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		cmd.PrintErrf("warning: list-sessions failed (%v); session drift not checked\n", err)
		return nil, fmt.Errorf("list-sessions: %w", err)
	}
	return sessions, nil
}

// liveBranches lists which of the run's branches still exist.
//
// Returns (nil, err) when the probe fails, so the branch half is SKIPPED rather
// than reporting every branch as deleted. An empty set and an unmeasurable one
// are different answers: conflating them manufactures a clean bill of health in
// the check whose job is finding leaks.
func liveBranches(ctx context.Context, st *state.State) (map[string]bool, error) {
	out, err := reconcile.ExecGit{}.Run(ctx, doctorRepoDir, "branch", "--format=%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("cannot list branches in %s: %w — "+
			"run doctor from the orchestrator's worktree", doctorRepoDir, err)
	}
	existing := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if b := strings.TrimSpace(line); b != "" {
			existing[b] = true
		}
	}
	live := map[string]bool{}
	for _, lane := range st.Lanes {
		if lane.Branch != "" && existing[lane.Branch] {
			live[lane.Branch] = true
		}
	}
	return live, nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
