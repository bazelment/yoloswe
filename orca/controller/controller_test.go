package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
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

func appendLaneWatchEvent(t *testing.T, st store.Store, laneID, terminalID string, kind lane.EventKind) store.Event {
	t.Helper()
	payload, err := json.Marshal(controller.PhaseExitPayload{Kind: kind, TerminalID: terminalID})
	if err != nil {
		t.Fatalf("Marshal phase exit: %v", err)
	}
	ev, err := st.Events().Append(t.Context(), store.Event{RunID: "run-1", LaneID: laneID, Kind: store.EventPhaseExit, Source: "lane.watch", Payload: payload})
	if err != nil {
		t.Fatalf("Append lane watch event: %v", err)
	}
	return ev
}

func saveRecipeWithTimeout(t *testing.T, st store.Store, timeout time.Duration) {
	t.Helper()
	r := recipe.Default().Clone()
	r.RunID, r.Name = "run-1", "stall-test"
	r.Limits.PhaseTimeout = timeout
	if _, err := st.Recipes().Save(t.Context(), r); err != nil {
		t.Fatalf("Save recipe: %v", err)
	}
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

	t.Run("DefaultFourPhaseRecipeWithoutCI", func(t *testing.T) {
		t.Parallel()
		clk, st, drv, judge := setupTestEnv(t)
		ctx := t.Context()

		if err := st.Lanes().Upsert(ctx, lane.Lane{
			ID:     "lane-def",
			RunID:  "run-1",
			Title:  "Default recipe lane",
			Status: lane.StatusPlanned,
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
			var brief string
			if len(drv.Spawns) > 0 && len(drv.PhaseStarts) == 0 {
				brief = drv.Spawns[0].Brief
			} else if len(drv.PhaseStarts) > 0 {
				brief = drv.PhaseStarts[len(drv.PhaseStarts)-1].Brief
			}
			if !strings.Contains(brief, "phase report") {
				t.Fatalf("%s brief lacks fake-driver report contract: %q", phase, brief)
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
			if !strings.HasSuffix(d.Tool, ".intent") {
				tools = append(tools, d.Tool)
			}
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

func TestControllerReportOnlySweCompletesLane(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, head, change string
		wantDone           bool
	}{
		{"recorded base after branch moves", "base-sha", "", true},
		{"one new commit", "new-sha", "", false},
		{"explicit no change", "base-sha", "none", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk, st, drv, judge := setupTestEnv(t)
			ctx := t.Context()
			drv.ReportOnlyBranchExists = tc.name == "one new commit"
			drv.ReportOnlyCommitsAhead = map[bool]int{true: 1, false: 0}[drv.ReportOnlyBranchExists]
			if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-empty", RunID: "run-1", Status: lane.StatusPlanned, BaseBranch: "main", BaseCommit: "base-sha", Branch: "lane-empty"}); err != nil {
				t.Fatal(err)
			}
			ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			ln, err := st.Lanes().Get(ctx, "run-1", "lane-empty")
			if err != nil {
				t.Fatal(err)
			}
			// Move the mutable base while retaining the immutable spawn commit.
			ln.BaseBranch = "moved-base"
			if err := st.Lanes().Upsert(ctx, *ln); err != nil {
				t.Fatal(err)
			}
			drv.SetReport(ln.TerminalID, lane.PhaseReport{Phase: recipe.PhaseSWE, Outcome: lane.OutcomeDone, HeadSHA: tc.head, Change: tc.change})
			if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", Kind: store.EventLaneReport, LaneID: "lane-empty"}); err != nil {
				t.Fatal(err)
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			ln, err = st.Lanes().Get(ctx, "run-1", "lane-empty")
			if err != nil {
				t.Fatal(err)
			}
			if got := ln.Status == lane.StatusDone; got != tc.wantDone {
				t.Fatalf("status = %q, wantDone = %v", ln.Status, tc.wantDone)
			}
		})
	}
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
	if len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Harness != "codex" || drv.PhaseStarts[0].Model != "gpt-6.1-sol" {
		t.Fatalf("fallback PhaseStarts = %+v, want codex:gpt-6.1-sol", drv.PhaseStarts)
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

func TestControllerWatchRestartLadder(t *testing.T) {
	t.Parallel()
	for _, restartAfter := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("restart_after_%d", restartAfter), func(t *testing.T) {
			t.Parallel()
			clk, st, drv, judge := setupTestEnv(t)
			ctx := t.Context()
			if err := st.Lanes().Upsert(ctx, lane.Lane{
				ID: "lane-quiet", RunID: "run-1", Status: lane.StatusPlanned,
			}); err != nil {
				t.Fatal(err)
			}
			ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
			if err := ctrl.StartWatches(ctx); err != nil {
				t.Fatal(err)
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			for step := 1; step <= 4; step++ {
				if step-1 == restartAfter {
					ctrl.Kill()
					clk.Advance(controller.DefaultLeaseTTL + time.Second)
					ctrl = controller.New(controller.Config{RunID: "run-1", HolderID: "replacement"}, st, drv, clk, judge)
					if err := ctrl.StartWatches(ctx); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := st.Events().List(ctx, store.EventFilter{RunID: "run-1"})
				clk.Advance(time.Second)
				drv.Emit(lane.Event{RunID: "run-1", LaneID: "lane-quiet", Kind: lane.EventNoActivity, Text: "quiet", At: clk.Now()})
				waitFor(t, func() bool {
					events, _ := st.Events().List(ctx, store.EventFilter{RunID: "run-1"})
					return len(events) > len(before)
				})

				if err := ctrl.Step(ctx); err != nil {
					t.Fatal(err)
				}
				wantStarts := min(step-1, 2)
				if len(drv.Steers) != 1 || len(drv.PhaseStarts) != wantStarts {
					t.Fatalf("step %d: steers=%d starts=%d, want 1 and %d", step, len(drv.Steers), len(drv.PhaseStarts), wantStarts)
				}
			}
			if drv.PhaseStarts[0].Model != "opus" || drv.PhaseStarts[1].Model != "gpt-6.1-sol" {
				t.Fatalf("starts = %+v, want fresh opus then fallback gpt-6.1-sol", drv.PhaseStarts)
			}
			ln, err := st.Lanes().Get(ctx, "run-1", "lane-quiet")
			if err != nil {
				t.Fatal(err)
			}
			if ln.Status != lane.StatusBlocked {
				t.Fatalf("status = %q, want blocked after exhausting fallback", ln.Status)
			}
			decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1", LaneID: "lane-quiet"})
			if err != nil {
				t.Fatal(err)
			}
			retries, escalations := 0, 0
			for _, dec := range decs {
				if dec.Tool == controller.ToolPhaseRetry {
					retries++
				}
				if dec.Tool == controller.ToolLaneEscalate {
					escalations++
				}
			}
			if retries != 3 || escalations != 1 {
				t.Fatalf("ladder log: retries=%d escalations=%d", retries, escalations)
			}

		})
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

func TestControllerQuietWorkingBeforePhaseLimitDoesNotRestart(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	saveRecipeWithTimeout(t, st, 20*time.Minute)
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-quiet", RunID: "run-1", RecipeName: "stall-test", Status: lane.StatusPlanned}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: time.Minute, StallTimeout: controller.DefaultStallTimeout}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "lane-quiet")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}
	drv.AgentStates[ln.TerminalID] = "working"
	for _, advance := range []time.Duration{3 * time.Minute, 16 * time.Minute} {
		clk.Advance(advance)
		appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("quiet step at %s: %v", clk.Now().Sub(ln.LastEventAt), err)
		}
	}
	if len(drv.Steers) != 0 || len(drv.PhaseStarts) != 0 || len(drv.Spawns) != 1 {
		t.Fatalf("before phase limit: steers=%d phase starts=%d spawns=%d, want 0, 0, 1", len(drv.Steers), len(drv.PhaseStarts), len(drv.Spawns))
	}
	clk.Advance(2 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("timeout nudge: %v", err)
	}
	if len(drv.Steers) != 1 || len(drv.PhaseStarts) != 0 {
		t.Fatalf("at phase limit: steers=%d starts=%d", len(drv.Steers), len(drv.PhaseStarts))
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("timeout fresh terminal: %v", err)
	}
	if len(drv.PhaseStarts) != 1 || len(drv.Stops) != 1 || drv.Stops[0].TerminalID != ln.TerminalID {
		t.Fatalf("after phase limit: starts=%d stops=%d", len(drv.PhaseStarts), len(drv.Stops))
	}
}

func TestControllerTurnEndedRestartLadderStopsOldTerminals(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	saveRecipeWithTimeout(t, st, 20*time.Minute)
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-ladder", RunID: "run-1", RecipeName: "stall-test", Status: lane.StatusPlanned}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "lane-ladder")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}
	old := ln.TerminalID
	drv.AgentStates[old] = "working"
	appendLaneWatchEvent(t, st, ln.ID, old, lane.EventTurnEndedNoReport)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("first ended turn: %v", err)
	}
	if len(drv.Steers) != 1 || len(drv.PhaseStarts) != 0 {
		t.Fatalf("first rung: steers=%d starts=%d", len(drv.Steers), len(drv.PhaseStarts))
	}
	appendLaneWatchEvent(t, st, ln.ID, old, lane.EventTurnEndedNoReport)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("second ended turn: %v", err)
	}
	if len(drv.PhaseStarts) != 1 || len(drv.Stops) != 1 || drv.Stops[0].TerminalID != old {
		t.Fatalf("fresh rung: starts=%d stops=%+v", len(drv.PhaseStarts), drv.Stops)
	}
	if _, err := st.Lanes().Get(ctx, "run-1", ln.ID); err != nil {
		t.Fatalf("lane after fresh terminal: %v", err)
	}

	// Exhausting the fallback chain still stops the replacement terminal.
	cur, _ := st.Lanes().Get(ctx, "run-1", ln.ID)
	appendLaneWatchEvent(t, st, ln.ID, cur.TerminalID, lane.EventTurnEndedNoReport)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("fallback ended turn: %v", err)
	}
	if len(drv.Stops) < 2 || drv.Stops[1].TerminalID != cur.TerminalID {
		t.Fatalf("fallback stops=%+v, want replacement terminal stopped", drv.Stops)
	}
}

func TestControllerStallTimeoutOverridesRecipe(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cfg, recipe time.Duration
		stalled     bool
	}{
		{name: "shorter", cfg: 5 * time.Minute, recipe: 20 * time.Minute, stalled: true},
		{name: "longer", cfg: 30 * time.Minute, recipe: 20 * time.Minute, stalled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk, st, drv, judge := setupTestEnv(t)
			ctx := t.Context()
			saveRecipeWithTimeout(t, st, tc.recipe)
			if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-override", RunID: "run-1", RecipeName: "stall-test", Status: lane.StatusPlanned}); err != nil {
				t.Fatal(err)
			}
			ctrl := controller.New(controller.Config{RunID: "run-1", StallTimeout: tc.cfg, StallTimeoutSet: true}, st, drv, clk, judge)
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			clk.Advance(6 * time.Minute)
			if tc.stalled == false {
				clk.Advance(20 * time.Minute)
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if (len(drv.Stops) > 0) != tc.stalled {
				t.Fatalf("stops=%d, stalled=%v", len(drv.Stops), tc.stalled)
			}
		})
	}
}

func TestControllerReportOnlyCompletionSignals(t *testing.T) {
	t.Parallel()
	for _, outcome := range []lane.ReportOutcome{lane.OutcomeDone, lane.OutcomeNoChange} {
		for _, same := range []bool{true, false} {
			t.Run(string(outcome)+"/"+map[bool]string{true: "same", false: "changed"}[same], func(t *testing.T) {
				t.Parallel()
				clk, st, drv, judge := setupTestEnv(t)
				ctx := t.Context()
				base := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				drv.ReportOnlyBranchExists = !same
				drv.ReportOnlyCommitsAhead = map[bool]int{true: 0, false: 1}[same]
				if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-report", RunID: "run-1", Status: lane.StatusPlanned, Branch: "lane-report"}); err != nil {
					t.Fatal(err)
				}
				ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
				if err := ctrl.Step(ctx); err != nil {
					t.Fatal(err)
				}
				// Later spawn metadata must not replace the base persisted for this session.
				drv.SpawnBaseCommit = strings.Repeat("c", 40)
				head := base
				if !same {
					head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				}
				appendReportEvent(t, st, "lane-report", lane.PhaseReport{Phase: recipe.PhaseSWE, Outcome: outcome, HeadSHA: head})
				if err := ctrl.Step(ctx); err != nil {
					t.Fatal(err)
				}
				ln, err := st.Lanes().Get(ctx, "run-1", "lane-report")
				if err != nil {
					t.Fatal(err)
				}
				if outcome == lane.OutcomeNoChange && same {
					if ln.Status != lane.StatusDone || len(drv.PhaseStarts) != 0 {
						t.Fatalf("no-change unchanged lane=%+v starts=%d", ln, len(drv.PhaseStarts))
					}
				} else if outcome == lane.OutcomeNoChange && !same {
					if len(drv.PhaseStarts) != 1 || ln.Phase != recipe.PhaseClean {
						t.Fatalf("no-change changed starts=%d, want clean", len(drv.PhaseStarts))
					}
				} else if same && (ln.Status != lane.StatusDone || len(drv.PhaseStarts) != 0) {
					t.Fatalf("same-base lane=%+v starts=%d", ln, len(drv.PhaseStarts))
				} else if !same && (len(drv.PhaseStarts) != 1 || ln.Phase != recipe.PhaseClean) {
					t.Fatalf("changed-base starts=%d, want clean", len(drv.PhaseStarts))
				}
			})
		}
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
	d.assertDecisionLogged(controller.ToolPhaseStart+".intent", req.LaneID)
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
	if last.Tool != controller.ToolPhaseAdvance+".intent" && last.Tool != controller.ToolPhaseRework+".intent" && last.Tool != controller.ToolPhaseRetry+".intent" && last.Tool != "phase.launch.intent" {
		d.t.Fatalf("last decision before StartPhaseTerminal = %q, want phase.advance/rework/retry", last.Tool)
	}
	return d.FakeDriver.StartPhaseTerminal(ctx, req)
}

func (d *writeAheadCheckDriver) Stop(ctx context.Context, req lane.StopRequest) error {
	d.assertDecisionLogged(controller.ToolLaneStall, req.LaneID)
	return d.FakeDriver.Stop(ctx, req)
}

func (d *writeAheadCheckDriver) Steer(ctx context.Context, req lane.SteerRequest) error {
	d.assertDecisionLogged(controller.ToolOwnerAskDefault+".intent", req.LaneID)
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

type effectDriver struct {
	*lane.FakeDriver
	afterStart func()
	failSteer  bool
	failStart  bool
}

func (d *effectDriver) Steer(ctx context.Context, req lane.SteerRequest) error {
	if d.failSteer {
		d.failSteer = false
		return errors.New("steer failed")
	}
	return d.FakeDriver.Steer(ctx, req)
}
func (d *effectDriver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	if d.failStart {
		d.failStart = false
		return lane.StartPhaseResult{}, errors.New("start failed")
	}
	res, err := d.FakeDriver.StartPhaseTerminal(ctx, req)
	if d.afterStart != nil {
		d.afterStart()
	}
	return res, err
}
func mustStep(t *testing.T, ctrl *controller.Controller) {
	t.Helper()
	if err := ctrl.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func seedLane(t *testing.T, st store.Store) {
	t.Helper()
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{ID: "worker", RunID: "run-1", Status: lane.StatusPlanned}); err != nil {
		t.Fatal(err)
	}
}
func quiet(t *testing.T, st store.Store) {
	t.Helper()
	if _, err := st.Events().Append(t.Context(), store.Event{RunID: "run-1", LaneID: "worker", Kind: store.EventPhaseExit, Source: "lane.watch", Payload: json.RawMessage(`{"kind":"no_activity","error":"quiet"}`)}); err != nil {
		t.Fatal(err)
	}
}
func TestRestartFailedEffectsRetry(t *testing.T) {
	for _, rung := range []string{"nudge", "fresh", "fallback"} {
		t.Run(rung, func(t *testing.T) {
			clk, st, base, judge := setupTestEnv(t)
			d := &effectDriver{FakeDriver: base}
			seedLane(t, st)
			ctrl := controller.New(controller.Config{RunID: "run-1"}, st, d, clk, judge)
			mustStep(t, ctrl)
			for _, prior := range []string{"nudge", "fresh"} {
				if prior == rung {
					break
				}
				quiet(t, st)
				mustStep(t, ctrl)
			}
			if rung == "nudge" {
				d.failSteer = true
			} else {
				d.failStart = true
			}
			quiet(t, st)
			if err := ctrl.Step(t.Context()); err == nil {
				t.Fatal("expected injected failure")
			}
			ctrl.Kill()
			clk.Advance(controller.DefaultLeaseTTL + time.Second)
			ctrl = controller.New(controller.Config{RunID: "run-1", HolderID: "replacement"}, st, d, clk, judge)
			mustStep(t, ctrl)
			wantStarts := map[string]int{"nudge": 0, "fresh": 1, "fallback": 2}[rung]
			if len(base.Steers) != 1 || len(base.PhaseStarts) != wantStarts {
				t.Fatalf("failed %s skipped: steers=%d starts=%d want 1/%d", rung, len(base.Steers), len(base.PhaseStarts), wantStarts)
			}
		})
	}
}

type questionBox struct {
	asks int
}

func (b *questionBox) AskOwner(context.Context, mailbox.Question) (string, error) {
	b.asks++
	return "owner-ask-1", nil
}
func (*questionBox) SendLane(context.Context, string, string) error { return nil }
func (*questionBox) Watch(context.Context) (<-chan store.Event, error) {
	return make(chan store.Event), nil
}
func mailEvent(t *testing.T, st store.Store, msg mailbox.Message) {
	t.Helper()
	if _, err := mailbox.AppendEvent(t.Context(), st.Events(), "run-1", msg); err != nil {
		t.Fatal(err)
	}
}
func TestWorkerOwnerReplyAndDeadline(t *testing.T) {
	for _, resolution := range []string{"reply", "deadline"} {
		t.Run(resolution, func(t *testing.T) {
			clk, st, drv, judge := setupTestEnv(t)
			seedLane(t, st)
			box := &questionBox{}
			ctrl := controller.New(controller.Config{RunID: "run-1", StallTimeout: time.Minute, StallTimeoutSet: true}, st, drv, clk, judge, box)
			mustStep(t, ctrl)
			mailEvent(t, st, mailbox.Message{LaneID: "worker", Question: true, Body: "Which irreversible option?"})
			mustStep(t, ctrl)
			if box.asks != 1 || len(drv.Steers) != 1 || !strings.Contains(drv.Steers[0].Message, "default") {
				t.Fatal("missing ask/default steer")
			}
			answer := "Use PostgreSQL for storage."
			if resolution == "reply" {
				mailEvent(t, st, mailbox.Message{Owner: true, InReplyTo: "owner-ask-1", Body: answer})
			} else {
				clk.Advance(time.Minute)
				answer = "continue with task notes"
			}
			mustStep(t, ctrl)
			if len(drv.Steers) != 2 || drv.Steers[1].Message != answer {
				t.Fatalf("resolution not steered: %+v", drv.Steers)
			}
			notes, err := st.Notes().List(t.Context(), store.NoteFilter{RunID: "run-1"})
			if err != nil || len(notes) != 1 || !strings.Contains(notes[0].Text, answer) {
				t.Fatalf("answer notes: %+v %v", notes, err)
			}
			clk.Advance(time.Minute)
			mustStep(t, ctrl)
			if len(drv.Steers) != 2 {
				t.Fatal("answer applied twice")
			}
		})
	}
}
func TestWorkerQuestionAnsweredFromNotesWithoutOwner(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	if _, err := st.Notes().Append(t.Context(), store.RunNote{RunID: "run-1", TaskID: "worker", Text: "Use the repository merge queue."}); err != nil {
		t.Fatal(err)
	}
	box := &questionBox{}
	ctrl := controller.New(controller.Config{RunID: "run-1", AnswerQuestion: func(_ context.Context, question, notes string) (string, error) {
		if question != "Which merge path?" || !strings.Contains(notes, "repository merge queue") {
			t.Fatalf("dispatcher context: %q %q", question, notes)
		}
		return "Use the repository merge queue.", nil
	}}, st, drv, clk, judge, box)
	mustStep(t, ctrl)
	for range 2 {
		mailEvent(t, st, mailbox.Message{LaneID: "worker", Question: true, Body: "Which merge path?"})
		mustStep(t, ctrl)
	}
	if box.asks != 0 || len(drv.Steers) != 2 || !strings.Contains(drv.Steers[0].Message, "repository merge queue") {
		t.Fatalf("asks=%d steers=%+v", box.asks, drv.Steers)
	}
}

func TestReportNotesIngestionRetry(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	mustStep(t, ctrl)
	ev := appendReportEvent(t, st, "worker", lane.PhaseReport{Phase: "swe", Outcome: lane.OutcomeDone, Summary: "Implementation rationale"})
	mustStep(t, ctrl)
	notes, err := st.Notes().List(t.Context(), store.NoteFilter{RunID: "run-1", TaskID: "worker"})
	if err != nil || len(notes) != 1 || notes[0].ID != "report-"+ev.ID {
		t.Fatalf("report note must be idempotently identified: %+v %v", notes, err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("condition timed out")
		default:
			runtime.Gosched()
		}
	}
}

func TestIntegrateIntentSurvivesCrash(t *testing.T) {
	clk, st, base, judge := setupTestEnv(t)
	seedLane(t, st)
	ln, _ := st.Lanes().Get(t.Context(), "run-1", "worker")
	ln.Status = lane.StatusRunning
	ln.Phase = recipe.PhaseReview
	ln.Round = 1
	ln.SessionID = "session"
	ln.TerminalID = "review-terminal"
	ln.PRHead = "reviewed-head"
	if err := st.Lanes().Upsert(t.Context(), *ln); err != nil {
		t.Fatal(err)
	}
	d := &effectDriver{FakeDriver: base}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, d, clk, judge)
	d.afterStart = func() { panic("controller crash after launch") }
	appendReportEvent(t, st, "worker", lane.PhaseReport{Phase: recipe.PhaseReview, Outcome: lane.OutcomeDone, HeadSHA: "reviewed-head"})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected simulated crash")
			}
		}()
		_ = ctrl.Step(t.Context())
	}()
	ctrl.Kill()
	d.afterStart = nil
	clk.Advance(controller.DefaultLeaseTTL + time.Second)
	ctrl = controller.New(controller.Config{RunID: "run-1", HolderID: "replacement"}, st, d, clk, judge)
	mustStep(t, ctrl)
	got, err := st.Lanes().Get(t.Context(), "run-1", "worker")
	if err != nil || got.Status != lane.StatusBlocked || len(base.PhaseStarts) != 1 {
		t.Fatalf("uncertain integrate must escalate without relaunch: lane=%+v starts=%d err=%v", got, len(base.PhaseStarts), err)
	}
	decs, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	var intent bool
	for _, dec := range decs {
		if dec.Tool == "phase.launch.intent" && strings.Contains(string(dec.Args), "reviewed-head") {
			intent = true
		}
	}
	if !intent {
		t.Fatal("missing durable head-keyed integrate intent")
	}
}

type flakyEvents struct {
	store.EventStore
	failed chan struct{}
}

func (s *flakyEvents) Append(ctx context.Context, ev store.Event) (store.Event, error) {
	if ev.Source == "lane.watch" {
		select {
		case s.failed <- struct{}{}:
			return store.Event{}, errors.New("durable append unavailable")
		default:
		}
	}
	return s.EventStore.Append(ctx, ev)
}

type eventStoreWrapper struct {
	store.Store
	events store.EventStore
}

func (s eventStoreWrapper) Events() store.EventStore { return s.events }
func TestWatchAppendFailureRetries(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	fail := &flakyEvents{EventStore: st.Events(), failed: make(chan struct{}, 1)}
	ctrl := controller.New(controller.Config{RunID: "run-1", PollInterval: time.Second}, eventStoreWrapper{Store: st, events: fail}, drv, clk, judge)
	if err := ctrl.StartWatches(t.Context()); err != nil {
		t.Fatal(err)
	}
	drv.Emit(lane.Event{RunID: "run-1", LaneID: "worker", Kind: lane.EventNoActivity, Text: "quiet"})
	waitFor(t, func() bool { return len(fail.failed) == 1 })
	select {
	case err := <-ctrl.WatchErrors():
		if !strings.Contains(err.Error(), "durable append unavailable") {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("append failure was not surfaced")
	}
	waitFor(t, func() bool { return clk.Waiters() > 0 })
	clk.Advance(time.Second)
	waitFor(t, func() bool {
		events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "run-1"})
		return len(events) == 1
	})
}

func TestNamedWaitsCompleteAndExpireAcrossRestart(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint("complete_", complete), func(t *testing.T) {
			clk, st, drv, judge := setupTestEnv(t)
			ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: -1}, st, drv, clk, judge)
			type waiter interface {
				Wait(context.Context, string, string, time.Time) error
				CompleteWait(context.Context, string) error
			}
			w, ok := any(ctrl).(waiter)
			if !ok {
				t.Fatal("controller has no durable named waits")
			}
			if err := w.Wait(t.Context(), "repo merge queue", "", clk.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			mustStep(t, ctrl)
			if judge.BatchCount() != 0 {
				t.Fatal("wait expired before deadline")
			}
			if complete {
				if err := w.CompleteWait(t.Context(), "repo merge queue"); err != nil {
					t.Fatal(err)
				}
			}
			ctrl.Kill()
			clk.Advance(time.Minute)
			ctrl = controller.New(controller.Config{RunID: "run-1", HolderID: "replacement", JudgmentDebounce: -1}, st, drv, clk, judge)
			mustStep(t, ctrl)
			want := 1
			if complete {
				want = 0
			}
			if judge.BatchCount() != want {
				t.Fatalf("judgments=%d want %d", judge.BatchCount(), want)
			}
			mustStep(t, ctrl)
			if judge.BatchCount() != want {
				t.Fatal("wait deadline emitted twice")
			}
		})
	}
}

func TestWorkerQuestionDoesNotDumpUnrelatedTask(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	for _, text := range []string{"Use the repository merge queue.", "The UI background is purple."} {
		if _, err := st.Notes().Append(t.Context(), store.RunNote{RunID: "run-1", TaskID: "worker", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	box := &questionBox{}
	ctrl := controller.New(controller.Config{RunID: "run-1", AnswerQuestion: func(_ context.Context, question, notes string) (string, error) {
		return "Use the repository merge queue.", nil
	}}, st, drv, clk, judge, box)
	mustStep(t, ctrl)
	for range 2 {
		mailEvent(t, st, mailbox.Message{LaneID: "worker", Question: true, Body: "Which merge path?"})
		mustStep(t, ctrl)
	}
	if box.asks != 0 || len(drv.Steers) != 2 || strings.Contains(drv.Steers[0].Message, "purple") {
		t.Fatalf("question context routing: asks=%d steers=%+v", box.asks, drv.Steers)
	}
}

func TestTransportEnvelopeWaitsForWatch(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: -1}, st, drv, clk, judge)
	mustStep(t, ctrl)
	if _, err := st.Events().Append(t.Context(), store.Event{RunID: "run-1", LaneID: "worker", Kind: store.EventPhaseExit, Payload: json.RawMessage(`{"terminal_event":{"type":"turn.ended"}}`)}); err != nil {
		t.Fatal(err)
	}
	mustStep(t, ctrl)
	if len(drv.PhaseStarts) != 0 || judge.BatchCount() != 0 {
		t.Fatal("transport event bypassed authoritative Watch")
	}
}

func TestWorkerAskSteerFailureDoesNotRepeatAsk(t *testing.T) {
	clk, st, base, judge := setupTestEnv(t)
	d := &effectDriver{FakeDriver: base}
	seedLane(t, st)
	box := &questionBox{}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, d, clk, judge, box)
	mustStep(t, ctrl)
	d.failSteer = true
	mailEvent(t, st, mailbox.Message{LaneID: "worker", Question: true, Body: "Which irreversible option?"})
	if err := ctrl.Step(t.Context()); err == nil {
		t.Fatal("expected steer failure")
	}
	mustStep(t, ctrl)
	if box.asks != 1 || len(base.Steers) != 1 {
		t.Fatalf("asks=%d steers=%+v", box.asks, base.Steers)
	}
	mailEvent(t, st, mailbox.Message{Owner: true, InReplyTo: "owner-ask-1", Body: "pause"})
	d.failSteer = true
	if err := ctrl.Step(t.Context()); err == nil {
		t.Fatal("expected reply steer failure")
	}
	mustStep(t, ctrl)
	notes, err := st.Notes().List(t.Context(), store.NoteFilter{RunID: "run-1"})
	if err != nil || len(notes) != 1 || len(base.Steers) != 2 {
		t.Fatalf("reply retry: notes=%+v steers=%+v err=%v", notes, base.Steers, err)
	}
}

type failingNoteStore struct {
	store.NoteStore
	fail bool
}

func (s *failingNoteStore) Append(ctx context.Context, n store.RunNote) (store.RunNote, error) {
	if s.fail {
		s.fail = false
		return store.RunNote{}, errors.New("notes unavailable")
	}
	return s.NoteStore.Append(ctx, n)
}

type noteStoreWrapper struct {
	store.Store
	notes store.NoteStore
}

func (s noteStoreWrapper) Notes() store.NoteStore { return s.notes }
func TestReportNotesFailureRetriesBeforeTransition(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	notes := &failingNoteStore{NoteStore: st.Notes(), fail: true}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, noteStoreWrapper{Store: st, notes: notes}, drv, clk, judge)
	mustStep(t, ctrl)
	appendReportEvent(t, st, "worker", lane.PhaseReport{Phase: "swe", Outcome: lane.OutcomeDone, Summary: "Durable phase reasoning"})
	if err := ctrl.Step(t.Context()); err == nil {
		t.Fatal("notes error was dropped")
	}
	if len(drv.PhaseStarts) != 0 {
		t.Fatal("phase advanced before notes were durable")
	}
	mustStep(t, ctrl)
	got, err := st.Notes().List(t.Context(), store.NoteFilter{RunID: "run-1"})
	if err != nil || len(got) != 1 || len(drv.PhaseStarts) != 1 {
		t.Fatalf("notes retry: notes=%+v starts=%d err=%v", got, len(drv.PhaseStarts), err)
	}
}

type startupBox struct {
	connected chan struct{}
	questionBox
	calls int
}

func (b *startupBox) Watch(context.Context) (<-chan store.Event, error) {
	b.calls++
	if b.calls == 1 {
		return nil, errors.New("mailbox startup failed")
	}
	if b.connected != nil {
		close(b.connected)
	}
	return make(chan store.Event), nil
}

type watchContextDriver struct {
	*lane.FakeDriver
	watchCtx context.Context
}

func (d *watchContextDriver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	d.watchCtx = ctx
	return d.FakeDriver.Watch(ctx, runID)
}
func TestWatchStartupRetryAndKill(t *testing.T) {
	clk, st, base, judge := setupTestEnv(t)
	box := &startupBox{connected: make(chan struct{})}
	drv := &watchContextDriver{FakeDriver: base}
	ctrl := controller.New(controller.Config{RunID: "run-1", PollInterval: time.Second}, st, drv, clk, judge, box)
	if err := ctrl.StartWatches(t.Context()); err != nil {
		t.Fatalf("transient startup failure terminated watches: %v", err)
	}
	if drv.watchCtx == nil {
		t.Fatal("mailbox failure prevented lane Watch startup")
	}
	waitFor(t, func() bool { return clk.Waiters() > 0 })
	clk.Advance(time.Second)
	select {
	case <-box.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("initial mailbox Watch was not retried")
	}
	if box.calls != 2 {
		t.Fatalf("Watch calls=%d, want 2", box.calls)
	}
	ctrl.Kill()
	select {
	case <-drv.watchCtx.Done():
	default:
		t.Fatal("Kill left driver Watch alive")
	}
}

func TestWatchRunQuietTargetsLiveQuietLanes(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{ID: "warm", RunID: "run-1", Status: lane.StatusPlanned}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	mustStep(t, ctrl)
	if err := ctrl.StartWatches(t.Context()); err != nil {
		t.Fatal(err)
	}
	since := clk.Now()
	clk.Advance(30 * time.Second)
	warm, _ := st.Lanes().Get(t.Context(), "run-1", "warm")
	warm.LastEventAt = clk.Now()
	if err := st.Lanes().Upsert(t.Context(), *warm); err != nil {
		t.Fatal(err)
	}
	clk.Advance(30 * time.Second)
	drv.Emit(lane.Event{RunID: "run-1", Kind: lane.EventNoActivity, Since: since})
	waitFor(t, func() bool {
		events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "run-1"})
		return len(events) > 0
	})
	mustStep(t, ctrl)
	if len(drv.Steers) != 1 || drv.Steers[0].LaneID != "worker" {
		t.Fatalf("run quiet did not nudge its quiet lane: %+v", drv.Steers)
	}
}

func TestWatchReplayKeepsProducerIdentityAcrossRestart(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	mustStep(t, ctrl)
	if err := ctrl.StartWatches(t.Context()); err != nil {
		t.Fatal(err)
	}
	ln, _ := st.Lanes().Get(t.Context(), "run-1", "worker")
	ev := lane.Event{RunID: "run-1", LaneID: "worker", TerminalID: ln.TerminalID, Kind: lane.EventNoActivity, At: clk.Now(), Text: "quiet"}
	drv.Emit(ev)
	waitFor(t, func() bool {
		events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "run-1"})
		return len(events) == 1
	})
	mustStep(t, ctrl)
	ctrl.Kill()
	clk.Advance(controller.DefaultLeaseTTL + time.Second)
	ctrl = controller.New(controller.Config{RunID: "run-1", HolderID: "replacement"}, st, drv, clk, judge)
	if err := ctrl.StartWatches(t.Context()); err != nil {
		t.Fatal(err)
	}
	drv.Emit(ev)
	waitFor(t, func() bool {
		events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "run-1"})
		return len(events) == 2
	})
	mustStep(t, ctrl)
	if len(drv.Steers) != 1 || len(drv.PhaseStarts) != 0 {
		t.Fatalf("replayed nudge advanced ladder: steers=%+v starts=%+v", drv.Steers, drv.PhaseStarts)
	}
}

func TestWatchFailuresSkipQuietNudge(t *testing.T) {
	t.Parallel()
	for _, kind := range []lane.EventKind{lane.EventExited, lane.EventAuthFailure, lane.EventQuotaFailure} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			clk, st, drv, judge := setupTestEnv(t)
			seedLane(t, st)
			ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
			mustStep(t, ctrl)
			if err := ctrl.StartWatches(t.Context()); err != nil {
				t.Fatal(err)
			}
			ln, _ := st.Lanes().Get(t.Context(), "run-1", "worker")
			drv.Emit(lane.Event{RunID: "run-1", LaneID: "worker", TerminalID: ln.TerminalID, Kind: kind, Text: "terminal failed", At: clk.Now()})
			waitFor(t, func() bool {
				events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "run-1"})
				return len(events) == 1
			})
			mustStep(t, ctrl)
			if len(drv.Steers) != 0 || len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Model != "gpt-6.1-sol" {
				t.Fatalf("failed terminal entered quiet ladder: steers=%+v starts=%+v", drv.Steers, drv.PhaseStarts)
			}
		})
	}
}

func TestWatchQuietHandlingDoesNotCountAsActivity(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	seedLane(t, st)
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	mustStep(t, ctrl)
	if err := ctrl.StartWatches(t.Context()); err != nil {
		t.Fatal(err)
	}
	since := clk.Now()
	for step := 1; step <= 2; step++ {
		clk.Advance(time.Minute)
		emitted := clk.Now()
		drv.Emit(lane.Event{RunID: "run-1", Kind: lane.EventNoActivity, Since: since, At: emitted})
		waitFor(t, func() bool {
			events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "run-1"})
			return len(events) == step
		})
		// Handling occurs after emission, as it does with a real clock.
		clk.Advance(time.Second)
		mustStep(t, ctrl)
		since = emitted
	}
	if len(drv.Steers) != 1 || len(drv.PhaseStarts) != 1 {
		t.Fatalf("quiet bookkeeping delayed fresh terminal: steers=%+v starts=%+v", drv.Steers, drv.PhaseStarts)
	}
}
