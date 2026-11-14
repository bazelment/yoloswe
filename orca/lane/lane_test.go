package lane_test

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
)

func TestFakeDriverWatchEmitsEveryEventKind(t *testing.T) {
	t.Parallel()
	cases := []lane.EventKind{
		lane.EventPhaseReported, lane.EventTurnEndedNoReport, lane.EventWaitingForInput,
		lane.EventExited, lane.EventQuotaFailure, lane.EventAuthFailure, lane.EventNoActivity,
	}
	for _, kind := range cases {
		t.Run(string(kind), func(t *testing.T) {
			drv := lane.NewFakeDriver()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			watch := drv.Watch(ctx, "run-1")
			drv.Emit(lane.Event{Kind: kind, RunID: "run-1", Since: time.Now()})
			select {
			case got := <-watch:
				if got.Kind != kind {
					t.Fatalf("kind = %q, want %q", got.Kind, kind)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for event")
			}
		})
	}
}

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

func TestParsePhaseReportBulletedAndOrderIndependentVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		raw         string
		wantOutcome lane.ReportOutcome
		wantHead    string
		wantLane    string
		wantPhase   string
		wantReason  string
		wantPR      int
		wantRound   int
		wantErr     bool
	}{
		{
			name:        "bulleted review pass",
			raw:         "- Lane: m2-default\n- Phase: review\n- Round: r2\n- PR: #5705\n- Head-SHA: fb6c5bae\n- Verdict: pass\n- Rework-class: none\n",
			wantOutcome: lane.OutcomeDone,
			wantHead:    "fb6c5bae",
			wantPR:      5705,
			wantLane:    "m2-default",
			wantPhase:   "review",
			wantRound:   2,
			wantReason:  "none",
		},
		{
			name:        "verdict needs-changes before outcome done",
			raw:         "Verdict: needs-changes\nOutcome: done\nHEAD: abc123\n",
			wantOutcome: lane.OutcomeNeedsSWE,
			wantHead:    "abc123",
		},
		{
			name:        "outcome done before verdict needs-changes",
			raw:         "Outcome: done\nVerdict: needs-changes\nHEAD: abc123\n",
			wantOutcome: lane.OutcomeNeedsSWE,
			wantHead:    "abc123",
		},
		{
			name:    "free-text prose without outcome or verdict",
			raw:     "Polished PR #5705 and pushed fb6c5bae.\nAll tests pass.",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := lane.ParsePhaseReport(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.raw, rep)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePhaseReport error: %v", err)
			}
			if rep.Outcome != tc.wantOutcome || rep.HeadSHA != tc.wantHead || rep.PR != tc.wantPR ||
				rep.LaneID != tc.wantLane || rep.Phase != tc.wantPhase || rep.Round != tc.wantRound || rep.Reason != tc.wantReason {
				t.Fatalf("got %+v, want outcome=%q head=%q pr=%d lane=%q phase=%q round=%d reason=%q",
					rep, tc.wantOutcome, tc.wantHead, tc.wantPR, tc.wantLane, tc.wantPhase, tc.wantRound, tc.wantReason)
			}
		})
	}
}
