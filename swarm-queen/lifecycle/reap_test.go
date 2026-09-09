package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

func blocked(p ReapPlan, substr string) bool {
	for _, b := range p.Blockers {
		if strings.Contains(b, substr) {
			return true
		}
	}
	return false
}

func TestPlanReapRefusesNonTerminalLane(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	lane := &state.Lane{ID: "x", Status: state.StatusRunning, Branch: "b", Worktree: dir}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Path: dir, Exists: true}, "main", nil)
	if p.Safe || !blocked(p, "not terminal") {
		t.Errorf("a running lane must not be reapable: %+v", p)
	}
}

// Uncommitted work with no snapshot must block. A worktree removal destroys
// untracked files outright, with nothing to recover from.
func TestPlanReapRefusesUnsnapshottedWork(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	lane := &state.Lane{ID: "replay", Status: state.StatusDone, Branch: "b", Worktree: dir}
	wt := reconcile.WorktreeState{Path: dir, Exists: true, DirtyCount: 1, HasUntracked: true}

	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", nil)
	if p.Safe {
		t.Fatalf("unsnapshotted work must block the reap: %+v", p)
	}
	if !blocked(p, "UNTRACKED") {
		t.Errorf("expected the untracked-work blocker, got %v", p.Blockers)
	}
}

// Once snapshotted, the same lane becomes reapable.
func TestPlanReapAllowsAfterSnapshot(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, "wip.txt", "work")

	lane := &state.Lane{ID: "replay", Status: state.StatusDone, Branch: "b", Worktree: dir}
	wt := reconcile.WorktreeState{Path: dir, Exists: true, DirtyCount: 1, HasUntracked: true}

	if p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", nil); p.Safe {
		t.Fatal("precondition: should be unsafe before the snapshot")
	}
	if _, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "replay", dir); err != nil {
		t.Fatal(err)
	}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", nil)
	if !p.Safe {
		t.Errorf("snapshotted work should be reapable: %v", p.Blockers)
	}
}

// A recorded PR that has not landed must block: reaping strands the review.
func TestPlanReapRefusesUnmergedPR(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "f.txt", "unmerged")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-q", "-m", "work")
	git(t, dir, "checkout", "-q", "main")

	lane := &state.Lane{ID: "pr-lane", Status: state.StatusDone, Branch: "feature", PR: 11968}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Exists: false}, "main", nil)
	if p.Safe || !blocked(p, "not merged") {
		t.Errorf("unmerged PR branch must block: %+v", p)
	}
}

// Squash-merged work IS integrated, even though ancestry says otherwise. If this
// blocked, every completed lane would leak its worktree forever.
func TestPlanReapAcceptsSquashMergedPR(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "f.txt", "shipped")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-q", "-m", "work")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--squash", "feature")
	git(t, dir, "commit", "-q", "-m", "squashed")

	lane := &state.Lane{ID: "pr-lane", Status: state.StatusDone, Branch: "feature", PR: 11968}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Exists: false}, "main", nil)
	if !p.Safe {
		t.Errorf("squash-merged branch must be reapable: %v", p.Blockers)
	}
}

// The session must be killed BEFORE its worktree is removed: an agent left
// running against a deleted path keeps acting, and a freeze it must choose to
// obey is weaker than one enforced by the session not existing.
func TestReapStepsKillSessionBeforeRemovingWorktree(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	lane := &state.Lane{
		ID: "ordered", Status: state.StatusDone,
		Branch: "b", Worktree: dir, WindowID: "@1380",
	}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Path: dir, Exists: true}, "main", nil)

	var killIdx, rmIdx = -1, -1
	for i, s := range p.Steps {
		switch {
		case strings.HasPrefix(s, "kill tmux window"):
			killIdx = i
		case strings.HasPrefix(s, "remove worktree"):
			rmIdx = i
		}
	}
	if killIdx < 0 || rmIdx < 0 {
		t.Fatalf("expected both steps, got %v", p.Steps)
	}
	if killIdx > rmIdx {
		t.Errorf("worktree removed before the session was killed: %v", p.Steps)
	}
	// The backup ref release must be present -- it is the step that leaks.
	if last := p.Steps[len(p.Steps)-1]; !strings.Contains(last, BackupRefPrefix) {
		t.Errorf("backup-ref release must be the final step, got %q", last)
	}
}

// The five-zeros audit: a leaked backup ref is a real finding, not a nit.
func TestAuditLaneDetectsLeakedBackupRef(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, "wip.txt", "x")
	if _, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "leaky", dir); err != nil {
		t.Fatal(err)
	}
	tm, _ := privateTmux(t)

	lane := &state.Lane{ID: "leaky", Status: state.StatusDone}
	f := AuditLane(context.Background(), reconcile.ExecGit{}, tm, dir, lane, false)
	if f.Clean() {
		t.Error("a leaked backup ref must not read as fully closed")
	}
	if !f.BackupRef {
		t.Errorf("BackupRef should be true: %+v", f)
	}

	if err := ReleaseBackup(context.Background(), reconcile.ExecGit{}, dir, "leaky"); err != nil {
		t.Fatal(err)
	}
	f = AuditLane(context.Background(), reconcile.ExecGit{}, tm, dir, lane, false)
	if !f.Clean() {
		t.Errorf("after release the lane should be fully closed: %s", f)
	}
}

func TestAuditLaneDetectsEachLayer(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "branch", "lane-branch")
	tm, _ := privateTmux(t)
	win := newWindow(t, tm, "lane")

	lane := &state.Lane{
		ID: "full", Status: state.StatusDone,
		Branch: "lane-branch", Worktree: dir, WindowID: win,
	}
	f := AuditLane(context.Background(), reconcile.ExecGit{}, tm, dir, lane, true)
	if f.Clean() {
		t.Fatal("expected findings")
	}
	if !f.Session || !f.Worktree || !f.Branch || !f.TmuxPane {
		t.Errorf("every live layer should be reported: %+v", f)
	}
}

// The ledger's window_id decayed to 1-of-12 populated in a real run, so a lane
// with a LIVE agent can carry an empty window_id. Planning from the ledger alone
// would then remove the worktree out from under a running session.
//
// Observed sessions must win over the ledger.
func TestPlanReapKillsSessionsObservedNotRecorded(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	lane := &state.Lane{
		ID: "worker-ha-recreate-drain", Status: state.StatusDone,
		Branch: "b", Worktree: dir,
		WindowID: "", // exactly what the live ledger holds
	}
	live := []LiveSession{{
		ID: "deploy-harden-worker-ha-0908-builder-71c1de2e", Status: "idle", TmuxTarget: "@1380",
	}}

	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Path: dir, Exists: true}, "main", live)

	var killIdx, rmIdx = -1, -1
	for i, s := range p.Steps {
		switch {
		case strings.Contains(s, "kill tmux window @1380"):
			killIdx = i
		case strings.HasPrefix(s, "remove worktree"):
			rmIdx = i
		}
	}
	if killIdx < 0 {
		t.Fatalf("a live session with an empty ledger window_id must still be killed: %v", p.Steps)
	}
	if rmIdx >= 0 && killIdx > rmIdx {
		t.Errorf("worktree removed before the live session was killed: %v", p.Steps)
	}
}

func TestPlanReapKeepsEveryObservedWindow(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	lane := &state.Lane{ID: "many", Status: state.StatusDone, Worktree: dir}
	live := []LiveSession{
		{ID: "one", TmuxTarget: "@1"},
		{ID: "two", TmuxTarget: "@2"},
		{ID: "duplicate", TmuxTarget: "@1"},
	}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Path: dir, Exists: true}, "main", live)
	if got, want := len(p.WindowIDs), 2; got != want {
		t.Fatalf("got window ids %v, want two distinct targets", p.WindowIDs)
	}
}

// A session whose window is already gone should be recorded, not "killed".
func TestPlanReapHandlesPanelessSession(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	lane := &state.Lane{ID: "clustererrors", Status: state.StatusFailed, Branch: "b", Worktree: dir, WindowID: "@1380"}
	live := []LiveSession{{ID: "sess-dead", Status: "failed", TmuxTarget: ""}}

	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Path: dir, Exists: true}, "main", live)
	for _, s := range p.Steps {
		if strings.Contains(s, "kill tmux window ") && !strings.Contains(s, "session") {
			t.Errorf("must not attempt to kill a nonexistent pane: %v", p.Steps)
		}
	}
	var noted bool
	for _, s := range p.Steps {
		if strings.Contains(s, "no pane") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("a paneless session should be recorded: %v", p.Steps)
	}
	if p.WindowID != "" || len(p.WindowIDs) != 0 {
		t.Errorf("live paneless session must replace the stale ledger target: %+v", p)
	}
}
