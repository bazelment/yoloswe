package main

import (
	"context"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
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
