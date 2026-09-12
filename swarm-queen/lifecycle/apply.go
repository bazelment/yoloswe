package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// Applier executes decisions.
//
// Every destructive path re-checks its preconditions here rather than trusting
// that the decision step already did. The decision may have come from a model,
// and state may have moved since the tick began.
type Applier struct {
	Git     reconcile.GitRunner
	Tmux    Tmux
	Spawner Spawner
	Store   *state.Store
	// Missions supplies the authored half of a brief, keyed by lane id.
	Missions map[string]string
	// LiveSessions resolves a lane's sessions from bramble rather than from the
	// ledger: window_id decayed to 1-of-12 populated in real runs, so a lane
	// with a live agent can carry an empty one. It returns a SessionProbe, so a
	// probe that could not run refuses the reap instead of reading as an empty
	// fleet. A nil func is itself unknown, and therefore also refuses.
	LiveSessions func(*state.Lane) SessionProbe
	RunDir       string
	RepoDir      string
	// SelfWindow is this process's tmux window, used to refuse killing itself.
	// Empty means every kill is refused, which is the correct fail-closed
	// behaviour when we cannot prove what we are.
	SelfWindow string
	// Parent is the orchestrator session id passed to every spawn; without it a
	// completed lane reports nowhere.
	Parent string
	// Repo is the bramble repository name. Always set: inference can pick the
	// TUI's unrelated repository.
	Repo string
	// Standing rules injected into every brief.
	Standing []string
}

// Outcome records what one decision actually did.
type Outcome struct {
	Err      error
	Detail   string
	Decision decide.Decision
}

// OK reports whether the decision was applied.
func (o Outcome) OK() bool { return o.Err == nil }

func (o Outcome) String() string {
	if o.Err != nil {
		return fmt.Sprintf("FAILED  %s %s: %v", o.Decision.Kind, o.Decision.Lane, o.Err)
	}
	return fmt.Sprintf("applied %s %s: %s", o.Decision.Kind, o.Decision.Lane, o.Detail)
}

// Apply executes decisions in order, continuing past failures.
//
// One lane's failure must not abandon the rest of the tick: a swarm that stops
// entirely because a single worktree was unreadable is how free slots go
// unstaffed for hours.
func (a *Applier) Apply(ctx context.Context, ds []decide.Decision) []Outcome {
	out := make([]Outcome, 0, len(ds))
	for _, d := range ds {
		out = append(out, a.applyOne(ctx, d))
	}
	return out
}

func (a *Applier) applyOne(ctx context.Context, d decide.Decision) Outcome {
	switch d.Kind {
	case decide.KindSpawn, decide.KindRework, decide.KindAdvance:
		return a.applySpawn(ctx, d)
	case decide.KindReap:
		return a.applyReap(ctx, d)
	case decide.KindEscalate, decide.KindHold:
		// Recorded elsewhere; nothing to execute.
		return Outcome{Decision: d, Detail: "recorded"}
	default:
		return Outcome{Decision: d, Err: fmt.Errorf("unknown decision kind %q", d.Kind)}
	}
}

func (a *Applier) applySpawn(ctx context.Context, d decide.Decision) Outcome {
	st, err := a.Store.Read()
	if err != nil {
		return Outcome{Decision: d, Err: err}
	}
	lane, ok := st.Lane(d.Lane)
	if !ok {
		return Outcome{Decision: d, Err: fmt.Errorf("lane %q is not in the ledger", d.Lane)}
	}

	// Re-check the round against current state: a decision computed earlier in
	// the tick could otherwise overwrite an attempt recorded since.
	if d.Kind == decide.KindRework && d.Round <= lane.MaxRound(d.Phase) {
		return Outcome{Decision: d, Err: fmt.Errorf(
			"round %d would overwrite an existing attempt (max recorded %d)",
			d.Round, lane.MaxRound(d.Phase))}
	}

	mission := a.Missions[lane.ID]
	if mission == "" {
		mission = lane.Title
	}
	// One-shot nudges addressed to this lane ride along with the standing rules,
	// and are marked consumed once the spawn succeeds. Queued but never read,
	// `swarm-queen nudge` had no effect on orchestration at all.
	nudges, nerr := decide.NudgesFor(a.RunDir, lane.ID)
	if nerr != nil {
		return Outcome{Decision: d, Err: fmt.Errorf("read nudges: %w", nerr)}
	}
	instructions := a.Standing
	for _, n := range nudges {
		instructions = append(instructions, n.Text)
	}

	brief := SpawnBrief{
		Lane: lane.ID, Phase: d.Phase, Round: max(d.Round, 1),
		Type: "builder",
		Text: RenderBrief(BriefContext{
			RunDir: a.RunDir, Lane: lane, Phase: d.Phase, Round: max(d.Round, 1),
			Goal: st.Config.Goal, Target: st.Config.Target,
			Mission: mission, Standing: instructions, Findings: d.Evidence,
		}),
	}
	for _, p := range st.Config.Phases {
		if p.Name == d.Phase {
			brief.Model = p.Model
		}
	}

	req := bramble.SpawnRequest{Repo: a.Repo, Parent: a.Parent, Goal: lane.Title}
	if lane.Worktree != "" {
		req.Worktree = lane.Worktree
	} else {
		// -f resolves against the REMOTE, so this form is only correct for a
		// lane forking from a pushed base.
		req.CreateWorktree = true
		req.Branch = lane.Branch
		req.From = st.Config.Base
	}

	res, err := Spawn(ctx, a.Spawner, a.Store, a.RunDir, brief, req)
	if err != nil {
		return Outcome{Decision: d, Err: err}
	}
	// Stamp the baseline the phase starts from, or its completion cannot be
	// verified: an empty PhaseStartSHA leaves CommitsSinceFork at 0 and every
	// mutating `.done` is refused as an empty branch.
	worktree := res.WorktreePath
	if worktree == "" {
		worktree = lane.Worktree
	}
	// Consume only after the session exists: a nudge marked applied for a spawn
	// that then failed would be silently dropped.
	if err := decide.ConsumeNudges(a.RunDir, nudges); err != nil {
		return Outcome{Decision: d, Err: fmt.Errorf(
			"session %s IS LIVE but its nudges were not marked consumed (%w) — "+
				"they will be re-delivered on the next spawn", res.SessionID, err)}
	}
	if err := RecordPhaseBaseline(ctx, a.Git, a.Store, lane.ID, worktree); err != nil {
		return Outcome{Decision: d, Err: fmt.Errorf(
			"session %s IS LIVE at %s but its phase baseline was not recorded (%w) — "+
				"its completion cannot be verified until this is repaired",
			res.SessionID, orNoneBrief(worktree), err)}
	}
	return Outcome{Decision: d, Detail: fmt.Sprintf("session %s on %s",
		res.SessionID, orNoneBrief(res.WorktreePath))}
}

func (a *Applier) applyReap(ctx context.Context, d decide.Decision) Outcome {
	st, err := a.Store.Read()
	if err != nil {
		return Outcome{Decision: d, Err: err}
	}
	lane, ok := st.Lane(d.Lane)
	if !ok {
		return Outcome{Decision: d, Err: fmt.Errorf("lane %q is not in the ledger", d.Lane)}
	}

	wt, probeErr := reconcile.ProbeWorktree(ctx, a.Git, lane.Worktree, lane.ForkSHA)
	if probeErr != nil && wt.Path == "" {
		wt.Path = lane.Worktree
	}
	// A git failure mid-probe is UNKNOWN, not clean. ProbeWorktree sets
	// Exists=true before running any git command, so a failed status or rev-list
	// returns Exists=true with DirtyCount=0 -- exactly the shape of a measured,
	// clean worktree, and exactly the shape that permits removal. Only a
	// genuinely absent worktree is a safe error to continue past: there is then
	// nothing to destroy, and the lane still needs its branch and refs released.
	if probeErr != nil && !errors.Is(probeErr, reconcile.ErrNoWorktree) {
		return Outcome{Decision: d, Err: fmt.Errorf(
			"worktree %s could not be measured (%w); refusing to reap on an unknown state",
			lane.Worktree, probeErr)}
	}

	// Snapshot before anything is removed. Untracked work is protected by no
	// branch, so a worktree removal destroys it with nothing to recover from.
	if wt.Exists && wt.DirtyCount > 0 {
		if _, err := SnapshotAtRisk(ctx, a.Git, lane.ID, lane.Worktree); err != nil {
			return Outcome{Decision: d, Err: fmt.Errorf("snapshot before reap: %w", err)}
		}
	}

	// No resolver is not "no sessions": it is no measurement, and PlanReap
	// refuses on that rather than falling back to the decayed ledger field.
	probe := UnknownSessions()
	if a.LiveSessions != nil {
		probe = a.LiveSessions(lane)
	}
	reapLane := lane
	if d.FinalPhaseComplete && lane.Status == state.StatusRunning {
		if _, hasNext := st.Config.NextPhase(lane.Phase); hasNext {
			return Outcome{Decision: d, Err: fmt.Errorf("lane %q is not in its final phase", lane.ID)}
		}
		// A verified final phase has completed the lane's work, but do not write
		// that terminal status until the destructive cleanup has also succeeded.
		projected := *lane
		projected.Status = state.StatusDone
		reapLane = &projected
	}
	plan := PlanReap(ctx, a.Git, a.RepoDir, reapLane, wt, st.Config.Target, probe)
	if !plan.Safe {
		return Outcome{Decision: d, Err: fmt.Errorf("refused: %v", plan.Blockers)}
	}

	// Kill the session BEFORE removing its worktree: an agent left running
	// against a deleted path keeps acting.
	windowIDs := plan.WindowIDs
	if len(windowIDs) == 0 && plan.WindowID != "" {
		windowIDs = []string{plan.WindowID}
	}
	for _, windowID := range windowIDs {
		if err := SafeKillWindow(ctx, a.Tmux, windowID, a.SelfWindow, lane.ID); err != nil {
			return Outcome{Decision: d, Err: fmt.Errorf("kill window: %w", err)}
		}
	}
	if wt.Exists {
		if _, err := a.Git.Run(ctx, a.RepoDir, "worktree", "remove", "--force", lane.Worktree); err != nil {
			return Outcome{Decision: d, Err: fmt.Errorf("remove worktree: %w", err)}
		}
	}
	if lane.Branch != "" {
		// -D rather than -d: the branch was squash-merged, so -d refuses even
		// though the content landed. PlanReap already verified integration.
		if _, err := a.Git.Run(ctx, a.RepoDir, "branch", "-D", lane.Branch); err != nil {
			// A leaked branch is a FAILED reap, not a footnote on a successful
			// one. Returning a nil Err here made OK() true, so the tick counted
			// the lane closed, wrote StatusDone below, and the five-zeros audit
			// reported a clean close while the branch was still there.
			return Outcome{Decision: d,
				Detail: "worktree removed; branch NOT deleted",
				Err:    fmt.Errorf("delete branch %s: %w", lane.Branch, err)}
		}
	}
	if err := ReleaseBackup(ctx, a.Git, a.RepoDir, lane.ID); err != nil {
		return Outcome{Decision: d, Err: fmt.Errorf("release backup ref: %w", err)}
	}

	if err := a.Store.Update(func(s *state.State) error {
		l, ok := s.Lane(d.Lane)
		if !ok {
			return nil
		}
		l.Status = state.StatusDone
		l.Worktree = ""
		l.WindowID = ""
		return nil
	}); err != nil {
		return Outcome{Decision: d, Err: err}
	}
	return Outcome{Decision: d, Detail: fmt.Sprintf("closed %d step(s)", len(plan.Steps))}
}
