package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

func setupTestEnv(t *testing.T) (*clock.FakeClock, *store.MemoryStore, *lane.FakeDriver, *controller.FakeJudge) {
	t.Helper()
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()
	judge := controller.NewFakeJudge()

	if err := st.Runs().Create(t.Context(), store.Run{
		ID:        "run-1",
		Objective: "Implement deterministic controller",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	return clk, st, drv, judge
}

func appendReportEvent(t *testing.T, st store.Store, laneID string, rep lane.PhaseReport) store.Event {
	t.Helper()
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal report: %v", err)
	}
	ev, err := st.Events().Append(t.Context(), store.Event{
		RunID:   "run-1",
		Kind:    store.EventLaneReport,
		LaneID:  laneID,
		Payload: raw,
	})
	if err != nil {
		t.Fatalf("Append report event: %v", err)
	}
	return ev
}

func appendCIEvent(t *testing.T, st store.Store, laneID, status, headSHA string) store.Event {
	t.Helper()
	raw, err := json.Marshal(controller.CIPayload{
		Status:  status,
		HeadSHA: headSHA,
	})
	if err != nil {
		t.Fatalf("Marshal CI payload: %v", err)
	}
	ev, err := st.Events().Append(t.Context(), store.Event{
		RunID:   "run-1",
		Kind:    store.EventCI,
		LaneID:  laneID,
		Payload: raw,
	})
	if err != nil {
		t.Fatalf("Append CI event: %v", err)
	}
	return ev
}

func TestControllerSweReviewIntegrateAndDefaultRecipe(t *testing.T) {
	t.Parallel()

	t.Run("ThreePhaseSweReviewIntegrate", func(t *testing.T) {
		t.Parallel()
		clk, st, drv, judge := setupTestEnv(t)
		ctx := t.Context()

		def := recipe.Default()
		threePhase := def.Clone()
		threePhase.RunID = "run-1"
		threePhase.Name = "fast"
		threePhase.Phases = []recipe.Phase{def.Phases[0], def.Phases[2], def.Phases[3]}
		threePhase.Transitions = []recipe.Transition{
			{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
		}
		if _, err := st.Recipes().Save(ctx, threePhase); err != nil {
			t.Fatalf("Save recipe: %v", err)
		}

		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:         "item-1",
			RunID:      "run-1",
			Title:      "Build controller",
			Acceptance: "tests pass",
			Priority:   store.PriorityP0,
		}); err != nil {
			t.Fatalf("Add backlog: %v", err)
		}

		if err := st.Lanes().Upsert(ctx, lane.Lane{
			ID:         "lane-fast",
			RunID:      "run-1",
			ItemID:     "item-1",
			Title:      "Build controller",
			Status:     lane.StatusPlanned,
			RecipeName: "fast",
		}); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}

		ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)

		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step 1 (spawn swe): %v", err)
		}
		if len(drv.Spawns) != 1 || drv.Spawns[0].Phase != recipe.PhaseSWE {
			t.Fatalf("Spawns = %+v, want 1 spawn in swe", drv.Spawns)
		}

		appendReportEvent(t, st, "lane-fast", lane.PhaseReport{
			Phase:   recipe.PhaseSWE,
			Outcome: lane.OutcomeDone,
			HeadSHA: "sha-fast-1",
			PR:      101,
		})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step 2 (swe -> review): %v", err)
		}
		if len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Phase != recipe.PhaseReview {
			t.Fatalf("PhaseStarts = %+v, want [review]", drv.PhaseStarts)
		}

		// Review finishes before CI is green; transition holds until CI passes.
		appendReportEvent(t, st, "lane-fast", lane.PhaseReport{
			Phase:   recipe.PhaseReview,
			Outcome: lane.OutcomeDone,
			HeadSHA: "sha-fast-1",
		})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step 3 (review done awaiting CI): %v", err)
		}
		if len(drv.PhaseStarts) != 1 {
			t.Fatalf("expected review to wait for CI green before starting integrate, got %d starts", len(drv.PhaseStarts))
		}

		appendCIEvent(t, st, "lane-fast", "green", "sha-fast-1")
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step 4 (CI green -> integrate): %v", err)
		}
		if len(drv.PhaseStarts) != 2 || drv.PhaseStarts[1].Phase != recipe.PhaseIntegrate {
			t.Fatalf("PhaseStarts = %+v, want [review, integrate]", drv.PhaseStarts)
		}

		appendReportEvent(t, st, "lane-fast", lane.PhaseReport{
			Phase:   recipe.PhaseIntegrate,
			Outcome: lane.OutcomeDone,
			HeadSHA: "sha-fast-1",
		})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step 5 (integrate -> complete): %v", err)
		}

		gotLane, err := st.Lanes().Get(ctx, "run-1", "lane-fast")
		if err != nil {
			t.Fatalf("Get lane: %v", err)
		}
		if gotLane.Status != lane.StatusDone || gotLane.ApprovalSHA != "sha-fast-1" || gotLane.MergeSHA != "sha-fast-1" {
			t.Fatalf("lane after integrate = %+v, want StatusDone with matching SHAs", gotLane)
		}

		gotItem, err := st.Backlog().Get(ctx, "run-1", "item-1")
		if err != nil {
			t.Fatalf("Get backlog item: %v", err)
		}
		if gotItem.Status != store.BacklogDone {
			t.Fatalf("backlog item status = %q, want done", gotItem.Status)
		}
		if judge.BatchCount() != 0 {
			t.Fatalf("expected 0 judgment batches on happy path, got %d", judge.BatchCount())
		}
	})

	t.Run("DefaultFourPhaseRecipe", func(t *testing.T) {
		t.Parallel()
		clk, st, drv, judge := setupTestEnv(t)
		ctx := t.Context()

		if err := st.Lanes().Upsert(ctx, lane.Lane{
			ID:     "lane-def",
			RunID:  "run-1",
			Title:  "Default recipe lane",
			Status: lane.StatusPlanned,
			Checks: "green",
		}); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}

		ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)

		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (spawn): %v", err)
		}

		for _, phase := range []string{recipe.PhaseSWE, recipe.PhaseClean, recipe.PhaseReview, recipe.PhaseIntegrate} {
			ln, err := st.Lanes().Get(ctx, "run-1", "lane-def")
			if err != nil {
				t.Fatalf("Get lane in %s: %v", phase, err)
			}
			if ln.Phase != phase {
				t.Fatalf("lane.Phase = %q, want %q", ln.Phase, phase)
			}
			drv.SetReport(ln.TerminalID, lane.PhaseReport{
				Phase:   phase,
				Outcome: lane.OutcomeDone,
				HeadSHA: "sha-def-1",
				PR:      55,
			})
			if _, err := st.Events().Append(ctx, store.Event{
				RunID:  "run-1",
				Kind:   store.EventLaneReport,
				LaneID: "lane-def",
			}); err != nil {
				t.Fatalf("Append event for %s: %v", phase, err)
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatalf("Step (%s): %v", phase, err)
			}
		}

		ln, err := st.Lanes().Get(ctx, "run-1", "lane-def")
		if err != nil {
			t.Fatalf("Get lane: %v", err)
		}
		if ln.Status != lane.StatusDone {
			t.Fatalf("lane.Status = %q, want done", ln.Status)
		}

		decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1", LaneID: "lane-def"})
		if err != nil {
			t.Fatalf("List decisions: %v", err)
		}
		var tools []string
		for _, d := range decs {
			tools = append(tools, d.Tool)
		}
		wantTools := []string{
			controller.ToolPhaseStart,
			controller.ToolPhaseAdvance,
			controller.ToolPhaseAdvance,
			controller.ToolPhaseAdvance,
			controller.ToolLaneComplete,
		}
		if !slices.Equal(tools, wantTools) {
			t.Fatalf("decision tools = %v, want %v", tools, wantTools)
		}
	})
}

func TestControllerReworkAndRoundLimits(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-rework",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
		Checks: "green",
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{
		RunID:            "run-1",
		JudgmentDebounce: -1,
	}, st, drv, clk, judge)

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("initial Step: %v", err)
	}

	for round := 1; round <= 2; round++ {
		appendReportEvent(t, st, "lane-rework", lane.PhaseReport{
			Phase:   recipe.PhaseSWE,
			Round:   round,
			Outcome: lane.OutcomeDone,
			HeadSHA: "sha-r",
		})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("round %d swe Step: %v", round, err)
		}

		appendReportEvent(t, st, "lane-rework", lane.PhaseReport{
			Phase:   recipe.PhaseClean,
			Round:   round,
			Outcome: lane.OutcomeDone,
			HeadSHA: "sha-r",
		})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("round %d clean Step: %v", round, err)
		}

		appendReportEvent(t, st, "lane-rework", lane.PhaseReport{
			Phase:   recipe.PhaseReview,
			Round:   round,
			Outcome: lane.OutcomeNeedsSWE,
			Breaks:  "missing edge-case test",
		})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("round %d review Step: %v", round, err)
		}

		ln, err := st.Lanes().Get(ctx, "run-1", "lane-rework")
		if err != nil {
			t.Fatalf("Get lane after round %d rework: %v", round, err)
		}
		if ln.Phase != recipe.PhaseSWE || ln.Round != round+1 {
			t.Fatalf("after round %d rework: phase=%q round=%d, want swe round %d", round, ln.Phase, ln.Round, round+1)
		}
	}

	if judge.BatchCount() != 0 {
		t.Fatalf("expected 0 judgment batches during mechanical reworks r1-r2, got %d", judge.BatchCount())
	}

	// Round 3 review needs-swe hits MinRound: 3 -> TargetEscalate.
	for _, phase := range []string{recipe.PhaseSWE, recipe.PhaseClean} {
		appendReportEvent(t, st, "lane-rework", lane.PhaseReport{
			Phase:   phase,
			Round:   3,
			Outcome: lane.OutcomeDone,
			HeadSHA: "sha-r3",
		})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("round 3 %s Step: %v", phase, err)
		}
	}

	appendReportEvent(t, st, "lane-rework", lane.PhaseReport{
		Phase:   recipe.PhaseReview,
		Round:   3,
		Outcome: lane.OutcomeNeedsSWE,
		Breaks:  "still failing check",
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("round 3 review Step: %v", err)
	}

	ln, err := st.Lanes().Get(ctx, "run-1", "lane-rework")
	if err != nil {
		t.Fatalf("Get lane after round 3 escalation: %v", err)
	}
	if ln.Status != lane.StatusBlocked {
		t.Fatalf("lane.Status = %q, want blocked on round 3 escalation", ln.Status)
	}
	if judge.BatchCount() != 1 {
		t.Fatalf("judge.BatchCount() = %d, want 1 after round 3 escalation", judge.BatchCount())
	}

	for _, key := range []string{"swe", "swe2", "swe3", "clean", "clean2", "clean3", "review", "review2", "review3"} {
		if ln.Sessions[key] == "" || ln.Terminals[key] == "" {
			t.Fatalf("missing recorded attempt key %q in Sessions=%v Terminals=%v", key, ln.Sessions, ln.Terminals)
		}
	}
}

func TestControllerJudgmentRuleAndPhaseExitFallback(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	custom := recipe.Default()
	custom.RunID = "run-1"
	custom.Name = "security-aware"
	custom.Transitions = append(custom.Transitions, recipe.Transition{
		From: recipe.PhaseClean,
		On: recipe.Condition{
			Verdict:      recipe.OutcomeDone,
			JudgmentRule: "if the diff modifies auth or permissions",
		},
		To: recipe.PhaseReview,
	})
	if _, err := st.Recipes().Save(ctx, custom); err != nil {
		t.Fatalf("Save custom recipe: %v", err)
	}

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:         "lane-sec",
		RunID:      "run-1",
		Status:     lane.StatusPlanned,
		RecipeName: "security-aware",
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{
		RunID:            "run-1",
		JudgmentDebounce: -1,
	}, st, drv, clk, judge)

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (spawn): %v", err)
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].Harness != "claude" || drv.Spawns[0].Model != "opus" {
		t.Fatalf("initial spawn = %+v, want claude:opus", drv.Spawns)
	}

	// Non-zero exit without a report triggers attempt 2 on the fallback agent chain.
	rawExit, _ := json.Marshal(controller.PhaseExitPayload{
		ExitCode: 1,
		Error:    "rate limited",
	})
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventPhaseExit,
		LaneID:  "lane-sec",
		Payload: rawExit,
	}); err != nil {
		t.Fatalf("Append phase_exit: %v", err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (phase_exit fallback): %v", err)
	}
	if len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Harness != "codex" || drv.PhaseStarts[0].Model != "gpt-5.4" {
		t.Fatalf("fallback PhaseStarts = %+v, want codex:gpt-5.4", drv.PhaseStarts)
	}
	if judge.BatchCount() != 0 {
		t.Fatalf("expected fallback retry to be auto (0 judgment batches), got %d", judge.BatchCount())
	}

	appendReportEvent(t, st, "lane-sec", lane.PhaseReport{
		Phase:   recipe.PhaseSWE,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-sec-1",
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (swe done): %v", err)
	}

	appendReportEvent(t, st, "lane-sec", lane.PhaseReport{
		Phase:   recipe.PhaseClean,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-sec-1",
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (clean done with judgment rule): %v", err)
	}
	if judge.BatchCount() != 1 {
		t.Fatalf("expected 1 judgment batch for JudgmentRule transition, got %d", judge.BatchCount())
	}
}

func TestControllerStallDetection(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-stall",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{
		RunID:            "run-1",
		JudgmentDebounce: 2 * time.Minute,
	}, st, drv, clk, judge)

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (spawn): %v", err)
	}

	clk.Advance(44 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 44m: %v", err)
	}
	if len(drv.Stops) != 0 {
		t.Fatalf("expected 0 stops at 44m, got %+v", drv.Stops)
	}

	clk.Advance(time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 45m: %v", err)
	}
	if len(drv.Stops) != 1 || drv.Stops[0].LaneID != "lane-stall" {
		t.Fatalf("Stops at 45m = %+v, want 1 stop for lane-stall", drv.Stops)
	}

	ln, err := st.Lanes().Get(ctx, "run-1", "lane-stall")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}
	if ln.Status != lane.StatusBlocked {
		t.Fatalf("lane.Status = %q, want blocked", ln.Status)
	}

	if judge.BatchCount() != 0 {
		t.Fatalf("expected stall event to wait for debounce window, got %d batches", judge.BatchCount())
	}
	clk.Advance(2 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step after debounce: %v", err)
	}
	if judge.BatchCount() != 1 || len(judge.Batches[0]) != 1 || judge.Batches[0][0].Kind != store.EventStall {
		t.Fatalf("judge batches = %+v, want 1 batch with EventStall", judge.Batches)
	}
}

func TestControllerJudgmentBatchingAndUrgentBypass(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-auto",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
	}); err != nil {
		t.Fatalf("Upsert lane-auto: %v", err)
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-blocked",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
	}); err != nil {
		t.Fatalf("Upsert lane-blocked: %v", err)
	}

	ctrl := controller.New(controller.Config{
		RunID:            "run-1",
		JudgmentDebounce: 3 * time.Minute,
	}, st, drv, clk, judge)

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (spawn): %v", err)
	}

	appendReportEvent(t, st, "lane-blocked", lane.PhaseReport{
		Phase:   recipe.PhaseSWE,
		Outcome: lane.OutcomeBlocked,
		Reason:  "needs sharedHome=true",
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at t=0: %v", err)
	}
	if judge.BatchCount() != 0 {
		t.Fatalf("BatchCount at t=0 = %d, want 0 (debouncing)", judge.BatchCount())
	}

	clk.Advance(time.Minute)
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventMail,
		LaneID:  "lane-blocked",
		Summary: "worker question about fixture path",
	}); err != nil {
		t.Fatalf("Append mail: %v", err)
	}
	appendReportEvent(t, st, "lane-auto", lane.PhaseReport{
		Phase:   recipe.PhaseSWE,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-auto-1",
	})

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at t=1m: %v", err)
	}
	if len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].LaneID != "lane-auto" || drv.PhaseStarts[0].Phase != recipe.PhaseClean {
		t.Fatalf("expected lane-auto to advance to clean immediately at t=1m, got %+v", drv.PhaseStarts)
	}
	if judge.BatchCount() != 0 {
		t.Fatalf("BatchCount at t=1m = %d, want 0", judge.BatchCount())
	}

	clk.Advance(2 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at t=3m: %v", err)
	}
	if judge.BatchCount() != 1 {
		t.Fatalf("BatchCount at t=3m = %d, want 1", judge.BatchCount())
	}
	if len(judge.Batches[0]) != 2 ||
		judge.Batches[0][0].Kind != store.EventLaneReport ||
		judge.Batches[0][1].Kind != store.EventMail {
		t.Fatalf("Batches[0] = %+v, want [lane_report, mail]", judge.Batches[0])
	}

	clk.Advance(time.Minute)
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventMail,
		Summary: "non-urgent status note",
	}); err != nil {
		t.Fatalf("Append mail 2: %v", err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at t=4m: %v", err)
	}
	if judge.BatchCount() != 1 {
		t.Fatalf("expected non-urgent mail to debounce at t=4m, got %d batches", judge.BatchCount())
	}

	clk.Advance(15 * time.Second)
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Urgent:  true,
		Summary: "stop lane-blocked and reprioritize P0",
	}); err != nil {
		t.Fatalf("Append urgent owner_msg: %v", err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at t=4m15s (urgent bypass): %v", err)
	}
	if judge.BatchCount() != 2 {
		t.Fatalf("BatchCount after urgent event = %d, want 2", judge.BatchCount())
	}
	if len(judge.Batches[1]) != 2 || !judge.Batches[1][1].Urgent {
		t.Fatalf("Batches[1] = %+v, want [mail, urgent owner_msg]", judge.Batches[1])
	}
}

func TestControllerOwnerAskDefaultAtDeadline(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-ask",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (spawn): %v", err)
	}

	badAsk := controller.OwnerAsk{
		ID:       "ask-bad",
		Question: "Which DB?",
		Options:  []string{"sqlite", "postgres"},
		Default:  "mysql",
		Deadline: clk.Now().Add(10 * time.Minute),
	}
	if err := ctrl.AskOwner(ctx, badAsk); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("AskOwner(invalid default) = %v, want ErrInvalidArgument", err)
	}

	deadline := clk.Now().Add(10 * time.Minute)
	ask1 := controller.OwnerAsk{
		ID:        "ask-1",
		LaneID:    "lane-ask",
		Question:  "Keep SQLite WAL mode or rollback journal?",
		Options:   []string{"wal", "journal"},
		Default:   "wal",
		SteerNote: "Use SQLite WAL mode (default applied at deadline)",
		Deadline:  deadline,
	}
	if err := ctrl.AskOwner(ctx, ask1); err != nil {
		t.Fatalf("AskOwner(ask1): %v", err)
	}

	ask2 := controller.OwnerAsk{
		ID:       "ask-2",
		Question: "Include shadow mode in MVP?",
		Options:  []string{"defer", "include"},
		Default:  "defer",
		Deadline: deadline,
	}
	if err := ctrl.AskOwner(ctx, ask2); err != nil {
		t.Fatalf("AskOwner(ask2): %v", err)
	}

	ln, err := st.Lanes().Get(ctx, "run-1", "lane-ask")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}
	if ln.Status != lane.StatusBlocked {
		t.Fatalf("lane.Status after AskOwner = %q, want blocked", ln.Status)
	}

	clk.Advance(5 * time.Minute)
	if err := ctrl.AnswerOwnerAsk(ctx, "ask-2", "include"); err != nil {
		t.Fatalf("AnswerOwnerAsk(ask-2): %v", err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at t=5m: %v", err)
	}
	if len(drv.Steers) != 0 {
		t.Fatalf("expected 0 steers on lane-ask at t=5m, got %+v", drv.Steers)
	}

	clk.Advance(5 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at t=10m (deadline): %v", err)
	}

	if len(drv.Steers) != 1 || drv.Steers[0].LaneID != "lane-ask" || drv.Steers[0].Message != ask1.SteerNote {
		t.Fatalf("Steers at t=10m = %+v, want 1 steer with %q", drv.Steers, ask1.SteerNote)
	}

	ln, err = st.Lanes().Get(ctx, "run-1", "lane-ask")
	if err != nil {
		t.Fatalf("Get lane after ask expiry: %v", err)
	}
	if ln.Status != lane.StatusRunning {
		t.Fatalf("lane.Status after ask default = %q, want running", ln.Status)
	}

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatalf("List decisions: %v", err)
	}
	var askDecs []store.Decision
	for _, d := range decs {
		if d.Tool == controller.ToolOwnerAskDefault {
			askDecs = append(askDecs, d)
		}
	}
	if len(askDecs) != 1 || askDecs[0].Result != "wal" {
		t.Fatalf("owner.ask.default decisions = %+v, want 1 decision with Result=wal", askDecs)
	}

	ansDir, err := st.Directives().Get(ctx, "run-1", "ask-answer-ask-2")
	if err != nil || ansDir.Text != "include" {
		t.Fatalf("ask-2 answer directive = %+v, err=%v, want Text=include", ansDir, err)
	}
}

type writeAheadCheckDriver struct {
	*lane.FakeDriver
	store store.Store
	t     *testing.T
}

func (d *writeAheadCheckDriver) assertDecisionLogged(tool, laneID string) {
	d.t.Helper()
	decs, err := d.store.Decisions().List(d.t.Context(), store.DecisionFilter{
		RunID:  "run-1",
		LaneID: laneID,
	})
	if err != nil {
		d.t.Fatalf("List decisions inside driver callback: %v", err)
	}
	for i := len(decs) - 1; i >= 0; i-- {
		if decs[i].Tool == tool {
			return
		}
	}
	d.t.Fatalf("driver action for tool %q on lane %q executed BEFORE Decision was written to DecisionStore (decisions=%+v)", tool, laneID, decs)
}

func (d *writeAheadCheckDriver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	d.assertDecisionLogged(controller.ToolPhaseStart, req.LaneID)
	return d.FakeDriver.SpawnSession(ctx, req)
}

func (d *writeAheadCheckDriver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	d.t.Helper()
	decs, err := d.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:  "run-1",
		LaneID: req.LaneID,
	})
	if err != nil || len(decs) == 0 {
		d.t.Fatalf("StartPhaseTerminal executed without prior decision: err=%v decs=%+v", err, decs)
	}
	last := decs[len(decs)-1]
	if last.Tool != controller.ToolPhaseAdvance && last.Tool != controller.ToolPhaseRework && last.Tool != controller.ToolPhaseRetry {
		d.t.Fatalf("last decision before StartPhaseTerminal = %q, want phase.advance/rework/retry", last.Tool)
	}
	return d.FakeDriver.StartPhaseTerminal(ctx, req)
}

func (d *writeAheadCheckDriver) Stop(ctx context.Context, req lane.StopRequest) error {
	d.assertDecisionLogged(controller.ToolLaneStall, req.LaneID)
	return d.FakeDriver.Stop(ctx, req)
}

func (d *writeAheadCheckDriver) Steer(ctx context.Context, req lane.SteerRequest) error {
	d.assertDecisionLogged(controller.ToolOwnerAskDefault, req.LaneID)
	return d.FakeDriver.Steer(ctx, req)
}

func TestControllerKillRestartIdempotenceAndLeaseTakeover(t *testing.T) {
	t.Parallel()
	clk, st, baseDrv, judge := setupTestEnv(t)
	ctx := t.Context()

	drv := &writeAheadCheckDriver{
		FakeDriver: baseDrv,
		store:      st,
		t:          t,
	}

	judge.OnJudge = func(ctx context.Context, runID string, events []store.Event) error {
		decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
		if err != nil || len(decs) == 0 || decs[len(decs)-1].Tool != controller.ToolJudgmentDispatch {
			t.Fatalf("Judge invoked before ToolJudgmentDispatch was written to DecisionStore: %+v", decs)
		}
		return nil
	}

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-ha",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
		Checks: "green",
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl1 := controller.New(controller.Config{
		RunID:            "run-1",
		HolderID:         "controller-a",
		LeaseTTL:         30 * time.Second,
		JudgmentDebounce: 2 * time.Minute,
	}, st, drv, clk, judge)

	if err := ctrl1.Step(ctx); err != nil {
		t.Fatalf("ctrl1 Step 1 (spawn): %v", err)
	}
	appendReportEvent(t, st, "lane-ha", lane.PhaseReport{
		Phase:   recipe.PhaseSWE,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-ha-1",
		PR:      77,
	})
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventMail,
		LaneID:  "lane-ha",
		Summary: "pending judgment event across restart",
	}); err != nil {
		t.Fatalf("Append mail: %v", err)
	}
	if err := ctrl1.AskOwner(ctx, controller.OwnerAsk{
		ID:       "ask-ha",
		Question: "Keep default retry cap?",
		Options:  []string{"yes", "no"},
		Default:  "yes",
		Deadline: clk.Now().Add(90 * time.Second),
	}); err != nil {
		t.Fatalf("ctrl1 AskOwner: %v", err)
	}
	if err := ctrl1.Step(ctx); err != nil {
		t.Fatalf("ctrl1 Step 2 (swe -> clean + queue mail): %v", err)
	}

	if len(drv.Spawns) != 1 || len(drv.PhaseStarts) != 1 {
		t.Fatalf("after ctrl1 Step 2: Spawns=%d PhaseStarts=%d, want 1, 1", len(drv.Spawns), len(drv.PhaseStarts))
	}

	ctrl1.Kill()
	if err := ctrl1.Step(ctx); !errors.Is(err, controller.ErrControllerStopped) {
		t.Fatalf("ctrl1.Step after Kill = %v, want ErrControllerStopped", err)
	}

	ctrl2 := controller.New(controller.Config{
		RunID:            "run-1",
		HolderID:         "controller-b",
		LeaseTTL:         30 * time.Second,
		JudgmentDebounce: 2 * time.Minute,
	}, st, drv, clk, judge)

	clk.Advance(10 * time.Second)
	if err := ctrl2.Step(ctx); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("ctrl2.Step before lease expiry = %v, want ErrLeaseHeld", err)
	}

	clk.Advance(20 * time.Second)
	if err := ctrl2.Step(ctx); err != nil {
		t.Fatalf("ctrl2.Step after lease expiry: %v", err)
	}
	if len(drv.Spawns) != 1 || len(drv.PhaseStarts) != 1 {
		t.Fatalf("after ctrl2 takeover: Spawns=%d PhaseStarts=%d, want 1, 1 (no duplicates)", len(drv.Spawns), len(drv.PhaseStarts))
	}

	clk.Advance(90 * time.Second)
	if err := ctrl2.Step(ctx); err != nil {
		t.Fatalf("ctrl2.Step at t=2m: %v", err)
	}
	if judge.BatchCount() != 1 || len(judge.Batches[0]) != 1 || judge.Batches[0][0].Kind != store.EventMail {
		t.Fatalf("recovered judgment batch on ctrl2 = %+v, want 1 batch with mail event", judge.Batches)
	}
	defDir, err := st.Directives().Get(ctx, "run-1", "ask-default-ask-ha")
	if err != nil || defDir.Text != "yes" {
		t.Fatalf("recovered ask default directive on ctrl2 = %+v, err=%v", defDir, err)
	}

	for _, phase := range []string{recipe.PhaseClean, recipe.PhaseReview, recipe.PhaseIntegrate} {
		appendReportEvent(t, st, "lane-ha", lane.PhaseReport{
			Phase:   phase,
			Outcome: lane.OutcomeDone,
			HeadSHA: "sha-ha-1",
		})
		if err := ctrl2.Step(ctx); err != nil {
			t.Fatalf("ctrl2 Step (%s): %v", phase, err)
		}
	}

	ln, err := st.Lanes().Get(ctx, "run-1", "lane-ha")
	if err != nil || ln.Status != lane.StatusDone {
		t.Fatalf("lane-ha after ctrl2 completion = %+v, err=%v", ln, err)
	}

	spawnsBeforeReplay := len(drv.Spawns)
	startsBeforeReplay := len(drv.PhaseStarts)
	batchesBeforeReplay := judge.BatchCount()
	decsBeforeReplay, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatalf("List decisions before replay: %v", err)
	}

	// Reset the event cursor to 0 after decisions are committed; a restarted
	// controller must skip already-decided events when replaying from Seq 1.
	ctrl2.Kill()
	if err := st.Events().SetCursor(ctx, "run-1", controller.DefaultConsumer, 0); err != nil {
		t.Fatalf("SetCursor(0): %v", err)
	}
	clk.Advance(31 * time.Second)

	ctrl3 := controller.New(controller.Config{
		RunID:            "run-1",
		HolderID:         "controller-c",
		LeaseTTL:         30 * time.Second,
		JudgmentDebounce: 2 * time.Minute,
	}, st, drv, clk, judge)

	if err := ctrl3.Step(ctx); err != nil {
		t.Fatalf("ctrl3.Step (full event replay from cursor 0): %v", err)
	}

	if len(drv.Spawns) != spawnsBeforeReplay {
		t.Fatalf("Spawns after replay = %d, want %d", len(drv.Spawns), spawnsBeforeReplay)
	}
	if len(drv.PhaseStarts) != startsBeforeReplay {
		t.Fatalf("PhaseStarts after replay = %d, want %d", len(drv.PhaseStarts), startsBeforeReplay)
	}
	if judge.BatchCount() != batchesBeforeReplay {
		t.Fatalf("BatchCount after replay = %d, want %d", judge.BatchCount(), batchesBeforeReplay)
	}
	decsAfterReplay, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatalf("List decisions after replay: %v", err)
	}
	if len(decsAfterReplay) != len(decsBeforeReplay) {
		t.Fatalf("len(decisions) after replay = %d, want %d", len(decsAfterReplay), len(decsBeforeReplay))
	}
}

func TestControllerStaleApprovalAndRunLoop(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:          "lane-stale",
		RunID:       "run-1",
		Status:      lane.StatusRunning,
		Phase:       recipe.PhaseIntegrate,
		Round:       1,
		Attempt:     1,
		SessionID:   "sess-stale",
		TerminalID:  "term-stale",
		PRHead:      "sha-new-unreviewed",
		ApprovalSHA: "sha-old-reviewed",
		Checks:      "green",
	}); err != nil {
		t.Fatalf("Upsert lane-stale: %v", err)
	}

	judgedCh := make(chan struct{}, 1)
	judge.OnJudge = func(_ context.Context, _ string, _ []store.Event) error {
		select {
		case judgedCh <- struct{}{}:
		default:
		}
		return nil
	}

	ctrl := controller.New(controller.Config{
		RunID:             "run-1",
		HolderID:          "controller-loop",
		LeaseTTL:          30 * time.Second,
		HeartbeatInterval: 10 * time.Second,
		PollInterval:      5 * time.Second,
		JudgmentDebounce:  -1,
	}, st, drv, clk, judge)

	appendReportEvent(t, st, "lane-stale", lane.PhaseReport{
		Phase:   recipe.PhaseIntegrate,
		Outcome: lane.OutcomeDone,
	})

	runCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- ctrl.Run(runCtx)
	}()

	<-judgedCh

	ln, err := st.Lanes().Get(ctx, "run-1", "lane-stale")
	if err != nil {
		t.Fatalf("Get lane-stale: %v", err)
	}
	if ln.Status != lane.StatusBlocked {
		t.Fatalf("lane-stale status = %q, want blocked on stale approval", ln.Status)
	}

	ctrl.Wake()
	cancel()
	if err := <-doneCh; err != nil {
		t.Fatalf("ctrl.Run error: %v", err)
	}

	if err := ctrl.ReleaseLease(ctx); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	run, err := st.Runs().Get(ctx, "run-1")
	if err != nil || run.Lease != nil {
		t.Fatalf("run after ReleaseLease = %+v, err=%v, want nil Lease", run, err)
	}
}
