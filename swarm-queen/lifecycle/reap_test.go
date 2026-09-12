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
		reconcile.WorktreeState{Path: dir, Exists: true}.Measure(), "main", KnownSessions(nil))
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
	wt := reconcile.WorktreeState{Path: dir, Exists: true, DirtyCount: 1, HasUntracked: true}.Measure()

	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", KnownSessions(nil))
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
	// Branch "b" must exist and be integrated, or the integration check blocks
	// for that reason instead and this test would never reach the snapshot
	// question it is asking.
	git(t, dir, "branch", "b")
	write(t, dir, "wip.txt", "work")

	lane := &state.Lane{ID: "replay", Status: state.StatusDone, Branch: "b", Worktree: dir}
	wt := reconcile.WorktreeState{Path: dir, Exists: true, DirtyCount: 1, HasUntracked: true}.Measure()

	if p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", KnownSessions(nil)); p.Safe {
		t.Fatal("precondition: should be unsafe before the snapshot")
	}
	if _, err := SnapshotAtRisk(context.Background(), reconcile.ExecGit{}, "replay", dir); err != nil {
		t.Fatal(err)
	}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", KnownSessions(nil))
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
		reconcile.WorktreeState{Exists: false}.Measure(), "main", KnownSessions(nil))
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
		reconcile.WorktreeState{Exists: false}.Measure(), "main", KnownSessions(nil))
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
	git(t, dir, "branch", "b")
	lane := &state.Lane{
		ID: "ordered", Status: state.StatusDone,
		Branch: "b", Worktree: dir, WindowID: "@1380",
	}
	// An OBSERVED session: a kill step only exists for a window bramble reported.
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Path: dir, Exists: true}.Measure(), "main",
		KnownSessions([]LiveSession{{ID: "sess-ordered", Status: "idle", TmuxTarget: "@1380"}}))

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
		reconcile.WorktreeState{Path: dir, Exists: true}.Measure(), "main", KnownSessions(live))

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
		reconcile.WorktreeState{Path: dir, Exists: true}.Measure(), "main", KnownSessions(live))
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
		reconcile.WorktreeState{Path: dir, Exists: true}.Measure(), "main", KnownSessions(live))
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

// An unmeasured session probe must block the reap. "No sessions" and "could not
// ask" arrive as the same empty slice, and only the first is safe: proceeding on
// the second falls back to the ledger's window_id -- 1-of-12 populated in a real
// run -- and removes a worktree without proving no live agent holds it.
func TestPlanReapRefusesUnknownSessionProbe(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "branch", "b")
	lane := &state.Lane{ID: "unmeasured", Status: state.StatusDone, Branch: "b", Worktree: dir}
	wt := reconcile.WorktreeState{Path: dir, Exists: true}.Measure()

	// Same lane, same worktree: only the probe's Known bit differs.
	if p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main",
		KnownSessions(nil)); !p.Safe {
		t.Fatalf("precondition: a MEASURED empty fleet must be reapable: %v", p.Blockers)
	}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main",
		UnknownSessions())
	if p.Safe {
		t.Errorf("an unmeasured session probe must block the reap: %+v", p)
	}
	if !blocked(p, "session probe did not run") {
		t.Errorf("the blocker must name the unmeasured probe, got %v", p.Blockers)
	}
}

// The zero SessionProbe is unknown, so a caller that forgets to set it gets the
// refusal rather than the destructive path.
func TestZeroSessionProbeIsUnknown(t *testing.T) {
	t.Parallel()
	if (SessionProbe{}).Known {
		t.Error("the zero SessionProbe must be unknown, or forgetting to set it permits a reap")
	}
	if !KnownSessions(nil).Known {
		t.Error("KnownSessions must record that the probe ran, even with no sessions")
	}
}

// A branch is deleted only when its content is observably on the target. Gating
// the check on `PR != 0 && MergeSHA == ""` skipped it for a lane with no PR and
// for one carrying a stale MergeSHA, deleting the branch on the strength of a
// ledger field rather than of the repository.
func TestPlanReapVerifiesIntegrationWithoutAPR(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "orphan")
	write(t, dir, "only-here.txt", "never landed")
	git(t, dir, "add", "only-here.txt")
	git(t, dir, "commit", "-q", "-m", "work that never merged")
	git(t, dir, "checkout", "-q", "main")

	// No PR recorded at all -- the case the old gate skipped entirely.
	lane := &state.Lane{ID: "no-pr", Status: state.StatusDone, Branch: "orphan"}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Exists: false}.Measure(), "main", KnownSessions(nil))
	if p.Safe {
		t.Errorf("an unintegrated branch must block even with no PR: %+v", p)
	}
	if !blocked(p, "not merged") {
		t.Errorf("expected the integration blocker, got %v", p.Blockers)
	}
}

// A recorded MergeSHA is a ledger field, not a measurement. It must not stand in
// for the content check: the SHA can name a merge that was reverted, or a branch
// that was force-pushed since.
func TestPlanReapVerifiesIntegrationDespiteRecordedMergeSHA(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "claimed")
	write(t, dir, "unlanded.txt", "still only here")
	git(t, dir, "add", "unlanded.txt")
	git(t, dir, "commit", "-q", "-m", "work")
	git(t, dir, "checkout", "-q", "main")

	lane := &state.Lane{
		ID: "stale-merge", Status: state.StatusDone, Branch: "claimed",
		PR: 42, MergeSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane,
		reconcile.WorktreeState{Exists: false}.Measure(), "main", KnownSessions(nil))
	if p.Safe {
		t.Errorf("a recorded MergeSHA must not substitute for the content check: %+v", p)
	}
}

// Once bramble has answered, only what bramble OBSERVED may be killed --
// including when it observed nothing. The ledger's window_id decayed to 1-of-12
// populated in a real run, so a recorded target is as likely to name a window
// tmux has since reused for something unrelated as it is to name this lane's.
func TestPlanReapNeverKillsFromTheLedgerOnceProbed(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "branch", "b")
	lane := &state.Lane{
		ID: "stale-window", Status: state.StatusDone,
		Branch: "b", Worktree: dir, WindowID: "@1380",
	}
	wt := reconcile.WorktreeState{Path: dir, Exists: true}.Measure()

	// bramble answered and found no session on this lane.
	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", KnownSessions(nil))
	if !p.Safe {
		t.Fatalf("a measured-empty fleet must still be reapable: %v", p.Blockers)
	}
	for _, step := range p.Steps {
		if strings.Contains(step, "kill tmux window") {
			t.Errorf("nothing observed holds this lane, so nothing may be killed: %v", p.Steps)
		}
	}
	if p.WindowID != "" || len(p.WindowIDs) != 0 {
		t.Errorf("no kill target may survive a successful probe, got %q / %v", p.WindowID, p.WindowIDs)
	}
}

// When the probe did NOT run the plan is refused anyway, but the ledger's claim
// is still shown -- marked unverified -- so an operator can see what it said.
func TestPlanReapShowsUnverifiedLedgerWindowOnlyWhenUnprobed(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	git(t, dir, "branch", "b")
	lane := &state.Lane{
		ID: "unprobed", Status: state.StatusDone,
		Branch: "b", Worktree: dir, WindowID: "@1380",
	}
	wt := reconcile.WorktreeState{Path: dir, Exists: true}.Measure()

	p := PlanReap(context.Background(), reconcile.ExecGit{}, dir, lane, wt, "main", UnknownSessions())
	if p.Safe {
		t.Fatal("an unmeasured fleet must refuse the reap")
	}
	var shown bool
	for _, step := range p.Steps {
		if strings.Contains(step, "@1380") && strings.Contains(step, "UNVERIFIED") {
			shown = true
		}
	}
	if !shown {
		t.Errorf("the refusal should still show the ledger's unverified claim: %v", p.Steps)
	}
}
