package lane_test

import (
	"testing"

	"github.com/bazelment/yoloswe/orca/lane"
)

func TestLaneAttemptKeysAndValidation(t *testing.T) {
	t.Parallel()

	if got := lane.PhaseRoundKey("swe", 1); got != "swe" {
		t.Fatalf("PhaseRoundKey(swe, 1) = %q, want swe", got)
	}
	if got := lane.PhaseRoundKey("swe", 2); got != "swe2" {
		t.Fatalf("PhaseRoundKey(swe, 2) = %q, want swe2", got)
	}
	p, r := lane.SplitPhaseRound("review3")
	if p != "review" || r != 3 {
		t.Fatalf("SplitPhaseRound(review3) = (%q, %d), want (review, 3)", p, r)
	}

	l := lane.Lane{
		ID:            "core",
		RunID:         "run-1",
		Status:        lane.StatusRunning,
		RecipeName:    "default",
		RecipeVersion: 1,
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error: %v", err)
	}
	l.RecordAttempt("swe", 1, "sess-1", "term-1")
	l.RecordAttempt("swe", 2, "sess-2", "term-2")
	if got := l.MaxRound("swe"); got != 2 {
		t.Fatalf("MaxRound(swe) = %d, want 2", got)
	}
	if !l.ApprovalStale() {
		t.Fatal("expected empty approval SHA to be stale")
	}
	l.PRHead = "abc123"
	l.ApprovalSHA = "abc123"
	if l.ApprovalStale() {
		t.Fatal("expected matching approval SHA not to be stale")
	}
}

func TestParsePhaseReportAndFakeDriver(t *testing.T) {
	t.Parallel()

	raw := `Outcome: done
HEAD: 0123456789abcdef0123456789abcdef01234567
Riskiest step: sqlite lease lock contention
Observed: bazel test //orca/store/... -> pass

Implemented the orch foundation module and verified store contracts.`

	rep, err := lane.ParsePhaseReport(raw)
	if err != nil {
		t.Fatalf("ParsePhaseReport error: %v", err)
	}
	if rep.Outcome != lane.OutcomeDone {
		t.Fatalf("Outcome = %q, want done", rep.Outcome)
	}
	if rep.HeadSHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("HeadSHA = %q", rep.HeadSHA)
	}
	if rep.RiskiestStep != "sqlite lease lock contention" {
		t.Fatalf("RiskiestStep = %q", rep.RiskiestStep)
	}

	drv := lane.NewFakeDriver()
	res, err := drv.SpawnSession(t.Context(), lane.SpawnRequest{
		RunID:   "run-1",
		LaneID:  "core",
		Phase:   "swe",
		Round:   1,
		Harness: "claude",
		Model:   "opus",
		Brief:   "build orch",
	})
	if err != nil {
		t.Fatalf("SpawnSession error: %v", err)
	}
	drv.SetReport(res.TerminalID, *rep)
	gotRep, err := drv.ReadReport(t.Context(), "run-1", "core", res.TerminalID)
	if err != nil {
		t.Fatalf("ReadReport error: %v", err)
	}
	if gotRep.Outcome != lane.OutcomeDone {
		t.Fatalf("ReadReport Outcome = %q, want done", gotRep.Outcome)
	}
}
