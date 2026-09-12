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
	// The error is deliberately dropped HERE, and only here. LedgerDrift reads
	// measurement from the slice itself -- a nil slice means the probe could not
	// run and the session half of the drift report is SKIPPED rather than read as
	// "no sessions" -- and liveSessions has already printed the warning naming
	// what failed. The reap path does NOT reuse this snapshot: it re-probes per
	// lane at apply time, where a failed query becomes UnknownSessions and
	// refuses the reap. Keeping a stale error here would only invite someone to
	// answer an apply-time question with a tick-start measurement.
	sessions, _ := liveSessions(ctx, cmd)
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
	laneSignals, verdicts := readClaims(st, signals, worktrees)
	drift := verify.LedgerDrift(st, worktrees, sessions)

	// 3. DECIDE — rules first; anything unsettled escalates.
	decisions := decide.Plan(planInputs(st, laneSignals, verdicts, drift, tickMaxConcurrent))

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
	queued, err := decide.AppendEscalations(runDir, decide.EscalationsFrom(decisions))
	if err != nil {
		cmd.PrintErrf("warning: queue escalations: %v\n", err)
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
		// Re-probe the fleet HERE, per lane, at the moment of the reap -- not
		// from the `sessions` slice reconciled at the top of this tick.
		//
		// applyReap already re-probes the worktree because state moves between
		// deciding and acting; session ownership moves the same way and was the
		// one precondition still answered from a stale snapshot. A session that
		// started after reconciliation was invisible, so its worktree could be
		// removed with the agent still live -- the exact failure the ledger
		// window_id fallback was removed to prevent, reintroduced by the
		// closure's captured slice.
		//
		// An unmeasured fleet reaches PlanReap as unknown, which refuses the
		// reap, rather than as an empty one, which would permit it. That holds
		// for the fresh probe too: a bramble query that fails at apply time is
		// UnknownSessions, so the reap is refused rather than proceeding on the
		// older, more optimistic answer.
		LiveSessions: func(l *state.Lane) lifecycle.SessionProbe {
			fresh, ferr := liveSessions(ctx, cmd)
			return laneProbe(fresh, ferr == nil, l.Worktree)
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

	// The baseline advances only after acting, and only when every decision
	// applied. Saving it first marked this tick's signals as seen, so a lane
	// whose spawn or reap failed had its .done / .needs-swe dropped permanently:
	// the next tick would not see the claim again and nothing would retry it.
	advanced, err := reconcile.CommitBaseline(runDir, filepath.Join(runDir, baselineName), failed)
	if err != nil {
		return err
	}
	if !advanced {
		return fmt.Errorf("%d decision(s) failed to apply; "+
			"signals left unconsumed for the next tick", failed)
	}
	return nil
}

// laneProbe correlates the fleet's live sessions to one lane.
//
// bramble reports worktree_name and never a path, so the ledger's Worktree is
// the only key available -- which means a lane with no recorded worktree cannot
// be correlated AT ALL. That is a third state, and conflating it with "nothing
// holds this lane" is how a successful fleet probe produced a confident empty
// answer: the probe ran, the correlation did not, and PlanReap would clear the
// ledger fallback and proceed as if ownership had been disproved.
//
// probed says the fleet query itself succeeded. An uncorrelatable lane returns
// UnknownSessions regardless, because what the caller needs to know is whether
// THIS LANE's ownership was established, not whether some query somewhere
// returned rows.
func laneProbe(sessions []bramble.Session, probed bool, worktree string) lifecycle.SessionProbe {
	if !probed {
		return lifecycle.UnknownSessions()
	}
	name := filepath.Base(strings.TrimRight(worktree, "/"))
	if name == "" || name == "." || name == "/" {
		return lifecycle.UnknownSessions()
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
	return lifecycle.KnownSessions(out)
}

// readClaims turns the signal files a tick found into the claims the rule engine
// judges, and verifies each one against what was measured.
//
// Kept separate from runTick for the same reason planInputs is: wiring that only
// exists inside the command body cannot be tested, and this package has already
// shipped a correct rule that nothing called. Every branch here is a decision
// about whether a file on disk is a live claim, so each needs to be assertable
// without a bramble TUI, a tmux server, or a real run directory.
func readClaims(
	st *state.State,
	signals []reconcile.Signal,
	worktrees map[string]reconcile.WorktreeState,
) ([]decide.LaneSignal, map[string]verify.Verdict) {
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
		// `<lane>.done` omits the phase segment, and ParseSignalName documents
		// that shorthand as naming the FIRST declared phase. Resolve it HERE,
		// once, before either consumer sees it: an empty phase was a wildcard to
		// both of them, and in opposite directions. decide's attempt-identity
		// guard skipped its comparison, so a stale shorthand `.done` from an
		// earlier wave matched whatever the lane was running now; and
		// MutatingPhase("") is false, which switched off the empty-branch
		// refusal for exactly the mutating first-phase work it protects. A lane
		// could therefore be advanced past unverified work by a leftover file.
		phase := sig.Phase
		if phase == "" {
			phase = firstDeclaredPhase(st)
		}
		laneSignals = append(laneSignals, decide.LaneSignal{
			Lane: sig.Lane, Phase: phase, Round: sig.Round,
			NeedsSWE: sig.Kind == reconcile.SignalNeedsSWE,
		})
		if ok && sig.Kind == reconcile.SignalDone {
			verdicts[lane.ID] = verify.PhaseCompletion(lane, worktrees[lane.ID],
				state.MutatingPhase(phase))
		}
	}
	return laneSignals, verdicts
}

// firstDeclaredPhase is the phase a bare `<lane>.done` names.
//
// The run dir's shorthand omits the phase segment for first-phase signals, so
// resolving it needs the run's own contract rather than a hardcoded name: the
// first phase is `swe` in some runs and `simplify` or `plan` in others. An empty
// result means the config declares no phases at all, in which case the signal is
// genuinely unplaceable and the attempt-identity guard escalates it.
func firstDeclaredPhase(st *state.State) string {
	if names := st.Config.PhaseNames(); len(names) > 0 {
		return names[0]
	}
	return ""
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

// planInputs assembles what the rule engine decides from. Kept separate from
// runTick so the wiring is testable: SlotExempt was implemented in decide and
// documented in decide_test while tick passed nothing, which made the exemption
// correct, tested, and dead.
func planInputs(
	st *state.State,
	signals []decide.LaneSignal,
	verdicts map[string]verify.Verdict,
	drift []verify.Finding,
	maxConcurrent int,
) decide.Inputs {
	return decide.Inputs{
		State:         st,
		Signals:       signals,
		Verdicts:      verdicts,
		Drift:         drift,
		MaxConcurrent: maxConcurrent,
		// A read-only phase is not the work a concurrency cap exists to limit.
		// Unset, report/gaps/review lanes occupied slots and blocked staffing of
		// dependency-ready lanes -- the stall this harness exists to prevent.
		SlotExempt: slotExempt(st),
	}
}

// slotExempt marks every non-mutating phase as not consuming concurrency
// capacity, derived from the one shared predicate rather than from a second list
// to remember to update.
//
// Covers the phases a lane is actually IN as well as those the contract
// declares: a lane can carry a phase the config no longer lists (a contract
// edited mid-run), and such a lane still must not hold a slot it does not need.
func slotExempt(st *state.State) map[string]bool {
	out := map[string]bool{}
	mark := func(name string) {
		if name != "" && !state.MutatingPhase(name) {
			out[name] = true
		}
	}
	for _, name := range st.Config.PhaseNames() {
		mark(name)
	}
	for _, lane := range st.Lanes {
		mark(lane.Phase)
	}
	return out
}
