package concierge_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/concierge"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
)

func setupSeededStore(t *testing.T) (*clock.FakeClock, *store.MemoryStore, *lane.FakeDriver) {
	t.Helper()
	start := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()
	ctx := t.Context()

	if err := st.Runs().Create(ctx, store.Run{
		ID:            "run-1",
		Objective:     "Build the Nebula swarm orchestrator harness in orch/",
		PlanRef:       "post:5aaec193-a1f1-4009-aa55-f6174d583653",
		DefaultRecipe: recipe.DefaultName,
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	if _, err := st.Runs().AcquireLease(ctx, "run-1", "controller-1", 30*time.Second); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	for _, it := range []store.BacklogItem{
		{
			ID:         "item-alpha",
			RunID:      "run-1",
			Title:      "Implement dispatcher",
			Acceptance: "bazel test //orch/dispatcher/... green",
			Priority:   store.PriorityP0,
			Status:     store.BacklogInProgress,
			LaneID:     "lane-alpha",
			Order:      1,
		},
		{
			ID:         "item-beta",
			RunID:      "run-1",
			Title:      "Implement concierge and planner",
			Acceptance: "recorded-output tests green",
			Priority:   store.PriorityP1,
			Status:     store.BacklogOpen,
			DependsOn:  []string{"item-alpha"},
			Order:      2,
		},
	} {
		if err := st.Backlog().Add(ctx, it); err != nil {
			t.Fatalf("Add backlog %s: %v", it.ID, err)
		}
	}

	ln := lane.Lane{
		ID:            "lane-alpha",
		RunID:         "run-1",
		ItemID:        "item-alpha",
		Title:         "Implement dispatcher",
		Branch:        "swarm-lane/20261112/dispatcher",
		BaseBranch:    "main",
		Status:        lane.StatusRunning,
		RecipeName:    recipe.DefaultName,
		RecipeVersion: 1,
		PR:            42,
		PRHead:        "abc12345def67890",
		ApprovalSHA:   "abc12345def67890",
		Checks:        "green",
		Attempt:       1,
		LastEventAt:   clk.Now().Add(-3 * time.Minute),
	}
	ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-alpha-1", "term-alpha-swe-1")
	ln.RecordAttempt(recipe.PhaseReview, 1, "sess-alpha-1", "term-alpha-review-1")
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatalf("Upsert lane: %v", err)
	}

	drv.SetReport("term-alpha-review-1", lane.PhaseReport{
		LaneID:       "lane-alpha",
		TerminalID:   "term-alpha-review-1",
		Phase:        recipe.PhaseReview,
		Round:        1,
		Outcome:      lane.OutcomeDone,
		HeadSHA:      "abc12345def67890",
		PR:           42,
		RiskiestStep: "invariant vetting on tool loop",
		Observed:     "bazel test //orch/dispatcher/... -> passed",
		Summary:      "Dispatcher review passed on abc12345def67890.",
	})

	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID:    "run-1",
		Category: store.DecisionAuto,
		Actor:    "controller",
		LaneID:   "lane-alpha",
		Tool:     "phase.advance",
		Reason:   "clean -> review on verdict done (round 1)",
		Result:   "started phase review",
		Vetted:   true,
	}); err != nil {
		t.Fatalf("Append decision: %v", err)
	}

	return clk, st, drv
}

func TestInnerLoop_StatusQuestionAnsweredFromSeededMemoryStore(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	replayClient, err := model.LoadFakeClient("testdata/status_question_replay.json")
	if err != nil {
		t.Fatalf("LoadFakeClient: %v", err)
	}

	conc := concierge.New(concierge.Config{}, st, drv, clk, replayClient)
	res, err := conc.Ask(ctx, "run-1", "Where are we on run-1 and how is lane-alpha doing?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if res.Model != concierge.DefaultModel {
		t.Fatalf("Model = %q, want %q (cheap model)", res.Model, concierge.DefaultModel)
	}
	if res.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2", res.TurnCount)
	}
	if !slices.Equal(res.ToolCalls, []string{concierge.ToolStatus, concierge.ToolLaneShow}) {
		t.Fatalf("ToolCalls = %v, want [status lane.show]", res.ToolCalls)
	}
	if len(res.SubmittedDirectives) != 0 {
		t.Fatalf("expected 0 submitted directives on read-only status question, got %+v", res.SubmittedDirectives)
	}
	if len(res.Decisions) != 0 {
		t.Fatalf("expected 0 decision writes on read-only status question, got %+v", res.Decisions)
	}

	for _, wantFact := range []string{"run-1", "lane-alpha", "item-alpha", "item-beta", "#42", "abc12345"} {
		if !slices.Contains(res.CitedFacts, wantFact) {
			t.Fatalf("CitedFacts = %v, missing %q", res.CitedFacts, wantFact)
		}
	}

	if len(replayClient.Requests) != 2 {
		t.Fatalf("len(Requests) = %d, want 2", len(replayClient.Requests))
	}
	toolResMsg := replayClient.Requests[1].Messages[2]
	if len(toolResMsg.Content) != 2 {
		t.Fatalf("tool result count = %d, want 2", len(toolResMsg.Content))
	}
	if !strings.Contains(toolResMsg.Content[0].Content, "Objective: Build the Nebula swarm orchestrator harness in orch/") {
		t.Fatalf("status tool output missing objective:\n%s", toolResMsg.Content[0].Content)
	}
	if !strings.Contains(toolResMsg.Content[1].Content, "Dispatcher review passed on abc12345def67890.") {
		t.Fatalf("lane.show tool output missing phase report summary:\n%s", toolResMsg.Content[1].Content)
	}

	th, err := st.Threads().GetThread(ctx, res.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Role != store.ThreadRoleConcierge || th.RunID != "run-1" {
		t.Fatalf("thread = %+v", th)
	}

	replayClient.Enqueue(model.Response{
		ID:         "msg_second_conv",
		Model:      concierge.DefaultModel,
		StopReason: model.StopReasonEndTurn,
		Message: model.NewAssistantMessage(
			model.NewTextBlock("On `run-1`, `item-beta` is still `open` while `lane-alpha` runs."),
		),
	}, nil)
	res2, err := conc.Ask(ctx, "run-1", "What is next after lane-alpha?")
	if err != nil {
		t.Fatalf("second Ask: %v", err)
	}
	if res2.ThreadID == res.ThreadID {
		t.Fatalf("expected fresh thread per owner conversation, got same thread %q", res2.ThreadID)
	}
}

func TestConcierge_RuleDirectiveReplay(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	replayClient, err := model.LoadFakeClient("testdata/rule_directive_replay.json")
	if err != nil {
		t.Fatalf("LoadFakeClient: %v", err)
	}

	conc := concierge.New(concierge.Config{}, st, drv, clk, replayClient)
	res, err := conc.Ask(ctx, "run-1", "Standing rule for the next 24h: never spawn more than 2 concurrent lanes touching store/.")
	if err != nil {
		t.Fatalf("Ask (rule directive): %v", err)
	}

	if len(res.SubmittedDirectives) != 1 {
		t.Fatalf("len(SubmittedDirectives) = %d, want 1", len(res.SubmittedDirectives))
	}
	dir := res.SubmittedDirectives[0]
	if dir.ID != "dir-rule-store-cap" || dir.Kind != store.DirectiveStanding || dir.Source != store.DirectiveSourceConcierge {
		t.Fatalf("submitted directive = %+v", dir)
	}
	if dir.Status != store.DirectiveActive || dir.ExpiresAt == nil {
		t.Fatalf("expected active directive with expiry, got %+v", dir)
	}
	wantExp := clk.Now().Add(24 * time.Hour)
	if !dir.ExpiresAt.Equal(wantExp) {
		t.Fatalf("ExpiresAt = %v, want %v", dir.ExpiresAt, wantExp)
	}

	storedDir, err := st.Directives().Get(ctx, "run-1", "dir-rule-store-cap")
	if err != nil {
		t.Fatalf("Get directive: %v", err)
	}
	if storedDir.Text != "Never spawn more than 2 concurrent lanes touching store/." {
		t.Fatalf("storedDir.Text = %q", storedDir.Text)
	}

	if len(res.Decisions) != 1 || res.Decisions[0].Tool != concierge.ToolDirectiveSubmit {
		t.Fatalf("Decisions = %+v, want 1 directive.submit decision", res.Decisions)
	}
	if !slices.Contains(res.CitedFacts, "dir-rule-store-cap") {
		t.Fatalf("CitedFacts = %v, expected dir-rule-store-cap", res.CitedFacts)
	}
}

func TestConcierge_DecisionsSinceAndBacklogTools(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	callDecisions, _ := json.Marshal(concierge.DecisionsSinceParams{SinceSeq: 0, Limit: 5})
	callBacklog, _ := json.Marshal(concierge.BacklogParams{Status: "open"})
	callOrder, _ := json.Marshal(concierge.DirectiveSubmitParams{
		ID:   "dir-order-1",
		Kind: concierge.KindOrder,
		Text: "Prioritize item-beta as soon as lane-alpha merges",
	})

	fakeClient := model.NewFakeClient(
		model.Response{
			ID:         "msg_tools_1",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_1", concierge.ToolDecisionsSince, callDecisions),
				model.NewToolUseBlock("tu_2", concierge.ToolBacklog, callBacklog),
				model.NewToolUseBlock("tu_3", concierge.ToolDirectiveSubmit, callOrder),
			),
		},
		model.Response{
			ID:         "msg_tools_2",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Decision `dec-1` (`phase.advance`) moved `lane-alpha` to review; open backlog item is `item-beta`, and order `dir-order-1` is queued."),
			),
		},
	)

	conc := concierge.New(concierge.Config{}, st, drv, clk, fakeClient)
	res, err := conc.Ask(ctx, "run-1", "Show recent decisions and open backlog, and queue order to prioritize item-beta.")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if !slices.Equal(res.ToolCalls, []string{concierge.ToolDecisionsSince, concierge.ToolBacklog, concierge.ToolDirectiveSubmit}) {
		t.Fatalf("ToolCalls = %v", res.ToolCalls)
	}
	if len(res.SubmittedDirectives) != 1 || res.SubmittedDirectives[0].Kind != store.DirectiveInstruction {
		t.Fatalf("SubmittedDirectives = %+v", res.SubmittedDirectives)
	}
}

func TestConcierge_RejectsUncitedOrFabricatedAnswers(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	t.Run("RejectsUncitedAnswer", func(t *testing.T) {
		t.Parallel()
		fakeClient := model.NewFakeClient(model.Response{
			ID:         "msg_uncited",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Everything is going great, no issues at all!"),
			),
		})
		conc := concierge.New(concierge.Config{}, st, drv, clk, fakeClient)
		_, err := conc.Ask(ctx, "run-1", "How are things going?")
		if !errors.Is(err, concierge.ErrUncitedAnswer) {
			t.Fatalf("expected ErrUncitedAnswer, got %v", err)
		}
	})

	t.Run("RejectsFabricatedLaneID", func(t *testing.T) {
		t.Parallel()
		fakeClient := model.NewFakeClient(model.Response{
			ID:         "msg_fabricated",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("On `run-1`, `lane-phantom` is currently running."),
			),
		})
		conc := concierge.New(concierge.Config{}, st, drv, clk, fakeClient)
		_, err := conc.Ask(ctx, "run-1", "Which lanes are running?")
		if !errors.Is(err, concierge.ErrUncitedAnswer) {
			t.Fatalf("expected ErrUncitedAnswer for fabricated lane-phantom, got %v", err)
		}
	})
}
