package lifecycle

import (
	"context"
	"fmt"
	"os"

	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// ReapPlan is what a reap would do, and why it is or is not safe.
//
// Reaping is three independent layers -- process/session, worktree, branch --
// plus the backup ref, and it drifts on whichever step is least visible. A
// leaked backup ref costs nothing operationally, which is exactly why it is the
// step that survives: it needs a mechanical check, not a habit.
type ReapPlan struct {
	Lane      string
	Worktree  string
	Branch    string
	WindowID  string
	WindowIDs []string
	Blockers  []string
	Steps     []string
	Safe      bool
}

// LiveSession describes a session still attached to a lane's worktree, as
// observed from bramble rather than read from the ledger.
type LiveSession struct {
	ID         string
	Status     string
	TmuxTarget string
}

// SessionProbe is the RESULT of asking bramble which sessions hold a lane, and
// it distinguishes the two answers a bare slice cannot.
//
// "No sessions" and "could not ask" arrive as the same empty slice, and the
// second is the dangerous one: it takes the ledger-fallback path and can remove
// a worktree out from under a live agent. Doctor already draws this distinction
// for branches -- an unmeasurable probe is SKIPPED, never reported as empty --
// and this is the same rule for sessions.
//
// The zero value is deliberately the unknown one: a caller that forgets to set
// Known gets the refusal, not the destructive path.
type SessionProbe struct {
	Sessions []LiveSession
	// Known reports that bramble actually answered. False means unmeasured.
	Known bool
}

// KnownSessions records a successful probe, including one that found nothing.
func KnownSessions(s []LiveSession) SessionProbe { return SessionProbe{Sessions: s, Known: true} }

// UnknownSessions records a probe that could not run.
func UnknownSessions() SessionProbe { return SessionProbe{} }

// PlanReap decides whether a lane can be reaped, without touching anything.
//
// The preconditions encode failures that each cost real time:
//   - the lane must be terminal;
//   - its integration must be verified BY CONTENT, since squash-merges rewrite
//     history and ancestry alone is insufficient;
//   - uncommitted work must already be snapshotted, because a worktree removal
//     destroys untracked files with nothing to recover from;
//   - the session probe must have actually RUN, because an unmeasured fleet is
//     indistinguishable from an empty one and only the latter is safe.
func PlanReap(
	ctx context.Context,
	g reconcile.GitRunner,
	repoDir string,
	lane *state.Lane,
	wt reconcile.WorktreeState,
	target string,
	probe SessionProbe,
) ReapPlan {
	live := probe.Sessions
	p := ReapPlan{
		Lane:     lane.ID,
		Worktree: lane.Worktree,
		Branch:   lane.Branch,
		WindowID: lane.WindowID,
		Safe:     true,
	}
	if len(live) > 0 {
		// Live observations replace the decayed ledger field, including when a
		// session has no pane. A stale recorded target must not be killed.
		p.WindowID = ""
	}

	// An unmeasured fleet blocks the reap outright. Proceeding would fall back to
	// the ledger's window_id -- which decayed to 1-of-12 populated -- and remove a
	// worktree without having proved that no live session owns it.
	if !probe.Known {
		p.Safe = false
		p.Blockers = append(p.Blockers,
			"bramble session probe did not run; cannot prove no live session holds this lane")
	}

	if !lane.Status.Terminal() {
		p.Safe = false
		p.Blockers = append(p.Blockers,
			fmt.Sprintf("lane is %s, not terminal", lane.Status))
	}

	// What must be TRUE to delete a branch: its content is observably present on
	// the target, measured now. Not "it has no PR", not "the ledger remembers a
	// MergeSHA" -- those are cases where nobody looked. Gating on
	// `PR != 0 && MergeSHA == ""` skipped the check for a lane with no PR and for
	// one carrying a stale MergeSHA, and deleted the branch on the strength of a
	// field rather than of the repository.
	if lane.Branch != "" {
		merged, err := BranchMerged(ctx, g, repoDir, lane.Branch, target)
		switch {
		case err != nil:
			p.Safe = false
			p.Blockers = append(p.Blockers,
				fmt.Sprintf("cannot verify integration of %s: %v", lane.Branch, err))
		case !merged:
			p.Safe = false
			p.Blockers = append(p.Blockers,
				fmt.Sprintf("%s is not merged into %s", lane.Branch, target))
		}
	}

	if wt.Exists && wt.DirtyCount > 0 {
		if !HasBackup(ctx, g, wt.Path, lane.ID) {
			p.Safe = false
			what := "uncommitted"
			if wt.HasUntracked {
				// No branch protects an untracked file.
				what = "UNTRACKED"
			}
			p.Blockers = append(p.Blockers,
				fmt.Sprintf("%s work present and not snapshotted; run a snapshot first", what))
		}
	}

	// The ledger's window_id is NOT trusted here. It decayed to 1-of-12 populated
	// in a real run, so a lane with a live agent can carry an empty window_id --
	// and planning from the ledger alone would remove the worktree out from under
	// a running session. Observed sessions win; the ledger is only a fallback.
	seenWindows := map[string]bool{}
	for _, s := range live {
		if s.TmuxTarget == "" {
			// No pane means the window is already gone; nothing to kill, but the
			// session's existence is still worth recording.
			p.Steps = append(p.Steps, "session "+s.ID+" has no pane (already gone)")
			continue
		}
		if seenWindows[s.TmuxTarget] {
			continue
		}
		seenWindows[s.TmuxTarget] = true
		p.WindowIDs = append(p.WindowIDs, s.TmuxTarget)
		p.WindowID = s.TmuxTarget // retained for callers with a single target.
		p.Steps = append(p.Steps,
			fmt.Sprintf("kill tmux window %s (session %s, %s)", s.TmuxTarget, s.ID, s.Status))
	}
	// Order matters: kill the session before removing its worktree, or the
	// agent keeps running against a path that no longer exists. A freeze the
	// lane must choose to obey is weaker than one enforced by it not existing.
	if len(live) == 0 && p.WindowID != "" {
		p.Steps = append(p.Steps, "kill tmux window "+p.WindowID)
	}
	if wt.Exists {
		p.Steps = append(p.Steps, "remove worktree "+p.Worktree)
	}
	if p.Branch != "" {
		p.Steps = append(p.Steps, "delete branch "+p.Branch)
	}
	p.Steps = append(p.Steps, "release "+BackupRef(lane.ID))
	return p
}

// BranchMerged re-exports the content-based merge check so callers of lifecycle
// do not need to reach into reconcile for it.
func BranchMerged(ctx context.Context, g reconcile.GitRunner, repoDir, branch, target string) (bool, error) {
	return reconcile.BranchMerged(ctx, g, repoDir, branch, target)
}

// FiveZeros is the cleanup audit: a reaped lane must leave nothing behind.
//
// Five things must all be zero -- no live session, no worktree, no branch, no
// backup ref, no tmux pane. Manual per-merge cleanup measured 3-for-5
// consistent, with backup refs leaking repeatedly.
type FiveZeros struct {
	Lane      string
	Session   bool
	Worktree  bool
	Branch    bool
	BackupRef bool
	TmuxPane  bool
}

// Clean reports whether every resource is released.
func (f FiveZeros) Clean() bool {
	return !f.Session && !f.Worktree && !f.Branch && !f.BackupRef && !f.TmuxPane
}

func (f FiveZeros) String() string {
	if f.Clean() {
		return f.Lane + ": fully closed"
	}
	return fmt.Sprintf("%s NOT FULLY CLOSED: session=%v worktree=%v branch=%v backupref=%v tmux=%v",
		f.Lane, f.Session, f.Worktree, f.Branch, f.BackupRef, f.TmuxPane)
}

// AuditLane checks all five resources for a terminal lane.
func AuditLane(
	ctx context.Context,
	g reconcile.GitRunner,
	tm Tmux,
	repoDir string,
	lane *state.Lane,
	hasLiveSession bool,
) FiveZeros {
	f := FiveZeros{Lane: lane.ID, Session: hasLiveSession}

	if lane.Worktree != "" {
		if fi, err := os.Stat(lane.Worktree); err == nil && fi.IsDir() {
			f.Worktree = true
		}
	}
	if lane.Branch != "" {
		if out, err := g.Run(ctx, repoDir, "branch", "--list", lane.Branch); err == nil && out != "" {
			f.Branch = true
		}
	}
	f.BackupRef = HasBackup(ctx, g, repoDir, lane.ID)
	if lane.WindowID != "" && WindowExists(ctx, tm, lane.WindowID) {
		f.TmuxPane = true
	}
	return f
}
