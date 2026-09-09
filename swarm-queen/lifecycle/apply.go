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
	// with a live agent can carry an empty one. Nil means no session data, which
	// makes a reap plan omit the kill step.
	LiveSessions func(*state.Lane) []LiveSession
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
	brief := SpawnBrief{
		Lane: lane.ID, Phase: d.Phase, Round: max(d.Round, 1),
		Type: "builder",
		Text: RenderBrief(BriefContext{
			RunDir: a.RunDir, Lane: lane, Phase: d.Phase, Round: max(d.Round, 1),
			Goal: st.Config.Goal, Target: st.Config.Target,
			Mission: mission, Standing: a.Standing, Findings: d.Evidence,
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

	// Snapshot before anything is removed. Untracked work is protected by no
	// branch, so a worktree removal destroys it with nothing to recover from.
	if wt.Exists && wt.DirtyCount > 0 {
		if _, err := SnapshotAtRisk(ctx, a.Git, lane.ID, lane.Worktree); err != nil {
			return Outcome{Decision: d, Err: fmt.Errorf("snapshot before reap: %w", err)}
		}
	}

	var live []LiveSession
	if a.LiveSessions != nil {
		live = a.LiveSessions(lane)
	}
	plan := PlanReap(ctx, a.Git, a.RepoDir, lane, wt, st.Config.Target, live)
	if !plan.Safe {
		return Outcome{Decision: d, Err: fmt.Errorf("refused: %v", plan.Blockers)}
	}

	// Kill the session BEFORE removing its worktree: an agent left running
	// against a deleted path keeps acting.
	if plan.WindowID != "" {
		if err := SafeKillWindow(ctx, a.Tmux, plan.WindowID, a.SelfWindow, lane.ID); err != nil {
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
			return Outcome{Decision: d, Detail: "worktree removed; branch delete failed: " + err.Error()}
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
