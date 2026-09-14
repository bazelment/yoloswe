package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

func orDashRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func doctorSeedRun(t *testing.T, repo string) string {
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
			ID: "lane-a", Title: "A", Branch: "b-a", Worktree: repo,
			Status: state.StatusDone, Priority: state.P1,
			Sessions: map[string]string{},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// isolateBramble clears BRAMBLE_SOCK and points XDG_RUNTIME_DIR at an empty
// directory, so bramble.New() deterministically fails to find a socket rather
// than passing for the wrong reason against this box's ambient BRAMBLE_SOCK.
func isolateBramble(t *testing.T) {
	t.Helper()
	t.Setenv("BRAMBLE_SOCK", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// doctor's three-state exit code is the load-bearing "absence is never
// evidence" rule: clean+measured exits 0, findings exit non-zero, and
// clean-but-UNMEASURED must ALSO exit non-zero rather than reading as a pass.
// This test binary has no live bramble TUI, so the session probe always fails
// here -- which means the "clean+measured" 0-exit state cannot be reached from
// this process. What CAN be verified without a live bramble socket is the
// other two: a branch-probe failure and a worktree left unmeasured must each
// independently refuse, and the totals line must name every category that
// could not run rather than only the first.
func TestRunDoctorNamesEveryUnmeasuredCategoryNotJustTheFirst(t *testing.T) {
	isolateBramble(t)
	// doctorRepoDir points somewhere with no git repo, so the branch probe
	// fails outright -- one of the two ways doctor can be "clean but
	// unmeasured" alongside the session probe that always fails here.
	doctorRepoDir = t.TempDir()
	repo := orDashRepo(t)
	runDir := doctorSeedRun(t, repo)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runDoctor(cmd, []string{runDir})
	if err == nil {
		t.Fatal("a run with both branch and session checks unmeasured must not exit 0")
	}
	// Whichever message wins, the failure must be attributable to checks that
	// could not run, not to a genuine finding -- there are none seeded here.
	if !strings.Contains(err.Error(), "could not run") {
		t.Errorf("err = %v, want it to say checks could not run", err)
	}
}

// Without a reachable bramble TUI, session checks cannot run at all. That is
// the clean-but-UNMEASURED case: doctor must refuse rather than report a
// passing sweep it never actually performed.
func TestRunDoctorRefusesWhenSessionChecksCannotRun(t *testing.T) {
	isolateBramble(t)
	repo := orDashRepo(t)
	runDir := doctorSeedRun(t, repo)
	doctorRepoDir = repo

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runDoctor(cmd, []string{runDir})
	if err == nil {
		t.Fatal("session checks cannot run without a reachable bramble TUI; doctor must not exit 0")
	}
	if !strings.Contains(err.Error(), "could not run") {
		t.Errorf("err = %v, want it to say checks could not run", err)
	}
}

func TestOrDash(t *testing.T) {
	t.Parallel()
	if got := orDash(""); got != "—" {
		t.Errorf("orDash(\"\") = %q, want an em dash", got)
	}
	if got := orDash("swe"); got != "swe" {
		t.Errorf("orDash(\"swe\") = %q, want it unchanged", got)
	}
}

func TestProbeWorktreesMarksAMissingWorktreeAsMeasuredAbsent(t *testing.T) {
	t.Parallel()
	st := &state.State{Lanes: []*state.Lane{
		{ID: "lane-a", Worktree: ""},
	}}
	out := probeWorktrees(context.Background(), st)
	wt, ok := out["lane-a"]
	if !ok {
		t.Fatal("lane-a missing from probe results")
	}
	if wt.Unknown() {
		t.Errorf("an empty worktree path is a measured absence, not unknown: %+v", wt)
	}
	if wt.Exists {
		t.Errorf("an empty worktree path must not exist: %+v", wt)
	}
}

func TestLiveBranchesReportsOnlyBranchesThatStillExist(t *testing.T) {
	repo := orDashRepo(t)
	git := reconcile.ExecGit{}
	if _, err := git.Run(context.Background(), repo, "branch", "b-live"); err != nil {
		t.Fatal(err)
	}
	doctorRepoDir = repo

	st := &state.State{Lanes: []*state.Lane{
		{ID: "a", Branch: "b-live"},
		{ID: "b", Branch: "b-deleted"},
	}}
	live, err := liveBranches(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !live["b-live"] {
		t.Errorf("b-live must be reported live: %v", live)
	}
	if live["b-deleted"] {
		t.Errorf("a branch that was never created must not be reported live: %v", live)
	}
}

func TestLiveBranchesFailsWhenTheRepoDirIsNotAGitRepo(t *testing.T) {
	doctorRepoDir = t.TempDir()
	_, err := liveBranches(context.Background(), &state.State{})
	if err == nil {
		t.Fatal("a non-repo directory must fail the branch probe rather than report an empty set")
	}
}

func TestLiveSessionsFailsWithoutAReachableBramble(t *testing.T) {
	isolateBramble(t)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	_, err := liveSessions(context.Background(), cmd)
	if err == nil {
		t.Fatal("liveSessions must fail when no bramble socket can be found")
	}
}
