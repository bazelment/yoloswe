package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

func reapSeedRun(t *testing.T, worktree string) string {
	t.Helper()
	dir := t.TempDir()
	store := state.NewStore(dir)
	if err := store.Create(state.Config{
		Goal: "g", Base: "main", Target: "swarm/t",
		Phases: []state.Phase{{Name: "swe"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) error {
		st.Lanes = append(st.Lanes, &state.Lane{
			ID: "lane-a", Title: "A", Branch: "b-a", Worktree: worktree,
			Status: state.StatusRunning, Priority: state.P1,
			Sessions: map[string]string{},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A non-terminal lane is refused, not reaped, regardless of anything else --
// reap must never remove a lane that is still doing work.
func TestRunReapRefusesANonTerminalLane(t *testing.T) {
	isolateBramble(t)
	repo := orDashRepo(t)
	runDir := reapSeedRun(t, repo)
	reapRepoDir = repo
	reapApply = false
	t.Cleanup(func() { reapApply = false })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	// Dry run with a running lane and no bramble: nothing is safe to reap, so
	// the command must not error purely because sessions were unmeasured --
	// unmeasured only matters for lanes that would otherwise be reapable.
	err := runReap(cmd, []string{runDir})
	if err == nil {
		t.Fatal("session checks could not run; reap must not report a clean sweep")
	}
	if !strings.Contains(err.Error(), "session probe could not run") {
		t.Errorf("err = %v", err)
	}
}

// newReapApplier refuses when bramble is unreachable, before touching
// anything -- --apply must not proceed with no way to spawn the transactional
// applier's teardown steps.
func TestNewReapApplierRefusesWithoutAReachableBramble(t *testing.T) {
	isolateBramble(t)
	runDir := t.TempDir()
	store := state.NewStore(runDir)
	if err := store.Create(state.Config{Goal: "g", Base: "main", Target: "t"}); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	_, err := newReapApplier(context.Background(), cmd, runDir)
	if err == nil {
		t.Fatal("newReapApplier must refuse without a reachable bramble TUI")
	}
	if !strings.Contains(err.Error(), "needs a reachable bramble TUI") {
		t.Errorf("err = %v", err)
	}
}

// The session fleet must be re-probed FOR EACH LANE at apply time, not captured
// once and reused.
//
// reap built its applier's LiveSessions from a single snapshot taken when the
// applier was constructed, so a session that started after that snapshot -- or
// after an earlier lane's kill in the same run -- was invisible, and its
// worktree could be removed with the agent still live. tick already re-probed
// per lane; reap shared lifecycle.Applier but not the wiring.
func TestFreshLaneProbeAsksBrambleForEveryLane(t *testing.T) {
	calls := 0
	prev := reapSessions
	t.Cleanup(func() { reapSessions = prev })
	reapSessions = func(context.Context, *cobra.Command) ([]bramble.Session, error) {
		calls++
		return []bramble.Session{}, nil
	}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	probe := freshLaneProbe(context.Background(), cmd)

	for _, wt := range []string{"/wt/lane-a", "/wt/lane-b", "/wt/lane-c"} {
		if p := probe(&state.Lane{ID: wt, Worktree: wt}); !p.Known {
			t.Errorf("%s: a successful probe must be Known", wt)
		}
	}
	if calls != 3 {
		t.Errorf("bramble was asked %d time(s) for 3 lanes; a reused snapshot "+
			"cannot see a session that started mid-run", calls)
	}
}

// A probe that FAILS is unknown, never an empty fleet: absence of a measurement
// must refuse the reap rather than permit it.
func TestFreshLaneProbeReportsUnknownWhenTheQueryFails(t *testing.T) {
	prev := reapSessions
	t.Cleanup(func() { reapSessions = prev })
	reapSessions = func(context.Context, *cobra.Command) ([]bramble.Session, error) {
		return nil, errors.New("bramble unreachable")
	}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if p := freshLaneProbe(context.Background(), cmd)(&state.Lane{Worktree: "/wt/lane-a"}); p.Known {
		t.Error("a failed probe must be UNKNOWN, not an empty fleet")
	}
}

// A lane that kept its snapshot on purpose is still CLOSED.
//
// applyReap deliberately retains the backup of a lane reaped while dirty: that
// work exists nowhere else. laneFullyClosed treated any backup ref as "not
// closed", so the lane went back through PlanReap every run -- and its branch
// was already deleted, so BranchMerged failed and it printed "cannot verify
// integration of <branch>". The lanes whose work was most worth protecting were
// exactly the ones reported as a branch-integration failure.
func TestLaneFullyClosedAcceptsALaneThatRetainedItsBackup(t *testing.T) {
	repo := orDashRepo(t)
	prev := reapRepoDir
	reapRepoDir = repo
	t.Cleanup(func() { reapRepoDir = prev })

	// A real backup ref, written the way applyReap writes one.
	wtDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wtDir, "wip.txt"), []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("git", "init", "-q", "-b", "main", ".")
	run.Dir = wtDir
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"config", "user.email", "t@e.com"}, {"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		c := exec.Command("git", args...)
		c.Dir = wtDir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	g := reconcile.ExecGit{}
	if _, err := lifecycle.SnapshotAtRisk(context.Background(), g, "lane-a", wtDir); err != nil {
		t.Fatal(err)
	}
	// Move the ref into the repo doctor/reap looks at.
	if _, err := g.Run(context.Background(), repo,
		"update-ref", lifecycle.BackupRef("lane-a"), "HEAD"); err != nil {
		t.Fatal(err)
	}

	lane := &state.Lane{
		ID: "lane-a", Status: state.StatusDone,
		Worktree: "/wt/gone", Branch: "",
	}
	wt := reconcile.WorktreeState{Path: "/wt/gone", Exists: false, Measured: true}

	// A probe that RAN and found nothing: closure now requires measured session
	// evidence, not merely an absent worktree directory.
	c := laneFullyClosed(context.Background(), g, lane, wt, lifecycle.KnownSessions(nil))
	if !c.Closed {
		t.Fatalf("a terminal lane with no worktree, no branch and a measured empty "+
			"fleet is closed even when its snapshot was deliberately retained: %s", c.Why)
	}
	if !c.BackupRetained {
		t.Error("the retained snapshot must be reported, not silently dropped")
	}
	if !strings.Contains(c.Why, "backup retained") {
		t.Errorf("the reason must say the backup was kept on purpose, got %q", c.Why)
	}

	// codex r10 (0.97): a bramble session can outlive its worktree directory, so
	// an absent directory is not evidence that no agent is still attached. The
	// skip used to run BEFORE the probe and judged closure without it, so a lane
	// with an orphaned agent read as CLOSED and skipped the very probe that would
	// have found it.
	live := lifecycle.KnownSessions([]lifecycle.LiveSession{
		{ID: "sess-orphan", Status: "running", TmuxTarget: "@9"},
	})
	if c := laneFullyClosed(context.Background(), g, lane, wt, live); c.Closed {
		t.Errorf("a lane whose session is still live must not read as closed: %s", c.Why)
	}
	if c := laneFullyClosed(context.Background(), g, lane, wt, lifecycle.UnknownSessions()); c.Closed {
		t.Errorf("an unmeasured fleet is not evidence of closure: %s", c.Why)
	}
}
