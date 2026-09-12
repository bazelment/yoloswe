package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

func applier(t *testing.T, repo string) (*Applier, *state.Store, string, *fakeSpawner) {
	t.Helper()
	runDir, store := seedRun(t)
	sp := &fakeSpawner{res: bramble.SpawnResult{SessionID: "sess-1", WorktreePath: repo}}
	return &Applier{
		Store: store, RunDir: runDir, RepoDir: repo,
		Git: reconcile.ExecGit{}, Spawner: sp,
		Parent: "orchestrator-1", Repo: "kernel",
		// A probe that RAN and found nothing. Omitting this is "unknown", which
		// refuses the reap -- covered by TestApplyReapRefusesUnknownSessionProbe.
		LiveSessions: func(*state.Lane) SessionProbe { return KnownSessions(nil) },
	}, store, runDir, sp
}

// Spawning must carry --parent, or a completed lane reports nowhere.
func TestApplySpawnPassesParentAndRepo(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, _, _, sp := applier(t, repo)

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if !outs[0].OK() {
		t.Fatalf("spawn failed: %v", outs[0].Err)
	}
	if sp.seen.Parent != "orchestrator-1" {
		t.Errorf("--parent not passed: %+v", sp.seen)
	}
	if sp.seen.Repo != "kernel" {
		t.Errorf("-r not passed: %+v", sp.seen)
	}
	if !strings.Contains(sp.seen.Prompt, "touch") {
		t.Errorf("brief must carry the done-file instruction: %q", sp.seen.Prompt)
	}
}

// A decision computed earlier in the tick must not overwrite an attempt recorded
// since: state can move between decide and apply.
func TestApplyRefusesReworkThatWouldOverwriteARound(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)

	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.RecordSession("swe", 1, "s1")
		lane.RecordSession("swe", 2, "s2")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindRework, Phase: "swe", Round: 2},
	})
	if outs[0].OK() {
		t.Fatal("a stale rework round must be refused at apply time")
	}
	if !strings.Contains(outs[0].Err.Error(), "would overwrite") {
		t.Errorf("err = %v", outs[0].Err)
	}
}

// One lane's failure must not abandon the rest of the tick.
func TestApplyContinuesPastFailures(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, _, _, _ := applier(t, repo)

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "ghost", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if len(outs) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(outs))
	}
	if outs[0].OK() {
		t.Error("the unknown lane should have failed")
	}
	if !outs[1].OK() {
		t.Errorf("the good lane must still be applied: %v", outs[1].Err)
	}
}

// Reap must refuse a lane whose preconditions fail, even if a decision said to.
func TestApplyReapReChecksPreconditions(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)

	// Lane is still running: not reapable regardless of the decision.
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Status = state.StatusRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindReap},
	})
	if outs[0].OK() {
		t.Fatal("reap of a running lane must be refused at apply time")
	}
	if !strings.Contains(outs[0].Err.Error(), "not terminal") {
		t.Errorf("err = %v", outs[0].Err)
	}
}

func TestApplyReapCompletesVerifiedFinalPhase(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Status = state.StatusRunning
		lane.Phase = "clean"
		lane.Branch = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outs := a.Apply(context.Background(), []decide.Decision{{
		Lane: "lane-a", Kind: decide.KindReap, FinalPhaseComplete: true,
	}})
	if !outs[0].OK() {
		t.Fatalf("final-phase reap failed: %v", outs[0].Err)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.Status != state.StatusDone {
		t.Errorf("status = %s, want done", lane.Status)
	}
}

// A reap must snapshot uncommitted work before removing anything.
func TestApplyReapSnapshotsBeforeRemoving(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)
	a.SelfWindow = "@1" // provable non-self, so a kill would be permitted

	write(t, repo, "wip.txt", "uncommitted")
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Status = state.StatusDone
		lane.Worktree = repo
		lane.Branch = "" // no branch to delete in this fixture
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	a.Apply(context.Background(), []decide.Decision{{Lane: "lane-a", Kind: decide.KindReap}})

	// Whether or not the removal succeeded on this fixture, the work must be
	// recoverable: that is the invariant.
	if !HasBackup(context.Background(), reconcile.ExecGit{}, repo, "lane-a") {
		if _, err := os.Stat(filepath.Join(repo, "wip.txt")); err != nil {
			t.Error("uncommitted work was neither snapshotted nor left in place")
		}
	}
}

// failingGit fails the git subcommands named in fail, and delegates the rest to
// the real binary, so a test can break exactly one probe or one mutation.
type failingGit struct {
	inner reconcile.GitRunner
	fail  map[string]bool
}

func (f failingGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	if len(args) > 0 && f.fail[args[0]] {
		return "", fmt.Errorf("simulated failure: git %s", strings.Join(args, " "))
	}
	return f.inner.Run(ctx, dir, args...)
}

// A git probe that could not run is UNKNOWN, not clean. ProbeWorktree sets
// Exists=true before running any git command, so a failed status returns
// Exists=true with DirtyCount=0 -- the exact shape of a measured, clean worktree,
// and the shape that permits removal.
func TestApplyReapRefusesUnmeasurableWorktree(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Status = state.StatusDone
		lane.Worktree = repo
		lane.Branch = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.Git = failingGit{inner: reconcile.ExecGit{}, fail: map[string]bool{"status": true}}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindReap},
	})
	if outs[0].OK() {
		t.Fatal("a worktree that could not be measured must not be reaped")
	}
	if !strings.Contains(outs[0].Err.Error(), "unknown state") {
		t.Errorf("the error must name the unknown state, got %v", outs[0].Err)
	}
	// The worktree must still be there: a refusal that already removed it is not
	// a refusal.
	if _, err := os.Stat(repo); err != nil {
		t.Errorf("the worktree was removed despite the refusal: %v", err)
	}
}

// An absent worktree is a safe error to continue past: there is nothing to
// destroy, and the lane still needs its branch and backup ref released.
func TestApplyReapProceedsWhenWorktreeIsSimplyGone(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Status = state.StatusDone
		lane.Worktree = filepath.Join(t.TempDir(), "never-existed")
		lane.Branch = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindReap},
	})
	if !outs[0].OK() {
		t.Fatalf("an absent worktree is not an unknown one: %v", outs[0].Err)
	}
}

// A leaked branch is a FAILED reap, not a footnote on a successful one. With a
// nil Err the tick counted the lane closed, wrote StatusDone, and the five-zeros
// audit reported a clean close while the branch was still there.
func TestApplyReapFailsWhenBranchDeleteFails(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)
	// The branch must be genuinely integrated into the run's target, or the reap
	// is refused at the integration check and never reaches the branch delete.
	git(t, repo, "branch", "swarm/t")
	git(t, repo, "branch", "b-a")
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Status = state.StatusDone
		lane.Worktree = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Fail only `git branch -D`; the integration check uses merge-base/diff.
	a.Git = failingGit{inner: reconcile.ExecGit{}, fail: map[string]bool{"branch": true}}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindReap},
	})
	if outs[0].OK() {
		t.Fatal("a branch that was not deleted must report the reap as failed")
	}
	if !strings.Contains(outs[0].Err.Error(), "delete branch") {
		t.Errorf("the error must name the leaked branch, got %v", outs[0].Err)
	}
	// The branch is still there -- that is what makes this a leak rather than a
	// cosmetic error -- and the lane must still name it, or a retry cannot find
	// the resource left behind.
	if out := git(t, repo, "branch", "--list", "b-a"); !strings.Contains(out, "b-a") {
		t.Fatalf("premise broken: the branch should have survived the failed delete, got %q", out)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.Branch != "b-a" {
		t.Errorf("lane.Branch = %q; a failed reap must keep naming the leaked branch", lane.Branch)
	}
	// FiveZeros is the audit this protects: it must see the branch, not a clean
	// close. A nil Err here previously let the tick report a fully closed lane.
	z := AuditLane(context.Background(), reconcile.ExecGit{}, nil, repo, lane, KnownSessions(nil))
	if !z.Branch {
		t.Error("the five-zeros audit must report the leaked branch")
	}
	if z.Clean() {
		t.Error("the audit must not report a clean close while the branch exists")
	}
}

// An Applier with no session resolver has not measured the fleet, so it must
// refuse rather than fall back to the ledger's decayed window_id.
func TestApplyReapRefusesUnknownSessionProbe(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)
	a.LiveSessions = nil // no measurement at all
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Status = state.StatusDone
		lane.Worktree = repo
		lane.Branch = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindReap},
	})
	if outs[0].OK() {
		t.Fatal("a reap with no session measurement must be refused")
	}
	if !strings.Contains(outs[0].Err.Error(), "session probe did not run") {
		t.Errorf("the error must name the unmeasured probe, got %v", outs[0].Err)
	}
}

// A spawn must stamp the baseline HEAD its phase starts from. Without it
// PhaseStartSHA stays empty, ProbeWorktree skips commit counting, and
// PhaseCompletion refuses EVERY mutating `.done` as an empty branch -- the
// refusal that exists to catch a lane that did nothing fires on every lane that
// worked.
func TestApplySpawnRecordsPhaseBaseline(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if !outs[0].OK() {
		t.Fatalf("spawn failed: %v", outs[0].Err)
	}
	head := git(t, repo, "rev-parse", "HEAD")
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.PhaseStartSHA != head {
		t.Errorf("PhaseStartSHA = %q, want the worktree head %q", lane.PhaseStartSHA, head)
	}
	if lane.ForkSHA != head {
		t.Errorf("ForkSHA = %q, want the head at first spawn %q", lane.ForkSHA, head)
	}

	// The stamp is what makes the empty-branch refusal meaningful: with it
	// recorded, a phase that commits nothing is measurable as such.
	wt, err := reconcile.ProbeWorktree(context.Background(), reconcile.ExecGit{}, repo, lane.PhaseStartSHA)
	if err != nil {
		t.Fatal(err)
	}
	if wt.CommitsSinceFork != 0 {
		t.Errorf("a phase that has not committed should measure 0 commits, got %d", wt.CommitsSinceFork)
	}
}

// ForkSHA is the lane's ORIGINAL fork point and is stamped once; PhaseStartSHA
// moves with each phase, so a phase is measured against what it inherited.
func TestPhaseBaselineAdvancesButForkSHAIsStampedOnce(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, _ := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Worktree = repo
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first := git(t, repo, "rev-parse", "HEAD")
	if err := RecordPhaseBaseline(context.Background(), reconcile.ExecGit{}, store, "lane-a", repo); err != nil {
		t.Fatal(err)
	}

	write(t, repo, "phase-one-work.txt", "committed")
	git(t, repo, "add", "phase-one-work.txt")
	git(t, repo, "commit", "-q", "-m", "phase one did work")
	second := git(t, repo, "rev-parse", "HEAD")
	if err := RecordPhaseBaseline(context.Background(), reconcile.ExecGit{}, store, "lane-a", repo); err != nil {
		t.Fatal(err)
	}

	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.PhaseStartSHA != second {
		t.Errorf("PhaseStartSHA = %q, want the new phase's head %q", lane.PhaseStartSHA, second)
	}
	if lane.ForkSHA != first {
		t.Errorf("ForkSHA = %q, want the ORIGINAL fork point %q", lane.ForkSHA, first)
	}
	_ = a
}

// A spawn that does not complete must not retire the operator's instruction.
//
// This asserts the OUTCOME (the nudge survives a failed spawn), not the internal
// ordering. The ordering rule in FinishSpawn -- baseline before consume -- is
// kept as defence, but it is no longer independently observable: since the
// baseline is established BEFORE the session exists in every path, there is no
// reachable state between the two steps to construct. Three attempts to build
// one each ended up failing before the window instead (a rev-parse failure now
// refuses pre-spawn; deleting the lane fails Spawn's own ledger write), and a
// test that passes either way is worse than none because it counts as coverage.
func TestSpawnKeepsNudgesWhenTheSpawnDoesNotComplete(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, runDir, sp := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Worktree = repo
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := decide.AppendNudge(runDir, decide.Nudge{Text: "do the thing", Lane: "lane-a"}); err != nil {
		t.Fatal(err)
	}
	sp.err = errors.New("bramble refused the session")

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if outs[0].OK() {
		t.Fatal("a spawn that failed must not report success")
	}
	pending, err := decide.LaneNudges(runDir, "lane-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("the nudge must survive a failed spawn for a later one to deliver, got %v", pending)
	}
}

// Where the worktree already exists the baseline is stamped BEFORE the session,
// so a git failure refuses the decision instead of leaving a live session whose
// completion can never be verified.
func TestSpawnRefusesBeforeGoingLiveWhenBaselineFails(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, sp := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Worktree = repo
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.Git = failingGit{inner: reconcile.ExecGit{}, fail: map[string]bool{"rev-parse": true}}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if outs[0].OK() {
		t.Fatal("the spawn must be refused when its baseline cannot be recorded")
	}
	// Nothing may have gone live: that is the whole point of stamping first.
	if sp.seen.Prompt != "" {
		t.Errorf("no session may be created when the baseline failed: %+v", sp.seen)
	}
	if !strings.Contains(outs[0].Err.Error(), "refusing to spawn") {
		t.Errorf("the error should say the spawn was refused, got %v", outs[0].Err)
	}
}

// One-shot nudges reach the brief, and are retired once the spawn is usable.
func TestSpawnDeliversAndRetiresNudges(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, runDir, sp := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Worktree = repo
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := decide.AppendNudge(runDir, decide.Nudge{Text: "SENTINEL-run-the-suite", Lane: "lane-a"}); err != nil {
		t.Fatal(err)
	}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if !outs[0].OK() {
		t.Fatalf("spawn failed: %v", outs[0].Err)
	}
	if !strings.Contains(sp.seen.Prompt, "SENTINEL-run-the-suite") {
		t.Errorf("the one-shot nudge must reach the brief: %q", sp.seen.Prompt)
	}
	pending, err := decide.NudgesFor(runDir, "lane-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("a delivered nudge must be retired, got %v", pending)
	}
}

// A run-wide one-shot nudge is addressed to the run, so every lane the tick
// staffs must receive it. Consuming per-spawn retired it on the first lane, so a
// nudge whose whole meaning is "tell the run" reached exactly one of its
// addressees.
func TestRunWideNudgeReachesEveryLaneStaffedInTheTick(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, runDir, sp := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		st.Lanes[0].Worktree = repo
		st.Lanes = append(st.Lanes, &state.Lane{
			ID: "lane-b", Title: "B", Branch: "b-b", Worktree: repo,
			Status: state.StatusPlanned, Priority: state.P1, Sessions: map[string]string{},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := decide.AppendNudge(runDir, decide.Nudge{Text: "SENTINEL-run-wide"}); err != nil {
		t.Fatal(err)
	}

	var prompts []string
	sp.onSpawn = func() { prompts = append(prompts, sp.seen.Prompt) }

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
		{Lane: "lane-b", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	for i := range outs {
		if !outs[i].OK() {
			t.Fatalf("spawn %d failed: %v", i, outs[i].Err)
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("expected two spawns, got %d", len(prompts))
	}
	for i, p := range prompts {
		if !strings.Contains(p, "SENTINEL-run-wide") {
			t.Errorf("lane %d did not receive the run-wide nudge: %q", i, p)
		}
	}
	// Retired once, at the end of the tick.
	pending, err := decide.RunWideNudges(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("the run-wide nudge must be retired after the tick, got %v", pending)
	}
}

// A tick that spawned nothing has delivered nothing, and must not swallow an
// instruction the next tick would have carried.
func TestRunWideNudgeSurvivesATickThatStaffedNobody(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, _, runDir, _ := applier(t, repo)
	if err := decide.AppendNudge(runDir, decide.Nudge{Text: "SENTINEL-undelivered"}); err != nil {
		t.Fatal(err)
	}

	a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindHold, Reason: "nothing to do"},
	})

	pending, err := decide.RunWideNudges(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("an undelivered run-wide nudge must survive the tick, got %v", pending)
	}
}

// A lane whose worktree does not exist yet still gets its baseline BEFORE the
// session, resolved from the base it will be created at. Without it the stamp
// happened after the spawn and a fast agent could commit first, making that
// commit the recorded starting HEAD and the phase's real work measure as zero.
func TestSpawnStampsBaselineFromTheBaseBeforeGoingLive(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, sp := applier(t, repo)
	// origin/main is what `-f` resolves against for a created worktree.
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	baseSHA := git(t, repo, "rev-parse", "refs/remotes/origin/main")
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Worktree = "" // bramble will create it
		st.Config.Base = "main"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The baseline must already be recorded by the time the session is created.
	var stampedAtSpawn string
	sp.onSpawn = func() {
		if st, err := store.Read(); err == nil {
			lane, _ := st.Lane("lane-a")
			stampedAtSpawn = lane.PhaseStartSHA
		}
	}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if !outs[0].OK() {
		t.Fatalf("spawn failed: %v", outs[0].Err)
	}
	if stampedAtSpawn != baseSHA {
		t.Errorf("baseline at spawn time = %q, want the base SHA %q recorded BEFORE the session",
			stampedAtSpawn, baseSHA)
	}
}

// An unresolvable base refuses the spawn rather than leaving a live session with
// no baseline: a caller that cannot establish one should refuse while nothing is
// running, not discover it afterwards.
func TestSpawnRefusesWhenTheBaseCannotBeResolved(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	a, store, _, sp := applier(t, repo)
	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.Worktree = ""
		st.Config.Base = "no-such-base"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	outs := a.Apply(context.Background(), []decide.Decision{
		{Lane: "lane-a", Kind: decide.KindSpawn, Phase: "swe", Round: 1},
	})
	if outs[0].OK() {
		t.Fatal("an unresolvable base must refuse the spawn")
	}
	if sp.seen.Prompt != "" {
		t.Errorf("nothing may go live when the baseline cannot be resolved: %+v", sp.seen)
	}
}
