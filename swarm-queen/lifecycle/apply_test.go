package lifecycle

import (
	"context"
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
