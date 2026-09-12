package decide

import (
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/state"
	"github.com/bazelment/yoloswe/swarm-queen/verify"
)

func testState(lanes ...*state.Lane) *state.State {
	return &state.State{
		Config: state.Config{
			Goal:   "test",
			Base:   "main",
			Target: "swarm/t",
			Phases: []state.Phase{
				{Name: "swe"}, {Name: "clean"}, {Name: "local-review"}, {Name: "github-review"},
			},
		},
		Lanes: lanes,
	}
}

func find(ds []Decision, lane string, kind Kind) (Decision, bool) {
	for _, d := range ds {
		if d.Lane == lane && d.Kind == kind {
			return d, true
		}
	}
	return Decision{}, false
}

// A verified .done advances to the next phase.
func TestVerifiedDoneAdvances(t *testing.T) {
	t.Parallel()
	st := testState(&state.Lane{ID: "a", Status: state.StatusRunning, Phase: "swe"})
	ds := Plan(Inputs{
		State:    st,
		Signals:  []LaneSignal{{Lane: "a", Phase: "swe", Round: 1}},
		Verdicts: map[string]verify.Verdict{"a": {OK: true}},
	})
	d, ok := find(ds, "a", KindAdvance)
	if !ok {
		t.Fatalf("expected an advance, got %v", ds)
	}
	if d.Phase != "clean" {
		t.Errorf("advanced to %q, want clean", d.Phase)
	}
}

// A .done whose verification was REFUSED must never advance. This is the
// empty-branch case: the file says done, the branch says nothing happened.
func TestRefusedClaimEscalatesInsteadOfAdvancing(t *testing.T) {
	t.Parallel()
	st := testState(&state.Lane{ID: "a", Status: state.StatusRunning, Phase: "swe"})
	blocked := verify.Verdict{OK: false, Findings: []verify.Finding{{
		Lane: "a", Severity: verify.SeverityBlock,
		Evidence: "branch has 0 commits since the phase started",
		Action:   "EMPTY BRANCH: back up and nudge, do NOT merge",
	}}}

	ds := Plan(Inputs{
		State:    st,
		Signals:  []LaneSignal{{Lane: "a", Phase: "swe", Round: 1}},
		Verdicts: map[string]verify.Verdict{"a": blocked},
	})
	if _, ok := find(ds, "a", KindAdvance); ok {
		t.Fatalf("a refused claim must not advance: %v", ds)
	}
	if _, ok := find(ds, "a", KindEscalate); !ok {
		t.Errorf("expected an escalation, got %v", ds)
	}
}

// An unverified claim holds rather than advancing. Absence of a verdict is not
// permission.
func TestUnverifiedClaimHolds(t *testing.T) {
	t.Parallel()
	st := testState(&state.Lane{ID: "a", Status: state.StatusRunning, Phase: "swe"})
	ds := Plan(Inputs{State: st, Signals: []LaneSignal{{Lane: "a", Phase: "swe"}}})
	if _, ok := find(ds, "a", KindAdvance); ok {
		t.Fatalf("must not advance without a verdict: %v", ds)
	}
	if _, ok := find(ds, "a", KindHold); !ok {
		t.Errorf("expected a hold, got %v", ds)
	}
}

// Rework must INCREMENT the round so the previous attempt's session survives.
// Overwriting is what destroyed rounds 2-6 of an 11-round lane in a real run.
func TestReworkIncrementsRound(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{
		ID: "migration-pool-tuning", Status: state.StatusRunning, Phase: "local-review",
		Sessions: map[string]string{"swe": "s1", "swe2": "s2", "local-review": "r1"},
	}
	ds := Plan(Inputs{
		State:   testState(lane),
		Signals: []LaneSignal{{Lane: lane.ID, Phase: "local-review", Round: 2, NeedsSWE: true}},
	})
	d, ok := find(ds, lane.ID, KindRework)
	if !ok {
		t.Fatalf("expected rework, got %v", ds)
	}
	if d.Phase != "swe" {
		t.Errorf("rework phase = %q, want swe", d.Phase)
	}
	if d.Round != 3 {
		t.Errorf("rework round = %d, want 3 (max existing swe round was 2)", d.Round)
	}
}

// The final phase completing means the lane is done, not advanced into nothing.
func TestFinalPhaseCompletionReaps(t *testing.T) {
	t.Parallel()
	st := testState(&state.Lane{ID: "a", Status: state.StatusRunning, Phase: "github-review"})
	ds := Plan(Inputs{
		State:    st,
		Signals:  []LaneSignal{{Lane: "a", Phase: "github-review"}},
		Verdicts: map[string]verify.Verdict{"a": {OK: true}},
	})
	d, ok := find(ds, "a", KindReap)
	if !ok {
		t.Errorf("final phase should reap, got %v", ds)
	}
	if !d.FinalPhaseComplete {
		t.Error("final phase reap must carry the verified-completion marker")
	}
}

// A free slot with ready work must be staffed. "idle, available" repeated tick
// after tick is the stall signal, not a status.
func TestFreeSlotsAreRefilled(t *testing.T) {
	t.Parallel()
	st := testState(
		&state.Lane{ID: "running", Status: state.StatusRunning, Phase: "swe"},
		&state.Lane{ID: "ready-p0", Status: state.StatusPlanned, Priority: state.P0},
		&state.Lane{ID: "ready-p2", Status: state.StatusPlanned, Priority: state.P2},
		&state.Lane{ID: "ready-p1", Status: state.StatusPlanned, Priority: state.P1},
	)
	ds := Plan(Inputs{State: st, MaxConcurrent: 3})

	var spawned []string
	for _, d := range ds {
		if d.Kind == KindSpawn {
			spawned = append(spawned, d.Lane)
		}
	}
	// One slot occupied, cap of 3, so two spawns, highest priority first.
	want := []string{"ready-p0", "ready-p1"}
	if len(spawned) != len(want) {
		t.Fatalf("spawned %v, want %v", spawned, want)
	}
	for i := range want {
		if spawned[i] != want[i] {
			t.Errorf("spawned[%d] = %q, want %q", i, spawned[i], want[i])
		}
	}
}

func TestConcurrencyCapIsRespected(t *testing.T) {
	t.Parallel()
	st := testState(
		&state.Lane{ID: "r1", Status: state.StatusRunning, Phase: "swe"},
		&state.Lane{ID: "r2", Status: state.StatusRunning, Phase: "swe"},
		&state.Lane{ID: "ready", Status: state.StatusPlanned, Priority: state.P0},
	)
	ds := Plan(Inputs{State: st, MaxConcurrent: 2})
	if _, ok := find(ds, "ready", KindSpawn); ok {
		t.Errorf("must not exceed the concurrency cap: %v", ds)
	}
}

// Recurring roles (reporting, gap analysis) do not consume a slot.
func TestSlotExemptPhasesDoNotConsumeCapacity(t *testing.T) {
	t.Parallel()
	st := testState(
		&state.Lane{ID: "report", Status: state.StatusRunning, Phase: "report"},
		&state.Lane{ID: "gaps", Status: state.StatusRunning, Phase: "gaps"},
		&state.Lane{ID: "ready", Status: state.StatusPlanned, Priority: state.P0},
	)
	ds := Plan(Inputs{
		State: st, MaxConcurrent: 1,
		SlotExempt: map[string]bool{"report": true, "gaps": true},
	})
	if _, ok := find(ds, "ready", KindSpawn); !ok {
		t.Errorf("exempt lanes must not block staffing: %v", ds)
	}
}

// Dependencies gate dispatch.
func TestBlockedDependenciesAreNotStaffed(t *testing.T) {
	t.Parallel()
	st := testState(
		&state.Lane{ID: "dep", Status: state.StatusRunning, Phase: "swe"},
		&state.Lane{ID: "blocked", Status: state.StatusPlanned, Priority: state.P0, DependsOn: []string{"dep"}},
	)
	ds := Plan(Inputs{State: st, MaxConcurrent: 8})
	if _, ok := find(ds, "blocked", KindSpawn); ok {
		t.Errorf("a lane with an unfinished dependency must not be staffed: %v", ds)
	}
}

// Blocking drift escalates; it must not be silently worked around.
func TestBlockingDriftEscalates(t *testing.T) {
	t.Parallel()
	st := testState(&state.Lane{ID: "a", Status: state.StatusRunning, Phase: "swe"})
	ds := Plan(Inputs{
		State: st,
		Drift: []verify.Finding{{
			Lane: "a", Severity: verify.SeverityBlock,
			Claim: "status=running", Evidence: "worktree is gone",
			Action: "the lane died; recover its branch or mark it failed",
		}},
	})
	d, ok := find(ds, "a", KindEscalate)
	if !ok {
		t.Fatalf("blocking drift must escalate: %v", ds)
	}
	if !strings.Contains(d.Reason, "recover its branch") {
		t.Errorf("escalation should carry the action: %+v", d)
	}
}

// A signal naming an unknown lane is a real condition -- an orphan reporting
// into the run dir -- and must surface rather than be dropped.
func TestSignalForUnknownLaneEscalates(t *testing.T) {
	t.Parallel()
	ds := Plan(Inputs{
		State:   testState(),
		Signals: []LaneSignal{{Lane: "ghost", Phase: "swe"}},
	})
	if _, ok := find(ds, "ghost", KindEscalate); !ok {
		t.Errorf("an unknown lane's signal must escalate: %v", ds)
	}
}

func TestSummarise(t *testing.T) {
	t.Parallel()
	got := Summarise([]Decision{
		{Kind: KindSpawn}, {Kind: KindSpawn}, {Kind: KindAdvance}, {Kind: KindEscalate},
	})
	if !strings.Contains(got, "spawn 2") || !strings.Contains(got, "advance 1") {
		t.Errorf("Summarise = %q", got)
	}
	if got := Summarise(nil); got != "no decisions" {
		t.Errorf("empty Summarise = %q", got)
	}
}

// A signal is a report from a LIVE attempt. A `.done` left over from a previous
// wave, or one naming a lane that never ran, is history rather than a claim --
// acting on it advances a lane that did no work.
func TestSignalForANonRunningLaneIsNotActedOn(t *testing.T) {
	t.Parallel()
	st := testState(&state.Lane{ID: "a", Status: state.StatusPlanned})
	ds := Plan(Inputs{
		State:    st,
		Signals:  []LaneSignal{{Lane: "a", Phase: "swe", Round: 1}},
		Verdicts: map[string]verify.Verdict{"a": {OK: true}},
	})
	if _, ok := find(ds, "a", KindAdvance); ok {
		t.Errorf("a planned lane's stray .done must not advance it: %v", ds)
	}
	if _, ok := find(ds, "a", KindEscalate); !ok {
		t.Errorf("an unplaceable signal must be escalated, not dropped: %v", ds)
	}
}

// The same rule protects the rework path, which has no verdict gate at all: a
// stale `.needs-swe` naming a phase the lane has moved past would otherwise send
// a healthy lane back to the start and burn a round.
func TestStaleNeedsSWEForAnotherPhaseIsNotActedOn(t *testing.T) {
	t.Parallel()
	lane := &state.Lane{
		ID: "a", Status: state.StatusRunning, Phase: "clean",
		Sessions: map[string]string{"swe": "s1", "local-review": "r1"},
	}
	ds := Plan(Inputs{
		State:   testState(lane),
		Signals: []LaneSignal{{Lane: "a", Phase: "local-review", Round: 1, NeedsSWE: true}},
	})
	if _, ok := find(ds, "a", KindRework); ok {
		t.Errorf("a signal for a phase the lane has left must not rework it: %v", ds)
	}
	if _, ok := find(ds, "a", KindEscalate); !ok {
		t.Errorf("expected an escalation for the mismatched signal: %v", ds)
	}
}

// A signal matching the lane's current attempt is still actioned; the guard must
// not swallow the live case it exists to protect.
func TestSignalMatchingTheCurrentAttemptStillActs(t *testing.T) {
	t.Parallel()
	st := testState(&state.Lane{ID: "a", Status: state.StatusRunning, Phase: "swe"})
	ds := Plan(Inputs{
		State:    st,
		Signals:  []LaneSignal{{Lane: "a", Phase: "swe", Round: 1}},
		Verdicts: map[string]verify.Verdict{"a": {OK: true}},
	})
	if _, ok := find(ds, "a", KindAdvance); !ok {
		t.Errorf("a signal from the live attempt must still advance: %v", ds)
	}
}
