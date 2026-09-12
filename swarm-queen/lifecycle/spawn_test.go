package lifecycle

import (
	"context"
	"encoding/json"
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

type fakeSpawner struct {
	err error
	// onSpawn runs after the session is "created", so a test can observe or
	// disturb state while the session is already live.
	onSpawn func()
	res     bramble.SpawnResult
	seen    bramble.SpawnRequest
}

func (f *fakeSpawner) NewSession(_ context.Context, req bramble.SpawnRequest) (bramble.SpawnResult, error) {
	f.seen = req
	if f.onSpawn != nil {
		f.onSpawn()
	}
	return f.res, f.err
}

func seedRun(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Create(state.Config{
		Goal: "g", Base: "main", Target: "swarm/t",
		Phases: []state.Phase{{Name: "swe"}, {Name: "clean"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) error {
		st.Lanes = append(st.Lanes, &state.Lane{
			ID: "lane-a", Title: "A", Branch: "b-a",
			Status: state.StatusPlanned, Priority: state.P1,
			Sessions: map[string]string{},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dir, store
}

// Spawning and recording are ONE operation. In real runs the ledger's brief
// decayed 13/13 -> 0/12 and window_id 6/13 -> 1/12 because recording was a
// separate step that got skipped under load.
func TestSpawnRecordsLedgerAndSpawnJSONAtomically(t *testing.T) {
	t.Parallel()
	dir, store := seedRun(t)
	sp := &fakeSpawner{res: bramble.SpawnResult{
		SessionID: "lane-a-builder-abc123", WorktreePath: "/wt/lane-a",
	}}

	brief := SpawnBrief{
		Lane: "lane-a", Phase: "swe", Round: 1, Model: "opus", Type: "builder",
		Text: "Read your brief. Touch " + DonePath(dir, "lane-a", "swe", 1) + " LAST.",
	}
	res, err := Spawn(context.Background(), sp, store, dir, brief,
		bramble.SpawnRequest{Repo: "kernel", Worktree: "/wt/lane-a", Parent: "orch"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res.SessionID != "lane-a-builder-abc123" {
		t.Errorf("result = %+v", res)
	}

	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.Status != state.StatusRunning || lane.Phase != "swe" {
		t.Errorf("lane not marked running on swe: %+v", lane)
	}
	if got, _ := lane.SessionFor("swe", 1); got != "lane-a-builder-abc123" {
		t.Errorf("session not recorded: %v", lane.Sessions)
	}
	if lane.Brief == "" {
		t.Error("brief not recorded — this is the field that decayed to 0/12")
	}
	if lane.Worktree != "/wt/lane-a" {
		t.Errorf("worktree = %q", lane.Worktree)
	}

	// spawn.json, undocumented in SKILL.md but real in every live run.
	b, err := os.ReadFile(filepath.Join(dir, "lane-a.swe.spawn.json"))
	if err != nil {
		t.Fatalf("spawn.json: %v", err)
	}
	var rec SpawnRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.SessionID != res.SessionID {
		t.Errorf("spawn.json = %+v", rec)
	}

	// The brief file must exist; the agent is told to read it.
	if _, err := os.Stat(filepath.Join(dir, "lane-a.swe.brief.txt")); err != nil {
		t.Errorf("brief file missing: %v", err)
	}
}

// A rework round must not overwrite the previous attempt's session id.
func TestSpawnRoundTwoPreservesRoundOne(t *testing.T) {
	t.Parallel()
	dir, store := seedRun(t)
	sp := &fakeSpawner{res: bramble.SpawnResult{SessionID: "sess-r1"}}

	base := SpawnBrief{Lane: "lane-a", Phase: "swe", Round: 1,
		Text: "round one; touch " + DonePath(dir, "lane-a", "swe", 1)}
	if _, err := Spawn(context.Background(), sp, store, dir, base,
		bramble.SpawnRequest{Worktree: "/wt/a"}); err != nil {
		t.Fatal(err)
	}

	sp.res = bramble.SpawnResult{SessionID: "sess-r2"}
	base.Round = 2
	base.Text = "round two; touch " + DonePath(dir, "lane-a", "swe", 2)
	if _, err := Spawn(context.Background(), sp, store, dir, base,
		bramble.SpawnRequest{Worktree: "/wt/a"}); err != nil {
		t.Fatal(err)
	}

	st, _ := store.Read()
	lane, _ := st.Lane("lane-a")
	if got, _ := lane.SessionFor("swe", 1); got != "sess-r1" {
		t.Errorf("round 1 session was destroyed: %v", lane.Sessions)
	}
	if got, _ := lane.SessionFor("swe", 2); got != "sess-r2" {
		t.Errorf("round 2 session missing: %v", lane.Sessions)
	}
	// Separate artifacts per round, so a rework does not clobber the record.
	for _, f := range []string{"lane-a.swe.spawn.json", "lane-a.swe2.spawn.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
}

// A failed spawn must record nothing: a lane marked running with no session is
// indistinguishable from one that died, and would be nudged rather than staffed.
func TestFailedSpawnRecordsNothing(t *testing.T) {
	t.Parallel()
	dir, store := seedRun(t)
	sp := &fakeSpawner{err: errors.New("bramble unreachable")}

	_, err := Spawn(context.Background(), sp, store, dir,
		SpawnBrief{Lane: "lane-a", Phase: "swe", Round: 1,
			Text: "brief; touch " + DonePath(dir, "lane-a", "swe", 1)},
		bramble.SpawnRequest{Worktree: "/wt/a"})
	if err == nil {
		t.Fatal("expected a spawn error")
	}

	st, _ := store.Read()
	lane, _ := st.Lane("lane-a")
	if lane.Status != state.StatusPlanned {
		t.Errorf("a failed spawn must leave the lane planned, got %s", lane.Status)
	}
	if len(lane.Sessions) != 0 {
		t.Errorf("no session should be recorded: %v", lane.Sessions)
	}
	if _, err := os.Stat(filepath.Join(dir, "lane-a.swe.spawn.json")); err == nil {
		t.Error("spawn.json written for a failed spawn")
	}
}

// If the session exists but recording fails, the error must NAME the live
// session. An unrecorded live session is the worst outcome: nothing can find it.
func TestRecordFailureNamesTheLiveSession(t *testing.T) {
	t.Parallel()
	dir, store := seedRun(t)
	sp := &fakeSpawner{res: bramble.SpawnResult{SessionID: "orphan-1", WorktreePath: "/wt/x"}}

	err := store.Update(func(st *state.State) error {
		st.Lanes = nil // the lane disappears mid-spawn
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Spawn(context.Background(), sp, store, dir,
		SpawnBrief{Lane: "lane-a", Phase: "swe", Round: 1,
			Text: "brief; touch " + DonePath(dir, "lane-a", "swe", 1)},
		bramble.SpawnRequest{Worktree: "/wt/x"})
	if err == nil {
		t.Fatal("expected a recording error")
	}
	if !strings.Contains(err.Error(), "orphan-1") || !strings.Contains(err.Error(), "IS LIVE") {
		t.Errorf("error must name the live session so it can be found: %v", err)
	}
}

// Briefs must carry LITERAL report paths: a child environment does not point
// back at the run directory.
func TestArtifactPathsAreLiteralAndRoundAware(t *testing.T) {
	t.Parallel()
	if got := DonePath("/run", "lane-a", "swe", 1); got != "/run/lane-a.swe.done" {
		t.Errorf("DonePath = %q", got)
	}
	if got := DonePath("/run", "lane-a", "swe", 3); got != "/run/lane-a.swe3.done" {
		t.Errorf("round-aware DonePath = %q", got)
	}
	if got := NeedsSWEPath("/run", "a", "local-review", 2); got != "/run/a.local-review2.needs-swe" {
		t.Errorf("NeedsSWEPath = %q", got)
	}
	if got := ReportPath("/run", "a", "github-review", 1); got != "/run/a.github-review.md" {
		t.Errorf("ReportPath = %q", got)
	}
}

// A brief without its literal report path produces a lane that can never report:
// a child environment does not point back at the run directory, so a relative
// path or an env var reaches nothing. Refuse rather than spawn a silent lane.
func TestSpawnRefusesABriefWithNoReportPath(t *testing.T) {
	t.Parallel()
	dir, store := seedRun(t)
	sp := &fakeSpawner{res: bramble.SpawnResult{SessionID: "sess-1"}}

	_, err := Spawn(context.Background(), sp, store, dir,
		SpawnBrief{Lane: "lane-a", Phase: "swe", Round: 1,
			Text: "Do the work and touch $RUN/lane-a.swe.done when finished."},
		bramble.SpawnRequest{Worktree: "/wt/a"})
	if err == nil {
		t.Fatal("a brief using $RUN indirection must be refused")
	}
	if !strings.Contains(err.Error(), "omits its literal done path") {
		t.Errorf("err = %v", err)
	}
	// Nothing may be recorded for a refused spawn.
	st, _ := store.Read()
	lane, _ := st.Lane("lane-a")
	if lane.Status != state.StatusPlanned {
		t.Errorf("lane changed state despite the refusal: %s", lane.Status)
	}
}

// PrepareSpawn is the one entry point both commands use, so a caller cannot
// omit a step. dispatch hand-rolled this sequence and fell behind three rounds
// running -- on nudge delivery, then the pre-stamp, then the base-resolved
// pre-stamp, one missing step each time.
func TestPrepareSpawnStampsFromTheBaseWhenThereIsNoWorktree(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	runDir, store := seedRun(t)
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	baseSHA := git(t, repo, "rev-parse", "refs/remotes/origin/main")

	if err := decide.AppendNudge(runDir, decide.Nudge{Text: "SENTINEL-lane", Lane: "lane-a"}); err != nil {
		t.Fatal(err)
	}

	instructions, nudges, preStamped, err := PrepareSpawn(context.Background(),
		reconcile.ExecGit{}, store, runDir, repo, "lane-a", "main",
		[]string{"a standing rule"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !preStamped {
		t.Error("a lane with no worktree must still be stamped from its base, before the session")
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.PhaseStartSHA != baseSHA {
		t.Errorf("PhaseStartSHA = %q, want the base SHA %q", lane.PhaseStartSHA, baseSHA)
	}
	// And the instructions carry both the standing rule and the lane's nudge.
	joined := strings.Join(instructions, "\n")
	if !strings.Contains(joined, "a standing rule") || !strings.Contains(joined, "SENTINEL-lane") {
		t.Errorf("instructions must carry the standing rules AND the nudge: %v", instructions)
	}
	if len(nudges) != 1 {
		t.Errorf("the lane's own nudge must be returned for retirement: %v", nudges)
	}
}

// An unresolvable base refuses while nothing is live.
func TestPrepareSpawnRefusesAnUnresolvableBase(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	runDir, store := seedRun(t)

	_, _, _, err := PrepareSpawn(context.Background(), reconcile.ExecGit{}, store,
		runDir, repo, "lane-a", "no-such-base", nil, nil)
	if err == nil {
		t.Fatal("an unresolvable base must refuse before anything is spawned")
	}
	if !strings.Contains(err.Error(), "refusing to spawn") {
		t.Errorf("the error should say the spawn was refused, got %v", err)
	}
}

// BriefInstructions renders standing rules, run-wide nudges, and the lane's own
// nudges into one instruction set, but returns ONLY the lane's own nudges for
// retirement. A lane must not be able to retire an instruction addressed to its
// siblings or to the whole run.
func TestBriefInstructionsReturnsOnlyTheLanesOwnNudgesForRetirement(t *testing.T) {
	t.Parallel()
	runDir, _ := seedRun(t)

	if err := decide.AppendNudge(runDir, decide.Nudge{Text: "OWN-NUDGE", Lane: "lane-a"}); err != nil {
		t.Fatal(err)
	}
	// A nudge addressed to a different lane must not leak into lane-a's brief
	// or its returned (and therefore retirable) set.
	if err := decide.AppendNudge(runDir, decide.Nudge{Text: "SIBLING-NUDGE", Lane: "lane-b"}); err != nil {
		t.Fatal(err)
	}

	extra := []decide.Nudge{{Text: "RUN-WIDE-NUDGE"}}
	rendered, own, err := BriefInstructions(runDir, "lane-a", []string{"STANDING-RULE"}, extra)
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(rendered, "\n")
	for _, want := range []string{"STANDING-RULE", "RUN-WIDE-NUDGE", "OWN-NUDGE"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rendered instructions must contain %q; got %v", want, rendered)
		}
	}
	if strings.Contains(joined, "SIBLING-NUDGE") {
		t.Errorf("a sibling's lane-scoped nudge must not appear in lane-a's brief: %v", rendered)
	}

	// The asymmetry under test: only the lane's OWN nudges come back for
	// consumption. A run-wide nudge retires on the tick's own schedule, not
	// this lane's -- if it were returned here, this lane's spawn would retire
	// an instruction still owed to every other lane the tick staffs.
	if len(own) != 1 || own[0].Text != "OWN-NUDGE" {
		t.Errorf("own nudges = %v, want exactly [OWN-NUDGE]", own)
	}
}

// SpawnBaseline returns (false, nil) for an empty worktree: "not pre-stamped",
// not "failed". A lane whose worktree bramble has not created yet has no HEAD to
// read, and a caller checking only the error would misread this as success.
func TestSpawnBaselineReportsNotPreStampedForEmptyWorktree(t *testing.T) {
	t.Parallel()
	_, store := seedRun(t)

	pre, err := SpawnBaseline(context.Background(), reconcile.ExecGit{}, store, "lane-a", "")
	if err != nil {
		t.Fatalf("an empty worktree must not error: %v", err)
	}
	if pre {
		t.Error("an empty worktree has no HEAD to stamp; pre must be false, not true")
	}

	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.PhaseStartSHA != "" {
		t.Errorf("nothing should be stamped for an empty worktree, got PhaseStartSHA=%q", lane.PhaseStartSHA)
	}
}

// SpawnBaseline stamps the baseline and reports pre=true when a worktree
// already exists — every case but a lane's first spawn.
func TestSpawnBaselineStampsAndReportsPreStampedForExistingWorktree(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	_, store := seedRun(t)
	head := git(t, repo, "rev-parse", "HEAD")

	pre, err := SpawnBaseline(context.Background(), reconcile.ExecGit{}, store, "lane-a", repo)
	if err != nil {
		t.Fatal(err)
	}
	if !pre {
		t.Error("an existing worktree must be reported as pre-stamped")
	}

	st, _ := store.Read()
	lane, _ := st.Lane("lane-a")
	if lane.PhaseStartSHA != head {
		t.Errorf("PhaseStartSHA = %q, want %q", lane.PhaseStartSHA, head)
	}
}

// argFailGit fails a git invocation only when its full argument list matches
// one in fail, delegating everything else to the real binary. Unlike
// failingGit (apply_test.go), which fails by subcommand name alone, this lets a
// test distinguish `rev-parse origin/<base>` from `rev-parse <base>`.
type argFailGit struct {
	inner reconcile.GitRunner
	fail  map[string]bool
}

func (f argFailGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	if f.fail[strings.Join(args, " ")] {
		return "", fmt.Errorf("simulated failure: git %s", strings.Join(args, " "))
	}
	return f.inner.Run(ctx, dir, args...)
}

// SpawnBaselineFromBase refuses immediately on an empty base: there is nothing
// to resolve, and a lane without a recorded base must not silently proceed.
func TestSpawnBaselineFromBaseRefusesEmptyBase(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	_, store := seedRun(t)

	err := SpawnBaselineFromBase(context.Background(), reconcile.ExecGit{}, store, repo, "lane-a", "")
	if err == nil {
		t.Fatal("an empty base must refuse")
	}
	if !strings.Contains(err.Error(), "no base recorded") {
		t.Errorf("err = %v", err)
	}
}

// When origin/<base> cannot be resolved, SpawnBaselineFromBase falls back to a
// LOCAL rev-parse of the base before erroring. A run whose base is not pushed
// is misconfigured for `-f`, but resolving it locally still beats stamping
// nothing.
func TestSpawnBaselineFromBaseFallsBackToLocalRefWhenOriginIsUnresolvable(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	_, store := seedRun(t)
	head := git(t, repo, "rev-parse", "HEAD")
	// Deliberately no refs/remotes/origin/main: origin/main cannot resolve, so
	// the fallback must hit the local `main` ref instead.

	g := argFailGit{inner: reconcile.ExecGit{}, fail: map[string]bool{
		"rev-parse origin/main": true,
	}}

	err := SpawnBaselineFromBase(context.Background(), g, store, repo, "lane-a", "main")
	if err != nil {
		t.Fatalf("the local-ref fallback should have succeeded: %v", err)
	}

	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.PhaseStartSHA != head {
		t.Errorf("PhaseStartSHA = %q, want the locally-resolved %q", lane.PhaseStartSHA, head)
	}
	if lane.ForkSHA != head {
		t.Errorf("ForkSHA = %q, want %q", lane.ForkSHA, head)
	}
}

// When BOTH origin and the local ref fail to resolve, SpawnBaselineFromBase
// errors rather than stamping nothing silently.
func TestSpawnBaselineFromBaseErrorsWhenBothOriginAndLocalFail(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	_, store := seedRun(t)

	err := SpawnBaselineFromBase(context.Background(), reconcile.ExecGit{}, store, repo, "lane-a", "no-such-base")
	if err == nil {
		t.Fatal("an unresolvable base (origin AND local) must error")
	}
	if !strings.Contains(err.Error(), "resolve base") {
		t.Errorf("err = %v", err)
	}
}

// SpawnBaselineFromBase must not clobber an existing ForkSHA: ForkSHA is the
// lane's ORIGINAL fork point, stamped once, while PhaseStartSHA moves at every
// phase. A rework round that re-resolves the base must not overwrite the fork
// point recorded at the lane's true beginning.
func TestSpawnBaselineFromBaseDoesNotClobberExistingForkSHA(t *testing.T) {
	t.Parallel()
	repo := newRepo(t)
	_, store := seedRun(t)
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	originalFork := git(t, repo, "rev-parse", "HEAD")

	if err := store.Update(func(st *state.State) error {
		lane, _ := st.Lane("lane-a")
		lane.ForkSHA = originalFork
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Advance HEAD (and origin/main with it) so a naive re-stamp would produce
	// a different SHA than the recorded fork point.
	write(t, repo, "more.txt", "more work")
	git(t, repo, "add", "more.txt")
	git(t, repo, "commit", "-q", "-m", "advance")
	git(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	newHead := git(t, repo, "rev-parse", "HEAD")
	if newHead == originalFork {
		t.Fatal("test setup: HEAD did not advance")
	}

	if err := SpawnBaselineFromBase(context.Background(), reconcile.ExecGit{}, store, repo, "lane-a", "main"); err != nil {
		t.Fatal(err)
	}

	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	lane, _ := st.Lane("lane-a")
	if lane.PhaseStartSHA != newHead {
		t.Errorf("PhaseStartSHA = %q, want the freshly resolved %q", lane.PhaseStartSHA, newHead)
	}
	if lane.ForkSHA != originalFork {
		t.Errorf("ForkSHA was clobbered: got %q, want the original %q", lane.ForkSHA, originalFork)
	}
}
