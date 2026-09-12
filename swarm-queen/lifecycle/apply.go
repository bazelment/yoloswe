package lifecycle

import (
	"context"
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

	// tickNudges holds the run-wide one-shot nudges for the Apply call in
	// progress, and tickDelivered records whether a spawn rendered them. Both
	// are reset by Apply; they are per-call scratch, not configuration.
	tickNudges    []decide.Nudge
	tickDelivered bool
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
	// Run-wide one-shot nudges are delivered to every lane staffed by THIS tick,
	// then retired once at the end. Consuming them per-spawn retired them on the
	// first lane, so a nudge whose whole meaning is "tell the run" reached
	// exactly one of the lanes it was addressed to.
	//
	// Lane-scoped nudges are unaffected: they have one addressee, and
	// applySpawn still retires each as its own spawn completes.
	a.tickNudges = nil
	a.tickDelivered = false
	if runWide, err := decide.RunWideNudges(a.RunDir); err == nil {
		a.tickNudges = runWide
	}

	out := make([]Outcome, 0, len(ds))
	for _, d := range ds {
		out = append(out, a.applyOne(ctx, d))
	}

	// Retire only if a spawn actually rendered them into a brief. A tick that
	// spawned nothing has not delivered anything, and must not silently swallow
	// an instruction the next tick would have carried.
	if a.tickDelivered {
		if err := decide.ConsumeNudges(a.RunDir, a.tickNudges); err != nil {
			out = append(out, Outcome{
				Decision: decide.Decision{Kind: decide.KindHold, Lane: "(run)"},
				Err: fmt.Errorf("run-wide nudges were delivered but not marked "+
					"consumed (%w) — they will be re-delivered next tick", err),
			})
		}
	}
	a.tickNudges, a.tickDelivered = nil, false
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
	// and are retired once the spawn is usable. Queued but never read,
	// `swarm-queen nudge` had no effect on orchestration at all.
	instructions, nudges, nerr := BriefInstructions(a.RunDir, lane.ID, a.Standing, a.tickNudges)
	if nerr != nil {
		return Outcome{Decision: d, Err: nerr}
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

	// Stamp the baseline BEFORE spawning where the worktree already exists, which
	// is every case but a first spawn. Nothing is live yet, so a git failure here
	// costs a refused decision rather than an unverifiable session -- and the
	// window in which a session exists without a baseline closes entirely.
	//
	// It cannot be done first for a lane whose worktree bramble is about to
	// create: there is no HEAD to read until it exists. That case keeps the
	// after-the-fact stamp, and its failure is reported as an explicit repair
	// obligation naming the live session.
	preStamped, err := SpawnBaseline(ctx, a.Git, a.Store, lane.ID, lane.Worktree)
	if err == nil && !preStamped {
		// No worktree yet: resolve the fork point from the base it will be
		// created at, so the baseline is recorded before anything can commit.
		err = SpawnBaselineFromBase(ctx, a.Git, a.Store, a.RepoDir, lane.ID, st.Config.Base)
		preStamped = err == nil
	}
	if err != nil {
		return Outcome{Decision: d, Err: fmt.Errorf(
			"refusing to spawn %s: its phase baseline could not be recorded (%w); "+
				"a session without one has every mutating `.done` refused",
			lane.ID, err)}
	}

	res, err := Spawn(ctx, a.Spawner, a.Store, a.RunDir, brief, req)
	if err != nil {
		return Outcome{Decision: d, Err: err}
	}
	worktree := res.WorktreePath
	if worktree == "" {
		worktree = lane.Worktree
	}
	// The brief carrying them is written, so the run-wide nudges have now been
	// delivered to at least one lane and may be retired at the end of the tick.
	if len(a.tickNudges) > 0 {
		a.tickDelivered = true
	}
	if err := FinishSpawn(ctx, a.Git, a.Store, a.RunDir, lane.ID, worktree, nudges, preStamped); err != nil {
		return Outcome{Decision: d, Err: fmt.Errorf(
			"REPAIR REQUIRED: session %s IS LIVE at %s but the spawn did not complete: %w",
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
	// A git failure mid-probe is UNKNOWN, not clean. A genuinely absent worktree
	// is measured (nothing to destroy, branch and refs still to release); a probe
	// that could not complete is not.
	if wt.Unknown() {
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
