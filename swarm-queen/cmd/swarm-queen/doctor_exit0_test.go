package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// doctor's THIRD exit state: clean AND fully measured, the only one that exits
// 0. It was unreachable from a hermetic test before doctorSessions became
// injectable, because liveSessions calls bramble.New() directly and this
// process has no live TUI -- so the session probe always failed and the one
// branch that asserts "this run is genuinely healthy" was the one branch
// nothing could pin. A regression here reports every healthy run as unmeasured.
func TestRunDoctorExitsZeroWhenCleanAndFullyMeasured(t *testing.T) {
	isolateBramble(t)
	repo := orDashRepo(t)
	runDir := doctorSeedCleanRun(t)
	doctorRepoDir = repo

	// A measurement, not a live TUI: an empty fleet that was actually probed.
	prev := doctorSessions
	t.Cleanup(func() { doctorSessions = prev })
	doctorSessions = func(context.Context, *cobra.Command) ([]bramble.Session, error) {
		return []bramble.Session{}, nil
	}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runDoctor(cmd, []string{runDir}); err != nil {
		t.Fatalf("a clean, fully measured run must exit 0, got %v", err)
	}
}

// The same seam must not let an unmeasured fleet pass: a probe that FAILS is
// still a refusal even though the injected function returns an empty slice in
// both cases. Empty-and-measured versus empty-and-unknown is exactly the
// distinction the exit code exists to carry.
func TestRunDoctorStillRefusesWhenTheInjectedProbeFails(t *testing.T) {
	isolateBramble(t)
	repo := orDashRepo(t)
	runDir := doctorSeedRun(t, repo)
	doctorRepoDir = repo

	prev := doctorSessions
	t.Cleanup(func() { doctorSessions = prev })
	doctorSessions = func(context.Context, *cobra.Command) ([]bramble.Session, error) {
		return nil, context.DeadlineExceeded
	}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runDoctor(cmd, []string{runDir})
	if err == nil {
		t.Fatal("an unmeasured fleet must not exit 0")
	}
}

// doctorSeedCleanRun seeds a run with NO drift: a terminal lane that has
// already released its worktree. The shared doctorSeedRun deliberately leaves a
// done lane holding one, which is itself a finding -- correct for the refusal
// tests, but it means a run seeded that way can never legitimately exit 0.
func doctorSeedCleanRun(t *testing.T) string {
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
			ID: "lane-clean", Title: "A", Branch: "", Worktree: "",
			Status: state.StatusDone, Priority: state.P1,
			Sessions: map[string]string{},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// doctor must exit 0 on a run whose lanes were closed by the REAL reaper.
//
// The other exit-0 test hand-builds a closed lane as the old reaper left it
// (Worktree: "", Branch: ""), so it could not see this: now that applyReap
// RETAINS both fields for the audit, the drift rule that flags a terminal lane
// with a recorded-but-absent worktree matched every properly closed lane, and
// doctor exits non-zero on any finding. A run that had reaped anything could
// never come back clean. Seeding from a real reap is the only way this stays
// honest -- a hand-built fixture just encodes whatever shape I believe today.
func TestRunDoctorExitsZeroAfterARealReap(t *testing.T) {
	isolateBramble(t)
	repo := orDashRepo(t)

	// A lane with a real worktree and a real branch, integrated into the target.
	g := reconcile.ExecGit{}
	ctx := context.Background()
	if _, err := g.Run(ctx, repo, "branch", "swarm/t"); err != nil {
		t.Fatal(err)
	}
	wtDir := filepath.Join(t.TempDir(), "lane-a-wt")
	if _, err := g.Run(ctx, repo, "worktree", "add", "-q", "-b", "b-a", wtDir); err != nil {
		t.Fatal(err)
	}

	runDir := t.TempDir()
	store := state.NewStore(runDir)
	if err := store.Create(state.Config{
		Goal: "g", Base: "main", Target: "swarm/t",
		Phases: []state.Phase{{Name: "swe"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) error {
		st.Lanes = append(st.Lanes, &state.Lane{
			ID: "lane-a", Title: "A", Branch: "b-a", Worktree: wtDir,
			Status: state.StatusDone, Priority: state.P1,
			Sessions: map[string]string{},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Close it through the real teardown path.
	applier := &lifecycle.Applier{
		Git: g, Store: store, RunDir: runDir, RepoDir: repo,
		SelfWindow:   "@1",
		LiveSessions: func(*state.Lane) lifecycle.SessionProbe { return lifecycle.KnownSessions(nil) },
	}
	outs := applier.Apply(ctx, []decide.Decision{{Lane: "lane-a", Kind: decide.KindReap}})
	if !outs[0].OK() {
		t.Fatalf("the reap must succeed for this fixture to mean anything: %v", outs[0].Err)
	}

	doctorRepoDir = repo
	prev := doctorSessions
	t.Cleanup(func() { doctorSessions = prev })
	doctorSessions = func(context.Context, *cobra.Command) ([]bramble.Session, error) {
		return []bramble.Session{}, nil
	}

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	if err := runDoctor(cmd, []string{runDir}); err != nil {
		t.Fatalf("doctor must exit 0 on a run whose lanes the reaper closed: %v", err)
	}
}
