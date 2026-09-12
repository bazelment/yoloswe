package main

import (
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/decide"
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
