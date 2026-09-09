package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
	"github.com/bazelment/yoloswe/swarm-queen/verify"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor <run-dir>",
	Short: "Report drift between the ledger and reality (read-only)",
	Long: `doctor probes git, tmux and bramble and reports where the ledger disagrees
with what is actually on disk and running.

It never writes. Run it against a live swarm.`,
	Args: cobra.ExactArgs(1),
	RunE: runDoctor,
}

func init() { rootCmd.AddCommand(doctorCmd) }

func runDoctor(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	runDir := args[0]

	st, err := state.NewStore(runDir).Read()
	if err != nil {
		return err
	}

	worktrees := probeWorktrees(ctx, st)
	sessions := liveSessions(ctx, cmd)

	findings := verify.LedgerDrift(st, worktrees, sessions)
	for _, f := range findings {
		fmt.Println(f)
	}

	nonTerminal := st.NonTerminal()
	fmt.Printf("\ndoctor: %d finding(s) across %d lane(s); %d non-terminal\n",
		len(findings), len(st.Lanes), len(nonTerminal))
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

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
