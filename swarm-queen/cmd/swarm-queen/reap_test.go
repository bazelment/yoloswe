package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
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
