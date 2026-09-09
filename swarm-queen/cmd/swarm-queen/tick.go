package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
	"github.com/bazelment/yoloswe/swarm-queen/verify"
)

var (
	tickRepoDir       string
	tickApply         bool
	tickMaxConcurrent int
	tickExplain       bool
	tickParent        string
	tickRepo          string
)

var tickCmd = &cobra.Command{
	Use:   "tick <run-dir>",
	Short: "Run one orchestration tick: reconcile, verify, decide",
	Long: `tick is the whole loop, once: reconcile observed state, verify every claim
against it, and decide what to do.

It is a DRY RUN by default. The reconcile and verify steps never call a model;
decisions come from the rule engine, and anything the rules cannot settle is
escalated rather than guessed.`,
	Args: cobra.ExactArgs(1),
	RunE: runTick,
}

func init() {
	tickCmd.Flags().StringVar(&tickRepoDir, "repo", ".", "repository directory for branch checks")
	tickCmd.Flags().BoolVar(&tickApply, "apply", false, "apply decisions (default: dry run)")
	tickCmd.Flags().IntVar(&tickMaxConcurrent, "max-concurrent", 0,
		"cap on slot-consuming lanes; 0 disables refill")
	tickCmd.Flags().BoolVar(&tickExplain, "explain", false, "print the evidence behind each decision")
	tickCmd.Flags().StringVar(&tickParent, "parent", "",
		"orchestrator session id passed to every spawn; without it completed lanes report nowhere")
	tickCmd.Flags().StringVar(&tickRepo, "bramble-repo", "",
		"bramble repository name; inference can pick an unrelated repo")
	rootCmd.AddCommand(tickCmd)
}

func runTick(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	runDir := args[0]

	st, err := state.NewStore(runDir).Read()
	if err != nil {
		return err
	}

	// 1. RECONCILE — measure reality. No model involved.
	git := reconcile.ExecGit{}
	sessions := liveSessions(ctx, cmd)
	worktrees := make(map[string]reconcile.WorktreeState, len(st.Lanes))
	for _, lane := range st.Lanes {
		wt, err := reconcile.ProbeWorktree(ctx, git, lane.Worktree, lane.PhaseStartSHA)
		if err != nil && wt.Path == "" {
			wt.Path = lane.Worktree
		}
		worktrees[lane.ID] = wt
	}

	// Only signals that appeared since the last tick are actionable. Scanning
	// everything re-litigates history: a run dir accumulates dozens of signal
	// files (44 in one observed run), and a completed lane's old .done would be
	// re-verified against a worktree that was correctly reaped long ago.
	signals, err := reconcile.NewSince(runDir, loadBaseline(runDir))
	if err != nil {
		return err
	}

	// 2. VERIFY — every claim checked against what was measured.
	verdicts := make(map[string]verify.Verdict, len(signals))
	var laneSignals []decide.LaneSignal
	for _, sig := range signals {
		lane, ok := st.Lane(sig.Lane)
		// A signal for a lane that is already terminal is history, not a claim:
		// its worktree is legitimately gone once reaped, so re-verifying it
		// would refuse a completion that already succeeded.
		if ok && lane.Status.Terminal() {
			continue
		}
		laneSignals = append(laneSignals, decide.LaneSignal{
			Lane: sig.Lane, Phase: sig.Phase, Round: sig.Round,
			NeedsSWE: sig.Kind == reconcile.SignalNeedsSWE,
		})
		if ok && sig.Kind == reconcile.SignalDone {
			verdicts[lane.ID] = verify.PhaseCompletion(lane, worktrees[lane.ID],
				mutatingPhase(sig.Phase))
		}
	}
	drift := verify.LedgerDrift(st, worktrees, sessions)

	// 3. DECIDE — rules first; anything unsettled escalates.
	decisions := decide.Plan(decide.Inputs{
		State:         st,
		Signals:       laneSignals,
		Verdicts:      verdicts,
		Drift:         drift,
		MaxConcurrent: tickMaxConcurrent,
	})

	// Standing rules are re-read every tick, so a correction survives the
	// compaction that would otherwise drop it.
	if rules, err := decide.StandingRules(runDir); err == nil && len(rules) > 0 {
		fmt.Println("standing rules:")
		for _, r := range rules {
			fmt.Println("  -", r)
		}
		fmt.Println()
	}

	for _, f := range drift {
		if f.Severity != verify.SeverityBlock {
			fmt.Println(f)
		}
	}
	for _, d := range decisions {
		fmt.Println(d)
		if tickExplain {
			for _, e := range d.Evidence {
				fmt.Println("        evidence:", e)
			}
		}
	}

	fmt.Printf("\ntick: %s | %d drift finding(s), %d lane(s), %d non-terminal\n",
		decide.Summarise(decisions), len(drift), len(st.Lanes), len(st.NonTerminal()))
	// Escalations are queued even in a dry run: a question needing a person is
	// worth recording whether or not this tick acts on anything.
	var queued int
	for _, e := range decide.EscalationsFrom(decisions) {
		added, err := decide.AppendEscalation(runDir, e)
		if err != nil {
			cmd.PrintErrf("warning: queue escalation for %s: %v\n", e.Lane, err)
			continue
		}
		if added {
			queued++
		}
	}
	if queued > 0 {
		fmt.Printf("queued %d new escalation(s); see `swarm-queen escalations %s`\n", queued, runDir)
	}

	if !tickApply {
		fmt.Println("dry run; pass --apply to act")
		return nil
	}

	// 4. APPLY — every destructive path re-checks its own preconditions here,
	// because state can move between deciding and acting.
	client, cerr := bramble.New()
	if cerr != nil {
		return fmt.Errorf("--apply needs a reachable bramble TUI: %w", cerr)
	}
	standing, _ := decide.StandingRules(runDir)
	tmux := lifecycle.ExecTmux{}
	self, serr := lifecycle.ResolveSelf(ctx, tmux)
	if serr != nil {
		// Not fatal: an unresolvable self only means kills are refused, which is
		// the correct fail-closed behaviour rather than a reason to stop.
		cmd.PrintErrf("warning: cannot resolve own tmux window (%v); "+
			"window kills will be refused\n", serr)
	}

	// Missions are the AUTHORED half of a brief, one file per lane+phase in the
	// run dir. Without them a brief falls back to the lane title, which is a
	// label rather than an instruction -- so the lane is told what it is called,
	// not what to do.
	applier := &lifecycle.Applier{
		Missions: loadMissions(runDir, st, decisions),
		Store:    state.NewStore(runDir), RunDir: runDir, RepoDir: tickRepoDir,
		Git: git, Tmux: tmux, Spawner: client,
		SelfWindow: self, Parent: tickParent, Repo: tickRepo,
		Standing: standing,
		LiveSessions: func(l *state.Lane) []lifecycle.LiveSession {
			return sessionsForLane(sessions, l.Worktree)
		},
	}

	var failed int
	outcomes := applier.Apply(ctx, decisions)
	for i := range outcomes {
		fmt.Println(outcomes[i])
		if !outcomes[i].OK() {
			failed++
		}
	}

	// The baseline advances only after acting, so a failed tick re-sees its
	// signals rather than silently dropping them.
	current, err := reconcile.Baseline(runDir)
	if err != nil {
		return err
	}
	if err := reconcile.SaveBaseline(filepath.Join(runDir, baselineName), current); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d decision(s) failed to apply", failed)
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// mutatingPhase reports whether a phase is expected to change the branch. A
// read-only phase legitimately commits nothing, so the empty-branch refusal must
// not apply to it.
func mutatingPhase(phase string) bool {
	switch phase {
	case "gaps", "replay", "report", "verify", "":
		return false
	default:
		return !strings.HasPrefix(phase, "review")
	}
}

// sessionsForLane selects the live sessions attached to a lane's worktree.
// bramble reports worktree_name and never a path, so match on the basename.
func sessionsForLane(sessions []bramble.Session, worktree string) []lifecycle.LiveSession {
	name := filepath.Base(strings.TrimRight(worktree, "/"))
	if name == "" || name == "." || name == "/" {
		return nil
	}
	var out []lifecycle.LiveSession
	for i := range sessions {
		s := &sessions[i]
		if s.WorktreeName != name {
			continue
		}
		out = append(out, lifecycle.LiveSession{
			ID: s.ID, Status: s.Status, TmuxTarget: s.TmuxTarget,
		})
	}
	return out
}

// baselineName is where a tick records which signals it has already seen.
const baselineName = ".swarm-queen-baseline"

// loadBaseline reads the signals seen by the previous tick. A missing baseline
// means the first tick treats the whole run dir as history rather than as a
// wave of fresh claims -- adopting an in-flight run must not replay every signal
// it has ever emitted.
func loadBaseline(runDir string) reconcile.SignalSet {
	if base, err := reconcile.LoadBaseline(filepath.Join(runDir, baselineName)); err == nil {
		return base
	}
	// First tick on this run: adopt everything already present as history.
	current, err := reconcile.Baseline(runDir)
	if err != nil {
		return reconcile.SignalSet{}
	}
	return current
}

// loadMissions reads <lane>.<phase>.mission.txt for every lane, falling back to
// <lane>.mission.txt so a single-instruction lane needs one file.
//
// A missing mission is not an error: the brief falls back to the lane title, and
// the operator finds out by reading the brief rather than by the spawn failing.
func loadMissions(runDir string, st *state.State, decisions []decide.Decision) map[string]string {
	// A lane's recorded phase is empty before its first spawn, and stale for a
	// lane about to advance, so resolve against the phase the DECISION will run.
	next := map[string]string{}
	for _, d := range decisions {
		if d.Phase != "" {
			next[d.Lane] = state.PhaseRoundKey(d.Phase, max(d.Round, 1))
		}
	}
	out := map[string]string{}
	for _, lane := range st.Lanes {
		candidates := []string{}
		if k, ok := next[lane.ID]; ok {
			candidates = append(candidates, fmt.Sprintf("%s.%s.mission.txt", lane.ID, k))
		}
		candidates = append(candidates,
			fmt.Sprintf("%s.%s.mission.txt", lane.ID, state.PhaseRoundKey(lane.Phase, lane.Round)),
			lane.ID+".mission.txt")
		for _, name := range candidates {
			if b, err := os.ReadFile(filepath.Join(runDir, name)); err == nil {
				out[lane.ID] = strings.TrimSpace(string(b))
				break
			}
		}
	}
	return out
}
