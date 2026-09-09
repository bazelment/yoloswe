package state

import (
	"reflect"
	"testing"
)

func TestSplitPhaseRound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key       string
		wantPhase string
		wantRound int
	}{
		{"swe", "swe", 1},
		{"clean", "clean", 1},
		{"swe2", "swe", 2},
		{"swe11", "swe", 11},
		{"local-review", "local-review", 1},
		{"local-review7", "local-review", 7},
		{"local-review-r3", "local-review", 3}, // the other convention seen live
		{"github-review", "github-review", 1},
	}
	for _, tc := range cases {
		p, r := SplitPhaseRound(tc.key)
		if p != tc.wantPhase || r != tc.wantRound {
			t.Errorf("SplitPhaseRound(%q) = (%q, %d), want (%q, %d)",
				tc.key, p, r, tc.wantPhase, tc.wantRound)
		}
	}
}

func TestPhaseRoundKeyRoundTrips(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"swe", "local-review", "github-review"} {
		for round := 1; round <= 12; round++ {
			key := PhaseRoundKey(phase, round)
			gotPhase, gotRound := SplitPhaseRound(key)
			if gotPhase != phase || gotRound != round {
				t.Errorf("round-trip %q/%d -> %q -> (%q,%d)",
					phase, round, key, gotPhase, gotRound)
			}
		}
	}
	// Round 1 must stay the bare name so keys match what ledger.py writes.
	if got := PhaseRoundKey("swe", 1); got != "swe" {
		t.Errorf("PhaseRoundKey(swe,1) = %q, want %q", got, "swe")
	}
}

// RecordSession must not clobber an earlier round, which is the bug that
// destroyed session ids in real runs.
func TestRecordSessionKeepsEveryRound(t *testing.T) {
	t.Parallel()
	l := &Lane{ID: "x"}
	l.RecordSession("swe", 1, "sess-1")
	l.RecordSession("swe", 2, "sess-2")
	l.RecordSession("swe", 3, "sess-3")

	for round, want := range map[int]string{1: "sess-1", 2: "sess-2", 3: "sess-3"} {
		got, ok := l.SessionFor("swe", round)
		if !ok || got != want {
			t.Errorf("SessionFor(swe,%d) = (%q,%v), want %q", round, got, ok, want)
		}
	}
	if got := l.MaxRound("swe"); got != 3 {
		t.Errorf("MaxRound = %d, want 3", got)
	}
	if lost := l.LostRounds("swe"); len(lost) != 0 {
		t.Errorf("LostRounds = %v, want none", lost)
	}
}

// The real `error-taxonomy` lane: swe/clean/local-review then a jump straight to
// swe4. Rounds 2 and 3 were overwritten and are gone. Reconcile must surface
// that rather than present a partial history as complete.
func TestLostRoundsDetectsOverwrittenHistory(t *testing.T) {
	t.Parallel()
	l := &Lane{ID: "error-taxonomy", Sessions: map[string]string{
		"swe": "a", "clean": "b", "local-review": "c",
		"swe4": "d", "local-review4": "e",
		"swe5": "f", "local-review5": "g",
		"swe6": "h", "local-review6": "i",
		"github-review": "j",
	}}
	if got, want := l.MaxRound("swe"), 6; got != want {
		t.Errorf("MaxRound(swe) = %d, want %d", got, want)
	}
	if got, want := l.LostRounds("swe"), []int{2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("LostRounds(swe) = %v, want %v", got, want)
	}
	// local-review has round 1 then 4,5,6 -> 2 and 3 lost as well.
	if got, want := l.LostRounds("local-review"), []int{2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("LostRounds(local-review) = %v, want %v", got, want)
	}
}

// Real runs set phase to values the config never declared ("merged", "rebase"),
// which makes any phase-ordered logic silently wrong.
func TestUnknownPhasesDetectsRuntimeInventedPhases(t *testing.T) {
	t.Parallel()
	cfg := &Config{Phases: []Phase{
		{Name: "swe"}, {Name: "clean"}, {Name: "local-review"}, {Name: "github-review"},
	}}
	l := &Lane{
		ID:    "migration-elision-enable",
		Phase: "github-review",
		Sessions: map[string]string{
			"swe": "a", "clean": "b", "local-review": "c",
			"github-review": "d", "rebase": "e",
		},
	}
	if got, want := l.UnknownPhases(cfg), []string{"rebase"}; !reflect.DeepEqual(got, want) {
		t.Errorf("UnknownPhases = %v, want %v", got, want)
	}

	merged := &Lane{ID: "datadog-monitors", Phase: "merged", Sessions: map[string]string{"swe": "a"}}
	if got, want := merged.UnknownPhases(cfg), []string{"merged"}; !reflect.DeepEqual(got, want) {
		t.Errorf("UnknownPhases = %v, want %v", got, want)
	}
}

func TestAttemptsAreOrdered(t *testing.T) {
	t.Parallel()
	l := &Lane{Sessions: map[string]string{
		"swe3": "c", "swe": "a", "clean": "z", "swe2": "b",
	}}
	got := l.Attempts()
	want := []Attempt{
		{Phase: "clean", Round: 1, Session: "z"},
		{Phase: "swe", Round: 1, Session: "a"},
		{Phase: "swe", Round: 2, Session: "b"},
		{Phase: "swe", Round: 3, Session: "c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Attempts() = %+v, want %+v", got, want)
	}
}
