//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/lifecycle"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// The run-dir artifacts a spawn writes must be parseable by the same scanner
// watch_lanes.sh keys on, or a real lane's signals would be invisible.
func TestSpawnArtifactsAreScannable(t *testing.T) {
	c := client(t)
	repo := repoRoot(t)
	r := newReaper(t, c, repo)

	laneID := "sq-art-" + uniqueSuffix()
	branch := "swarm-queen-test/" + laneID
	runDir, store := seedRun(t,
		[]state.Phase{{Name: "swe", Model: "sonnet"}, {Name: "local-review", Model: "sonnet"}},
		&state.Lane{ID: laneID, Title: "artifact probe", Branch: branch,
			Status: state.StatusPlanned, Priority: state.P0, Sessions: map[string]string{}})

	st0, _ := store.Read()
	lane0, _ := st0.Lane(laneID)
	res, err := lifecycle.Spawn(ctx(t), c, store, runDir, lifecycle.SpawnBrief{
		Lane: laneID, Phase: "swe", Round: 1, Model: "sonnet", Type: "builder",
		Text: lifecycle.RenderBrief(lifecycle.BriefContext{
			RunDir: runDir, Lane: lane0, Phase: "swe", Round: 1,
			Goal: "artifact probe", Target: "main",
			Mission: "You are a harness probe. Wait quietly; do nothing.",
		}),
	}, bramble.SpawnRequest{
		Repo: testRepo, Branch: branch, From: "main", CreateWorktree: true,
		Model: "sonnet", Goal: "artifact probe",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	r.session(res.SessionID)
	r.branch(branch)
	st, _ := store.Read()
	lane, _ := st.Lane(laneID)
	r.tree(lane.Worktree)

	// The brief must carry LITERAL paths; a child env points nowhere.
	b, err := os.ReadFile(filepath.Join(runDir, laneID+".swe.brief.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), runDir) {
		t.Errorf("brief lacks the literal run dir:\n%s", b)
	}

	// Signals the lane will emit must parse, including a rework round.
	for _, name := range []string{
		laneID + ".swe.done",
		laneID + ".local-review.needs-swe",
		laneID + ".swe2.done",
	} {
		if err := os.WriteFile(filepath.Join(runDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sigs, err := reconcile.ScanSignals(runDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 3 {
		t.Fatalf("scanned %d signals, want 3: %+v", len(sigs), sigs)
	}
	var sawRework, sawRound2 bool
	for _, s := range sigs {
		if s.Lane != laneID {
			t.Errorf("signal misattributed to %q", s.Lane)
		}
		if s.Kind == reconcile.SignalNeedsSWE {
			sawRework = true
		}
		if s.Phase == "swe" && s.Round == 2 {
			sawRound2 = true
		}
	}
	if !sawRework {
		t.Error("the needs-swe rework signal was not recognised")
	}
	if !sawRound2 {
		t.Error("the round-2 signal was not decoded")
	}
	// Briefs and spawn.json must NOT be mistaken for signals.
	for _, s := range sigs {
		if strings.Contains(s.Path, "brief") || strings.Contains(s.Path, "spawn.json") {
			t.Errorf("non-signal file parsed as a claim: %s", s.Path)
		}
	}
}
