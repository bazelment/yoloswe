package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

var (
	dispatchLane    string
	dispatchPhase   string
	dispatchRound   int
	dispatchModel   string
	dispatchType    string
	dispatchBackend string
	dispatchParent  string
	dispatchRepo    string
	dispatchRepoDir string
	dispatchApply   bool
)

var dispatchCmd = &cobra.Command{
	Use:   "dispatch <run-dir>",
	Short: "Spawn one lane phase, at an explicit round",
	Long: `dispatch staffs a single lane phase.

tick refills slots from the dependency-ready queue and always starts a phase at
its next round. dispatch exists for the cases tick cannot express: retrying a
phase whose earlier attempt failed, or staffing a phase out of band -- without
overwriting the previous attempt's session id.

Round defaults to one past the highest already recorded for that phase, so a
retry never clobbers the attempt it is retrying.`,
	Args: cobra.ExactArgs(1),
	RunE: runDispatch,
}

func init() {
	dispatchCmd.Flags().StringVar(&dispatchLane, "lane", "", "lane id (required)")
	dispatchCmd.Flags().StringVar(&dispatchPhase, "phase", "", "phase to run (default: the lane's current phase)")
	dispatchCmd.Flags().IntVar(&dispatchRound, "round", 0, "round; 0 means next unused")
	dispatchCmd.Flags().StringVar(&dispatchModel, "model", "", "model override (default: the phase's model)")
	dispatchCmd.Flags().StringVar(&dispatchType, "type", "builder", "session type")
	dispatchCmd.Flags().StringVar(&dispatchBackend, "backend", "", "CLI backend; empty infers from the model")
	dispatchCmd.Flags().StringVar(&dispatchParent, "parent", "",
		"orchestrator session id; without it a completed lane reports nowhere")
	dispatchCmd.Flags().StringVar(&dispatchRepoDir, "repo", ".",
		"repository directory, used to resolve the base a new worktree forks from")
	dispatchCmd.Flags().StringVar(&dispatchRepo, "bramble-repo", "",
		"bramble repository name; inference can pick an unrelated repo")
	dispatchCmd.Flags().BoolVar(&dispatchApply, "apply", false, "actually spawn (default: dry run)")
	_ = dispatchCmd.MarkFlagRequired("lane")
	rootCmd.AddCommand(dispatchCmd)
}

func runDispatch(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	runDir := args[0]
	store := state.NewStore(runDir)

	st, err := store.Read()
	if err != nil {
		return err
	}
	lane, ok := st.Lane(dispatchLane)
	if !ok {
		return fmt.Errorf("lane %q is not in the ledger", dispatchLane)
	}

	phase := dispatchPhase
	if phase == "" {
		phase = lane.Phase
	}
	if phase == "" {
		return fmt.Errorf("lane %q has no current phase; pass --phase", lane.ID)
	}
	if !declaredPhase(st, phase) {
		return fmt.Errorf("phase %q is not declared by this run (%v)", phase, st.Config.PhaseNames())
	}

	// Default to the next unused round so a retry cannot overwrite the attempt
	// it is retrying -- the sessions map is keyed by phase, so reusing a round
	// silently destroys the earlier session id.
	round := dispatchRound
	if round == 0 {
		round = lane.MaxRound(phase) + 1
	}
	if existing, taken := lane.SessionFor(phase, round); taken {
		return fmt.Errorf("round %d of %q already recorded session %s; "+
			"pick a higher round rather than overwriting it", round, phase, existing)
	}

	model := dispatchModel
	if model == "" {
		for _, p := range st.Config.Phases {
			if p.Name == phase {
				model = p.Model
			}
		}
	}

	mission, missionPath, err := readMission(runDir, lane.ID, phase, round)
	if err != nil {
		return err
	}
	standing, _ := decide.StandingRules(runDir)
	// dispatch stages ONE lane, so the run-wide nudges have a single addressee
	// here and retire with this spawn like any other.
	runWide, err := decide.RunWideNudges(runDir)
	if err != nil {
		return err
	}

	req := bramble.SpawnRequest{
		Repo: dispatchRepo, Parent: dispatchParent, Goal: lane.Title,
		Model: model, Backend: dispatchBackend,
	}
	if lane.Worktree != "" {
		req.Worktree = lane.Worktree
	} else {
		req.CreateWorktree = true
		req.Branch = lane.Branch
		req.From = st.Config.Base
	}

	// A dry run reports; it does not write. This return comes BEFORE
	// PrepareSpawn because that call stamps PhaseStartSHA/ForkSHA through the
	// ledger via store.Update -- so a default `dispatch` (no --apply) mutated
	// the run's recorded baseline while printing that nothing would be spawned.
	// A preview that changes what it previews is worse than no preview: the
	// operator's next real dispatch measures its phase from a SHA this dry run
	// silently recorded.
	if !dispatchApply {
		fmt.Printf("WOULD DISPATCH %s [%s r%d] model=%s mission=%s\n",
			lane.ID, phase, round, orDash(model), missionPath)
		fmt.Printf("  bramble %v\n", req.Args())
		fmt.Println("dry run; pass --apply to spawn")
		return nil
	}

	// The whole pre-spawn sequence, shared with the tick path: instructions, and
	// a baseline recorded while a failure still costs only a refusal.
	// Hand-rolling these steps here is how dispatch fell behind three rounds
	// running -- on nudge delivery, then the pre-stamp, then the base-resolved
	// pre-stamp, one missing step each time.
	instructions, own, preStamped, err := lifecycle.PrepareSpawn(ctx, reconcile.ExecGit{},
		store, runDir, dispatchRepoDir, lane.ID, st.Config.Base, standing, runWide)
	if err != nil {
		return err
	}
	nudges := append(append([]decide.Nudge(nil), runWide...), own...)

	brief := lifecycle.SpawnBrief{
		Lane: lane.ID, Phase: phase, Round: round,
		Model: model, Type: dispatchType, Backend: dispatchBackend,
		Text: lifecycle.RenderBrief(lifecycle.BriefContext{
			RunDir: runDir, Lane: lane, Phase: phase, Round: round,
			Goal: st.Config.Goal, Target: st.Config.Target,
			Mission: mission, Standing: instructions,
		}),
	}

	client, err := bramble.New()
	if err != nil {
		// A missing or ambiguous socket is ordinary infrastructure trouble, not
		// a programming error. Panicking on it crashed the CLI with a stack
		// trace instead of naming the problem and what to do about it.
		return fmt.Errorf("--apply needs a reachable bramble TUI: %w", err)
	}
	// Re-read immediately before spawning: a lane that started running since the
	// checks above must not get a second agent on its worktree, which would
	// overwrite its phase and session state. dispatch bypasses the rule engine
	// and its Vet invariants, so it owes this check itself.
	fresh, err := store.Read()
	if err != nil {
		return err
	}
	if cur, ok := fresh.Lane(lane.ID); ok && cur.Status == state.StatusRunning {
		return fmt.Errorf("refusing to dispatch %s: it is already running (%s); "+
			"a second agent on one worktree is a concurrent-ownership collision",
			lane.ID, orDash(cur.Worktree))
	}

	res, err := lifecycle.Spawn(ctx, client, store, runDir, brief, req)
	if err != nil {
		return err
	}
	worktree := res.WorktreePath
	if worktree == "" {
		worktree = lane.Worktree
	}
	if err := lifecycle.FinishSpawn(ctx, reconcile.ExecGit{}, store, runDir, lane.ID, worktree, nudges, preStamped); err != nil {
		return fmt.Errorf("REPAIR REQUIRED: session %s IS LIVE at %s but the spawn did "+
			"not complete: %w", res.SessionID, orDash(worktree), err)
	}
	fmt.Printf("dispatched %s [%s r%d]: session %s on %s\n",
		lane.ID, phase, round, res.SessionID, orDash(res.WorktreePath))
	return nil
}

func declaredPhase(st *state.State, phase string) bool {
	for _, n := range st.Config.PhaseNames() {
		if n == phase {
			return true
		}
	}
	return false
}

// readMission loads the authored half of the brief, preferring the round-
// specific file so a retry can carry different instructions than the attempt it
// replaces. A missing mission is an ERROR here rather than a title fallback:
// dispatch is explicit, and silently briefing a lane with its own name is how a
// retry repeats the failure it was meant to fix.
func readMission(runDir, lane, phase string, round int) (text, path string, err error) {
	for _, name := range []string{
		fmt.Sprintf("%s.%s.mission.txt", lane, state.PhaseRoundKey(phase, round)),
		fmt.Sprintf("%s.%s.mission.txt", lane, phase),
		lane + ".mission.txt",
	} {
		p := filepath.Join(runDir, name)
		b, readErr := os.ReadFile(p)
		if readErr == nil {
			return string(b), name, nil
		}
	}
	return "", "", fmt.Errorf("no mission file for %s.%s round %d; expected one of "+
		"%s.%s.mission.txt, %s.%s.mission.txt, or %s.mission.txt in %s",
		lane, phase, round,
		lane, state.PhaseRoundKey(phase, round), lane, phase, lane, runDir)
}
