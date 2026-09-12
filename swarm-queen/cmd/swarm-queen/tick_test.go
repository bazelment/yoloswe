package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/bramble"
	"github.com/bazelment/yoloswe/swarm-queen/decide"
	"github.com/bazelment/yoloswe/swarm-queen/reconcile"
	"github.com/bazelment/yoloswe/swarm-queen/state"
)

// The exemption is derived from the run, not from a hardcoded list. decide
// implemented SlotExempt and decide_test documented it, but tick never passed
// one -- so report/gaps/review lanes occupied capacity and could block staffing
// of dependency-ready lanes whenever --max-concurrent was set.
func TestSlotExemptCoversEveryNonMutatingPhase(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{Phases: []state.Phase{
			{Name: "swe"}, {Name: "local-review"}, {Name: "report"}, {Name: "gaps"},
		}},
	}
	got := slotExempt(st)

	for _, phase := range []string{"local-review", "report", "gaps"} {
		if !got[phase] {
			t.Errorf("%q is read-only and must not consume a slot: %v", phase, got)
		}
	}
	if got["swe"] {
		t.Errorf("a mutating phase must still consume a slot: %v", got)
	}
}

// A lane can carry a phase the config no longer lists, after a contract edited
// mid-run. It still must not hold a slot it does not need.
func TestSlotExemptCoversPhasesOnlyLanesCarry(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{Phases: []state.Phase{{Name: "swe"}}},
		Lanes: []*state.Lane{
			{ID: "a", Phase: "github-review-r3", Status: state.StatusRunning},
			{ID: "b", Phase: "swe", Status: state.StatusRunning},
		},
	}
	got := slotExempt(st)

	if !got["github-review-r3"] {
		t.Errorf("a review phase held only by a lane must still be exempt: %v", got)
	}
	if got["swe"] {
		t.Errorf("a mutating phase must still consume a slot: %v", got)
	}
}

// The wiring itself, not just the helper. decide implemented SlotExempt and
// decide_test documented it, while tick passed nothing -- so the exemption was
// correct, tested, and dead. This asserts the Inputs tick builds actually carry
// it, which is the half that was missing.
func TestTickPassesSlotExemptToThePlanner(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{
			Goal:   "g",
			Phases: []state.Phase{{Name: "swe"}, {Name: "report"}},
		},
		Lanes: []*state.Lane{
			{ID: "reporting", Phase: "report", Status: state.StatusRunning},
			{ID: "ready", Status: state.StatusPlanned, Priority: state.P0},
		},
	}

	in := planInputs(st, nil, nil, nil, 1)
	if !in.SlotExempt["report"] {
		t.Fatalf("tick must exempt read-only phases from the slot count: %v", in.SlotExempt)
	}
	// And the exemption must have the effect it exists for: the running report
	// lane must not consume the single slot the ready lane needs.
	ds := decide.Plan(in)
	var staffed bool
	for _, d := range ds {
		if d.Lane == "ready" && d.Kind == decide.KindSpawn {
			staffed = true
		}
	}
	if !staffed {
		t.Errorf("a report lane must not block staffing at --max-concurrent=1: %v", ds)
	}
}

// A lane with no recorded worktree cannot be correlated to a bramble session at
// all: bramble reports worktree_name and never a path, so the ledger's Worktree
// is the only key. Reporting that as a MEASURED empty set asserts that nothing
// holds the lane, when in fact nobody looked -- and PlanReap would then clear
// the ledger fallback and proceed as if ownership had been disproved.
func TestLaneProbeTreatsAnUncorrelatableLaneAsUnknown(t *testing.T) {
	t.Parallel()
	sessions := []bramble.Session{
		{ID: "s1", WorktreeName: "lane-a", Status: "idle", TmuxTarget: "@1"},
	}

	if p := laneProbe(sessions, true, ""); p.Known {
		t.Errorf("a lane with no worktree cannot be correlated; it must be unknown: %+v", p)
	}
	for _, worktree := range []string{".", "/"} {
		if p := laneProbe(sessions, true, worktree); p.Known {
			t.Errorf("worktree %q yields no usable key; it must be unknown: %+v", worktree, p)
		}
	}

	// A correlatable lane that genuinely has no session IS measured-empty.
	p := laneProbe(sessions, true, "/wt/lane-b")
	if !p.Known {
		t.Errorf("a correlatable lane must be measured: %+v", p)
	}
	if len(p.Sessions) != 0 {
		t.Errorf("lane-b holds no session: %+v", p)
	}

	// And a correlatable lane that does have one reports it.
	held := laneProbe(sessions, true, "/wt/lane-a")
	if !held.Known || len(held.Sessions) != 1 || held.Sessions[0].ID != "s1" {
		t.Errorf("lane-a's live session must be reported: %+v", held)
	}
}

// A failed fleet query is unknown for every lane, correlatable or not.
func TestLaneProbeIsUnknownWhenTheFleetQueryFailed(t *testing.T) {
	t.Parallel()
	if p := laneProbe(nil, false, "/wt/lane-a"); p.Known {
		t.Errorf("a failed list-sessions must be unknown: %+v", p)
	}
}

// A phase-less `<lane>.done` must reach BOTH consumers as the first declared
// phase, because an empty string is a wildcard to each of them in a different
// direction.
//
// This asserts the wiring, not a helper's arithmetic. Reverting the resolution
// in readClaims must fail this test, which is what a resolver tested in
// isolation cannot detect: the first version of this test called the helper
// directly and passed against the reverted call site.
//
// Two properties, one signal:
//   - the LaneSignal carries `swe`, so decide's attempt-identity guard compares
//     it against the lane's real attempt instead of skipping the comparison;
//   - the verdict is BLOCKED, because MutatingPhase("swe") re-arms the
//     empty-branch refusal that MutatingPhase("") switches off.
func TestReadClaimsResolvesThePhaseLessShorthand(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{Phases: []state.Phase{
			{Name: "swe"}, {Name: "local-review"},
		}},
		Lanes: []*state.Lane{
			{ID: "a", Status: state.StatusRunning, Phase: "swe", Round: 1},
		},
	}
	// A measured worktree that exists and committed NOTHING since the phase
	// began: the empty-branch case the refusal exists for.
	worktrees := map[string]reconcile.WorktreeState{
		"a": reconcile.WorktreeState{Path: "/wt/a", Exists: true, CommitsSinceFork: 0}.Measure(),
	}
	// `a.done` -- no phase segment, exactly what ParseSignalName yields for the
	// run dir's first-phase shorthand.
	signals := []reconcile.Signal{
		{Lane: "a", Phase: "", Round: 1, Kind: reconcile.SignalDone, Path: "/run/a.done"},
	}

	laneSignals, verdicts := readClaims(st, signals, worktrees)

	if len(laneSignals) != 1 {
		t.Fatalf("the claim must be passed to the rule engine, got %v", laneSignals)
	}
	if laneSignals[0].Phase != "swe" {
		t.Errorf("phase = %q, want the resolved first phase swe; an empty phase is "+
			"a wildcard to decide's attempt-identity guard", laneSignals[0].Phase)
	}
	v, ok := verdicts["a"]
	if !ok {
		t.Fatalf("a .done must be verified, got verdicts %v", verdicts)
	}
	if !v.Blocked() {
		t.Errorf("an empty branch claiming a mutating phase must be REFUSED, got %+v", v)
	}
}

// A lane already past the first phase must not have a stale shorthand `.done`
// treated as its current attempt. Together with the resolution above, the claim
// reaches decide naming `swe` while the lane is on `clean`, so the guard
// escalates instead of advancing -- covered end to end in
// decide.TestPhaseLessSignalDoesNotAdvanceALaneThatMovedOn.
func TestReadClaimsSkipsTerminalLanesButKeepsLiveOnes(t *testing.T) {
	t.Parallel()
	st := &state.State{
		Config: state.Config{Phases: []state.Phase{{Name: "swe"}, {Name: "clean"}}},
		Lanes: []*state.Lane{
			{ID: "done-lane", Status: state.StatusDone, Phase: "clean", Round: 1},
			{ID: "live", Status: state.StatusRunning, Phase: "clean", Round: 1},
		},
	}
	signals := []reconcile.Signal{
		{Lane: "done-lane", Kind: reconcile.SignalDone, Path: "/run/done-lane.done"},
		{Lane: "live", Kind: reconcile.SignalDone, Path: "/run/live.done"},
	}

	laneSignals, _ := readClaims(st, signals, map[string]reconcile.WorktreeState{})

	if len(laneSignals) != 1 || laneSignals[0].Lane != "live" {
		t.Fatalf("a reaped lane's old .done is history, not a claim: %v", laneSignals)
	}
	// And the live lane's shorthand still resolved rather than staying empty.
	if laneSignals[0].Phase != "swe" {
		t.Errorf("phase = %q, want swe", laneSignals[0].Phase)
	}
}

// An unmeasured run must not read as a pass. Every lane is REFUSED when a probe
// could not run, so `failed` stays 0 and a caller gating on the exit code would
// see a clean sweep -- the same false green doctor refuses to print.
func TestReapExitRefusesToPassAnUnmeasuredRun(t *testing.T) {
	t.Parallel()
	if err := reapExit(0, nil, nil); err != nil {
		t.Errorf("a fully measured run with nothing failing is a pass: %v", err)
	}
	if err := reapExit(0, []string{"lane-a"}, nil); err == nil {
		t.Error("a lane whose worktree could not be measured must not exit 0")
	}
	if err := reapExit(0, nil, errors.New("list-sessions failed")); err == nil {
		t.Error("an unmeasured session probe must not exit 0")
	}
	// A real failure still wins the message, since it is the more actionable one.
	err := reapExit(2, []string{"lane-a"}, errors.New("probe"))
	if err == nil || !strings.Contains(err.Error(), "failed to close") {
		t.Errorf("a genuine failure should be reported first, got %v", err)
	}
}
