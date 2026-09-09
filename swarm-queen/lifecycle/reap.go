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
	Lane     string
	Worktree string
	Branch   string
	WindowID string
	Blockers []string
	Steps    []string
	Safe     bool
}

// LiveSession describes a session still attached to a lane's worktree, as
// observed from bramble rather than read from the ledger.
type LiveSession struct {
	ID         string
	Status     string
	TmuxTarget string
}

// PlanReap decides whether a lane can be reaped, without touching anything.
//
// The preconditions encode failures that each cost real time:
//   - the lane must be terminal;
//   - its integration must be verified BY CONTENT, since squash-merges rewrite
//     history and ancestry alone is insufficient;
//   - uncommitted work must already be snapshotted, because a worktree removal
//     destroys untracked files with nothing to recover from.
func PlanReap(
	ctx context.Context,
	g reconcile.GitRunner,
	repoDir string,
	lane *state.Lane,
	wt reconcile.WorktreeState,
	target string,
	live []LiveSession,
) ReapPlan {
	p := ReapPlan{
		Lane:     lane.ID,
		Worktree: lane.Worktree,
		Branch:   lane.Branch,
		WindowID: lane.WindowID,
		Safe:     true,
	}

	if !lane.Status.Terminal() {
		p.Safe = false
		p.Blockers = append(p.Blockers,
			fmt.Sprintf("lane is %s, not terminal", lane.Status))
	}

	// An open PR still needs its worktree; reaping one strands the review.
	if lane.PR != 0 && lane.MergeSHA == "" {
		merged, err := BranchMerged(ctx, g, repoDir, lane.Branch, target)
		switch {
		case err != nil:
			p.Safe = false
			p.Blockers = append(p.Blockers,
				fmt.Sprintf("cannot verify integration of %s: %v", lane.Branch, err))
		case !merged:
			p.Safe = false
			p.Blockers = append(p.Blockers,
				fmt.Sprintf("PR #%d is recorded but %s is not merged into %s",
					lane.PR, lane.Branch, target))
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
	for _, s := range live {
		if s.TmuxTarget == "" {
			// No pane means the window is already gone; nothing to kill, but the
			// session's existence is still worth recording.
			p.Steps = append(p.Steps, "session "+s.ID+" has no pane (already gone)")
			continue
		}
		p.WindowID = s.TmuxTarget
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
