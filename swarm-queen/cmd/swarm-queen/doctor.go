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
	sessions := liveSessions(ctx, cmd)

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

	nonTerminal := st.NonTerminal()
	fmt.Printf("\ndoctor: %d finding(s) across %d lane(s); %d non-terminal",
		len(findings), len(st.Lanes), len(nonTerminal))
	if branchErr != nil {
		fmt.Print(" (branch checks skipped)")
	}
	fmt.Println()
	for _, l := range nonTerminal {
		fmt.Printf("  non-terminal: %s (%s/%s)\n", l.ID, l.Status, orDash(l.Phase))
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

// liveSessions asks bramble what is running. An unreachable TUI is reported and
// treated as "unknown", never as "no sessions" -- an empty list would read as a
// healthy idle swarm.
func liveSessions(ctx context.Context, cmd *cobra.Command) []bramble.Session {
	client, err := bramble.New()
	if err != nil {
		cmd.PrintErrf("warning: bramble unreachable (%v); session drift not checked\n", err)
		return nil
	}
	sessions, err := client.ListSessions(ctx)
	if err != nil {
		cmd.PrintErrf("warning: list-sessions failed (%v); session drift not checked\n", err)
		return nil
	}
	return sessions
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
