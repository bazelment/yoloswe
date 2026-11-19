package dispatcher_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxfake "github.com/bazelment/yoloswe/orca/mailbox/fake"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

func setupTestEnv(t *testing.T) (*clock.FakeClock, *store.MemoryStore, *lane.FakeDriver) {
	t.Helper()
	start := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()

	if err := st.Runs().Create(t.Context(), store.Run{
		ID:        "run-1",
		Objective: "Build the Nebula swarm orchestrator harness in orca/",
		PlanRef:   "post:5aaec193-a1f1-4009-aa55-f6174d583653",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	return clk, st, drv
}

func TestOwnerAnswerResolvesSelectedAskAndSteersItsLane(t *testing.T) {
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()
	deadline := clk.Now().Add(time.Hour)
	for _, ask := range []controller.OwnerAsk{
		{ID: "q1", RunID: "run-1", Question: "Question one?", Options: []string{"A"}, Default: "A", Deadline: deadline},
		{ID: "q2", RunID: "run-1", LaneID: "lane-q2", Question: "Question two?", Options: []string{"A", "B"}, Default: "A", Deadline: deadline},
	} {
		raw, err := json.Marshal(ask)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Directives().Add(ctx, store.Directive{ID: "ask-" + ask.ID, RunID: "run-1", Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Ref: "owner_ask:" + ask.ID, Scope: ask.LaneID, Text: string(raw), ExpiresAt: &deadline, Status: store.DirectiveActive}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-q2", RunID: "run-1", Status: lane.StatusBlocked, SessionID: "sess-q2", TerminalID: "term-q2"}); err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1", HolderID: "test-controller"}, st, drv, clk, controller.NewFakeJudge())
	fake := model.NewFakeClient(toolUseResponse(model.ToolCall{ID: "answer", Name: dispatcher.ToolOwnerAnswer, Input: json.RawMessage(`{"ask_id":"q2","answer":"go with B"}`)}), endTurnResponse("done"))
	var logs bytes.Buffer
	d := dispatcher.New(dispatcher.Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))}, st, drv, clk, fake)
	d.SetOwnerAnswer(func(ctx context.Context, runID, askID, answer string) error {
		if runID != "run-1" {
			t.Fatalf("runID=%q", runID)
		}
		return ctrl.AnswerOwnerAskText(ctx, askID, answer)
	})
	ev, err := st.Events().Append(ctx, store.Event{RunID: "run-1", Kind: store.EventOwnerMessage, Class: store.EventClassJudgment, Summary: "go with B for q2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, "run-1", []store.Event{ev}); err != nil {
		t.Fatal(err)
	}
	q2, err := st.Directives().Get(ctx, "run-1", "ask-q2")
	if err != nil {
		t.Fatal(err)
	}
	q1, err := st.Directives().Get(ctx, "run-1", "ask-q1")
	if err != nil {
		t.Fatal(err)
	}
	if q2.Status != store.DirectiveHandled || q1.Status != store.DirectiveActive {
		t.Fatalf("q2=%s q1=%s", q2.Status, q1.Status)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "lane-q2")
	if err != nil {
		t.Fatal(err)
	}
	if ln.Status != lane.StatusRunning {
		t.Fatalf("lane status=%s", ln.Status)
	}
	requests := drv.Steers
	if len(requests) != 1 || requests[0].LaneID != "lane-q2" || requests[0].Message != "go with B" {
		t.Fatalf("steers=%+v", requests)
	}
	want := `level=INFO msg="tool call" actor=dispatcher turn=1 tool=` + dispatcher.ToolOwnerAnswer + ` args="{\"ask_id\":\"q2\",\"answer\":\"go with B\"}" result_bytes=`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("tool call log missing %q:\n%s", want, logs.String())
	}
}

func TestInnerLoop_StalledLaneProducesLaneRetry(t *testing.T) {
	t.Parallel()

	t.Run("ReplayFixtureOnStalledLaneBatch", func(t *testing.T) {
		t.Parallel()
		clk, st, drv := setupTestEnv(t)
		ctx := t.Context()

		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:         "item-alpha",
			RunID:      "run-1",
			Title:      "Implement dispatcher",
			Acceptance: "tests green",
			Priority:   store.PriorityP0,
			Status:     store.BacklogInProgress,
			LaneID:     "lane-alpha",
		}); err != nil {
			t.Fatalf("Add backlog: %v", err)
		}

		ln := lane.Lane{
			ID:          "lane-alpha",
			RunID:       "run-1",
			ItemID:      "item-alpha",
			Title:       "Implement dispatcher",
			Status:      lane.StatusBlocked,
			RecipeName:  recipe.DefaultName,
			Attempt:     1,
			LastEventAt: clk.Now().Add(-50 * time.Minute),
		}
		ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-alpha-1", "term-alpha-swe-1")
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}

		stallEv, err := st.Events().Append(ctx, store.Event{
			RunID:   "run-1",
			Kind:    store.EventStall,
			Class:   store.EventClassJudgment,
			LaneID:  "lane-alpha",
			Source:  controller.ActorController,
			Summary: "lane lane-alpha stalled in phase swe (round 1, attempt 1) after 50m0s",
		})
		if err != nil {
			t.Fatalf("Append stall event: %v", err)
		}

		fakeClient, err := model.LoadFakeClient("testdata/stalled_lane_retry_replay.json")
		if err != nil {
			t.Fatalf("LoadFakeClient: %v", err)
		}

		disp := dispatcher.New(dispatcher.Config{}, st, drv, clk, fakeClient)
		res, err := disp.Dispatch(ctx, "run-1", []store.Event{stallEv})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}

		if res.TurnCount != 2 {
			t.Fatalf("TurnCount = %d, want 2", res.TurnCount)
		}
		if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolLaneRetry {
			t.Fatalf("Accepted = %+v, want 1 lane.retry action", res.Accepted)
		}
		if len(res.Rejected) != 0 {
			t.Fatalf("Rejected = %v, want none", res.Rejected)
		}
		if res.Snapshot.EstimatedTokens <= 0 || res.Snapshot.EstimatedTokens >= 30000 {
			t.Fatalf("Snapshot.EstimatedTokens = %d, want in (0, 30000)", res.Snapshot.EstimatedTokens)
		}

		if len(fakeClient.Requests) != 2 {
			t.Fatalf("len(Requests) = %d, want 2", len(fakeClient.Requests))
		}
		if !strings.Contains(fakeClient.Requests[0].System, dispatcher.SystemPromptVersion) {
			t.Fatalf("request System missing version %q", dispatcher.SystemPromptVersion)
		}

		if len(drv.PhaseStarts) != 1 {
			t.Fatalf("len(drv.PhaseStarts) = %d, want 1", len(drv.PhaseStarts))
		}
		started := drv.PhaseStarts[0]
		if started.LaneID != "lane-alpha" || started.Phase != recipe.PhaseSWE || started.Round != 2 || started.SessionID != "sess-alpha-1" {
			t.Fatalf("PhaseStarts[0] = %+v", started)
		}

		gotLane, err := st.Lanes().Get(ctx, "run-1", "lane-alpha")
		if err != nil {
			t.Fatalf("Get lane: %v", err)
		}
		if gotLane.Status != lane.StatusRunning || gotLane.Phase != recipe.PhaseSWE || gotLane.Round != 2 {
			t.Fatalf("lane after retry = %+v, want running in swe round 2", gotLane)
		}
		if gotLane.MaxRound(recipe.PhaseSWE) != 2 {
			t.Fatalf("MaxRound(swe) = %d, want 2", gotLane.MaxRound(recipe.PhaseSWE))
		}

		decs, err := st.Decisions().List(ctx, store.DecisionFilter{
			RunID:    "run-1",
			Category: store.DecisionJudgment,
		})
		if err != nil {
			t.Fatalf("List decisions: %v", err)
		}
		if len(decs) != 1 {
			t.Fatalf("len(decisions) = %d, want 1: %+v", len(decs), decs)
		}
		dec := decs[0]
		if dec.Tool != dispatcher.ToolLaneRetry || dec.LaneID != "lane-alpha" || !dec.Vetted || dec.Actor != dispatcher.ActorDispatcher {
			t.Fatalf("decision = %+v", dec)
		}
		if dec.Reason != "swe round 1 stalled after 45m timeout" {
			t.Fatalf("decision.Reason = %q", dec.Reason)
		}
		if dec.InputTokens != 1850 || dec.OutputTokens != 95 {
			t.Fatalf("decision token attribution = (%d, %d), want (1850, 95)", dec.InputTokens, dec.OutputTokens)
		}
	})

	t.Run("EndToEndWithControllerStallDetection", func(t *testing.T) {
		t.Parallel()
		clk, st, drv := setupTestEnv(t)
		ctx := t.Context()

		if err := st.Lanes().Upsert(ctx, lane.Lane{
			ID:     "lane-alpha",
			RunID:  "run-1",
			Title:  "Stalling lane",
			Status: lane.StatusPlanned,
		}); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}

		fakeClient, err := model.LoadFakeClient("testdata/stalled_lane_retry_replay.json")
		if err != nil {
			t.Fatalf("LoadFakeClient: %v", err)
		}

		disp := dispatcher.New(dispatcher.Config{}, st, drv, clk, fakeClient)
		ctrl := controller.New(controller.Config{
			RunID:            "run-1",
			JudgmentDebounce: -1,
			StallTimeout:     45 * time.Minute,
		}, st, drv, clk, disp)

		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step 1 (spawn): %v", err)
		}
		if len(drv.Spawns) != 1 {
			t.Fatalf("len(Spawns) = %d, want 1", len(drv.Spawns))
		}

		clk.Advance(46 * time.Minute)
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("Step 2 (stall -> judge -> retry): %v", err)
		}

		if len(drv.Stops) != 1 {
			t.Fatalf("len(Stops) = %d, want 1 (stalled terminal stopped by controller)", len(drv.Stops))
		}
		if len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Round != 2 {
			t.Fatalf("PhaseStarts = %+v, want round 2 started by dispatcher", drv.PhaseStarts)
		}
		req := drv.PhaseStarts[0]
		if want := drv.ReportInstructions(req.RunID, req.LaneID, req.Phase, req.Round); !strings.Contains(req.Brief, want) {
			t.Fatalf("retry brief=%q, missing %q", req.Brief, want)
		}

		gotLane, err := st.Lanes().Get(ctx, "run-1", "lane-alpha")
		if err != nil {
			t.Fatalf("Get lane: %v", err)
		}
		if gotLane.Status != lane.StatusRunning || gotLane.Round != 2 {
			t.Fatalf("lane = %+v, want running on round 2", gotLane)
		}
	})
}

func TestDispatcher_FullToolKitReplay(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	// Seed initial backlog items: item-done, item-open, item-drop.
	for _, it := range []store.BacklogItem{
		{
			ID:       "item-done",
			RunID:    "run-1",
			Title:    "Completed prerequisite",
			Priority: store.PriorityP0,
			Status:   store.BacklogDone,
		},
		{
			ID:         "item-open",
			RunID:      "run-1",
			Title:      "Open feature item",
			Acceptance: "tests pass",
			Priority:   store.PriorityP1,
			Status:     store.BacklogOpen,
			DependsOn:  []string{"item-done"},
		},
		{
			ID:       "item-drop",
			RunID:    "run-1",
			Title:    "Obsolete item to drop",
			Priority: store.PriorityP3,
			Status:   store.BacklogOpen,
		},
		{
			ID:       "item-doomed",
			RunID:    "run-1",
			Title:    "Doomed lane item",
			Priority: store.PriorityP2,
			Status:   store.BacklogInProgress,
			LaneID:   "lane-doomed",
		},
	} {
		if err := st.Backlog().Add(ctx, it); err != nil {
			t.Fatalf("Add backlog %s: %v", it.ID, err)
		}
	}

	// Increase concurrency limit on default recipe so 3+ lanes can run concurrently.
	defRecipe := recipe.Default()
	defRecipe.RunID = "run-1"
	defRecipe.Limits.MaxConcurrentLanes = 8
	if _, err := st.Recipes().Save(ctx, defRecipe); err != nil {
		t.Fatalf("Save default recipe: %v", err)
	}

	// Seed lanes: lane-active (running), lane-stalled (blocked), lane-doomed (running).
	laneActive := lane.Lane{
		ID:          "lane-active",
		RunID:       "run-1",
		Title:       "Active lane with PR",
		Branch:      "swarm/lane-active",
		BaseBranch:  "main",
		Status:      lane.StatusRunning,
		PR:          42,
		PRHead:      "abcdef123456",
		ApprovalSHA: "oldsha000000",
		Checks:      "red",
		Attempt:     1,
		LastEventAt: clk.Now().Add(-5 * time.Minute),
	}
	laneActive.RecordAttempt(recipe.PhaseSWE, 1, "sess-active-1", "term-active-swe-1")

	laneStalled := lane.Lane{
		ID:          "lane-stalled",
		RunID:       "run-1",
		Title:       "Stalled lane",
		Status:      lane.StatusBlocked,
		Attempt:     1,
		LastEventAt: clk.Now().Add(-50 * time.Minute),
	}
	laneStalled.RecordAttempt(recipe.PhaseSWE, 1, "sess-stalled-1", "term-stalled-swe-1")
	drv.SetReport("term-stalled-swe-1", lane.PhaseReport{
		LaneID:       "lane-stalled",
		TerminalID:   "term-stalled-swe-1",
		Phase:        recipe.PhaseSWE,
		Round:        1,
		Outcome:      lane.OutcomeBlocked,
		HeadSHA:      "111122223333",
		Reason:       "timed out waiting for build lock",
		RiskiestStep: "bazel test //orca/...",
		Observed:     "sandbox -> lock contention",
		Summary:      strings.Repeat("Detailed phase report body line. ", 20),
	})

	laneDoomed := lane.Lane{
		ID:          "lane-doomed",
		RunID:       "run-1",
		ItemID:      "item-doomed",
		Title:       "Doomed lane",
		Status:      lane.StatusRunning,
		Attempt:     1,
		LastEventAt: clk.Now().Add(-10 * time.Minute),
	}
	laneDoomed.RecordAttempt(recipe.PhaseSWE, 1, "sess-doomed-1", "term-doomed-swe-1")

	for _, ln := range []lane.Lane{laneActive, laneStalled, laneDoomed} {
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			t.Fatalf("Upsert lane %s: %v", ln.ID, err)
		}
	}

	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventCI,
		Class:   store.EventClassJudgment,
		LaneID:  "lane-active",
		Summary: "CI check Lint failed on PR #42",
		Payload: json.RawMessage(`{"status":"red","failed_check":"Lint","pr":42,"head_sha":"abcdef123456"}`),
	}); err != nil {
		t.Fatalf("Append CI event: %v", err)
	}

	replayClient, err := model.LoadFakeClient("testdata/full_toolkit_replay.json")
	if err != nil {
		t.Fatalf("LoadFakeClient: %v", err)
	}

	var plannerReason, plannerRef string
	disp := dispatcher.New(dispatcher.Config{
		AllowMultiAction: true, // scripts every tool of the kit into one batch
		RecipeCompiler: dispatcher.RecipeCompilerFunc(func(_ context.Context, base recipe.Recipe, override string) (recipe.Recipe, error) {
			return dispatcher.CompileRecipeOverride(base, override), nil
		}),
		OnPlannerRequest: func(_ context.Context, _, reason, planRef string) error {
			plannerReason = reason
			plannerRef = planRef
			return nil
		},
	}, st, drv, clk, replayClient)

	// Queue a steer on the dispatcher thread before the turn starts to verify steer draining.
	if _, err := st.Threads().StartThread(ctx, store.Thread{
		ID:    "dispatcher-run-1",
		RunID: "run-1",
		Role:  store.ThreadRoleDispatcher,
	}); err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	if err := st.Threads().SteerTurn(ctx, "dispatcher-run-1", "", "Prioritize item-open and drop item-drop"); err != nil {
		t.Fatalf("SteerTurn: %v", err)
	}

	ctrl := controller.New(controller.Config{
		RunID:            "run-1",
		JudgmentDebounce: -1,
	}, st, drv, clk, disp)

	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("ctrl.Step (dispatch full toolkit): %v", err)
	}
	res := disp.LastResult()
	if res == nil {
		t.Fatalf("expected non-nil LastResult")
	}

	if res.TurnCount != 3 {
		t.Fatalf("TurnCount = %d, want 3", res.TurnCount)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("Rejected = %v, want none", res.Rejected)
	}
	if len(res.Accepted) != 14 {
		t.Fatalf("len(Accepted) = %d, want 14: %+v", len(res.Accepted), res.Accepted)
	}
	if !strings.Contains(res.Snapshot.Text, "Prioritize item-open and drop item-drop") {
		t.Fatalf("Snapshot missing drained steer message:\n%s", res.Snapshot.Text)
	}

	// Verify bounded read outputs were sent back to the model in turn 2's request messages.
	if len(replayClient.Requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(replayClient.Requests))
	}
	turn2UserMsg := replayClient.Requests[1].Messages[2]
	if len(turn2UserMsg.Content) != 3 {
		t.Fatalf("turn 2 tool_result count = %d, want 3", len(turn2UserMsg.Content))
	}
	for i, blk := range turn2UserMsg.Content {
		if blk.IsError {
			t.Fatalf("read tool %d returned error: %s", i, blk.Content)
		}
		if len(blk.Content) > 256 {
			t.Fatalf("read tool %d output length = %d, exceeds max_bytes=256", i, len(blk.Content))
		}
	}

	itemNew, err := st.Backlog().Get(ctx, "run-1", "item-new")
	if err != nil {
		t.Fatalf("Get item-new: %v", err)
	}
	if itemNew.Priority != store.PriorityP1 || itemNew.Title != "Add replay evaluation harness" {
		t.Fatalf("item-new = %+v", itemNew)
	}

	itemOpen, err := st.Backlog().Get(ctx, "run-1", "item-open")
	if err != nil {
		t.Fatalf("Get item-open: %v", err)
	}
	if itemOpen.Priority != store.PriorityP0 || itemOpen.Status != store.BacklogInProgress || itemOpen.LaneID != "lane-open" {
		t.Fatalf("item-open = %+v", itemOpen)
	}

	if itemOpen.Order != 1 || itemNew.Order != 2 {
		t.Fatalf("orders after reorder: item-open=%d item-new=%d, want 1 and 2", itemOpen.Order, itemNew.Order)
	}

	itemDrop, err := st.Backlog().Get(ctx, "run-1", "item-drop")
	if err != nil {
		t.Fatalf("Get item-drop: %v", err)
	}
	if itemDrop.Status != store.BacklogDropped {
		t.Fatalf("item-drop.Status = %q, want dropped", itemDrop.Status)
	}

	gotOpenLane, err := st.Lanes().Get(ctx, "run-1", "lane-open")
	if err != nil {
		t.Fatalf("Get lane-open: %v", err)
	}
	if gotOpenLane.Status != lane.StatusRunning || gotOpenLane.Phase != recipe.PhaseSWE || gotOpenLane.Round != 1 {
		t.Fatalf("lane-open = %+v", gotOpenLane)
	}
	overrideRec, err := st.Recipes().Get(ctx, "run-1", gotOpenLane.RecipeName, gotOpenLane.RecipeVersion)
	if err != nil {
		t.Fatalf("Get compiled override recipe: %v", err)
	}
	wantPhases := []string{recipe.PhaseSWE, recipe.PhaseIntegrate}
	if got := overrideRec.PhaseNames(); fmt.Sprint(got) != fmt.Sprint(wantPhases) {
		t.Fatalf("override recipe phases = %v, want %v", got, wantPhases)
	}
	if overrideRec.MergePolicy.RequireReviewPass {
		t.Fatalf("expected override recipe RequireReviewPass=false")
	}

	if len(drv.Steers) != 1 || drv.Steers[0].LaneID != "lane-active" || !strings.Contains(drv.Steers[0].Message, "Fix the failing lint check") {
		t.Fatalf("drv.Steers = %+v", drv.Steers)
	}

	gotStalled, err := st.Lanes().Get(ctx, "run-1", "lane-stalled")
	if err != nil {
		t.Fatalf("Get lane-stalled: %v", err)
	}
	if gotStalled.Status != lane.StatusRunning || gotStalled.Round != 2 {
		t.Fatalf("lane-stalled = %+v, want running on round 2", gotStalled)
	}

	if len(drv.Stops) != 1 || drv.Stops[0].LaneID != "lane-doomed" {
		t.Fatalf("drv.Stops = %+v", drv.Stops)
	}
	gotDoomed, err := st.Lanes().Get(ctx, "run-1", "lane-doomed")
	if err != nil {
		t.Fatalf("Get lane-doomed: %v", err)
	}
	if gotDoomed.Status != lane.StatusFailed {
		t.Fatalf("lane-doomed.Status = %q, want failed", gotDoomed.Status)
	}
	gotDoomedItem, err := st.Backlog().Get(ctx, "run-1", "item-doomed")
	if err != nil {
		t.Fatalf("Get item-doomed: %v", err)
	}
	if gotDoomedItem.Status != store.BacklogBlocked {
		t.Fatalf("item-doomed.Status = %q, want blocked", gotDoomedItem.Status)
	}

	gotActive, err := st.Lanes().Get(ctx, "run-1", "lane-active")
	if err != nil {
		t.Fatalf("Get lane-active: %v", err)
	}
	if gotActive.Status != lane.StatusBlocked {
		t.Fatalf("lane-active.Status = %q, want blocked while owner ask is open", gotActive.Status)
	}
	askDir, err := st.Directives().Get(ctx, "run-1", "ask-ask-schema")
	if err != nil {
		t.Fatalf("Get owner ask directive: %v", err)
	}
	if askDir.Status != store.DirectiveActive {
		t.Fatalf("askDir.Status = %q, want active", askDir.Status)
	}

	noteDir, err := st.Directives().Get(ctx, "run-1", "note-flaky-ci")
	if err != nil {
		t.Fatalf("Get directive note: %v", err)
	}
	if noteDir.Kind != store.DirectiveNote || noteDir.Scope != "lane-active" || noteDir.ExpiresAt == nil {
		t.Fatalf("noteDir = %+v", noteDir)
	}

	if plannerReason != "Milestone 1 items spawned; reconcile phase 2 plan" || plannerRef != "post:5aaec193" {
		t.Fatalf("planner callback got reason=%q ref=%q", plannerReason, plannerRef)
	}

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{
		RunID:    "run-1",
		Category: store.DecisionJudgment,
	})
	if err != nil {
		t.Fatalf("List decisions: %v", err)
	}
	if len(decs) != 14 {
		t.Fatalf("len(decisions) = %d, want 14", len(decs))
	}
	for i, dec := range decs {
		if !dec.Vetted {
			t.Fatalf("decision %d (%s) Vetted = false", i, dec.Tool)
		}
		if strings.TrimSpace(dec.Reason) == "" {
			t.Fatalf("decision %d (%s) missing Reason", i, dec.Tool)
		}
	}

	clk.Advance(31 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("ctrl.Step after owner ask expiry: %v", err)
	}
	gotActiveAfter, err := st.Lanes().Get(ctx, "run-1", "lane-active")
	if err != nil {
		t.Fatalf("Get lane-active after ask expiry: %v", err)
	}
	if gotActiveAfter.Status != lane.StatusRunning {
		t.Fatalf("lane-active.Status after ask expiry = %q, want running", gotActiveAfter.Status)
	}
	if len(drv.Steers) != 2 || drv.Steers[1].Message != "Keep v0 support for backwards compatibility." {
		t.Fatalf("expected default steer_note to be sent on ask expiry, got Steers=%+v", drv.Steers)
	}
}

func TestDispatcher_InvariantRejections(t *testing.T) {
	t.Parallel()

	t.Run("DirectVetSuiteCoversAllInvariants", func(t *testing.T) {
		t.Parallel()
		clk, st, _ := setupTestEnv(t)
		ctx := t.Context()

		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:       "item-unfinished-dep",
			RunID:    "run-1",
			Title:    "Unfinished dependency",
			Priority: store.PriorityP0,
			Status:   store.BacklogOpen,
		}); err != nil {
			t.Fatalf("Add backlog: %v", err)
		}
		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:        "item-blocked-by-dep",
			RunID:     "run-1",
			Title:     "Depends on unfinished item",
			Priority:  store.PriorityP1,
			Status:    store.BacklogOpen,
			DependsOn: []string{"item-unfinished-dep"},
		}); err != nil {
			t.Fatalf("Add backlog: %v", err)
		}
		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:       "item-ready",
			RunID:    "run-1",
			Title:    "Ready item",
			Priority: store.PriorityP1,
			Status:   store.BacklogOpen,
		}); err != nil {
			t.Fatalf("Add backlog: %v", err)
		}
		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:       "item-in-flight",
			RunID:    "run-1",
			Title:    "In flight item",
			Priority: store.PriorityP0,
			Status:   store.BacklogInProgress,
			LaneID:   "lane-running",
		}); err != nil {
			t.Fatalf("Add backlog: %v", err)
		}

		lnRunning := lane.Lane{
			ID:     "lane-running",
			RunID:  "run-1",
			ItemID: "item-in-flight",
			Status: lane.StatusRunning,
		}
		lnRunning.RecordAttempt(recipe.PhaseSWE, 1, "sess-run-1", "term-run-1")

		lnStaleReview := lane.Lane{
			ID:          "lane-stale",
			RunID:       "run-1",
			Status:      lane.StatusBlocked,
			Phase:       recipe.PhaseReview,
			Round:       1,
			PR:          99,
			PRHead:      "new-head-sha",
			ApprovalSHA: "old-approval-sha",
			Checks:      "green",
		}
		lnStaleReview.RecordAttempt(recipe.PhaseSWE, 1, "sess-stale-1", "term-stale-swe-1")
		lnStaleReview.RecordAttempt(recipe.PhaseReview, 1, "sess-stale-1", "term-stale-rev-1")

		lnDone := lane.Lane{
			ID:     "lane-done",
			RunID:  "run-1",
			Status: lane.StatusDone,
			Phase:  recipe.PhaseIntegrate,
			Round:  1,
		}
		lnNoSession := lane.Lane{
			ID:     "lane-nosess",
			RunID:  "run-1",
			Status: lane.StatusRunning,
			Phase:  recipe.PhaseSWE,
			Round:  1,
		}

		customSelfMerge := recipe.Default()
		customSelfMerge.RunID = "run-1"
		customSelfMerge.Name = "custom-automerge"
		customSelfMerge.MergePolicy.Mode = recipe.MergeModeSelf
		customSelfMerge.Limits.MaxBriefBytes = 64
		if _, err := st.Recipes().Save(ctx, customSelfMerge); err != nil {
			t.Fatalf("Save customSelfMerge: %v", err)
		}

		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:         "item-custom-recipe",
			RunID:      "run-1",
			Title:      "Item using custom-automerge recipe",
			Priority:   store.PriorityP1,
			Status:     store.BacklogOpen,
			RecipeName: "custom-automerge",
		}); err != nil {
			t.Fatalf("Add backlog item-custom-recipe: %v", err)
		}

		for _, ln := range []lane.Lane{lnRunning, lnStaleReview, lnDone, lnNoSession} {
			if err := st.Lanes().Upsert(ctx, ln); err != nil {
				t.Fatalf("Upsert lane %s: %v", ln.ID, err)
			}
		}

		view, err := dispatcher.LoadStateView(ctx, st, clk, "run-1", 256)
		if err != nil {
			t.Fatalf("LoadStateView: %v", err)
		}

		mustParse := func(tool, rawJSON string) dispatcher.ProposedAction {
			act, pErr := dispatcher.ParseAction("id", tool, json.RawMessage(rawJSON))
			if pErr != nil {
				t.Fatalf("ParseAction(%s): %v", tool, pErr)
			}
			return act
		}

		invs := dispatcher.DefaultInvariants()

		allowedCases := []struct {
			name   string
			action dispatcher.ProposedAction
		}{
			{
				name:   "SpawnBriefInstructingDirectMergeAllowedWhenRecipeSelfMerge",
				action: mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-ready","brief":"Implement and run gh pr merge","recipe_override":"lanes merge themselves"}`),
			},
			{
				name:   "SpawnBriefInstructingDirectMergeAllowedViaItemRecipe",
				action: mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-custom-recipe","brief":"Run gh pr merge"}`),
			},
			{
				name:   "SpawnBriefInstructingDirectMergeAllowedViaNamedRecipeOverride",
				action: mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-ready","brief":"Run gh pr merge","recipe_override":"custom-automerge"}`),
			},
		}
		for _, tc := range allowedCases {
			t.Run(tc.name, func(t *testing.T) {
				if vErr := dispatcher.VetAction(view, tc.action, invs); vErr != nil {
					t.Fatalf("expected allowed, got error: %v", vErr)
				}
			})
		}

		rejectedCases := []struct {
			name       string
			wantErrSub string
			action     dispatcher.ProposedAction
		}{
			{
				name:       "ForbiddenDirectMergeTool",
				action:     mustParse("lane.merge", `{"lane_id":"lane-stale"}`),
				wantErrSub: "no merge outside recipe",
			},
			{
				name:       "UnknownToolNotInToolkit",
				action:     mustParse("shell.exec", `{"cmd":"rm -rf /"}`),
				wantErrSub: "tool must be in dispatcher toolkit",
			},
			{
				name:       "SpawnBriefInstructingDirectMergeUnderControllerMode",
				action:     mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-ready","brief":"Implement and run gh pr merge --squash"}`),
				wantErrSub: "no merge outside recipe",
			},
			{
				name:       "SteerNoteInstructingDirectMergeUnderControllerMode",
				action:     mustParse(dispatcher.ToolLaneSteer, `{"lane_id":"lane-running","note":"Please run gh pr merge now"}`),
				wantErrSub: "no merge outside recipe",
			},
			{
				name:       "RetryIntoIntegrateWithStaleApproval",
				action:     mustParse(dispatcher.ToolLaneRetry, `{"lane_id":"lane-stale","phase":"integrate","round":1}`),
				wantErrSub: "no merge outside recipe",
			},
			{
				name:       "RetryIntoCompleteTargetForbidden",
				action:     mustParse(dispatcher.ToolLaneRetry, `{"lane_id":"lane-stale","phase":"complete","round":1}`),
				wantErrSub: "no merge outside recipe",
			},
			{
				name:       "CloseBacklogAsDoneWhileLaneRunning",
				action:     mustParse(dispatcher.ToolBacklogClose, `{"id":"item-in-flight","status":"done","reason":"shortcut"}`),
				wantErrSub: "no merge outside recipe",
			},
			{
				name:       "OwnerAskMissingOptions",
				action:     mustParse(dispatcher.ToolOwnerAsk, `{"question":"Which DB?","default":"pg","deadline":"30m"}`),
				wantErrSub: "owner.ask requires options, default, and deadline",
			},
			{
				name: "OwnerAskDefaultNotInOptions",
				// Built by hand: ParseAction itself turns a stated default into an
				// option, so only a non-parsed action can reach the vetter mismatched.
				action: dispatcher.ProposedAction{
					Tool: dispatcher.ToolOwnerAsk,
					OwnerAsk: &dispatcher.OwnerAskParams{
						Question: "Which DB?", Options: []string{"pg", "sqlite"}, Default: "mysql", Deadline: "30m",
					},
				},
				wantErrSub: "must be one of options",
			},
			{
				name:       "OwnerAskMissingDeadline",
				action:     mustParse(dispatcher.ToolOwnerAsk, `{"question":"Which DB?","options":["pg","sqlite"],"default":"pg"}`),
				wantErrSub: "deadline is required",
			},
			{
				name:       "OwnerAskPastDeadline",
				action:     mustParse(dispatcher.ToolOwnerAsk, `{"question":"Which DB?","options":["pg","sqlite"],"default":"pg","deadline":"-10m"}`),
				wantErrSub: "must be in the future",
			},
			{
				name:       "SpawnBriefExceedsSizeCap",
				action:     mustParse(dispatcher.ToolLaneSpawn, fmt.Sprintf(`{"item_id":"item-ready","brief":%q}`, strings.Repeat("x", 300))),
				wantErrSub: "brief size within cap",
			},
			{
				name:       "SpawnBriefExceedsItemRecipeSizeCap",
				action:     mustParse(dispatcher.ToolLaneSpawn, fmt.Sprintf(`{"item_id":"item-custom-recipe","brief":%q}`, strings.Repeat("x", 100))),
				wantErrSub: "brief size within cap",
			},
			{
				name:       "RetryReusingRecordedRound",
				action:     mustParse(dispatcher.ToolLaneRetry, `{"lane_id":"lane-stale","phase":"swe","round":1}`),
				wantErrSub: "must not reuse a recorded round",
			},
			{
				name:       "RetryExceedingMaxRounds",
				action:     mustParse(dispatcher.ToolLaneRetry, `{"lane_id":"lane-stale","phase":"swe","round":5}`),
				wantErrSub: "exceeds recipe",
			},
			{
				name:       "RetryUndeclaredPhase",
				action:     mustParse(dispatcher.ToolLaneRetry, `{"lane_id":"lane-stale","phase":"deploy","round":2}`),
				wantErrSub: "phase must be declared",
			},
			{
				name:       "RetryNonExistentLane",
				action:     mustParse(dispatcher.ToolLaneRetry, `{"lane_id":"lane-ghost","round":2}`),
				wantErrSub: "lane must exist",
			},
			{
				name:       "SpawnOntoOccupiedRunningLane",
				action:     mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-ready","lane_id":"lane-running","brief":"work"}`),
				wantErrSub: "occupied lane",
			},
			{
				name:       "SpawnOntoOccupiedBlockedLane",
				action:     mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-ready","lane_id":"lane-stale","brief":"work"}`),
				wantErrSub: "occupied lane",
			},
			{
				name:       "SpawnForAlreadyInProgressBacklogItem",
				action:     mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-in-flight","lane_id":"lane-fresh","brief":"work"}`),
				wantErrSub: "already in progress",
			},
			{
				name:       "SpawnItemWithUnfinishedDependency",
				action:     mustParse(dispatcher.ToolLaneSpawn, `{"item_id":"item-blocked-by-dep","brief":"work"}`),
				wantErrSub: "depends on",
			},
			{
				name:       "SteerLaneWithoutStartedSession",
				action:     mustParse(dispatcher.ToolLaneSteer, `{"lane_id":"lane-nosess","note":"hello"}`),
				wantErrSub: "lane state must allow steer, retry, or stop",
			},
			{
				name:       "SteerTerminalLane",
				action:     mustParse(dispatcher.ToolLaneSteer, `{"lane_id":"lane-done","note":"hello"}`),
				wantErrSub: "lane state must allow steer, retry, or stop",
			},
			{
				name:       "StopLaneWithoutReason",
				action:     mustParse(dispatcher.ToolLaneStop, `{"lane_id":"lane-running","reason":"   "}`),
				wantErrSub: "lane state must allow steer, retry, or stop",
			},
			{
				name:       "StopTerminalLane",
				action:     mustParse(dispatcher.ToolLaneStop, `{"lane_id":"lane-done","reason":"done already"}`),
				wantErrSub: "lane state must allow steer, retry, or stop",
			},
			{
				name:       "RetryRunningLane",
				action:     mustParse(dispatcher.ToolLaneRetry, `{"lane_id":"lane-running","phase":"swe","round":2}`),
				wantErrSub: "lane state must allow steer, retry, or stop",
			},
			{
				name:       "BacklogAddDuplicateID",
				action:     mustParse(dispatcher.ToolBacklogAdd, `{"id":"item-ready","title":"Dup"}`),
				wantErrSub: "backlog mutation invariants",
			},
			{
				name:       "BacklogAddSelfDependency",
				action:     mustParse(dispatcher.ToolBacklogAdd, `{"id":"item-self","title":"Self","depends_on":["item-self"]}`),
				wantErrSub: "backlog mutation invariants",
			},
			{
				name:       "BacklogUpdateTerminalStatusForbidden",
				action:     mustParse(dispatcher.ToolBacklogUpdate, `{"id":"item-ready","status":"done","reason":"use close instead"}`),
				wantErrSub: "backlog mutation invariants",
			},
			{
				name:       "BacklogReorderDuplicateIDs",
				action:     mustParse(dispatcher.ToolBacklogReorder, `{"item_ids":["item-ready","item-ready"]}`),
				wantErrSub: "backlog mutation invariants",
			},
			{
				name:       "DirectiveNoteEmptyFact",
				action:     mustParse(dispatcher.ToolDirectiveNote, `{"fact":"  "}`),
				wantErrSub: "directive.note and planner.request invariants",
			},
			{
				name:       "DirectiveNotePastExpiry",
				action:     mustParse(dispatcher.ToolDirectiveNote, `{"fact":"stale note","expiry":"-5m"}`),
				wantErrSub: "directive.note and planner.request invariants",
			},
			{
				name:       "PlannerRequestEmptyReason",
				action:     mustParse(dispatcher.ToolPlannerRequest, `{"reason":""}`),
				wantErrSub: "directive.note and planner.request invariants",
			},
		}

		for _, tc := range rejectedCases {
			t.Run(tc.name, func(t *testing.T) {
				accepted, rejected := dispatcher.Vet(view, dispatcher.Proposal{Actions: []dispatcher.ProposedAction{tc.action}}, invs)
				if len(accepted) != 0 || len(rejected) != 1 {
					t.Fatalf("expected 1 rejection, got accepted=%d rejected=%v", len(accepted), rejected)
				}
				if !strings.Contains(rejected[0], tc.wantErrSub) {
					t.Fatalf("rejection %q does not contain %q", rejected[0], tc.wantErrSub)
				}
			})
		}
	})

	t.Run("ReplayLoopLogsRejectedDecisionsAndModelRecovers", func(t *testing.T) {
		t.Parallel()
		clk, st, drv := setupTestEnv(t)
		ctx := t.Context()

		ln := lane.Lane{
			ID:     "lane-1",
			RunID:  "run-1",
			Status: lane.StatusBlocked,
			Phase:  recipe.PhaseSWE,
			Round:  1,
		}
		ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-1", "term-1")
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}

		// Turn 1: model proposes invalid owner.ask (missing default/deadline), forbidden lane.merge,
		// and lane.retry reusing round 1.
		// Turn 2: after seeing the invariant violation tool_results, model proposes valid lane.retry on round 2.
		// Turn 3: model finishes turn.
		fakeClient := model.NewFakeClient(
			model.Response{
				ID:         "msg-rej-1",
				Model:      dispatcher.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("c1", dispatcher.ToolOwnerAsk, json.RawMessage(`{"question":"What now?"}`)),
					model.NewToolUseBlock("c2", "lane.merge", json.RawMessage(`{"lane_id":"lane-1"}`)),
					model.NewToolUseBlock("c3", dispatcher.ToolLaneRetry, json.RawMessage(`{"lane_id":"lane-1","phase":"swe","round":1}`)),
				),
				Usage: model.Usage{InputTokens: 500, OutputTokens: 80},
			},
			model.Response{
				ID:         "msg-rej-2",
				Model:      dispatcher.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("c4", dispatcher.ToolLaneRetry, json.RawMessage(`{"lane_id":"lane-1","phase":"swe","round":2,"reason":"retry on unused round 2"}`)),
				),
				Usage: model.Usage{InputTokens: 700, OutputTokens: 50},
			},
			model.Response{
				ID:         "msg-rej-3",
				Model:      dispatcher.DefaultModel,
				StopReason: model.StopReasonEndTurn,
				Message: model.NewAssistantMessage(
					model.NewTextBlock("Recovered and retried lane-1 on round 2."),
				),
				Usage: model.Usage{InputTokens: 800, OutputTokens: 20},
			},
		)

		disp := dispatcher.New(dispatcher.Config{}, st, drv, clk, fakeClient)
		res, err := disp.Dispatch(ctx, "run-1", []store.Event{
			{ID: "ev-stall", RunID: "run-1", Kind: store.EventStall, LaneID: "lane-1", Seq: 1},
		})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}

		if len(res.Rejected) != 3 {
			t.Fatalf("len(Rejected) = %d, want 3: %v", len(res.Rejected), res.Rejected)
		}
		if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolLaneRetry {
			t.Fatalf("Accepted = %+v, want 1 lane.retry", res.Accepted)
		}

		decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
		if err != nil {
			t.Fatalf("List decisions: %v", err)
		}
		if len(decs) != 4 {
			t.Fatalf("len(decisions) = %d, want 4 (3 rejected + 1 accepted)", len(decs))
		}
		for i := 0; i < 3; i++ {
			if decs[i].Vetted {
				t.Fatalf("decision %d should have Vetted=false: %+v", i, decs[i])
			}
			if !strings.HasPrefix(decs[i].Result, "rejected:") {
				t.Fatalf("decision %d Result = %q, want prefix 'rejected:'", i, decs[i].Result)
			}
		}
		if !decs[3].Vetted || decs[3].Tool != dispatcher.ToolLaneRetry {
			t.Fatalf("decision 3 = %+v, want vetted lane.retry", decs[3])
		}
	})
}

func TestDispatcher_TurnBound(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	ln := lane.Lane{
		ID:     "lane-loop",
		RunID:  "run-1",
		Status: lane.StatusRunning,
		Phase:  recipe.PhaseSWE,
		Round:  1,
		PR:     10,
		Checks: "green",
	}
	ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-1", "term-1")
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	var responses []model.Response
	for i := 1; i <= 5; i++ {
		responses = append(responses, model.Response{
			ID:         fmt.Sprintf("msg-bound-%d", i),
			Model:      dispatcher.DefaultModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock(
					fmt.Sprintf("call-%d", i),
					dispatcher.ToolReadPR,
					json.RawMessage(`{"lane_id":"lane-loop"}`),
				),
			),
			Usage: model.Usage{InputTokens: 200, OutputTokens: 30},
		})
	}
	fakeClient := model.NewFakeClient(responses...)

	disp := dispatcher.New(dispatcher.Config{
		MaxTurns: 3,
	}, st, drv, clk, fakeClient)

	res, err := disp.Dispatch(ctx, "run-1", []store.Event{
		{ID: "ev-1", RunID: "run-1", Kind: store.EventOwnerMessage, Summary: "check loop", Seq: 1},
	})
	if !errors.Is(err, model.ErrMaxTurnsExceeded) {
		t.Fatalf("Dispatch err = %v, want %v", err, model.ErrMaxTurnsExceeded)
	}
	if res == nil || res.TurnCount != 3 {
		t.Fatalf("res = %+v, want TurnCount=3", res)
	}

	turn, tErr := st.Threads().GetTurn(ctx, res.ThreadID, res.TurnID)
	if tErr != nil {
		t.Fatalf("GetTurn: %v", tErr)
	}
	if turn.Status != store.TurnStatusFailed {
		t.Fatalf("turn.Status = %q, want failed", turn.Status)
	}
}

func TestDispatcher_SnapshotSizeAndStructure(t *testing.T) {
	t.Parallel()
	clk, st, _ := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Runs().UpdateObjective(ctx, "run-1", strings.Repeat("Objective paragraph. ", 500), "post:plan-1"); err != nil {
		t.Fatalf("UpdateObjective: %v", err)
	}

	for i := 0; i < 80; i++ {
		if err := st.Directives().Add(ctx, store.Directive{
			ID:     fmt.Sprintf("dir-%03d", i),
			RunID:  "run-1",
			Kind:   store.DirectiveStanding,
			Source: store.DirectiveSourceOwner,
			Text:   fmt.Sprintf("Directive rule %d: %s", i, strings.Repeat("enforce strict gating ", 40)),
		}); err != nil {
			t.Fatalf("Add directive %d: %v", i, err)
		}
	}

	for i := 0; i < 120; i++ {
		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:          fmt.Sprintf("item-%03d", i),
			RunID:       "run-1",
			Title:       fmt.Sprintf("Backlog work item %d", i),
			Description: strings.Repeat("Detailed implementation notes. ", 50),
			Acceptance:  strings.Repeat("Tests and lint must pass. ", 30),
			Priority:    store.PriorityP1,
			Status:      store.BacklogOpen,
		}); err != nil {
			t.Fatalf("Add backlog %d: %v", i, err)
		}
	}

	for i := 0; i < 60; i++ {
		ln := lane.Lane{
			ID:          fmt.Sprintf("lane-%03d", i),
			RunID:       "run-1",
			ItemID:      fmt.Sprintf("item-%03d", i),
			Title:       fmt.Sprintf("Lane %d", i),
			Status:      lane.StatusRunning,
			Phase:       recipe.PhaseSWE,
			Round:       1,
			Attempt:     1,
			PR:          100 + i,
			PRHead:      "1234567890abcdef",
			ApprovalSHA: "fedcba0987654321",
			Checks:      "green",
			LastEventID: fmt.Sprintf("ev-%d", i),
			LastEventAt: clk.Now().Add(-time.Duration(i) * time.Minute),
		}
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			t.Fatalf("Upsert lane %d: %v", i, err)
		}
	}

	var steers []string
	for i := 0; i < 30; i++ {
		steers = append(steers, fmt.Sprintf("Owner steer %d: %s", i, strings.Repeat("prioritize critical path ", 20)))
	}

	var batch []store.Event
	for i := 0; i < 40; i++ {
		ev, err := st.Events().Append(ctx, store.Event{
			RunID:   "run-1",
			Kind:    store.EventLaneReport,
			Class:   store.EventClassJudgment,
			LaneID:  fmt.Sprintf("lane-%03d", i),
			Summary: fmt.Sprintf("Event %d summary: %s", i, strings.Repeat("summary text ", 60)),
			Payload: json.RawMessage(fmt.Sprintf(`{"raw":%q}`, strings.Repeat("payload log line ", 500))),
		})
		if err != nil {
			t.Fatalf("Append event %d: %v", i, err)
		}
		batch = append(batch, ev)
	}

	snap, err := dispatcher.BuildSnapshot(ctx, st, clk, "run-1", batch, dispatcher.SnapshotConfig{
		BacklogTopN:       120,
		MaxSnapshotTokens: 4000,
		Steers:            steers,
	})
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	if snap.EstimatedTokens <= 0 || snap.EstimatedTokens >= 30000 {
		t.Fatalf("snap.EstimatedTokens = %d, want < 30000", snap.EstimatedTokens)
	}
	if !snap.Truncated {
		t.Fatalf("expected snap.Truncated = true on oversized store inputs")
	}
	for _, section := range []string{
		"# Run Snapshot: `run-1`",
		"## Objective",
		"## Active Directives",
		"## Backlog",
		"## Lanes",
		"## Judgment Event Batch",
	} {
		if !strings.Contains(snap.Text, section) {
			t.Fatalf("snapshot missing section %q", section)
		}
	}
	if strings.Contains(snap.Text, "...[snapshot truncated to fit <30k token budget]") {
		t.Fatalf("expected per-section budgets to bound snapshot without tail-chopping the event batch")
	}
	if !strings.Contains(snap.Text, "more events omitted for token budget") {
		t.Fatalf("expected event batch section to truncate cleanly at item boundary")
	}
}

func TestDispatcher_SQLiteAndHardBatchEscalation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC))

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "orch.db"), clk)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-sql",
		Objective: "SQLite backed run.",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "item-alias",
		RunID:    "run-sql",
		Title:    "Test alias params",
		Priority: store.PriorityP1,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatalf("Add backlog: %v", err)
	}

	drv := lane.NewFakeDriver()
	fakeClient := model.NewFakeClient(
		model.Response{
			ID:         "msg-hard-1",
			Model:      "claude-opus-4-6",
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock(
					"call-alias-1",
					dispatcher.ToolLaneSpawn,
					json.RawMessage(`{"item":"item-alias","lane":"lane-alias","brief":"Fix review issue","reason":"spawn via alias fields"}`),
				),
				model.NewToolUseBlock(
					"call-alias-2",
					dispatcher.ToolDirectiveNote,
					json.RawMessage(`{"fact":"Remember review feedback","expires_at":"2h","reason":"alias fact and expires_at"}`),
				),
			),
			Usage: model.Usage{InputTokens: 300, OutputTokens: 60},
		},
		model.Response{
			ID:         "msg-hard-2",
			Model:      "claude-opus-4-6",
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Spawned alias lane and recorded directive."),
			),
			Usage: model.Usage{InputTokens: 350, OutputTokens: 20},
		},
	)

	disp := dispatcher.New(dispatcher.Config{
		Model: "claude-sonnet-4-6",
	}, st, drv, clk, fakeClient)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-sql",
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "Review outcome needs-swe on round 2",
	})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}

	if err := disp.Judge(ctx, "run-sql", []store.Event{ev}); err != nil {
		t.Fatalf("Judge: %v", err)
	}

	reqs := fakeClient.Requests
	if len(reqs) != 2 || reqs[0].Model != dispatcher.DefaultHardBatchModel {
		t.Fatalf("model used = %q, want %s", reqs[0].Model, dispatcher.DefaultHardBatchModel)
	}

	ln, err := st.Lanes().Get(ctx, "run-sql", "lane-alias")
	if err != nil {
		t.Fatalf("Get lane-alias: %v", err)
	}
	if ln.Status != lane.StatusRunning || ln.ItemID != "item-alias" {
		t.Fatalf("lane-alias = %+v, want running on item-alias", ln)
	}
}

func TestDispatcher_RestartOnPersistedStoreDoesNotCollideIDs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "restart.db"), clk)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-restart",
		Objective: "Survive process restart without ID collisions.",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}

	drv := lane.NewFakeDriver()
	makeClient := func() *model.FakeClient {
		return model.NewFakeClient(
			model.Response{
				ID:         "msg-turn-1",
				Model:      dispatcher.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("c-note", dispatcher.ToolDirectiveNote, json.RawMessage(`{"fact":"fact from dispatcher instance","reason":"record fact"}`)),
					model.NewToolUseBlock("c-ask", dispatcher.ToolOwnerAsk, json.RawMessage(`{"question":"Proceed?","options":["yes","no"],"default":"yes","deadline":"30m","reason":"confirm"}`)),
					model.NewToolUseBlock("c-plan", dispatcher.ToolPlannerRequest, json.RawMessage(`{"reason":"reconcile backlog","urgent":true}`)),
				),
				Usage: model.Usage{InputTokens: 100, OutputTokens: 40},
			},
			model.Response{
				ID:         "msg-turn-2",
				Model:      dispatcher.DefaultModel,
				StopReason: model.StopReasonEndTurn,
				Message:    model.NewAssistantMessage(model.NewTextBlock("Done.")),
				Usage:      model.Usage{InputTokens: 120, OutputTokens: 10},
			},
		)
	}

	ev := store.Event{ID: "ev-1", RunID: "run-restart", Kind: store.EventOwnerMessage, Summary: "tick"}

	disp1 := dispatcher.New(dispatcher.Config{AllowMultiAction: true}, st, drv, clk, makeClient())
	res1, err := disp1.Dispatch(ctx, "run-restart", []store.Event{ev})
	if err != nil {
		t.Fatalf("disp1.Dispatch: %v", err)
	}

	disp2 := dispatcher.New(dispatcher.Config{AllowMultiAction: true}, st, drv, clk, makeClient())
	res2, err := disp2.Dispatch(ctx, "run-restart", []store.Event{ev})
	if err != nil {
		t.Fatalf("disp2.Dispatch after restart: %v", err)
	}
	if res1.TurnID == res2.TurnID {
		t.Fatalf("expected distinct TurnID across restart, both got %q", res1.TurnID)
	}

	turns, err := st.Threads().ListTurns(ctx, res1.ThreadID)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2", len(turns))
	}

	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: "run-restart"})
	if err != nil {
		t.Fatalf("List directives: %v", err)
	}
	if len(dirs) != 5 {
		t.Fatalf("len(directives) = %d, want 5 (two notes, two planner requests, one deduplicated ask)", len(dirs))
	}
}

func TestCompileRecipeOverride_WordBoundaries(t *testing.T) {
	t.Parallel()

	base := recipe.Default()
	base.MergePolicy.RequireCIGreen = true
	rec := dispatcher.CompileRecipeOverride(base, "expect no merge conflicts, no reviewers needed, no cleanup required")
	wantPhases := []string{recipe.PhaseSWE, recipe.PhaseClean, recipe.PhaseReview, recipe.PhaseIntegrate}
	if got := rec.PhaseNames(); fmt.Sprint(got) != fmt.Sprint(wantPhases) {
		t.Fatalf("PhaseNames() = %v, want %v (false-positive substring match stripped phases)", got, wantPhases)
	}
	if rec.MergePolicy.Mode != recipe.MergeModeController {
		t.Fatalf("MergePolicy.Mode = %q, want %q", rec.MergePolicy.Mode, recipe.MergeModeController)
	}
	if !rec.MergePolicy.RequireReviewPass || !rec.MergePolicy.RequireCIGreen {
		t.Fatalf("MergePolicy gates unexpectedly cleared: %+v", rec.MergePolicy)
	}
}

func TestDispatcher_EdgeCasesAndFallbacks(t *testing.T) {
	t.Parallel()

	t.Run("RecipeCompilerOutputVettedAndReadReportEventFallbackAndDuplicateAsk", func(t *testing.T) {
		t.Parallel()
		clk, st, drv := setupTestEnv(t)
		ctx := t.Context()

		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:       "item-comp",
			RunID:    "run-1",
			Title:    "Compiler item",
			Priority: store.PriorityP1,
			Status:   store.BacklogOpen,
		}); err != nil {
			t.Fatalf("Add backlog: %v", err)
		}

		ln := lane.Lane{
			ID:     "lane-ev-rep",
			RunID:  "run-1",
			Status: lane.StatusBlocked,
			Phase:  recipe.PhaseSWE,
			Round:  1,
		}
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			t.Fatalf("Upsert lane: %v", err)
		}
		if _, err := st.Events().Append(ctx, store.Event{
			RunID:   "run-1",
			LaneID:  "lane-ev-rep",
			Kind:    store.EventLaneReport,
			Summary: "fallback report summary",
			Payload: json.RawMessage(`{"outcome":"blocked"}`),
		}); err != nil {
			t.Fatalf("Append report event: %v", err)
		}

		fakeClient := model.NewFakeClient(
			model.Response{
				ID:         "msg-edge-1",
				Model:      dispatcher.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("c-bad-json", dispatcher.ToolBacklogAdd, json.RawMessage(`{"id":123}`)),
					model.NewToolUseBlock("c-read-fallback", dispatcher.ToolReadLaneReport, json.RawMessage(`{"lane_id":"lane-ev-rep"}`)),
					model.NewToolUseBlock("c-ask-1", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"dup-id","question":"Q1?","options":["a","b"],"default":"a","deadline":"10m"}`)),
					model.NewToolUseBlock("c-ask-2", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"dup-id","question":"Q2?","options":["a","b"],"default":"b","deadline":"20m"}`)),
					model.NewToolUseBlock("c-spawn-comp", dispatcher.ToolLaneSpawn, json.RawMessage(`{"item_id":"item-comp","brief":"Run gh pr merge now","recipe_override":"self-merge"}`)),
				),
				Usage: model.Usage{InputTokens: 200, OutputTokens: 50},
			},
			model.Response{
				ID:         "msg-edge-2",
				Model:      dispatcher.DefaultModel,
				StopReason: model.StopReasonEndTurn,
				Message:    model.NewAssistantMessage(model.NewTextBlock("Completed edge checks.")),
				Usage:      model.Usage{InputTokens: 250, OutputTokens: 20},
			},
		)

		disp := dispatcher.New(dispatcher.Config{
			RecipeCompiler: dispatcher.RecipeCompilerFunc(func(_ context.Context, base recipe.Recipe, _ string) (recipe.Recipe, error) {
				out := base.Clone()
				out.MergePolicy.Mode = recipe.MergeModeController
				return out, nil
			}),
		}, st, drv, clk, fakeClient)

		res, err := disp.Dispatch(ctx, "run-1", nil)
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if len(res.Accepted) != 2 {
			t.Fatalf("len(Accepted) = %d, want 2 (read.lane_report and first owner.ask): %+v", len(res.Accepted), res.Accepted)
		}
		if len(res.Rejected) < 2 {
			t.Fatalf("expected invalid JSON and unvetted compiler recipe to be rejected, got Rejected=%v", res.Rejected)
		}
	})

	t.Run("CanceledContextStillCompletesTurnOnSQLite", func(t *testing.T) {
		t.Parallel()
		clk := clock.NewFakeClock(time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC))
		st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "cancel.db"), clk)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })

		if err := st.Runs().Create(t.Context(), store.Run{ID: "run-cancel", Objective: "Cancel test"}); err != nil {
			t.Fatalf("Create run: %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancelClient := clientFunc(func(_ context.Context, _ model.Request) (*model.Response, error) {
			cancel()
			return nil, context.Canceled
		})

		disp := dispatcher.New(dispatcher.Config{}, st, lane.NewFakeDriver(), clk, cancelClient)
		res, err := disp.Dispatch(ctx, "run-cancel", nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Dispatch err = %v, want context.Canceled", err)
		}
		turn, gErr := st.Threads().GetTurn(t.Context(), res.ThreadID, res.TurnID)
		if gErr != nil {
			t.Fatalf("GetTurn: %v", gErr)
		}
		if turn.Status != store.TurnStatusFailed {
			t.Fatalf("turn.Status = %q, want failed even after ctx cancel", turn.Status)
		}
	})
}

type clientFunc func(context.Context, model.Request) (*model.Response, error)

func (f clientFunc) CreateMessage(ctx context.Context, req model.Request) (*model.Response, error) {
	return f(ctx, req)
}

func TestSnapshotMemoryIndexBudget(t *testing.T) {
	t.Parallel()
	clk, st, _ := setupTestEnv(t)
	ctx := t.Context()
	for i := 0; i < 500; i++ {
		_, err := st.Memories().Put(ctx, store.MemoryEntry{
			ID: fmt.Sprintf("memory-%03d", i), Name: "owner\ncorrection", Description: strings.Repeat("guidance ", 50),
			Type: "rule", Scope: "run", RunID: "run-1", Source: "owner correction", Body: "Why: clarity. How: scoped tests.", ProposedBy: "owner",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := dispatcher.SnapshotConfig{MaxSnapshotTokens: 1000}
	first, err := dispatcher.BuildSnapshot(ctx, st, clk, "run-1", nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatcher.BuildSnapshot(ctx, st, clk, "run-1", nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != second.Text {
		t.Fatal("memory index not deterministic")
	}
	if first.EstimatedTokens > cfg.MaxSnapshotTokens || !first.Truncated {
		t.Fatalf("snapshot tokens=%d truncated=%t", first.EstimatedTokens, first.Truncated)
	}
	if !strings.Contains(first.Text, "memories omitted for token budget") || strings.Contains(first.Text, "owner\ncorrection") {
		t.Fatal("memory index must be bounded and single-line")
	}
	if !strings.Contains(first.Text, "## Judgment Event Batch") {
		t.Fatal("event section lost")
	}
}

func TestFailedPlannerRequestRevokesInstruction(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	client := model.NewFakeClient(model.Response{StopReason: model.StopReasonToolUse, Message: model.Message{
		Role: model.RoleAssistant,
		Content: []model.ContentBlock{model.NewToolUseBlock("plan", dispatcher.ToolPlannerRequest,
			json.RawMessage(`{"reason":"Reconcile","plan_ref":"file:///etc/passwd"}`))},
	}}, model.Response{Message: model.NewAssistantMessage(model.NewTextBlock("Request rejected."))})
	disp := dispatcher.New(dispatcher.Config{OnPlannerRequest: func(context.Context, string, string, string) error {
		return errors.New("planner request requires a post reference")
	}}, st, drv, clk, client)
	if _, err := disp.Dispatch(t.Context(), "run-1", nil); err != nil {
		t.Fatal(err)
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0].Status != store.DirectiveRevoked {
		t.Fatalf("failed planner instruction must be revoked: %+v", dirs)
	}
}

func TestSpawnReportContractAndBase(t *testing.T) {
	t.Parallel()
	for _, capBytes := range []int{0, 400} {
		t.Run(fmt.Sprint(capBytes), func(t *testing.T) {
			t.Parallel()
			clk, st, drv := setupTestEnv(t)
			drv.SpawnBaseCommit = strings.Repeat("a", 40)
			ctx := t.Context()
			if err := st.Backlog().Add(ctx, store.BacklogItem{ID: "item", RunID: "run-1", Title: "Task", Description: strings.Repeat("文", 500), Status: store.BacklogOpen}); err != nil {
				t.Fatal(err)
			}
			client := model.NewFakeClient(toolUseResponse(model.ToolCall{ID: "spawn", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"item","brief":"Do the task"}`)}), endTurnResponse("done"))
			d := dispatcher.New(dispatcher.Config{MaxBriefBytes: capBytes}, st, drv, clk, client)
			if _, err := d.Dispatch(ctx, "run-1", nil); err != nil {
				t.Fatal(err)
			}
			if len(drv.Spawns) != 1 {
				t.Fatalf("spawns=%+v", drv.Spawns)
			}
			req := drv.Spawns[0]
			want := drv.ReportInstructions(req.RunID, req.LaneID, req.Phase, req.Round)
			if !strings.HasSuffix(req.Brief, want) {
				t.Fatalf("brief=%q, missing %q", req.Brief, want)
			}
			if !utf8.ValidString(req.Brief) {
				t.Fatal("brief is not valid UTF-8")
			}
			if capBytes > 0 && len(req.Brief) > capBytes {
				t.Fatalf("brief is %d bytes, cap %d", len(req.Brief), capBytes)
			}
			ln, err := st.Lanes().Get(ctx, "run-1", "item")
			if err != nil {
				t.Fatal(err)
			}
			if ln.BaseCommit != drv.SpawnBaseCommit {
				t.Fatalf("base=%q", ln.BaseCommit)
			}
		})
	}
}

func TestDispatcherReconcilesMergedAndClosedPRBeforeRetry(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID: "m2-default", RunID: "run-1", Title: "Default", Priority: store.PriorityP1, Status: store.BacklogInProgress, LaneID: "m2-default",
	}); err != nil {
		t.Fatal(err)
	}
	ln := lane.Lane{
		ID: "m2-default", ItemID: "m2-default", RunID: "run-1",
		Status: lane.StatusBlocked, Phase: recipe.PhaseReview, Round: 1, PR: 5705,
		SessionID: "sess-m2", TerminalID: "term-m2",
	}
	ln.RecordAttempt(recipe.PhaseReview, 1, "sess-m2", "term-m2")
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatal(err)
	}

	client := model.NewFakeClient(
		toolUseResponse(model.ToolCall{ID: "retry-1", Name: dispatcher.ToolLaneRetry, Input: json.RawMessage(`{"lane_id":"m2-default","phase":"review","round":2,"reason":"retry review"}`)}),
		endTurnResponse("done"),
	)
	d := dispatcher.New(dispatcher.Config{
		PRProbe: func(_ context.Context, runID, _ string, pr int) (controller.PRStatus, error) {
			if runID == "run-1" && pr == 5705 {
				return controller.PRStatus{Number: 5705, State: "MERGED", MergeSHA: "merge-sha-5705"}, nil
			}
			return controller.PRStatus{Number: pr, State: "OPEN"}, nil
		},
	}, st, drv, clk, client)
	if _, err := d.Dispatch(ctx, "run-1", nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.Lanes().Get(ctx, "run-1", "m2-default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != lane.StatusDone || got.MergeSHA != "merge-sha-5705" {
		t.Fatalf("lane after dispatcher retry on merged PR = %+v, want StatusDone and MergeSHA=merge-sha-5705", got)
	}
	if len(drv.PhaseStarts) != 0 || len(drv.Spawns) != 0 {
		t.Fatalf("dispatcher spawned/started on merged PR: starts=%+v spawns=%+v", drv.PhaseStarts, drv.Spawns)
	}
	if len(drv.Stops) == 0 {
		t.Fatalf("expected dispatcher to stop terminals on merged PR, stops=%+v", drv.Stops)
	}
}

func TestDispatcherStaleJudgmentDismissesQuestionAndEarlierOwnerLiftAllowsRevoke(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:            "m2-gaps3",
		RunID:         "run-1",
		Title:         "m2 gaps 3",
		RecipeName:    recipe.DefaultName,
		RecipeVersion: 1,
		Status:        lane.StatusRunning,
		Phase:         recipe.PhaseSWE,
		Round:         1,
	}); err != nil {
		t.Fatal(err)
	}

	// Hold directive created at t0.
	if err := st.Directives().Add(ctx, store.Directive{
		ID:        "hold-m4",
		RunID:     "run-1",
		Kind:      store.DirectiveStanding,
		Source:    store.DirectiveSourceOwner,
		Scope:     "run",
		Text:      "Do not start M4 until I confirm",
		Status:    store.DirectiveActive,
		CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Owner message at t1 lifting the hold (already handled in an earlier batch).
	clk.Advance(time.Minute)
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Summary: "yes: clear the hold and proceed with M4",
	}); err != nil {
		t.Fatal(err)
	}

	// Later non-owner mail event at t2 with a stale lane question + a withdrawn question.
	clk.Advance(time.Minute)
	withdrawnPayload, _ := json.Marshal(mailbox.Message{
		ID:       "msg-withdrawn",
		RunID:    "run-1",
		LaneID:   "m2-gaps3",
		Question: true,
		Status:   "withdrawn",
		Body:     "withdrawn question 04547e4e",
	})
	if _, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "m2-gaps3",
		Kind:    store.EventMail,
		Class:   store.EventClassAuto,
		Summary: "withdrawn question 04547e4e",
		Payload: withdrawnPayload,
	}); err != nil {
		t.Fatal(err)
	}
	if !mailbox.IsQuestionObsoleteOrClosed(ctx, st, "run-1", "m2-gaps3", "msg-withdrawn", "", "withdrawn question 04547e4e") {
		t.Fatal("expected withdrawn question to be treated as closed")
	}

	stalePayload, _ := json.Marshal(mailbox.Message{
		ID:       "msg-stale-1",
		RunID:    "run-1",
		LaneID:   "m2-gaps3",
		Question: true,
		Body:     "stale question from earlier round",
	})
	staleEv, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "m2-gaps3",
		Kind:    store.EventMail,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "lane question from m2-gaps3: stale question from earlier round",
		Payload: stalePayload,
	})
	if err != nil {
		t.Fatal(err)
	}

	fakeBox := mailboxfake.New(st.Events(), "run-1")
	client := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("dismiss-1", dispatcher.ToolMailDismiss, json.RawMessage(`{"message_id":"msg-stale-1","lane_id":"m2-gaps3","reason":"stale question from superseded round"}`)),
				model.NewToolUseBlock("revoke-1", dispatcher.ToolDirectiveRevoke, json.RawMessage(`{"id":"hold-m4","reason":"owner lifted hold earlier"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Dismissed stale question and revoked lifted hold.")),
		},
	)
	d := dispatcher.New(dispatcher.Config{
		Mailboxes:        []mailbox.Mailbox{fakeBox},
		AllowMultiAction: true,
	}, st, drv, clk, client)
	res, err := d.Dispatch(ctx, "run-1", []store.Event{staleEv})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	for _, dec := range res.Decisions {
		if !dec.Vetted {
			t.Fatalf("unvetted decision: %+v", dec)
		}
	}
	if got := fakeBox.Dismissed()["msg-stale-1"]; !strings.Contains(got, "stale") {
		t.Fatalf("fakeBox.Dismissed[msg-stale-1] = %q, want stale reason", got)
	}
	if !mailbox.IsQuestionObsoleteOrClosed(ctx, st, "run-1", "m2-gaps3", "msg-stale-1", staleEv.ID, "stale question from earlier round") {
		t.Fatal("expected dismissed question to be treated as closed")
	}
	gotHold, err := st.Directives().Get(ctx, "run-1", "hold-m4")
	if err != nil {
		t.Fatal(err)
	}
	if gotHold.Status != store.DirectiveRevoked {
		t.Fatalf("hold-m4 status = %q, want %q", gotHold.Status, store.DirectiveRevoked)
	}

	// Verify that an active lane question whose wording mentions "stale" (e.g.
	// "branch is stale, rebase?") is escalated by owner.ask rather than dismissed.
	livePayload, _ := json.Marshal(mailbox.Message{
		ID:       "msg-live-stale-word",
		RunID:    "run-1",
		LaneID:   "m2-gaps3",
		Question: true,
		Body:     "branch is stale, rebase onto main?",
	})
	liveEv, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "m2-gaps3",
		Kind:    store.EventMail,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "lane question from m2-gaps3: branch is stale, rebase onto main?",
		Payload: livePayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientEscalate := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("ask-live-1", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"ask-rebase","lane_id":"m2-gaps3","question":"branch is stale, rebase onto main?","options":["rebase","keep"],"default":"rebase","deadline":"10m","reason":"lane m2-gaps3 asked whether to rebase stale branch"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Escalated rebase question to owner.")),
		},
	)
	dEscalate := dispatcher.New(dispatcher.Config{
		Mailboxes: []mailbox.Mailbox{fakeBox},
	}, st, drv, clk, clientEscalate)
	if _, err := dEscalate.Dispatch(ctx, "run-1", []store.Event{liveEv}); err != nil {
		t.Fatalf("Dispatch(live question with word stale): %v", err)
	}
	if _, dismissed := fakeBox.Dismissed()["msg-live-stale-word"]; dismissed {
		t.Fatalf("live question mentioning 'stale' was wrongly dismissed: %+v", fakeBox.Dismissed())
	}
	if _, err := st.Directives().Get(ctx, "run-1", "ask-ask-rebase"); err != nil {
		t.Fatalf("expected escalated owner_ask directive ask-ask-rebase, got err=%v", err)
	}
}

func TestRunCloseoutToolVettingAndExecution(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Runs().UpdateObjective(ctx, "run-1", "Goal: trial-m1\nPlan: PLAN.md (post ac9da4cb)\nNext: write FINAL.md, update PLAN.md log, republish ac9da4cb", "post:ac9da4cb"); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:     "m5-finish",
		RunID:  "run-1",
		Status: lane.StatusRunning,
		Phase:  "review",
		Round:  1,
	}); err != nil {
		t.Fatal(err)
	}

	viewRunning, err := dispatcher.LoadStateView(ctx, st, clk, "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	actClose := dispatcher.ProposedAction{
		Tool: dispatcher.ToolRunCloseout,
		RunCloseout: &dispatcher.RunCloseoutParams{
			Outcome:  "done",
			PlanFile: "PLAN.md",
			PostID:   "ac9da4cb",
		},
	}
	if err := dispatcher.VetAction(viewRunning, actClose, dispatcher.DefaultInvariants()); err == nil {
		t.Fatal("expected VetAction to reject run.closeout while m5-finish is still running")
	}

	// Reject run.closeout when the run has zero lanes.
	viewNoLanes := *viewRunning
	viewNoLanes.Lanes = nil
	if err := dispatcher.VetAction(&viewNoLanes, actClose, dispatcher.DefaultInvariants()); err == nil {
		t.Fatal("expected VetAction to reject run.closeout when the run has 0 lanes")
	}

	// Mark lane done and test file confinement / named-file invariants.
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:       "m5-finish",
		RunID:    "run-1",
		Status:   lane.StatusDone,
		Phase:    "review",
		Round:    1,
		PR:       5899,
		MergeSHA: "def5899",
	}); err != nil {
		t.Fatal(err)
	}
	viewDone, err := dispatcher.LoadStateView(ctx, st, clk, "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	// A backlog item whose corresponding lane is terminal does not block run.closeout,
	// whereas a backlog item with no terminal lane does block run.closeout.
	viewWithSyncedBacklog := *viewDone
	viewWithSyncedBacklog.Backlog = map[string]store.BacklogItem{
		"m5-finish": {ID: "m5-finish", RunID: "run-1", Status: store.BacklogInProgress},
	}
	if err := dispatcher.VetAction(&viewWithSyncedBacklog, actClose, dispatcher.DefaultInvariants()); err != nil {
		t.Fatalf("expected run.closeout to pass when backlog item matches terminal lane, got: %v", err)
	}
	viewWithPendingBacklog := *viewDone
	viewWithPendingBacklog.Backlog = map[string]store.BacklogItem{
		"m6-unspawned": {ID: "m6-unspawned", RunID: "run-1", Status: store.BacklogInProgress},
	}
	if err := dispatcher.VetAction(&viewWithPendingBacklog, actClose, dispatcher.DefaultInvariants()); err == nil {
		t.Fatal("expected VetAction to reject run.closeout when an unspawned backlog item is ready")
	}

	// Reject post_id when Objective and PlanRef are empty or when post_id is only a substring.
	viewEmptyObj := *viewDone
	runEmpty := *viewDone.Run
	runEmpty.Objective = ""
	runEmpty.PlanRef = ""
	viewEmptyObj.Run = &runEmpty
	if err := dispatcher.VetAction(&viewEmptyObj, actClose, dispatcher.DefaultInvariants()); err == nil {
		t.Fatal("expected VetAction to reject post_id when Objective and PlanRef are empty")
	}

	for _, bad := range []dispatcher.RunCloseoutParams{
		{PlanFile: "../outside.md"},
		{PlanFile: "/tmp/escape.md"},
		{PlanFile: "UNLISTED.md"},
		{Files: map[string]string{"../outside.md": "bad"}},
		{Files: map[string]string{"UNLISTED.md": "bad"}},
		{PostID: "deadbeef"}, // malformed (no digits)
		{PostID: "a1b2c3d4"}, // well-formed hex with digits, but not named in OBJECTIVE.md
		{PostID: "ac9da4c"},  // prefix substring must not match ac9da4cb
	} {
		p := bad
		if err := dispatcher.VetAction(viewDone, dispatcher.ProposedAction{
			Tool:        dispatcher.ToolRunCloseout,
			RunCloseout: &p,
		}, dispatcher.DefaultInvariants()); err == nil {
			t.Fatalf("expected VetAction to reject invalid run.closeout params %+v", bad)
		}
	}

	if err := dispatcher.ValidateCloseoutPostID("deadbeef", viewDone.Run.Objective); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("expected malformed error for deadbeef, got %v", err)
	}
	if err := dispatcher.ValidateCloseoutPostID("a1b2c3d4", viewDone.Run.Objective); err == nil || !strings.Contains(err.Error(), "is not named in OBJECTIVE.md") {
		t.Fatalf("expected not-named error for a1b2c3d4, got %v", err)
	}
	if _, err := dispatcher.ValidateCloseoutFileRef("Inspect state.json and HANDOVER.md", "state.json"); err == nil {
		t.Fatal("expected ValidateCloseoutFileRef to reject state.json even when mentioned in OBJECTIVE.md")
	}
	if _, err := dispatcher.ValidateCloseoutFileRef("Inspect state.json and HANDOVER.md", "HANDOVER.md"); err == nil {
		t.Fatal("expected ValidateCloseoutFileRef to reject HANDOVER.md even when mentioned in OBJECTIVE.md")
	}
	if got := dispatcher.FindPostIDInText("republish the plan; m1 merged abc5604"); got != "" {
		t.Fatalf("FindPostIDInText without post marker = %q, want empty", got)
	}
	if err := dispatcher.CheckRunTerminal(
		[]lane.Lane{{ID: "m5-finish", Status: lane.StatusDone}},
		[]store.BacklogItem{{ID: "item-1", LaneID: "m5-finish", Status: store.BacklogInProgress}},
		true,
	); err != nil {
		t.Fatalf("CheckRunTerminal should treat backlog item paired with terminal lane as terminal: %v", err)
	}

	if err := dispatcher.VetAction(viewDone, actClose, dispatcher.DefaultInvariants()); err != nil {
		t.Fatalf("expected valid run.closeout to pass VetAction, got: %v", err)
	}

	var calledWith dispatcher.RunCloseoutParams
	client := model.NewFakeClient(
		toolUseResponse(model.ToolCall{
			ID:    "close-1",
			Name:  dispatcher.ToolRunCloseout,
			Input: json.RawMessage(`{"outcome":"done","plan_file":"PLAN.md","post_id":"ac9da4cb","summary":"All lanes merged (#5899)"}`),
		}),
		endTurnResponse("Closed out run."),
	)
	d := dispatcher.New(dispatcher.Config{
		OnRunCloseout: func(_ context.Context, runID string, params dispatcher.RunCloseoutParams) (string, error) {
			if runID != "run-1" {
				return "", fmt.Errorf("unexpected runID %q", runID)
			}
			calledWith = params
			return "wrote FINAL.md; updated PLAN.md and post ac9da4cb; run /release", nil
		},
	}, st, drv, clk, client)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Summary: "all lanes terminal; close out run",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch(run.closeout): %v", err)
	}
	if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolRunCloseout {
		t.Fatalf("Accepted = %+v, want 1 run.closeout action", res.Accepted)
	}
	if calledWith.PostID != "ac9da4cb" || calledWith.PlanFile != "PLAN.md" {
		t.Fatalf("OnRunCloseout calledWith = %+v", calledWith)
	}
}

func TestDispatcherLaneWaitToolSuppressesStallUntilDeadline(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:          "perf-sweep",
		RunID:       "run-1",
		Title:       "Azure sweep",
		Status:      lane.StatusRunning,
		Phase:       recipe.PhaseSWE,
		Round:       1,
		Attempt:     1,
		SessionID:   "sess-perf",
		TerminalID:  "term-perf",
		LastEventAt: clk.Now(),
	}); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "perf-sweep",
		Kind:    store.EventMail,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "perf-sweep launched 120-minute Azure sweep",
	})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}

	client := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("wait-1", dispatcher.ToolLaneWait, json.RawMessage(`{"lane_id":"perf-sweep","reason":"120-minute wait for perf-sweep's Azure sweep","deadline":"120m"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Recorded 120-minute expected wait on perf-sweep.")),
		},
	)

	disp := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
	res, err := disp.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch(lane.wait): %v", err)
	}
	if len(res.Decisions) != 1 || !res.Decisions[0].Vetted || res.Decisions[0].Tool != dispatcher.ToolLaneWait {
		t.Fatalf("decisions = %+v, want 1 vetted lane.wait decision", res.Decisions)
	}

	judge := controller.NewFakeJudge()
	ctrl := controller.New(controller.Config{
		RunID:            "run-1",
		JudgmentDebounce: -1,
		StallTimeout:     45 * time.Minute,
		StallTimeoutSet:  true,
	}, st, drv, clk, judge)

	// Advance 90m (past the 45m stall timeout, before the 120m deadline): no stall.
	clk.Advance(90 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 90m: %v", err)
	}
	if len(drv.Stops) != 0 {
		t.Fatalf("at 90m (before expected wait deadline): stops=%+v, want 0", drv.Stops)
	}

	// Advance past the 120m deadline (to 121m): stall fires.
	clk.Advance(31 * time.Minute)
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("Step at 121m: %v", err)
	}
	if len(drv.Stops) != 1 || drv.Stops[0].LaneID != "perf-sweep" {
		t.Fatalf("at 121m (after expected wait deadline): stops=%+v, want perf-sweep stopped", drv.Stops)
	}
}

func TestDispatchAdoptedFinishedRunSkipsTurnAndCompactSnapshot(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	const runID = "20261114-trial-m1"
	obj := "# Objective\nGoal: Slim subagent-swarm-nebula prompt footprint by >=35% with no regression.\n\n" +
		strings.Repeat("Milestone history line with merged PR details and verification evidence.\n", 25) +
		"\nNext: none. Run DONE 2026-11-14T18:10Z.\n"
	if err := st.Runs().Create(ctx, store.Run{
		ID:            runID,
		Repository:    "sycamore-labs/nebula",
		Objective:     obj,
		DefaultRecipe: "adopted",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	for i := 1; i <= 9; i++ {
		id := fmt.Sprintf("m%d-lane", i)
		status := store.BacklogDone
		laneStatus := lane.StatusDone
		if i == 9 {
			status = store.BacklogDropped
			laneStatus = lane.StatusFailed
		}
		if err := st.Backlog().Add(ctx, store.BacklogItem{
			ID:       id,
			RunID:    runID,
			Title:    fmt.Sprintf("Milestone lane %d", i),
			Priority: store.PriorityP1,
			Status:   status,
			LaneID:   id,
		}); err != nil {
			t.Fatalf("Add backlog %d: %v", i, err)
		}
		if err := st.Lanes().Upsert(ctx, lane.Lane{
			ID:     id,
			RunID:  runID,
			ItemID: id,
			Title:  fmt.Sprintf("Milestone lane %d", i),
			Status: laneStatus,
			Phase:  recipe.PhaseReview,
			Round:  1,
			PR:     5600 + i,
			Checks: "green",
		}); err != nil {
			t.Fatalf("Upsert lane %d: %v", i, err)
		}
	}

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Source:  mailbox.AdoptMailSummarySource,
		Summary: "Adopted finished run 20261114-trial-m1: 9 lanes (done:8, failed:1); no open questions.",
	})
	if err != nil {
		t.Fatalf("Append adopt summary event: %v", err)
	}

	// 1. Compact snapshot for a finished run stays small (< 1100 bytes, vs ~3900 bytes full).
	snap, err := dispatcher.BuildSnapshot(ctx, st, clk, runID, []store.Event{ev}, dispatcher.SnapshotConfig{})
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if !strings.Contains(snap.Text, "Status: DONE") || !strings.Contains(snap.Text, "All 9 backlog items are terminal") || !strings.Contains(snap.Text, "All 9 lanes are terminal") {
		t.Fatalf("expected compact DONE snapshot summary, got:\n%s", snap.Text)
	}
	if len(snap.Text) > 1200 {
		t.Fatalf("done run snapshot size = %d bytes, want <= 1200 bytes:\n%s", len(snap.Text), snap.Text)
	}

	// 2. Dispatching an adopted finished run with only the adopt summary (or non-owner events)
	// skips the model call completely: 0 turns, $0.00 cost, and "No action needed."
	client := model.NewFakeClient()
	disp := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
	res, err := disp.Dispatch(ctx, runID, []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch adopt summary on finished run: %v", err)
	}
	if res.TurnCount != 0 || res.Usage.CostUSD != 0 || len(client.Requests) != 0 {
		t.Fatalf("expected 0 model turns and $0 cost when adopting finished run, got TurnCount=%d CostUSD=%f requests=%d",
			res.TurnCount, res.Usage.CostUSD, len(client.Requests))
	}
	if !strings.Contains(res.Summary, "No action needed") {
		t.Fatalf("Summary = %q, want 'No action needed'", res.Summary)
	}

	// 3. Even if an owner message forces a dispatcher turn on a finished run with 4+ events,
	// it stays on the default model (never escalates to HardBatchModel) and stays well under $0.50.
	ownerEv, err := st.Events().Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Source:  "owner",
		Summary: "confirm trial-m1 needs no further work",
	})
	if err != nil {
		t.Fatalf("Append owner event: %v", err)
	}
	forcedClient := model.NewFakeClient(model.Response{
		ID:         "msg-done-1",
		Model:      dispatcher.DefaultModel,
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock("No action needed.")),
		Usage:      model.Usage{InputTokens: 220, OutputTokens: 12, CostUSD: 0.02},
	})
	dispForced := dispatcher.New(dispatcher.Config{}, st, drv, clk, forcedClient)
	resForced, err := dispForced.Dispatch(ctx, runID, []store.Event{ev, ev, ev, ownerEv})
	if err != nil {
		t.Fatalf("Dispatch forced owner event on finished run: %v", err)
	}
	reqs := forcedClient.Requests
	if len(reqs) != 1 {
		t.Fatalf("forced turn requests = %d, want 1 request", len(reqs))
	}
	if reqs[0].Model != dispatcher.DefaultModel {
		t.Fatalf("forced turn model = %q, want %q (no Opus escalation)", reqs[0].Model, dispatcher.DefaultModel)
	}
	if resForced.Usage.CostUSD >= 0.50 {
		t.Fatalf("forced turn CostUSD = %f, want < $0.50", resForced.Usage.CostUSD)
	}
	spawnSchema := string(model.SchemaFor[dispatcher.LaneSpawnParams]())
	if strings.Contains(spawnSchema, `"recipe_name"`) {
		t.Fatalf("LaneSpawnParams schema should omit recipe_name via jsonschema:\"-\": %s", spawnSchema)
	}

	// 4. Done-run skipping must NEVER apply to runs that are not done, even for AdoptMailSummarySource.
	activeEv, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Source:  mailbox.AdoptMailSummarySource,
		Summary: "Adopted active run run-1: 0 lanes, open backlog.",
	})
	if err != nil {
		t.Fatalf("Append active adopt summary event: %v", err)
	}
	activeClient := model.NewFakeClient(model.Response{
		ID:         "msg-active-1",
		Model:      dispatcher.DefaultModel,
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock("Reviewed active run.")),
		Usage:      model.Usage{InputTokens: 180, OutputTokens: 8, CostUSD: 0.01},
	})
	dispActive := dispatcher.New(dispatcher.Config{}, st, drv, clk, activeClient)
	resActive, err := dispActive.Dispatch(ctx, "run-1", []store.Event{activeEv})
	if err != nil {
		t.Fatalf("Dispatch adopt summary on active run: %v", err)
	}
	if resActive.TurnCount != 1 || len(activeClient.Requests) != 1 {
		t.Fatalf("expected active run to invoke model turn (not skip), got TurnCount=%d requests=%d",
			resActive.TurnCount, len(activeClient.Requests))
	}
}

//nolint:govet // transport capture groups the observations used below.
type ownerAskMailbox struct {
	*mailboxfake.Mailbox
	questions     []mailbox.Question
	notifications int
	failNext      bool
}

func (m *ownerAskMailbox) AskOwner(_ context.Context, q mailbox.Question) (string, error) {
	if m.failNext {
		m.failNext = false
		return "", errors.New("mail unavailable")
	}
	m.questions = append(m.questions, q)
	return q.ID, nil
}

func (m *ownerAskMailbox) NotifyOwnerAsk(context.Context, mailbox.Question) error {
	m.notifications++
	return nil
}

func TestOwnerAskDeliveryDedupeRestartAndRetry(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()
	if err := st.Lanes().Upsert(ctx, lane.Lane{RunID: "run-1", ID: "lane", Status: lane.StatusRunning, SessionID: "session", TerminalID: "terminal"}); err != nil {
		t.Fatal(err)
	}
	box := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1"), failNext: true}
	makeDispatcher := func() *dispatcher.Dispatcher {
		return dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, st, drv, clk, nil)
	}
	ask := dispatcher.OwnerAskParams{ID: "one", LaneID: "lane", Question: "Approve the budget?", Options: []string{"yes", "defer"}, Default: "defer", Deadline: "1h"}
	if _, err := makeDispatcher().RegisterOwnerAsk(ctx, "run-1", ask); err == nil {
		t.Fatal("expected first delivery failure")
	}
	ask.ID, ask.Question = "retry", " APPROVE   the budget? "
	if _, err := makeDispatcher().RegisterOwnerAsk(ctx, "run-1", ask); err != nil {
		t.Fatal(err)
	}
	ask.ID, ask.Question = "other", "A different question for the same lane?"
	if _, err := makeDispatcher().RegisterOwnerAsk(ctx, "run-1", ask); err != nil {
		t.Fatal(err)
	}
	if len(box.questions) != 1 || box.notifications != 1 {
		t.Fatalf("mail=%+v Slack=%d, want one each", box.questions, box.notifications)
	}
	q := box.questions[0]
	if q.RunID != "run-1" || q.LaneID != "lane" || q.ID != "one" || q.Default != "defer" || len(q.Options) != 2 || !q.Deadline.Equal(clk.Now().Add(time.Hour)) {
		t.Fatalf("mail lost ask context: %+v", q)
	}
	if len(drv.Steers) != 1 || !strings.Contains(drv.Steers[0].Message, "defer") {
		t.Fatalf("default should continue once: %+v", drv.Steers)
	}
	asks, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil || len(asks) != 1 {
		t.Fatalf("open asks=%+v err=%v", asks, err)
	}
	if err := st.Directives().UpdateStatus(ctx, "run-1", "ask-one", store.DirectiveHandled); err != nil {
		t.Fatal(err)
	}
	ask.ID, ask.Question = "after-answer", "approve the budget?"
	if _, err := makeDispatcher().RegisterOwnerAsk(ctx, "run-1", ask); err != nil {
		t.Fatal(err)
	}
	if len(box.questions) != 1 {
		t.Fatalf("answered question was mailed again: %+v", box.questions)
	}
}

func TestOwnerAskRegistrationDedupeAcrossDispatchers(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	box := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1")}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "run-1", ID: "lane", Status: lane.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	params := dispatcher.OwnerAskParams{LaneID: "lane", Question: "Approve the budget?", Options: []string{"yes", "defer"}, Default: "defer", Deadline: "1h"}
	d1 := dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, st, drv, clk, nil)
	d2 := dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, st, drv, clk, nil)
	errs := make(chan error, 2)
	go func() {
		p := params
		p.ID = "ask-one"
		_, err := d1.RegisterOwnerAsk(t.Context(), "run-1", p)
		errs <- err
	}()
	go func() {
		p := params
		p.ID = "ask-two"
		_, err := d2.RegisterOwnerAsk(t.Context(), "run-1", p)
		errs <- err
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if len(box.questions) != 1 {
		t.Fatalf("mail=%+v, want one question across dispatcher instances", box.questions)
	}
	asks, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil || len(asks) != 1 || !strings.HasPrefix(asks[0].Ref, "owner_ask:") {
		t.Fatalf("active asks=%+v err=%v, want one owner ask", asks, err)
	}
}

func TestOwnerAskHoldDefaultParksLane(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	box := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1")}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "run-1", ID: "lane", Status: lane.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	d := dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, st, drv, clk, nil)
	_, err := d.RegisterOwnerAsk(t.Context(), "run-1", dispatcher.OwnerAskParams{ID: "hold", LaneID: "lane", Question: "Approve?", Options: []string{"hold: wait"}, Default: "hold: wait", Deadline: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := st.Lanes().Get(t.Context(), "run-1", "lane")
	if err != nil || ln.Status != lane.StatusBlocked || len(drv.Steers) != 0 {
		t.Fatalf("hold default should park lane: lane=%+v err=%v steers=%+v", ln, err, drv.Steers)
	}
}

func TestOwnerAskRetriesOnlyFailedTransport(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	first := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1")}
	second := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1"), failNext: true}
	d := dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{first, second}}, st, drv, clk, nil)
	ask := dispatcher.OwnerAskParams{ID: "one", Question: "Approve the budget?", Options: []string{"yes", "defer"}, Default: "defer", Deadline: "1h"}
	if _, err := d.RegisterOwnerAsk(t.Context(), "run-1", ask); err == nil {
		t.Fatal("expected second transport failure")
	}
	if _, err := d.RegisterOwnerAsk(t.Context(), "run-1", ask); err != nil {
		t.Fatal(err)
	}
	if len(first.questions) != 1 || len(second.questions) != 1 || first.notifications != 1 || second.notifications != 1 {
		t.Fatalf("delivery repeated after partial failure: first=%+v second=%+v", first, second)
	}
}

func TestOwnerAskAcceptsRunMailSourceReference(t *testing.T) {
	t.Parallel()
	for _, messageID := range []string{"card-1", "ev-card-1", "mail:card-1"} {
		for _, laneID := range []string{"", "lane"} {
			t.Run(messageID+"/"+laneID, func(t *testing.T) {
				clk, st, drv := setupTestEnv(t)
				if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "run-1", ID: "lane", Status: lane.StatusRunning}); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(dispatcher.OwnerAskParams{MessageID: messageID, LaneID: laneID, Question: "Approve the launch budget?", Options: []string{"approve", "defer"}, Default: "defer", Deadline: "1h"})
				if err != nil {
					t.Fatal(err)
				}
				client := model.NewFakeClient(model.Response{
					StopReason: model.StopReasonToolUse,
					Message:    model.NewAssistantMessage(model.NewToolUseBlock("ask", dispatcher.ToolOwnerAsk, raw)),
				}, model.Response{StopReason: model.StopReasonEndTurn})
				d := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
				result, err := d.Dispatch(t.Context(), "run-1", []store.Event{{ID: "ev-card-1", RunID: "run-1", Kind: store.EventMail, Source: "mail:card-1", Summary: "Approve the launch budget? Options: approve, defer. Default: defer."}})
				if err != nil || len(result.Accepted) != 1 || result.Accepted[0].Tool != dispatcher.ToolOwnerAsk {
					t.Fatalf("run mail reference rejected: result=%+v err=%v", result, err)
				}
			})
		}
	}
}

func TestOwnerAskHourlyCapKeepsOverflowAndSendsOneDigest(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	box := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1")}
	d := dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, st, drv, clk, nil)
	for i := range 10 {
		id := fmt.Sprintf("lane-%02d", i)
		if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "run-1", ID: id, Status: lane.StatusRunning}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.RegisterOwnerAsk(t.Context(), "run-1", dispatcher.OwnerAskParams{ID: id, LaneID: id, Question: "Approve " + id + "?", Options: []string{"yes", "defer"}, Default: "defer", Deadline: "1h"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(box.questions) != 9 || box.notifications != 9 {
		t.Fatalf("mail=%d Slack=%d, want 8 individual plus 1 digest", len(box.questions), box.notifications)
	}
	if !strings.Contains(box.questions[8].Ask, "lane-08") || !strings.Contains(box.questions[8].Ask, "/asks queue") {
		t.Fatalf("digest lost overflow: %+v", box.questions[8])
	}
	asks, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil || len(asks) != 10 {
		t.Fatalf("all overflow must stay durable: asks=%+v err=%v", asks, err)
	}
	clk.Advance(time.Hour + time.Second)
	if _, err := d.RegisterOwnerAsk(t.Context(), "run-1", dispatcher.OwnerAskParams{ID: "next-hour", Question: "Next hourly budget?", Options: []string{"yes", "defer"}, Default: "defer", Deadline: "1h"}); err != nil {
		t.Fatal(err)
	}
	if len(box.questions) != 10 {
		t.Fatalf("hourly cap did not reopen: mail=%d", len(box.questions))
	}
}

func TestHostActionRecordsExactStepAndParksWithoutSteering(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	box := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1")}
	d := dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, st, drv, clk, nil)
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "run-1", ID: "lane", Status: lane.StatusRunning, SessionID: "session", TerminalID: "terminal"}); err != nil {
		t.Fatal(err)
	}
	const step = "testbed pool resize --pool chaos --size 4"
	ask := dispatcher.OwnerAskParams{ID: "host-step", LaneID: "lane", Question: "Provide the live testbed", HostAction: true, HostStep: step, Options: []string{"done", "park"}, Default: "park", Deadline: "1h", SteerNote: step}
	for range 2 {
		if _, err := d.RegisterOwnerAsk(t.Context(), "run-1", ask); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := st.Lanes().Get(t.Context(), "run-1", "lane")
	if err != nil || ln.Status != lane.StatusBlocked || len(drv.Steers) != 0 {
		t.Fatalf("host step must park: lane=%+v err=%v steers=%+v", ln, err, drv.Steers)
	}
	decisions, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: "run-1"})
	if err != nil || len(decisions) != 1 || decisions[0].Tool != "host.action" || decisions[0].Result != step {
		t.Fatalf("host task=%+v err=%v", decisions, err)
	}
	wait, err := st.Directives().Get(t.Context(), "run-1", "ask-host-step")
	if err != nil || !strings.Contains(wait.Text, step) || !wait.ExpiresAt.Equal(clk.Now().Add(time.Hour)) {
		t.Fatalf("expected wait=%+v err=%v", wait, err)
	}
	if len(box.questions) != 1 || !strings.Contains(box.questions[0].Ask, step) || box.questions[0].Default != "hold: park" {
		t.Fatalf("host ask=%+v", box.questions)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1", HolderID: "test"}, st, drv, clk, controller.NewFakeJudge())
	clk.Advance(time.Hour + time.Second)
	if err := ctrl.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	ln, err = st.Lanes().Get(t.Context(), "run-1", "lane")
	if err != nil || ln.Status != lane.StatusBlocked || len(drv.Steers) != 0 {
		t.Fatalf("deadline must keep host task parked: lane=%+v err=%v steers=%+v", ln, err, drv.Steers)
	}
}

func TestHostActionSupersedesOrdinaryAskAndCompletesItsWait(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	box := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1")}
	d := dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, st, drv, clk, nil)
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "run-1", ID: "lane", Status: lane.StatusRunning, SessionID: "session", TerminalID: "terminal"}); err != nil {
		t.Fatal(err)
	}
	ask := dispatcher.OwnerAskParams{ID: "ordinary", LaneID: "lane", Question: "Approve the budget?", Options: []string{"yes", "defer"}, Default: "defer", Deadline: "1h"}
	if _, err := d.RegisterOwnerAsk(t.Context(), "run-1", ask); err != nil {
		t.Fatal(err)
	}
	const step = "testbed pool resize --pool chaos --size 4"
	ask = dispatcher.OwnerAskParams{ID: "host", LaneID: "lane", Question: "Provide the testbed", HostAction: true, HostStep: step, Options: []string{"done", "park"}, Default: "park", Deadline: "1h"}
	for range 2 {
		if _, err := d.RegisterOwnerAsk(t.Context(), "run-1", ask); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := st.Lanes().Get(t.Context(), "run-1", "lane")
	if err != nil || ln.Status != lane.StatusBlocked || len(box.questions) != 2 || !strings.Contains(box.questions[1].Ask, step) {
		t.Fatalf("host prerequisite hidden: lane=%+v asks=%+v err=%v", ln, box.questions, err)
	}
	ctrl := controller.New(controller.Config{RunID: "run-1", HolderID: "test"}, st, drv, clk, controller.NewFakeJudge())
	if err := ctrl.AnswerOwnerAsk(t.Context(), "host", "done"); err != nil {
		t.Fatal(err)
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if strings.HasPrefix(dir.Ref, "owner_ask:") || strings.HasPrefix(dir.Ref, controller.NamedWaitRefPrefix) {
			t.Fatalf("completed host task retained a wait: %+v", dir)
		}
	}
	for _, steer := range drv.Steers {
		if strings.Contains(steer.Message, step) {
			t.Fatalf("host step steered to lane: %+v", steer)
		}
	}
}

type retryOwnerAskDriver struct {
	lane.LaneDriver
	failNext bool
}

func (d *retryOwnerAskDriver) Steer(ctx context.Context, req lane.SteerRequest) error {
	if d.failNext {
		d.failNext = false
		return errors.New("transient steer failure")
	}
	return d.LaneDriver.Steer(ctx, req)
}

type retryOwnerAskLaneStore struct {
	store.LaneStore
	failNext bool
}

func (s *retryOwnerAskLaneStore) Upsert(ctx context.Context, ln lane.Lane) error {
	if s.failNext {
		s.failNext = false
		return errors.New("transient park failure")
	}
	return s.LaneStore.Upsert(ctx, ln)
}

type retryOwnerAskStore struct {
	store.Store
	lanes store.LaneStore
}

func (s retryOwnerAskStore) Lanes() store.LaneStore { return s.lanes }

func TestOwnerAskRetriesUnfinishedLanePreparation(t *testing.T) {
	t.Parallel()
	for _, hostAction := range []bool{false, true} {
		t.Run(fmt.Sprintf("host=%t", hostAction), func(t *testing.T) {
			clk, st, drv := setupTestEnv(t)
			ctx := t.Context()
			if err := st.Lanes().Upsert(ctx, lane.Lane{RunID: "run-1", ID: "lane", Status: lane.StatusRunning, SessionID: "session", TerminalID: "terminal"}); err != nil {
				t.Fatal(err)
			}
			box := &ownerAskMailbox{Mailbox: mailboxfake.New(st.Events(), "run-1")}
			wrappedStore := retryOwnerAskStore{Store: st, lanes: &retryOwnerAskLaneStore{LaneStore: st.Lanes(), failNext: hostAction}}
			wrappedDriver := &retryOwnerAskDriver{LaneDriver: drv, failNext: !hostAction}
			makeDispatcher := func() *dispatcher.Dispatcher {
				return dispatcher.New(dispatcher.Config{Mailboxes: []mailbox.Mailbox{box}}, wrappedStore, wrappedDriver, clk, nil)
			}
			ask := dispatcher.OwnerAskParams{ID: "retry", LaneID: "lane", Question: "Approve the budget?", Options: []string{"yes", "park"}, Default: "park", Deadline: "1h"}
			if hostAction {
				ask.HostAction, ask.HostStep = true, "testbed pool resize --pool chaos --size 4"
			}
			if _, err := makeDispatcher().RegisterOwnerAsk(ctx, "run-1", ask); err == nil {
				t.Fatal("expected lane preparation failure")
			}
			if len(box.questions) != 0 {
				t.Fatal("owner notified before lane preparation completed")
			}
			// Retry after a restart using the durable original ask, even when
			// the new request is an ordinary decision for the same lane.
			ask.ID, ask.HostAction, ask.HostStep = "retry-again", false, ""
			for range 2 {
				if _, err := makeDispatcher().RegisterOwnerAsk(ctx, "run-1", ask); err != nil {
					t.Fatal(err)
				}
			}
			ln, err := st.Lanes().Get(ctx, "run-1", "lane")
			if err != nil {
				t.Fatal(err)
			}
			decisions, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
			if err != nil {
				t.Fatal(err)
			}
			if hostAction {
				if ln.Status != lane.StatusBlocked || len(drv.Steers) != 0 || len(decisions) != 1 {
					t.Fatalf("host preparation not resumed once: lane=%+v steers=%+v decisions=%+v", ln, drv.Steers, decisions)
				}
			} else if ln.Status != lane.StatusRunning || len(drv.Steers) != 1 {
				t.Fatalf("default preparation not resumed once: lane=%+v steers=%+v", ln, drv.Steers)
			}
			if len(box.questions) != 1 || box.notifications != 1 {
				t.Fatalf("notifications repeated: mail=%+v Slack=%d", box.questions, box.notifications)
			}
		})
	}
}

func TestLaneAnswersPreserveOwnerAndHostBlockRules(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{dispatcher.ToolLaneSteer, dispatcher.ToolLaneForward} {
		for _, block := range []string{"own report", "awaiting input", "owner ask", "host hold"} {
			t.Run(tool+"/"+block, func(t *testing.T) {
				clk, st, drv := setupTestEnv(t)
				ctx := t.Context()
				ln := lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusBlocked, SessionID: "s-1", TerminalID: "t-1", Question: "Restore main?"}
				wantStatus := lane.StatusRunning
				switch block {
				case "awaiting input":
					ln.Question = ""
					ln.Status = lane.StatusRunning
					ln.AwaitingInputSince = clk.Now()
				case "owner ask":
					if err := st.Directives().Add(ctx, store.Directive{ID: "ask", RunID: "run-1", Scope: ln.ID, Kind: store.DirectiveInstruction, Source: store.DirectiveSourceDispatcher, Ref: "owner_ask:pending", Text: "Owner-only decision", Status: store.DirectiveActive}); err != nil {
						t.Fatal(err)
					}
					wantStatus = lane.StatusBlocked
				case "host hold":
					ln.HostHoldReason = "host review"
					if tool == dispatcher.ToolLaneForward {
						wantStatus = lane.StatusBlocked
					}
				}
				if err := st.Lanes().Upsert(ctx, ln); err != nil {
					t.Fatal(err)
				}
				input := json.RawMessage(`{"lane_id":"question","note":"Restore main"}`)
				if tool == dispatcher.ToolLaneForward {
					input = json.RawMessage(`{"to_lane_id":"question","excerpt":"Restore main"}`)
				}
				client := model.NewFakeClient(toolUseResponse(model.ToolCall{ID: "answer", Name: tool, Input: input}), endTurnResponse("Answered."))
				d := dispatcher.New(dispatcher.Config{}, st, drv, clk, client)
				t.Cleanup(func() { _ = d.Close() })
				if _, err := d.Dispatch(ctx, "run-1", []store.Event{{ID: "question-event", LaneID: ln.ID, Kind: store.EventLaneReport, Summary: "Restore main?"}}); err != nil {
					t.Fatal(err)
				}
				got, err := st.Lanes().Get(ctx, "run-1", ln.ID)
				if err != nil || got.Status != wantStatus || !got.AwaitingInputSince.IsZero() || got.AnswerConfirmedAt.IsZero() || got.AnswerEpoch != 1 {
					t.Fatalf("answered lane=%+v err=%v, want %s with confirmation", got, err, wantStatus)
				}
				if block == "owner ask" {
					ask, err := st.Directives().Get(ctx, "run-1", "ask")
					if err != nil || ask.Status != store.DirectiveActive {
						t.Fatalf("owner ask=%+v err=%v", ask, err)
					}
				}
			})
		}
	}
}
