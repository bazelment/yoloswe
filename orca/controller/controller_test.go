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
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	"github.com/bazelment/yoloswe/orca/model"
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

type lifecycleFailDriver struct {
	*lane.FakeDriver
	releaseFailures  int
	stopFailures     int
	agentStateErrors int
}

func (d *lifecycleFailDriver) ReleaseSession(ctx context.Context, id string, force ...bool) error {
	if d.releaseFailures > 0 {
		d.releaseFailures--
		return errors.New("release failed")
	}
	return d.FakeDriver.ReleaseSession(ctx, id, force...)
}
func (d *lifecycleFailDriver) Stop(ctx context.Context, req lane.StopRequest) error {
	if d.stopFailures > 0 {
		d.stopFailures--
		d.FakeDriver.Stops = append(d.FakeDriver.Stops, req)
		return errors.New("stop failed")
	}
	return d.FakeDriver.Stop(ctx, req)
}
func (d *lifecycleFailDriver) TerminalAgentState(ctx context.Context, terminalID string) (string, error) {
	if d.agentStateErrors > 0 {
		d.agentStateErrors--
		return "", errors.New("transient terminal state error")
	}
	return d.FakeDriver.TerminalAgentState(ctx, terminalID)
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

func TestController_RuntimeSettingsLimitNextTickSpawns(t *testing.T) {
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("lane-%d", i)
		if err := st.Lanes().Upsert(ctx, lane.Lane{ID: id, RunID: "run-1", ItemID: id, Title: id, Status: lane.StatusPlanned, RecipeName: recipe.DefaultName}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Runs().UpdateSettings(ctx, "run-1", store.RunSettings{ConcurrencyCap: 2, StallTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	c := controller.New(controller.Config{RunID: "run-1", ConcurrencyCap: 8}, st, drv, clk, judge)
	c.SetRuntimeSettings(store.RunSettings{ConcurrencyCap: 2, StallTimeout: time.Hour})
	if err := c.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(drv.Spawns); got > 2 {
		t.Fatalf("spawns=%d, want at most 2", got)
	}
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

func TestControllerWaitingForInputIsAQuestion(t *testing.T) {
	t.Parallel()
	tests := []struct { //nolint:govet // table readability is more useful than field packing here
		name        string
		text        string
		wantBatch   bool
		wantSummary string
	}{
		{name: "prose question", text: "### Needed decision\n1. Fix sim restore\n2. Narrow the leader release", wantBatch: true, wantSummary: "lane lane-wait waiting for input: ### Needed decision\n1. Fix sim restore\n2. Narrow the leader release"},
		{name: "empty screen", wantBatch: true, wantSummary: "lane lane-wait waiting for input: (no screen text)"},
		{name: "ANSI screen", text: "\x1b]0;terminal title\a\x1b[31mChoose\x1b[0m\n\x1b]8;;https://example.test\x1b\\1. Restore\x1b]8;;\x1b\\\x1b(B", wantBatch: true, wantSummary: "lane lane-wait waiting for input: Choose\n1. Restore"},
		{name: "agy timer remains working", text: "30m timer running ... 1 task(s)", wantBatch: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk, st, drv, judge := setupTestEnv(t)
			ctx := t.Context()
			if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-wait", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE, Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: -1}, st, drv, clk, judge)
			raw, err := json.Marshal(controller.PhaseExitPayload{Kind: lane.EventWaitingForInput, TerminalID: "t-1", Phase: recipe.PhaseSWE, Error: tc.text})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: "lane-wait", Kind: store.EventPhaseExit, Source: "lane.watch", Payload: raw}); err != nil {
				t.Fatal(err)
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatalf("Step: %v", err)
			}
			t.Logf("%s decisions: batches=%d starts=%d stops=%d", tc.name, judge.BatchCount(), len(drv.PhaseStarts), len(drv.Stops))
			if got := judge.BatchCount(); (got == 1) != tc.wantBatch {
				t.Fatalf("judgment batches=%d, want question=%t", got, tc.wantBatch)
			}
			if len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
				t.Fatalf("waiting input changed terminal: starts=%+v stops=%+v", drv.PhaseStarts, drv.Stops)
			}
			if tc.wantSummary != "" {
				if len(judge.Batches) != 1 || len(judge.Batches[0]) != 1 || judge.Batches[0][0].Summary != tc.wantSummary || !judge.Batches[0][0].Urgent {
					t.Fatalf("judgment event=%+v, want urgent summary %q", judge.Batches, tc.wantSummary)
				}
			}
		})
	}

	t.Run("same question dedupes and steer releases quiet ladder", func(t *testing.T) {
		clk, st, drv, judge := setupTestEnv(t)
		ctx := t.Context()
		if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-wait", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: -1}, st, drv, clk, judge)
		appendWaiting := func(text string) {
			raw, _ := json.Marshal(controller.PhaseExitPayload{Kind: lane.EventWaitingForInput, TerminalID: "t-1", Phase: recipe.PhaseSWE, Error: text})
			if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: "lane-wait", Kind: store.EventPhaseExit, Source: "lane.watch", Payload: raw}); err != nil {
				t.Fatal(err)
			}
		}
		appendWaiting("choose restore or narrow")
		if err := ctrl.Step(ctx); err != nil {
			t.Fatal(err)
		}
		appendWaiting("choose restore or narrow")
		if err := ctrl.Step(ctx); err != nil {
			t.Fatal(err)
		}
		if judge.BatchCount() != 1 {
			t.Fatalf("same question batches=%d, want 1", judge.BatchCount())
		}
		appendWaiting("choose a different recovery path")
		if err := ctrl.Step(ctx); err != nil {
			t.Fatal(err)
		}
		if judge.BatchCount() != 2 {
			t.Fatalf("changed question batches=%d, want 2", judge.BatchCount())
		}
		ln, err := st.Lanes().Get(ctx, "run-1", "lane-wait")
		if err != nil {
			t.Fatal(err)
		}
		if ln.Status != lane.StatusRunning || ln.TerminalID != "t-1" || ln.AwaitingInputSince.IsZero() {
			t.Fatalf("waiting lane=%+v, want running with durable marker", ln)
		}
		client := model.NewFakeClient(
			model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("answer", dispatcher.ToolLaneSteer, json.RawMessage(`{"lane_id":"lane-wait","note":"Fix sim restore"}`)))},
			model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Answered the lane."))},
		)
		d := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
		t.Cleanup(func() { _ = d.Close() })
		if _, err := d.Dispatch(ctx, "run-1", judge.Batches[1]); err != nil {
			t.Fatal(err)
		}
		appendWaiting("choose a different recovery path")
		if err := ctrl.Step(ctx); err != nil {
			t.Fatal(err)
		}
		ln, err = st.Lanes().Get(ctx, "run-1", "lane-wait")
		if err != nil {
			t.Fatal(err)
		}
		if !ln.AwaitingInputSince.IsZero() || judge.BatchCount() != 2 {
			t.Fatalf("answered duplicate restored wait: lane=%+v batches=%d", ln, judge.BatchCount())
		}
		raw, _ := json.Marshal(controller.PhaseExitPayload{Kind: lane.EventNoActivity, TerminalID: "t-1", Phase: recipe.PhaseSWE, Error: "quiet"})
		if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: "lane-wait", Kind: store.EventPhaseExit, Source: "lane.watch", Payload: raw}); err != nil {
			t.Fatal(err)
		}
		if err := ctrl.Step(ctx); err != nil {
			t.Fatal(err)
		}
		t.Logf("dedupe and post-steer decisions: batches=%d steers=%d starts=%d stops=%d", judge.BatchCount(), len(drv.Steers), len(drv.PhaseStarts), len(drv.Stops))
		if len(drv.Steers) != 2 || len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
			t.Fatalf("post-steer quiet pulse actions: steers=%+v starts=%+v stops=%+v", drv.Steers, drv.PhaseStarts, drv.Stops)
		}
	})

	t.Run("phase report clears marker and advances", func(t *testing.T) {
		clk, st, drv, judge := setupTestEnv(t)
		ctx := t.Context()
		if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-wait", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: -1}, st, drv, clk, judge)
		rawWaiting, _ := json.Marshal(controller.PhaseExitPayload{Kind: lane.EventWaitingForInput, TerminalID: "t-1", Phase: recipe.PhaseSWE, Error: "choose restore or narrow"})
		if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: "lane-wait", Kind: store.EventPhaseExit, Source: "lane.watch", Payload: rawWaiting}); err != nil {
			t.Fatal(err)
		}
		if err := ctrl.Step(ctx); err != nil {
			t.Fatal(err)
		}
		appendReportEvent(t, st, "lane-wait", lane.PhaseReport{Phase: recipe.PhaseSWE, Outcome: lane.OutcomeDone, HeadSHA: "sha-wait"})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatal(err)
		}
		ln, err := st.Lanes().Get(ctx, "run-1", "lane-wait")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("phase report decisions: batches=%d starts=%d stops=%d lane=%+v", judge.BatchCount(), len(drv.PhaseStarts), len(drv.Stops), ln)
		if !ln.AwaitingInputSince.IsZero() || len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Phase != recipe.PhaseClean {
			t.Fatalf("phase report did not clear/advance: lane=%+v starts=%+v", ln, drv.PhaseStarts)
		}
	})
}

func TestControllerWaitingForInputLiveness(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"idle", "working", "busy", "state read failed"} {
		t.Run(state, func(t *testing.T) {
			clk, st, drv, judge := setupTestEnv(t)
			ctx := t.Context()
			start := clk.Now()
			ln := lane.Lane{ID: "lane-wait", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE, Attempt: 1, LastEventAt: start, AwaitingInputSince: start}
			if err := st.Lanes().Upsert(ctx, ln); err != nil {
				t.Fatal(err)
			}
			drv.AgentStates["t-1"] = state
			failing := &lifecycleFailDriver{FakeDriver: drv}
			if state == "state read failed" {
				failing.agentStateErrors = 100
			}
			ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: -1, StallTimeout: time.Minute}, st, failing, clk, judge)
			clk.Advance(3 * time.Hour)
			for _, kind := range []store.EventKind{store.EventPhaseExit, store.EventStall} {
				raw, _ := json.Marshal(controller.PhaseExitPayload{Kind: lane.EventNoActivity, TerminalID: "t-1", Phase: recipe.PhaseSWE})
				if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: ln.ID, Kind: kind, Source: "lane.watch", Payload: raw}); err != nil {
					t.Fatal(err)
				}
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := st.Lanes().Get(ctx, "run-1", ln.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantCleared := state == "working" || state == "busy"
			t.Logf("%s liveness decisions: awaiting=%s lastActivity=%s batches=%d steers=%d starts=%d stops=%d", state, got.AwaitingInputSince, got.LastEventAt, judge.BatchCount(), len(drv.Steers), len(drv.PhaseStarts), len(drv.Stops))
			if got.AwaitingInputSince.IsZero() != wantCleared || judge.BatchCount() != 0 || len(drv.Steers) != 0 || len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
				t.Fatalf("liveness changed waiting terminal: lane=%+v batches=%d steers=%+v starts=%+v stops=%+v", got, judge.BatchCount(), drv.Steers, drv.PhaseStarts, drv.Stops)
			}
			if !wantCleared && !got.LastEventAt.Equal(start) {
				t.Fatalf("waiting pulse counted as activity: %s, want %s", got.LastEventAt, start)
			}
		})
	}
}

func TestControllerWaitingForInputRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct { //nolint:govet // keep recovery inputs together
		name      string
		status    lane.Status
		terminal  string
		awaiting  bool
		class     store.EventClass
		wantBatch int
	}{
		{name: "pending question", status: lane.StatusRunning, terminal: "t-1", awaiting: true, wantBatch: 1},
		{name: "answered question", status: lane.StatusRunning, terminal: "t-1"},
		{name: "old terminal", status: lane.StatusRunning, terminal: "t-old", awaiting: true},
		{name: "completed lane", status: lane.StatusDone, terminal: "t-1", awaiting: true},
		{name: "auto event", status: lane.StatusRunning, terminal: "t-1", awaiting: true, class: store.EventClassAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk, st, drv, judge := setupTestEnv(t)
			ctx := t.Context()
			ln := lane.Lane{ID: "lane-wait", RunID: "run-1", Status: tc.status, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE, Attempt: 1, LastEventAt: clk.Now()}
			if tc.awaiting {
				ln.AwaitingInputSince = clk.Now()
			}
			if err := st.Lanes().Upsert(ctx, ln); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(controller.PhaseExitPayload{Kind: lane.EventWaitingForInput, TerminalID: tc.terminal, Phase: recipe.PhaseSWE, Error: "Choose restore or narrow"})
			ev, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: ln.ID, Kind: store.EventPhaseExit, Class: tc.class, Source: "lane.watch", Payload: raw})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Events().SetCursor(ctx, "run-1", controller.DefaultConsumer, ev.Seq); err != nil {
				t.Fatal(err)
			}
			ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: 2 * time.Minute}, st, drv, clk, judge)
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s recovery decisions: batches=%d starts=%d stops=%d", tc.name, judge.BatchCount(), len(drv.PhaseStarts), len(drv.Stops))
			if judge.BatchCount() != tc.wantBatch || len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
				t.Fatalf("recovery: batches=%d want=%d starts=%+v stops=%+v", judge.BatchCount(), tc.wantBatch, drv.PhaseStarts, drv.Stops)
			}
		})
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
				if step == 2 {
					clk.Advance(controller.DefaultStallTimeout / 2)
				} else {
					clk.Advance(time.Second)
				}
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
	appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("working past phase limit: %v", err)
	}
	if len(drv.Steers) != 0 || len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
		t.Fatalf("working terminal past phase limit: steers=%d starts=%d stops=%d, want 0, 0, 0", len(drv.Steers), len(drv.PhaseStarts), len(drv.Stops))
	}
	delete(drv.AgentStates, ln.TerminalID)
	appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("idle nudge: %v", err)
	}
	if len(drv.Steers) != 1 || len(drv.PhaseStarts) != 0 {
		t.Fatalf("after idle nudge: steers=%d starts=%d", len(drv.Steers), len(drv.PhaseStarts))
	}
	clk.Advance(10 * time.Minute)
	appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("timeout fresh terminal: %v", err)
	}
	if len(drv.PhaseStarts) != 1 || len(drv.Stops) != 1 || drv.Stops[0].TerminalID != ln.TerminalID {
		t.Fatalf("after phase limit: starts=%d stops=%d", len(drv.PhaseStarts), len(drv.Stops))
	}
}

func TestControllerWorkingTerminalProducesNoNoActivityRowsOverTenPolls(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	saveRecipeWithTimeout(t, st, 20*time.Minute)
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-working", RunID: "run-1", RecipeName: "stall-test", Status: lane.StatusPlanned}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1", StallTimeout: 5 * time.Minute, StallTimeoutSet: true}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := ctrl.StartWatches(ctx); err != nil {
		t.Fatalf("StartWatches: %v", err)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "lane-working")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}
	drv.AgentStates[ln.TerminalID] = "working"

	since := clk.Now()
	for poll := 1; poll <= 10; poll++ {
		clk.Advance(time.Minute)
		drv.Emit(lane.Event{RunID: "run-1", Kind: lane.EventNoActivity, Since: since, At: clk.Now()})
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step poll %d: %v", poll, err)
		}
		since = clk.Now()
	}

	evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run-1", Kinds: []store.EventKind{store.EventPhaseExit}})
	if err != nil {
		t.Fatalf("Events().List: %v", err)
	}
	var noActivityRows int
	for _, ev := range evs {
		var p controller.PhaseExitPayload
		if json.Unmarshal(ev.Payload, &p) == nil && p.Kind == lane.EventNoActivity {
			noActivityRows++
		}
	}
	if noActivityRows != 0 {
		t.Fatalf("working terminal produced %d no_activity rows over 10 polls, want 0", noActivityRows)
	}
	if len(drv.Steers) != 0 || len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
		t.Fatalf("working terminal steered/restarted/stopped: steers=%d starts=%d stops=%d, want 0", len(drv.Steers), len(drv.PhaseStarts), len(drv.Stops))
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

func TestControllerHostRunSWEReportOnlyRequiresJudgment(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	adopted := recipe.Default().Clone()
	adopted.RunID = "run-1"
	adopted.Name = "host-swe"
	adopted.Phases = []recipe.Phase{adopted.Phases[0]}
	adopted.Transitions = []recipe.Transition{{From: recipe.PhaseSWE, To: recipe.TargetComplete, On: recipe.Condition{Verdict: recipe.OutcomeDone}}}
	adopted.MergePolicy.Mode = recipe.MergeModeNone
	if _, err := st.Recipes().Save(ctx, adopted); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-host", RunID: "run-1", Status: lane.StatusPlanned, RecipeName: adopted.Name, BaseCommit: "base-sha", Branch: "lane-host"}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1", HostRun: true, NoPR: true}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	appendReportEvent(t, st, "lane-host", lane.PhaseReport{Phase: recipe.PhaseSWE, Outcome: lane.OutcomeDone, HeadSHA: "base-sha"})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "lane-host")
	if err != nil {
		t.Fatal(err)
	}
	if ln.Status == lane.StatusDone {
		t.Fatalf("host SWE report-only lane completed without a merge judgment: %+v", ln)
	}
}

func TestControllerReleaseFailureKeepsLaneCompleteAndRetries(t *testing.T) {
	clk, st, base, judge := setupTestEnv(t)
	drv := &lifecycleFailDriver{FakeDriver: base, releaseFailures: 1}
	ctx := t.Context()
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-release", RunID: "run-1", Status: lane.StatusPlanned, Branch: "lane-release"}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	appendReportEvent(t, st, "lane-release", lane.PhaseReport{Phase: recipe.PhaseSWE, Outcome: lane.OutcomeNoChange})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ln, _ := st.Lanes().Get(ctx, "run-1", "lane-release")
	if ln.Status != lane.StatusDone || len(base.Releases) != 0 {
		t.Fatalf("lane=%+v releases=%v", ln, base.Releases)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(base.Releases) != 1 {
		t.Fatalf("releases=%v, want retry success", base.Releases)
	}
	decs, _ := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1", LaneID: "lane-release"})
	found := false
	for _, d := range decs {
		if strings.Contains(d.Result, "release retry") {
			found = true
		}
	}
	if !found {
		t.Fatalf("decision log lacks release failure: %+v", decs)
	}
}

func TestControllerStopFailureWithholdsReplacementAndEscalates(t *testing.T) {
	clk, st, base, judge := setupTestEnv(t)
	drv := &lifecycleFailDriver{FakeDriver: base, stopFailures: 3}
	ctx := t.Context()
	saveRecipeWithTimeout(t, st, 20*time.Minute)
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-stop", RunID: "run-1", RecipeName: "stall-test", Status: lane.StatusPlanned}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ln, _ := st.Lanes().Get(ctx, "run-1", "lane-stop")
	old := ln.TerminalID
	base.AgentStates[old] = "working"
	for i := 0; i < 3; i++ {
		appendLaneWatchEvent(t, st, ln.ID, old, lane.EventTurnEndedNoReport)
		_ = ctrl.Step(ctx)
		if len(base.PhaseStarts) != 0 {
			t.Fatalf("replacement started after stop failure: %+v", base.PhaseStarts)
		}
		ln, _ = st.Lanes().Get(ctx, "run-1", ln.ID)
	}
	if ln.Status != lane.StatusBlocked {
		t.Fatalf("status=%q, want blocked after repeated stop failures", ln.Status)
	}
	if len(base.PhaseStarts) != 0 {
		t.Fatalf("phase starts=%v, want none", base.PhaseStarts)
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

	if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", Kind: store.EventMail, Summary: "routine progress update"}); err != nil {
		t.Fatal(err)
	}
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
		judge.Batches[0][0].Kind != store.EventMail ||
		judge.Batches[0][1].Kind != store.EventMail {
		t.Fatalf("Batches[0] = %+v, want [mail, mail]", judge.Batches[0])
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
	d.t.Helper()
	decs, err := d.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:  "run-1",
		LaneID: req.LaneID,
	})
	if err != nil || len(decs) == 0 {
		d.t.Fatalf("Stop executed without prior decision: err=%v decs=%+v", err, decs)
	}
	last := decs[len(decs)-1]
	switch last.Tool {
	case controller.ToolLaneStall,
		controller.ToolPhaseStart + ".intent",
		controller.ToolPhaseAdvance + ".intent",
		controller.ToolPhaseRework + ".intent",
		controller.ToolPhaseRetry + ".intent",
		"phase.launch.intent":
		return d.FakeDriver.Stop(ctx, req)
	}
	d.t.Fatalf("driver Stop on lane %q executed BEFORE Decision was written to DecisionStore (last=%q, decisions=%+v)", req.LaneID, last.Tool, decs)
	return nil
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

func TestControllerRunRetriesTransientTerminalCreateAndKeepsLeaseHeartbeating(t *testing.T) {
	t.Parallel()
	clk, st, base, judge := setupTestEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID: "lane-timeout", RunID: "run-1", Status: lane.StatusRunning,
		Phase: recipe.PhaseReview, Round: 1, Attempt: 1,
		SessionID: "session-timeout", TerminalID: "terminal-review",
		PRHead: "sha-timeout", ApprovalSHA: "sha-timeout", Checks: "green",
	}); err != nil {
		t.Fatal(err)
	}
	appendReportEvent(t, st, "lane-timeout", lane.PhaseReport{
		Phase: recipe.PhaseReview, Outcome: lane.OutcomeDone, HeadSHA: "sha-timeout",
	})
	appendCIEvent(t, st, "lane-timeout", "green", "sha-timeout")
	drv := &effectDriver{FakeDriver: base, failStart: true, failDone: make(chan struct{}), failWait: make(chan struct{}), started: make(chan struct{})}
	ctrl := controller.New(controller.Config{
		RunID: "run-1", HolderID: "controller-retry", LeaseTTL: 30 * time.Second,
		HeartbeatInterval: 10 * time.Second, PollInterval: time.Second,
	}, st, drv, clk, judge)
	done := make(chan error, 1)
	go func() { done <- ctrl.Run(ctx) }()

	// The first integrate terminal create fails once. Wait for that call so the
	// fake clock cannot advance past the retry timer before it is registered.
	select {
	case <-drv.failDone:
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not reach the transient terminal-create failure")
	}
	deadline := time.Now().Add(2 * time.Second)
	for clk.Waiters() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("controller did not register its heartbeat timer")
		}
		runtime.Gosched()
	}
	beforeHeartbeat, err := st.Runs().Get(ctx, "run-1")
	if err != nil || beforeHeartbeat.Lease == nil {
		t.Fatalf("run before blocked retry = %+v, err=%v", beforeHeartbeat, err)
	}
	// Keep the terminal-create call blocked while the independent heartbeat
	// ticker fires. This is the failure mode that previously let the lease
	// expire during a slow controller step.
	clk.Advance(10 * time.Second)
	heartbeatDeadline := time.Now().Add(2 * time.Second)
	for {
		run, getErr := st.Runs().Get(ctx, "run-1")
		if getErr == nil && run.Lease != nil && run.Lease.HeartbeatAt.After(beforeHeartbeat.Lease.HeartbeatAt) {
			break
		}
		if time.Now().After(heartbeatDeadline) {
			t.Fatalf("lease heartbeat did not advance while terminal create was blocked: run=%+v, err=%v", run, getErr)
		}
		runtime.Gosched()
	}
	close(drv.failWait)
	deadline = time.Now().Add(2 * time.Second)
	for clk.Waiters() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("controller did not register its poll timer")
		}
		runtime.Gosched()
	}
	clk.Advance(time.Second)
	select {
	case <-drv.started:
	case runErr := <-done:
		t.Fatalf("phase launch retry stopped the controller: %v", runErr)
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not retry the terminal launch")
	}
	if len(drv.PhaseStarts) != 1 {
		select {
		case runErr := <-done:
			t.Fatalf("phase starts = %d, run stopped with %v", len(drv.PhaseStarts), runErr)
		default:
		}
		t.Fatalf("phase starts = %d, want one retry launch", len(drv.PhaseStarts))
	}
	run, err := st.Runs().Get(ctx, "run-1")
	if err != nil || run.Lease == nil || !run.Lease.HeartbeatAt.After(beforeHeartbeat.Lease.HeartbeatAt) {
		t.Fatalf("run lease = %+v, err=%v, want a heartbeat after the transient failure", run.Lease, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

type effectDriver struct {
	*lane.FakeDriver
	afterStart func()
	failDone   chan struct{}
	failWait   chan struct{}
	started    chan struct{}
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
		if d.failDone != nil {
			close(d.failDone)
		}
		if d.failWait != nil {
			<-d.failWait
		}
		return lane.StartPhaseResult{}, errors.New("terminal create: context deadline exceeded")
	}
	res, err := d.FakeDriver.StartPhaseTerminal(ctx, req)
	if err == nil && d.started != nil {
		close(d.started)
	}
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
				if prior == "nudge" {
					clk.Advance(controller.DefaultStallTimeout / 2)
				}
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
	if err != nil || got.Status != lane.StatusRunning || len(base.PhaseStarts) != 2 {
		t.Fatalf("interrupted integrate intent must retry: lane=%+v starts=%d err=%v", got, len(base.PhaseStarts), err)
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
		if step == 2 {
			clk.Advance(controller.DefaultStallTimeout / 2)
		} else {
			clk.Advance(time.Minute)
		}
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

func TestControllerIgnoresReportFromPreviousTerminal(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{ID: "a", RunID: "run-1", Status: lane.StatusRunning, Phase: "review", Round: 2, SessionID: "session", TerminalID: "current-review"}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	appendReportEvent(t, st, "a", lane.PhaseReport{TerminalID: "old-review", Outcome: lane.OutcomeDone, HeadSHA: "old-head"})
	if err := ctrl.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	ln, err := st.Lanes().Get(t.Context(), "run-1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if ln.Phase != "review" || ln.PRHead != "" || len(drv.PhaseStarts) != 0 {
		t.Fatalf("stale report changed lane: %+v", ln)
	}
}

func TestControllerPlannedLaneWaitsForDependencies(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	for _, item := range []store.BacklogItem{
		{ID: "prerequisite", RunID: "run-1", Title: "Prerequisite", Priority: store.PriorityP2, Status: store.BacklogInProgress},
		{ID: "dependent", RunID: "run-1", Title: "Dependent", Priority: store.PriorityP2, Status: store.BacklogOpen, DependsOn: []string{"prerequisite"}},
	} {
		if err := st.Backlog().Add(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{ID: "dependent", ItemID: "dependent", RunID: "run-1", Status: lane.StatusPlanned}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(drv.Spawns) != 0 {
		t.Fatal("started lane before its dependency completed")
	}
	if err := st.Backlog().Close(t.Context(), "run-1", "prerequisite", store.BacklogDone, "test", "done"); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].LaneID != "dependent" {
		t.Fatalf("dependent lane not started: %+v", drv.Spawns)
	}
}

func TestControllerMergedPRBeforeRetry(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	if err := st.Backlog().Add(t.Context(), store.BacklogItem{
		ID:       "m2-default",
		RunID:    "run-1",
		Title:    "Default lane",
		Priority: store.PriorityP2,
		Status:   store.BacklogInProgress,
		LaneID:   "m2-default",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{
		ID:         "m2-default",
		ItemID:     "m2-default",
		RunID:      "run-1",
		Status:     lane.StatusRunning,
		Phase:      "review",
		Round:      1,
		Attempt:    1,
		PR:         5705,
		Branch:     "swarm-lane/trial-m1/m2-default",
		SessionID:  "sess-m2",
		TerminalID: "term-review-1",
	}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{
		RunID: "run-1",
		PRProbe: func(_ context.Context, _, _ string, pr int) (controller.PRStatus, error) {
			if pr == 5705 {
				return controller.PRStatus{Number: 5705, State: "MERGED", MergeSHA: "merged-sha-5705"}, nil
			}
			return controller.PRStatus{Number: pr, State: "OPEN"}, nil
		},
	}, st, drv, clk, judge)

	appendLaneWatchEvent(t, st, "m2-default", "term-review-1", lane.EventExited)
	mustStep(t, ctrl)

	ln, err := st.Lanes().Get(t.Context(), "run-1", "m2-default")
	if err != nil {
		t.Fatal(err)
	}
	if ln.Status != lane.StatusDone || ln.MergeSHA != "merged-sha-5705" {
		t.Fatalf("lane = %+v, want status=%q MergeSHA=merged-sha-5705 (starts=%d steers=%d stops=%d releases=%d)", ln, lane.StatusDone, len(drv.PhaseStarts), len(drv.Steers), len(drv.Stops), len(drv.Releases))
	}
	if len(drv.PhaseStarts) != 0 || len(drv.Steers) != 0 || len(drv.Spawns) != 0 {
		t.Fatalf("merged PR lane spawned/retried: starts=%+v steers=%+v spawns=%+v", drv.PhaseStarts, drv.Steers, drv.Spawns)
	}
	if len(drv.Stops) == 0 && len(drv.Releases) == 0 {
		t.Fatalf("expected merged PR lane terminals/session stopped, stops=%+v releases=%+v", drv.Stops, drv.Releases)
	}

	// Verify CLOSED PR marks lane failed and drops backlog item without spawning.
	if err := st.Backlog().Add(t.Context(), store.BacklogItem{
		ID:       "m2-closed",
		RunID:    "run-1",
		Title:    "Closed lane",
		Priority: store.PriorityP2,
		Status:   store.BacklogInProgress,
		LaneID:   "m2-closed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{
		ID:         "m2-closed",
		ItemID:     "m2-closed",
		RunID:      "run-1",
		Status:     lane.StatusRunning,
		Phase:      "review",
		Round:      1,
		Attempt:    1,
		PR:         5706,
		Branch:     "swarm-lane/trial-m1/m2-closed",
		SessionID:  "sess-closed",
		TerminalID: "term-closed-1",
	}); err != nil {
		t.Fatal(err)
	}
	ctrlClosed := controller.New(controller.Config{
		RunID: "run-1",
		PRProbe: func(_ context.Context, runID, _ string, pr int) (controller.PRStatus, error) {
			if runID == "run-1" && pr == 5706 {
				return controller.PRStatus{Number: 5706, State: "CLOSED"}, nil
			}
			return controller.PRStatus{Number: pr, State: "OPEN"}, nil
		},
	}, st, drv, clk, judge)
	appendLaneWatchEvent(t, st, "m2-closed", "term-closed-1", lane.EventExited)
	mustStep(t, ctrlClosed)
	closedLn, err := st.Lanes().Get(t.Context(), "run-1", "m2-closed")
	if err != nil {
		t.Fatal(err)
	}
	if closedLn.Status != lane.StatusFailed {
		t.Fatalf("closed PR lane status = %q, want %q", closedLn.Status, lane.StatusFailed)
	}
	closedItem, err := st.Backlog().Get(t.Context(), "run-1", "m2-closed")
	if err != nil || closedItem.Status != store.BacklogDropped {
		t.Fatalf("closed PR backlog item = %+v, err = %v; want dropped", closedItem, err)
	}

	// Verify a planned lane with a Branch (PR == 0) whose PR is already MERGED
	// settles before spawning any terminal.
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{
		ID:     "m2-planned-merged",
		ItemID: "m2-planned-merged",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
		Phase:  "swe",
		Round:  1,
		Branch: "swarm-lane/trial-m1/m2-planned-merged",
	}); err != nil {
		t.Fatal(err)
	}
	ctrlPlanned := controller.New(controller.Config{
		RunID: "run-1",
		PRProbe: func(_ context.Context, runID, branch string, _ int) (controller.PRStatus, error) {
			if runID == "run-1" && branch == "swarm-lane/trial-m1/m2-planned-merged" {
				return controller.PRStatus{Number: 5707, State: "MERGED", MergeSHA: "merged-sha-5707"}, nil
			}
			return controller.PRStatus{State: "OPEN"}, nil
		},
	}, st, drv, clk, judge)
	mustStep(t, ctrlPlanned)
	plannedLn, err := st.Lanes().Get(t.Context(), "run-1", "m2-planned-merged")
	if err != nil {
		t.Fatal(err)
	}
	if plannedLn.Status != lane.StatusDone || plannedLn.PR != 5707 || plannedLn.MergeSHA != "merged-sha-5707" {
		t.Fatalf("planned merged lane = %+v, want StatusDone PR=5707 MergeSHA=merged-sha-5707", plannedLn)
	}
}

func TestControllerCostGuardPausesTurnsAtLimit(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	for i, cost := range []float64{3.25, 2.50} {
		if _, err := st.Decisions().Append(ctx, store.Decision{
			RunID:    "run-1",
			Category: store.DecisionJudgment,
			Actor:    "dispatcher",
			Tool:     "dispatcher.turn",
			Reason:   "prior turn",
			Result:   "ok",
			CostUSD:  cost,
			Vetted:   true,
		}); err != nil {
			t.Fatalf("Append decision %d: %v", i, err)
		}
	}

	ctrl := controller.New(controller.Config{
		RunID:             "run-1",
		MaxCostPerHourUSD: 5.0,
	}, st, drv, clk, judge)

	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventMail,
		Urgent:  true,
		Summary: "urgent mail event while over cost limit",
	}); err != nil {
		t.Fatalf("Append event: %v", err)
	}

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if judge.BatchCount() != 0 {
		t.Fatalf("judge batches when cost guard exceeded = %d, want 0", judge.BatchCount())
	}
	settings, err := st.Runs().GetSettings(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if !settings.Paused {
		t.Fatalf("run settings Paused = false, want true when cost guard limit is reached")
	}
	if settings.MaxCostPerHourUSD != 0 {
		t.Fatalf("run settings MaxCostPerHourUSD = %.2f, want 0 (fallback limit must not overwrite run setting)", settings.MaxCostPerHourUSD)
	}

	// If the owner unpauses while still over the hourly limit, the next Step must
	// re-pause the run and record another cost.guard decision.
	settings.Paused = false
	if err := st.Runs().UpdateSettings(ctx, "run-1", settings); err != nil {
		t.Fatalf("UpdateSettings(unpause): %v", err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step after unpause: %v", err)
	}
	settingsAfter, err := st.Runs().GetSettings(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSettings after unpause: %v", err)
	}
	if !settingsAfter.Paused {
		t.Fatalf("run settings Paused after unpause while over limit = false, want true")
	}

	// If the owner explicitly disables the cost guard (max_cost_per_hour_usd=0),
	// SetRuntimeSettings and EnforceCostGuard must honor the disabled setting
	// instead of falling back to the startup $5 limit.
	settingsAfter.Paused = false
	settingsAfter.MaxCostPerHourUSD = 0
	settingsAfter.CostGuardDisabled = true
	if err := st.Runs().UpdateSettings(ctx, "run-1", settingsAfter); err != nil {
		t.Fatalf("UpdateSettings(disable cost guard): %v", err)
	}
	ctrl.SetRuntimeSettings(settingsAfter)
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventMail,
		Urgent:  true,
		Summary: "urgent mail after cost guard disabled",
	}); err != nil {
		t.Fatalf("Append event after disable: %v", err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step after cost guard disabled: %v", err)
	}
	settingsDisabled, err := st.Runs().GetSettings(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetSettings after disable: %v", err)
	}
	if settingsDisabled.Paused {
		t.Fatalf("run settings Paused when CostGuardDisabled=true = true, want false")
	}
}

func TestControllerUnpushedWorkSteersOnceThenReportsToDispatcher(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-unpushed",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{
		RunID:                 "run-1",
		JudgmentDebounce:      -1,
		StallTimeout:          4 * time.Hour,
		StallTimeoutSet:       true,
		UnpushedWorkThreshold: 45 * time.Minute,
	}, st, drv, clk, judge)

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (spawn): %v", err)
	}

	oldest := clk.Now()
	drv.GitStates["lane-unpushed"] = lane.GitState{
		UnpushedCommits:  2,
		DirtyFiles:       []string{"pkg/foo.go", "pkg/bar.go"},
		OldestUnpushedAt: oldest,
	}

	// Below the 45m threshold: no steer and no dispatcher event.
	clk.Advance(30 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 30m: %v", err)
	}
	if len(drv.Steers) != 0 || judge.BatchCount() != 0 {
		t.Fatalf("at 30m: steers=%d batches=%d, want 0, 0", len(drv.Steers), judge.BatchCount())
	}

	// Past the 45m threshold (at 50m): one steer telling the lane to commit and push WIP.
	clk.Advance(20 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 50m (first check past threshold): %v", err)
	}
	if len(drv.Steers) != 1 || !strings.Contains(drv.Steers[0].Message, "WIP") {
		t.Fatalf("at 50m: steers=%+v, want 1 WIP steer", drv.Steers)
	}
	if judge.BatchCount() != 0 {
		t.Fatalf("at 50m: batches=%d, want 0 before persistence check", judge.BatchCount())
	}

	// Next poll when unpushed work still persists: one dispatcher event, no second steer.
	clk.Advance(5 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 55m (persistent unpushed work): %v", err)
	}
	if len(drv.Steers) != 1 {
		t.Fatalf("at 55m: steers=%d, want still 1", len(drv.Steers))
	}
	if judge.BatchCount() != 1 || len(judge.Batches[0]) != 1 || judge.Batches[0][0].Source != "lane.unpushed" {
		t.Fatalf("at 55m: judge batches=%+v, want 1 batch with lane.unpushed event", judge.Batches)
	}

	// Subsequent polls do not emit repeated steers or dispatcher events.
	clk.Advance(10 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 65m: %v", err)
	}
	if len(drv.Steers) != 1 || judge.BatchCount() != 1 {
		t.Fatalf("at 65m: steers=%d batches=%d, want 1, 1", len(drv.Steers), judge.BatchCount())
	}
}

func TestControllerExpectedWaitSuppressesStallUntilDeadline(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "perf-sweep",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{
		RunID:            "run-1",
		JudgmentDebounce: -1,
		StallTimeout:     20 * time.Minute,
		StallTimeoutSet:  true,
	}, st, drv, clk, judge)

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step (spawn): %v", err)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "perf-sweep")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}

	deadline := clk.Now().Add(120 * time.Minute)
	if err := st.Directives().Add(ctx, store.Directive{
		ID:        "wait-perf-sweep",
		RunID:     "run-1",
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     "perf-sweep",
		Ref:       "named-wait:perf-sweep",
		Text:      "120-minute wait for perf-sweep's Azure sweep",
		ExpiresAt: &deadline,
		Status:    store.DirectiveActive,
	}); err != nil {
		t.Fatalf("Add expected wait directive: %v", err)
	}

	// Advance 60 minutes (3x the 20m stall timeout) and emit no_activity: suppressed until deadline.
	clk.Advance(60 * time.Minute)
	appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 60m: %v", err)
	}
	if len(drv.Steers) != 0 || len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 || judge.BatchCount() != 0 {
		t.Fatalf("during expected wait at 60m: steers=%d starts=%d stops=%d batches=%d, want all 0",
			len(drv.Steers), len(drv.PhaseStarts), len(drv.Stops), judge.BatchCount())
	}

	// Advance past the 120-minute deadline: wait expires and stall is no longer suppressed.
	clk.Advance(61 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 121m (past deadline): %v", err)
	}
	if len(drv.Stops) != 1 || drv.Stops[0].LaneID != "perf-sweep" {
		t.Fatalf("after deadline expiry: stops=%+v, want perf-sweep stopped on stall", drv.Stops)
	}
}

func TestControllerDiskPressure94PercentOneNoteAndOneEvent(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "lane-disk",
		RunID:  "run-1",
		Status: lane.StatusPlanned,
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

	drv.DiskUsage["lane-disk"] = 94

	for poll := 1; poll <= 5; poll++ {
		clk.Advance(time.Minute)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step poll %d: %v", poll, err)
		}
	}

	if len(drv.Steers) != 1 {
		t.Fatalf("disk 94%% steers=%d, want 1", len(drv.Steers))
	}
	msg := drv.Steers[0].Message
	if !strings.Contains(msg, "94%") || !strings.Contains(msg, "build cache") || !strings.Contains(msg, "rerun only the failed step") {
		t.Fatalf("disk steer message = %q, want 94%%, build cache, and rerun only the failed step", msg)
	}
	notes, err := st.Notes().List(ctx, store.NoteFilter{RunID: "run-1", TaskID: "lane-disk"})
	if err != nil || len(notes) != 1 {
		t.Fatalf("disk notes = %+v (err=%v), want 1 note", notes, err)
	}
	if judge.BatchCount() != 1 || len(judge.Batches[0]) != 1 || judge.Batches[0][0].Source != "lane.health" {
		t.Fatalf("disk judge batches = %+v, want 1 batch with lane.health event", judge.Batches)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "lane-disk")
	if err != nil || ln.Status != lane.StatusRunning {
		t.Fatalf("lane status after disk pressure alert = %+v, err=%v, want still running", ln, err)
	}
}

func TestControllerNewPhaseStopsPreviousTerminalAndRefusesInput(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:        "lane-stale",
		RunID:     "run-1",
		Status:    lane.StatusPlanned,
		Terminals: map[string]string{"prehandover": "19f8df22"},
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step 1 (spawn swe): %v", err)
	}

	// Pre-handover terminal 19f8df22 recorded on the lane must be stopped on Step.
	if !drv.IsTerminalStopped("19f8df22") {
		t.Fatalf("expected pre-handover terminal 19f8df22 to be stopped")
	}
	if err := drv.Steer(ctx, lane.SteerRequest{RunID: "run-1", LaneID: "lane-stale", TerminalID: "19f8df22", Message: "stale input"}); !errors.Is(err, lane.ErrStaleTerminal) {
		t.Fatalf("Steer to pre-handover terminal 19f8df22 = %v, want ErrStaleTerminal", err)
	}

	ln, err := st.Lanes().Get(ctx, "run-1", "lane-stale")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}
	sweTerm := ln.TerminalID

	// Report done on swe so the controller advances the lane to clean.
	appendReportEvent(t, st, "lane-stale", lane.PhaseReport{
		Phase:   recipe.PhaseSWE,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-stale-1",
		PR:      42,
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step 2 (advance swe -> clean): %v", err)
	}

	lnAfter, err := st.Lanes().Get(ctx, "run-1", "lane-stale")
	if err != nil {
		t.Fatalf("Get lane after advance: %v", err)
	}
	if lnAfter.Phase != recipe.PhaseClean || lnAfter.TerminalID == sweTerm {
		t.Fatalf("lane after advance = %+v, want clean on new terminal", lnAfter)
	}
	if !drv.IsTerminalStopped(sweTerm) {
		t.Fatalf("expected previous phase terminal %s to be stopped when clean started", sweTerm)
	}
	if err := drv.Steer(ctx, lane.SteerRequest{
		RunID:      "run-1",
		LaneID:     "lane-stale",
		SessionID:  lnAfter.SessionID,
		TerminalID: sweTerm,
		Message:    "late input to old phase terminal",
	}); !errors.Is(err, lane.ErrStaleTerminal) {
		t.Fatalf("Steer to previous phase terminal %s = %v, want ErrStaleTerminal", sweTerm, err)
	}

	// Verify that a newly constructed controller recovers StoppedTerminals from the decision store
	// and does not issue duplicate Stop calls for already-stopped previous-phase terminals.
	stopsBeforeRestart := len(drv.Stops)
	ctrlRestarted := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrlRestarted.Step(ctx); err != nil {
		t.Fatalf("ctrlRestarted.Step: %v", err)
	}
	if len(drv.Stops) != stopsBeforeRestart {
		t.Fatalf("restarted controller issued duplicate Stop calls: got %d, want %d", len(drv.Stops), stopsBeforeRestart)
	}

	// Verify that if stopping a previous-phase terminal fails during phase launch,
	// stopStaleRecordedTerminals retries on subsequent steps even after the new phase
	// has launched, and caps retries at 3 total attempts without retryStops resetting the cap.
	failDrv := &lifecycleFailDriver{FakeDriver: lane.NewFakeDriver(), stopFailures: 10}
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:         "lane-stale-retry",
		RunID:      "run-1",
		Status:     lane.StatusRunning,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		Attempt:    1,
		SessionID:  "sess-retry-1",
		TerminalID: "term-swe-retry-1",
		Sessions:   map[string]string{"swe:1": "sess-retry-1"},
		Terminals:  map[string]string{"swe:1": "term-swe-retry-1"},
	}); err != nil {
		t.Fatalf("Upsert lane-stale-retry: %v", err)
	}
	ctrlRetry := controller.New(controller.Config{RunID: "run-1"}, st, failDrv, clk, judge)
	appendReportEvent(t, st, "lane-stale-retry", lane.PhaseReport{
		Phase:   recipe.PhaseSWE,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-retry-1",
		PR:      43,
	})
	for step := 1; step <= 5; step++ {
		if err := ctrlRetry.Step(ctx); err != nil {
			t.Fatalf("ctrlRetry Step %d: %v", step, err)
		}
	}
	stopCount := 0
	for _, s := range failDrv.Stops {
		if s.TerminalID == "term-swe-retry-1" {
			stopCount++
		}
	}
	if stopCount != 3 {
		t.Fatalf("failed previous-phase terminal stop count across 5 steps = %d, want 3 (retried after launch and capped at 3)", stopCount)
	}
}

func TestControllerStallLadderNudgeThenActivityWithinWaitAndBusyProcess(t *testing.T) {
	t.Parallel()
	t.Run("nudge_then_activity_within_wait_no_fresh_terminal", func(t *testing.T) {
		t.Parallel()
		clk, st, baseDrv, judge := setupTestEnv(t)
		drv := &lifecycleFailDriver{FakeDriver: baseDrv}
		ctx := t.Context()
		saveRecipeWithTimeout(t, st, 20*time.Minute)
		if err := st.Lanes().Upsert(ctx, lane.Lane{
			ID:         "lane-nudge-active",
			RunID:      "run-1",
			RecipeName: "stall-test",
			Status:     lane.StatusPlanned,
		}); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}
		ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (spawn): %v", err)
		}
		ln, err := st.Lanes().Get(ctx, "run-1", "lane-nudge-active")
		if err != nil {
			t.Fatalf("Get lane: %v", err)
		}

		// Quiet pulse 1 -> nudges the terminal.
		clk.Advance(time.Minute)
		appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (nudge): %v", err)
		}
		if len(drv.Steers) != 1 || len(drv.PhaseStarts) != 0 {
			t.Fatalf("after nudge: steers=%d starts=%d, want 1, 0", len(drv.Steers), len(drv.PhaseStarts))
		}

		// Quiet pulse 2 seconds later (inside the 10m wait window) -> no fresh terminal.
		clk.Advance(2 * time.Second)
		appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (2s after nudge): %v", err)
		}
		if len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
			t.Fatalf("2s after nudge: starts=%d stops=%d, want 0, 0", len(drv.PhaseStarts), len(drv.Stops))
		}

		// Activity within the wait window (at +3m): CI event arrives (and/or terminal works).
		clk.Advance(3 * time.Minute)
		appendCIEvent(t, st, ln.ID, "pending", "sha-1")
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (activity within wait): %v", err)
		}

		// Advance past the original 10m nudge wait window and emit no_activity -> still no fresh terminal
		// because the +3m activity re-based the 10m wait window.
		clk.Advance(8 * time.Minute)
		appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (after wait window following activity): %v", err)
		}
		if len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
			t.Fatalf("after nudge + activity within wait: starts=%d stops=%d, want 0, 0", len(drv.PhaseStarts), len(drv.Stops))
		}

		// Once the lane stays quiet past the re-based 10m wait window (at +11m after the activity),
		// a transient TerminalAgentState error suppresses replacement on that tick without poisoning
		// future ticks; the next tick advances to a fresh terminal without another driver pulse.
		clk.Advance(3 * time.Minute)
		drv.agentStateErrors = 1
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (transient TerminalAgentState error after wait window): %v", err)
		}
		if len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
			t.Fatalf("during transient TerminalAgentState error: starts=%d stops=%d, want 0, 0", len(drv.PhaseStarts), len(drv.Stops))
		}
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (after re-based wait window expired and state probe recovered): %v", err)
		}
		if len(drv.PhaseStarts) != 1 || len(drv.Stops) != 1 {
			t.Fatalf("after re-based wait window expired: starts=%d stops=%d, want 1, 1", len(drv.PhaseStarts), len(drv.Stops))
		}
		evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run-1", LaneID: ln.ID})
		if err != nil {
			t.Fatalf("List events: %v", err)
		}
		for _, ev := range evs {
			if strings.HasPrefix(ev.ID, "nudge-expired:") {
				t.Fatalf("unexpected synthetic nudge-expired event persisted in store: %+v", ev)
			}
		}
	})

	t.Run("busy_review_gate_process_never_nudged_or_replaced", func(t *testing.T) {
		t.Parallel()
		clk, st, drv, judge := setupTestEnv(t)
		ctx := t.Context()
		saveRecipeWithTimeout(t, st, 20*time.Minute)
		if err := st.Lanes().Upsert(ctx, lane.Lane{
			ID:         "lane-busy-proc",
			RunID:      "run-1",
			RecipeName: "stall-test",
			Status:     lane.StatusPlanned,
		}); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}
		ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step (spawn): %v", err)
		}
		ln, err := st.Lanes().Get(ctx, "run-1", "lane-busy-proc")
		if err != nil {
			t.Fatalf("Get lane: %v", err)
		}

		for _, proc := range []string{"pr-polish review_cli --headless", "bazel test //orca/...", "scripts/prepush.sh"} {
			drv.BusyProcesses[ln.ID] = []string{proc}
			clk.Advance(25 * time.Minute)
			appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
			if err := ctrl.Step(ctx); err != nil {
				t.Fatalf("Step with busy proc %q: %v", proc, err)
			}
			if len(drv.Steers) != 0 || len(drv.PhaseStarts) != 0 || len(drv.Stops) != 0 {
				t.Fatalf("busy proc %q was nudged/replaced/stopped: steers=%d starts=%d stops=%d",
					proc, len(drv.Steers), len(drv.PhaseStarts), len(drv.Stops))
			}
		}
	})
}

type mergeRecordingDriver struct {
	*lane.FakeDriver
	Merges []int
}

func (d *mergeRecordingDriver) MergePR(_ context.Context, _ string, ln lane.Lane, _ string) (string, error) {
	d.Merges = append(d.Merges, ln.PR)
	return "merged-" + ln.PRHead, nil
}

func TestControllerNoMergeReportOnlyItemSkipsIntegrateAndNeverMerges(t *testing.T) {
	t.Parallel()
	clk, st, baseDrv, judge := setupTestEnv(t)
	drv := &mergeRecordingDriver{FakeDriver: baseDrv}
	ctx := t.Context()

	adopted := recipe.Default().Clone()
	adopted.RunID = "run-1"
	adopted.Name = "adopted"
	adopted.MergePolicy.Mode = recipe.MergeModeController
	if _, err := st.Recipes().Save(ctx, adopted); err != nil {
		t.Fatalf("Save recipe: %v", err)
	}

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:          "m4-metrics",
		RunID:       "run-1",
		Title:       "m4-metrics",
		Description: "Collect M4 metrics. Do not merge anything; this is a measurement/report task.",
		Status:      store.BacklogInProgress,
	}); err != nil {
		t.Fatalf("Add backlog item: %v", err)
	}

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:         "m4-metrics",
		RunID:      "run-1",
		ItemID:     "m4-metrics",
		Title:      "m4-metrics",
		Status:     lane.StatusPlanned,
		RecipeName: "adopted",
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step 1 (spawn swe): %v", err)
	}

	appendReportEvent(t, st, "m4-metrics", lane.PhaseReport{
		Phase:   recipe.PhaseSWE,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-m4-1",
		PR:      5826,
		Summary: "published measurement report on PR #5826",
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step 2 (swe -> clean): %v", err)
	}

	appendReportEvent(t, st, "m4-metrics", lane.PhaseReport{
		Phase:   recipe.PhaseClean,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-m4-1",
		PR:      5826,
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step 3 (clean -> review): %v", err)
	}

	appendCIEvent(t, st, "m4-metrics", "green", "sha-m4-1")
	appendReportEvent(t, st, "m4-metrics", lane.PhaseReport{
		Phase:   recipe.PhaseReview,
		Outcome: lane.OutcomeDone,
		HeadSHA: "sha-m4-1",
		PR:      5826,
	})
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step 4 (review -> done without integrate): %v", err)
	}

	for _, start := range drv.PhaseStarts {
		if start.Phase == recipe.PhaseIntegrate {
			t.Fatalf("unexpected integrate PhaseStart on no-merge report-only lane: %+v", start)
		}
	}
	if len(drv.Merges) != 0 {
		t.Fatalf("drv.Merges = %+v, want 0 merge calls on no-merge report-only lane", drv.Merges)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "m4-metrics")
	if err != nil {
		t.Fatalf("Get lane: %v", err)
	}
	if ln.Status != lane.StatusDone {
		t.Fatalf("lane status = %q (phase=%q), want done after review", ln.Status, ln.Phase)
	}
}

func TestAnsweredReportLaneRunsAndNudges(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1", JudgmentDebounce: -1}, st, drv, clk, judge)
	raw, _ := json.Marshal(lane.PhaseReport{TerminalID: "t-1", Phase: recipe.PhaseSWE, Outcome: lane.OutcomeNeedsInput, Summary: "Restore main?"})
	if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: "question", Kind: store.EventLaneReport, Source: "lane.watch", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "question")
	if err != nil || ln.Status != lane.StatusBlocked {
		t.Fatalf("reported lane=%+v err=%v, want blocked", ln, err)
	}
	client := model.NewFakeClient(
		model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("answer", dispatcher.ToolLaneSteer, json.RawMessage(`{"lane_id":"question","note":"Restore main"}`)))},
		model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Answered."))},
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Dispatch(ctx, "run-1", judge.Batches[0]); err != nil {
		t.Fatal(err)
	}
	appendLaneWatchEvent(t, st, "question", "t-1", lane.EventNoActivity)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	ln, err = st.Lanes().Get(ctx, "run-1", "question")
	if err != nil || ln.Status != lane.StatusRunning || len(drv.Steers) != 2 {
		t.Fatalf("answered lane=%+v steers=%d err=%v, want running and answer plus one nudge", ln, len(drv.Steers), err)
	}
}

func TestLaneQuestionBypassesDefaultDebounce(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusRunning, TerminalID: "t-1", Phase: recipe.PhaseSWE}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	raw, _ := json.Marshal(lane.PhaseReport{TerminalID: "t-1", Phase: recipe.PhaseSWE, Outcome: lane.OutcomeNeedsInput, Summary: "Restore main?"})
	if _, err := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: "question", Kind: store.EventLaneReport, Source: "lane.watch", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if judge.BatchCount() != 1 {
		t.Fatalf("judgment batches=%d, want 1 without clock advance", judge.BatchCount())
	}
}

func TestLaneAnswersReleaseOwnBlocksAndCollapseReasks(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{dispatcher.ToolLaneSteer, dispatcher.ToolLaneForward} {
		for _, outcome := range []lane.ReportOutcome{lane.OutcomeNeedsInput, lane.OutcomeBlocked} {
			t.Run(tool+"/"+string(outcome), func(t *testing.T) {
				clk, st, drv, judge := setupTestEnv(t)
				ctx := t.Context()
				if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE}); err != nil {
					t.Fatal(err)
				}
				ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
				appendLaneWatchEvent(t, st, "question", "t-1", lane.EventNoActivity)
				if err := ctrl.Step(ctx); err != nil {
					t.Fatal(err)
				}
				if len(drv.Steers) != 1 {
					t.Fatalf("initial nudges=%d", len(drv.Steers))
				}
				report := lane.PhaseReport{TerminalID: "t-1", Phase: recipe.PhaseSWE, Outcome: outcome, Summary: "Restore main?"}
				appendQuestion := func() {
					t.Helper()
					report.Raw += "\nMetadata: new revision"
					appendReportEvent(t, st, "question", report)
					if err := ctrl.Step(ctx); err != nil {
						t.Fatal(err)
					}
				}
				appendQuestion()
				appendQuestion()
				if judge.BatchCount() != 1 {
					t.Fatalf("unanswered re-asks: batches=%d, want 1", judge.BatchCount())
				}
				input := json.RawMessage(`{"lane_id":"question","note":"Restore main","select":1}`)
				if tool == dispatcher.ToolLaneForward {
					input = json.RawMessage(`{"to_lane_id":"question","excerpt":"Restore main"}`)
				}
				client := model.NewFakeClient(
					model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("answer", tool, input))},
					model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Answered."))},
				)
				d := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
				t.Cleanup(func() { _ = d.Close() })
				if _, err := d.Dispatch(ctx, "run-1", judge.Batches[0]); err != nil {
					t.Fatal(err)
				}
				got, err := st.Lanes().Get(ctx, "run-1", "question")
				if err != nil || got.Status != lane.StatusRunning || got.AnswerConfirmedAt.IsZero() || got.AnswerEpoch != 1 {
					t.Fatalf("answered lane=%+v err=%v", got, err)
				}
				if tool == dispatcher.ToolLaneSteer && drv.Steers[1].Select != 1 {
					t.Fatalf("selection not forwarded: %+v", drv.Steers[1])
				}
				// Delivery dedupe must survive controller restart and metadata-only re-filing.
				ctrl = controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
				clk.Advance(10 * time.Second)
				appendQuestion()
				if judge.BatchCount() != 1 {
					t.Fatalf("settling re-ask: batches=%d, want 1", judge.BatchCount())
				}
				// The durable report endpoint still exposes the old question after delivery.
				drv.SetReport("t-1", report)
				clk.Advance(lane.AnswerDeliveryWindow)
				appendLaneWatchEvent(t, st, "question", "t-1", lane.EventNoActivity)
				if err := ctrl.Step(ctx); err != nil {
					t.Fatal(err)
				}
				if len(drv.Steers) != 3 || len(drv.PhaseStarts) != 0 {
					t.Fatalf("quiet ladder did not reset: steers=%d starts=%d", len(drv.Steers), len(drv.PhaseStarts))
				}
				appendQuestion()
				if judge.BatchCount() != 2 || len(judge.Batches[1]) != 1 {
					t.Fatalf("later re-ask: batches=%d, want 2", judge.BatchCount())
				}
			})
		}
	}
}

type undeliveredDriver struct {
	*lane.FakeDriver
	calls            int
	preSubmitFailure bool
}

func (d *undeliveredDriver) Steer(_ context.Context, req lane.SteerRequest) error {
	d.calls++
	if d.preSubmitFailure {
		return errors.New("input API unavailable")
	}
	return &lane.UndeliveredError{TerminalID: req.TerminalID, Cause: errors.New("still idle after flush")}
}

func TestUndeliveredLaneAnswerReturnsToJudgmentOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action           string
		preSubmitFailure bool
	}{
		{action: dispatcher.ToolLaneSteer}, {action: dispatcher.ToolLaneForward}, {action: "nudge"},
		{action: dispatcher.ToolLaneSteer, preSubmitFailure: true}, {action: dispatcher.ToolLaneForward, preSubmitFailure: true},
	} {
		t.Run(fmt.Sprintf("%s/pre_submit_failure_%t", tc.action, tc.preSubmitFailure), func(t *testing.T) {
			action := tc.action
			clk, st, base, judge := setupTestEnv(t)
			drv := &undeliveredDriver{FakeDriver: base, preSubmitFailure: tc.preSubmitFailure}
			ctx := t.Context()
			ln := lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE}
			if action != "nudge" {
				ln.Status = lane.StatusBlocked
				ln.Question = "Restore main?"
			}
			if err := st.Lanes().Upsert(ctx, ln); err != nil {
				t.Fatal(err)
			}
			ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
			if action == "nudge" {
				appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
			} else {
				input := json.RawMessage(`{"lane_id":"question","note":"Restore main"}`)
				if action == dispatcher.ToolLaneForward {
					input = json.RawMessage(`{"to_lane_id":"question","excerpt":"Restore main"}`)
				}
				client := model.NewFakeClient(
					model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("answer", action, input))},
					model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Delivery failed."))},
				)
				d := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
				t.Cleanup(func() { _ = d.Close() })
				result, err := d.Dispatch(ctx, "run-1", []store.Event{{ID: "question-event", LaneID: ln.ID, Kind: store.EventLaneReport, Summary: ln.Question}})
				if err != nil {
					t.Fatal(err)
				}
				wantAccepted := 1
				if tc.preSubmitFailure {
					wantAccepted = 0
				}
				if len(result.Accepted) != wantAccepted {
					t.Fatalf("accepted mutations=%d, want %d", len(result.Accepted), wantAccepted)
				}
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if judge.BatchCount() != 1 || !strings.Contains(judge.Batches[0][0].Summary, "answer not delivered") {
				t.Fatalf("delivery judgment=%+v", judge.Batches)
			}
			got, err := st.Lanes().Get(ctx, "run-1", ln.ID)
			if err != nil || got.AwaitingInputSince.IsZero() || !got.AnswerConfirmedAt.IsZero() {
				t.Fatalf("undelivered lane=%+v err=%v", got, err)
			}
			clk.Advance(30 * time.Minute)
			appendLaneWatchEvent(t, st, ln.ID, ln.TerminalID, lane.EventNoActivity)
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if drv.calls != 1 || judge.BatchCount() != 1 || len(base.Stops) != 0 {
				t.Fatalf("delivery loop: calls=%d batches=%d stops=%d", drv.calls, judge.BatchCount(), len(base.Stops))
			}
		})
	}
}

func TestUndeliveredControllerSteersAreIdempotent(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"unpushed", "disk", "worker", "default", "owner"} {
		t.Run(kind, func(t *testing.T) {
			clk, st, base, judge := setupTestEnv(t)
			drv := &undeliveredDriver{FakeDriver: base}
			ctx := t.Context()
			ln := lane.Lane{ID: "lane-" + kind, RunID: "run-1", Status: lane.StatusRunning, SessionID: "s-1", TerminalID: "t-1", Phase: recipe.PhaseSWE}
			if kind == "owner" {
				ln.Status = lane.StatusBlocked
			}
			if err := st.Lanes().Upsert(ctx, ln); err != nil {
				t.Fatal(err)
			}
			cfg := controller.Config{RunID: "run-1", JudgmentDebounce: -1, UnpushedWorkThreshold: time.Minute}
			var box *questionBox
			var ask controller.OwnerAsk
			switch kind {
			case "worker":
				cfg.AnswerQuestion = func(context.Context, string, string) (string, error) { return "use the task notes", nil }
			case "default":
				box = &questionBox{}
			case "owner":
				ask = controller.OwnerAsk{ID: "owner-ask", RunID: "run-1", LaneID: ln.ID, Question: "choose", Options: []string{"continue"}, Default: "continue", Deadline: clk.Now().Add(time.Hour)}
			}
			ctrl := controller.New(cfg, st, drv, clk, judge, box)
			if kind == "owner" {
				if err := ctrl.AskOwner(ctx, ask); err != nil {
					t.Fatal(err)
				}
				mailEvent(t, st, mailbox.Message{Owner: true, InReplyTo: ask.ID, Body: "continue"})
				mailEvent(t, st, mailbox.Message{Body: "other event"})
			}
			switch kind {
			case "unpushed":
				drv.GitStates[ln.ID] = lane.GitState{UnpushedCommits: 1, OldestUnpushedAt: clk.Now().Add(-time.Hour)}
			case "disk":
				drv.DiskUsage[ln.ID] = 95
			case "worker", "default":
				msg := mailbox.Message{ID: "q-" + kind, LaneID: ln.ID, Question: true, Body: "choose"}
				if kind == "default" {
					msg.ID = ""
				}
				mailEvent(t, st, msg)
				mailEvent(t, st, mailbox.Message{Body: "other event"})
			}
			if kind == "unpushed" || kind == "disk" {
				mailEvent(t, st, mailbox.Message{Body: "other event"})
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if drv.calls != 1 {
				t.Fatalf("steer submissions = %d, want 1", drv.calls)
			}
			undelivered := 0
			otherSeen := false
			for _, batch := range judge.Batches {
				for _, ev := range batch {
					if ev.Source == lane.AnswerDeliverySource && strings.Contains(ev.Summary, "answer not delivered") {
						undelivered++
					}
					if ev.Summary == "other event" {
						otherSeen = true
					}
				}
			}
			if undelivered != 1 {
				t.Fatalf("undelivered judgments = %d in %+v, want 1", undelivered, judge.Batches)
			}
			if !otherSeen {
				t.Fatalf("judgment batches = %+v; want the following event consumed in the same Step", judge.Batches)
			}
			decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1", LaneID: ln.ID})
			if err != nil || len(decs) == 0 {
				t.Fatalf("decisions = %+v, err=%v; want recorded action", decs, err)
			}
			recordedKey := false
			for _, dec := range decs {
				var args controller.DecisionArgs
				if json.Unmarshal(dec.Args, &args) == nil && args.ActionKey != "" {
					recordedKey = true
				}
			}
			if !recordedKey {
				t.Fatalf("decisions = %+v; want an action key", decs)
			}
		})
	}
}

func TestLaneWatchQuestionsAreUrgent(t *testing.T) {
	t.Parallel()
	for _, outcome := range []lane.ReportOutcome{lane.OutcomeNeedsInput, lane.OutcomeBlocked} {
		t.Run(string(outcome), func(t *testing.T) {
			clk, st, drv, judge := setupTestEnv(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusRunning, TerminalID: "t-1", Phase: recipe.PhaseSWE}); err != nil {
				t.Fatal(err)
			}
			ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
			if err := ctrl.StartWatches(ctx); err != nil {
				t.Fatal(err)
			}
			drv.Emit(lane.Event{RunID: "run-1", LaneID: "question", TerminalID: "t-1", Kind: lane.EventPhaseReported, Report: &lane.PhaseReport{TerminalID: "t-1", Phase: recipe.PhaseSWE, Outcome: outcome, Summary: "Restore main?"}})
			waitFor(t, func() bool {
				events, _ := st.Events().List(ctx, store.EventFilter{RunID: "run-1", Kinds: []store.EventKind{store.EventLaneReport}})
				return len(events) == 1
			})
			events, err := st.Events().List(ctx, store.EventFilter{RunID: "run-1", Kinds: []store.EventKind{store.EventLaneReport}})
			if err != nil || !events[0].Urgent {
				t.Fatalf("watch events=%+v err=%v", events, err)
			}
			if err := ctrl.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if judge.BatchCount() != 1 {
				t.Fatalf("batches=%d, want immediate judgment", judge.BatchCount())
			}
		})
	}
}

func TestLaneReportQuestionRecoveryIsUrgent(t *testing.T) {
	t.Parallel()
	clk, st, drv, judge := setupTestEnv(t)
	ctx := t.Context()
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusBlocked, TerminalID: "t-1", Phase: recipe.PhaseSWE, Question: "Restore main?"}); err != nil {
		t.Fatal(err)
	}
	ev := appendReportEvent(t, st, "question", lane.PhaseReport{TerminalID: "t-1", Phase: recipe.PhaseSWE, Outcome: lane.OutcomeNeedsInput, Summary: "Restore main?"})
	if err := st.Events().SetCursor(ctx, "run-1", controller.DefaultConsumer, ev.Seq); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, judge)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if judge.BatchCount() != 1 || len(judge.Batches[0]) != 1 || !judge.Batches[0][0].Urgent {
		t.Fatalf("recovered question batches=%+v, want one urgent question", judge.Batches)
	}
}
