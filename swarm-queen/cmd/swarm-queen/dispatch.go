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

	brief := lifecycle.SpawnBrief{
		Lane: lane.ID, Phase: phase, Round: round,
		Model: model, Type: dispatchType, Backend: dispatchBackend,
		Text: lifecycle.RenderBrief(lifecycle.BriefContext{
			RunDir: runDir, Lane: lane, Phase: phase, Round: round,
			Goal: st.Config.Goal, Target: st.Config.Target,
			Mission: mission, Standing: standing,
		}),
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

	if !dispatchApply {
		fmt.Printf("WOULD DISPATCH %s [%s r%d] model=%s mission=%s\n",
			lane.ID, phase, round, orDash(model), missionPath)
		fmt.Printf("  bramble %v\n", req.Args())
		fmt.Println("dry run; pass --apply to spawn")
		return nil
	}

	res, err := lifecycle.Spawn(ctx, mustClient(), store, runDir, brief, req)
	if err != nil {
		return err
	}
	// Same baseline stamp as the tick path: a phase spawned without one has no
	// measurable notion of "committed nothing", so its `.done` cannot be verified.
	worktree := res.WorktreePath
	if worktree == "" {
		worktree = lane.Worktree
	}
	if err := lifecycle.RecordPhaseBaseline(ctx, reconcile.ExecGit{}, store, lane.ID, worktree); err != nil {
		return fmt.Errorf("session %s IS LIVE at %s but its phase baseline was not recorded: %w",
			res.SessionID, orDash(worktree), err)
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

func mustClient() *bramble.Client {
	c, err := bramble.New()
	if err != nil {
		panic(err)
	}
	return c
}
