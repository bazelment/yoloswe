package concierge_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/brief"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestMemoryWorkflow_OwnerCorrectionFlowsToNextDispatcherSnapshotAndWorkerBrief(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()
	fake := model.NewFakeClient(model.Response{
		StopReason: model.StopReasonToolUse,
		Message:    model.NewAssistantMessage(model.NewToolUseBlock("save", concierge.ToolMemorySubmit, json.RawMessage(`{"action":"create","id":"memory-random-ports","name":"Random ports","description":"Use random ports","body":"Worker tests use random ports.","type":"rule","scope":"run"}`))),
	}, model.Response{
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock("Saved.")),
	})
	c := concierge.New(concierge.Config{}, st, drv, clk, fake)
	_, askErr := c.Ask(ctx, "run-1", "Use random ports in worker tests.")
	if askErr != nil {
		t.Fatalf("concierge correction: %v calls=%v", askErr, fake.Requests)
	}
	mem, err := st.Memories().Get(ctx, "", "memory-random-ports")
	if err != nil || mem.Source != "Use random ports in worker tests." {
		t.Fatalf("memory = %+v, err=%v; want source quote", mem, err)
	}
	snap, err := dispatcher.BuildSnapshot(ctx, st, clk, "run-1", nil, dispatcher.SnapshotConfig{})
	if err != nil {
		t.Fatalf("dispatcher snapshot: %v", err)
	}
	if !strings.Contains(snap.Text, "memory-random-ports") || !strings.Contains(snap.Text, "Use random ports") {
		t.Fatalf("snapshot missing correction: %s", snap.Text)
	}
	raw := lane.NewFakeDriver()
	decorated := brief.WithMemories(st, raw)
	if _, err := decorated.SpawnSession(ctx, lane.SpawnRequest{RunID: "run-1", LaneID: "lane-alpha", Brief: "Implement tests"}); err != nil {
		t.Fatalf("worker brief: %v", err)
	}
	if !strings.Contains(raw.Spawns[0].Brief, "- [memory-random-ports] Random ports: Use random ports") || strings.Contains(raw.Spawns[0].Brief, "Source:") || strings.Contains(raw.Spawns[0].Brief, "Worker tests use random ports.") {
		t.Fatalf("worker brief missing one-line correction: %s", raw.Spawns[0].Brief)
	}
	if _, err := c.Ask(ctx, "run-1", "forget"); err != nil {
		t.Fatalf("forget correction: %v", err)
	}
	if _, err := st.Memories().Get(ctx, "", "memory-random-ports"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("forgotten memory still exists, err=%v", err)
	}
}

func TestRunSettings_UpdateStatusAndRejectInvalidValues(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()
	cap := 2
	fake := model.NewFakeClient(
		model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("set", concierge.ToolRunSettings, mustJSON(concierge.RunSettingsParams{Action: "set", ConcurrencyCap: &cap, StallTimeout: "1h", Words: "run at most 2 lanes at a time"})))},
		model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Updated run-1 settings."))},
	)
	conc := concierge.New(concierge.Config{}, st, drv, clk, fake)
	if _, err := conc.Ask(ctx, "run-1", "run at most 2 lanes at a time"); err != nil {
		t.Fatalf("Ask settings: %v", err)
	}
	settings, err := st.Runs().GetSettings(ctx, "run-1")
	if err != nil || settings.ConcurrencyCap != 2 || settings.StallTimeout != time.Hour {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil || len(decs) == 0 || !strings.Contains(decs[len(decs)-1].Reason, "run at most 2 lanes at a time") {
		t.Fatalf("decision words missing: %+v err=%v", decs, err)
	}
}

func TestRunSettings_StatusShowsCurrentSettings(t *testing.T) {
	clk, st, drv := setupSeededStore(t)
	if err := st.Runs().UpdateSettings(t.Context(), "run-1", store.RunSettings{ConcurrencyCap: 2, StallTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	fake := model.NewFakeClient(model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("get", concierge.ToolRunSettings, mustJSON(concierge.RunSettingsParams{Action: "get"})))}, model.Response{Message: model.NewAssistantMessage(model.NewTextBlock("run-1 settings shown."))})
	conc := concierge.New(concierge.Config{}, st, drv, clk, fake)
	if _, err := conc.Ask(t.Context(), "run-1", "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.Requests[1].Messages[2].Content[0].Content, "concurrency_cap=2") {
		t.Fatalf("status output missing cap: %s", fake.Requests[1].Messages[2].Content[0].Content)
	}
}

func TestRunSettings_InvalidValuesRejected(t *testing.T) {
	t.Parallel()
	for name, params := range map[string]concierge.RunSettingsParams{
		"cap":     {Action: "set", ConcurrencyCap: intPtr(17)},
		"timeout": {Action: "set", StallTimeout: "1m"},
	} {
		t.Run(name, func(t *testing.T) {
			clk, st, drv := setupSeededStore(t)
			fake := model.NewFakeClient(
				model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("bad", concierge.ToolRunSettings, mustJSON(params)))},
				model.Response{Message: model.NewAssistantMessage(model.NewTextBlock("run-1 settings rejected."))},
			)
			if _, err := concierge.New(concierge.Config{}, st, drv, clk, fake).Ask(t.Context(), "run-1", "set invalid settings"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(fake.Requests[1].Messages[2].Content[0].Content, "must be") {
				t.Fatalf("missing rejection: %s", fake.Requests[1].Messages[2].Content[0].Content)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

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
		Objective:     "Build the Nebula swarm orchestrator harness in orca/",
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
			Acceptance: "bazel test //orca/dispatcher/... green",
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
		Observed:     "bazel test //orca/dispatcher/... -> passed",
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
	if !strings.Contains(toolResMsg.Content[0].Content, "Objective: Build the Nebula swarm orchestrator harness in orca/") {
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

	for _, actor := range []string{"dispatcher", "planner"} {
		if _, err := st.Decisions().Append(ctx, store.Decision{
			RunID:    "run-1",
			Category: store.DecisionJudgment,
			Actor:    actor,
			LaneID:   "lane-alpha",
			Tool:     "lane.steer",
			Reason:   "steer by " + actor,
			Result:   "steered lane-alpha",
			Vetted:   true,
		}); err != nil {
			t.Fatalf("Append decision: %v", err)
		}
	}

	callDecisions, _ := json.Marshal(concierge.DecisionsSinceParams{SinceSeq: 0, Limit: 2})
	callBacklog, _ := json.Marshal(concierge.BacklogParams{Status: "open"})
	callOrder, _ := json.Marshal(concierge.DirectiveSubmitParams{
		ID:    "dir-order-1",
		Kind:  concierge.KindOrder,
		Scope: "run",
		Text:  "Prioritize item-beta as soon as lane-alpha merges",
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
				model.NewTextBlock("Recent decisions on `lane-alpha` reached `seq=3`; open backlog item is `item-beta`, and order `dir-order-1` is queued."),
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
	if len(res.Decisions) != 1 || res.Decisions[0].LaneID != "" {
		t.Fatalf("expected non-lane scope 'run' to leave Decision.LaneID empty, got %+v", res.Decisions)
	}

	toolResMsg := fakeClient.Requests[1].Messages[2]
	decOutput := toolResMsg.Content[0].Content
	if !strings.Contains(decOutput, "Decisions (2 shown of 3, next since_seq=3):") || !strings.Contains(decOutput, "seq=2") || !strings.Contains(decOutput, "seq=3") || strings.Contains(decOutput, "seq=1 ") {
		t.Fatalf("expected decisions.since without cursor to return latest 2 tail decisions, got:\n%s", decOutput)
	}
	backlogOutput := toolResMsg.Content[1].Content
	if !strings.Contains(backlogOutput, "item-beta") || strings.Contains(backlogOutput, "item-alpha [") {
		t.Fatalf("expected backlog status=open output to list only item-beta, got:\n%s", backlogOutput)
	}
}

func TestConcierge_DirectiveHandlerFailureRevokesDirectiveAndRecordsDecision(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	callSubmit, _ := json.Marshal(concierge.DirectiveSubmitParams{
		ID:    "dir-bad-plan",
		Kind:  concierge.KindPlanImport,
		Scope: "lane-alpha",
		Ref:   "missing-plan.md",
	})

	fakeClient := model.NewFakeClient(
		model.Response{
			ID:         "msg_fail_1",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_fail_1", concierge.ToolDirectiveSubmit, callSubmit),
			),
		},
		model.Response{
			ID:         "msg_fail_2",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Plan import on `run-1` for `lane-alpha` failed because `missing-plan.md` could not be read."),
			),
		},
	)

	conc := concierge.New(concierge.Config{
		OnDirective: func(_ context.Context, _ string, _ store.Directive) (string, error) {
			return "", errors.New("plan file not found")
		},
	}, st, drv, clk, fakeClient)

	res, err := conc.Ask(ctx, "run-1", "Import missing-plan.md")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(res.SubmittedDirectives) != 0 {
		t.Fatalf("expected failed directive to not be reported as submitted, got %+v", res.SubmittedDirectives)
	}
	dir, err := st.Directives().Get(ctx, "run-1", "dir-bad-plan")
	if err != nil {
		t.Fatalf("Get dir-bad-plan: %v", err)
	}
	if dir.Status != store.DirectiveRevoked {
		t.Fatalf("dir.Status = %q, want %q", dir.Status, store.DirectiveRevoked)
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Vetted || res.Decisions[0].LaneID != "lane-alpha" {
		t.Fatalf("expected 1 unvetted decision scoped to lane-alpha, got %+v", res.Decisions)
	}
}

func TestConcierge_RejectsUncitedOrFabricatedAnswers(t *testing.T) {
	t.Parallel()

	t.Run("RejectsUncitedAnswer", func(t *testing.T) {
		t.Parallel()
		clk, st, drv := setupSeededStore(t)
		fakeClient := model.NewFakeClient(model.Response{
			ID:         "msg_uncited",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Everything is in review with the default settings and status is great!"),
			),
		})
		conc := concierge.New(concierge.Config{}, st, drv, clk, fakeClient)
		_, err := conc.Ask(t.Context(), "run-1", "How are things going?")
		if !errors.Is(err, concierge.ErrUncitedAnswer) {
			t.Fatalf("expected ErrUncitedAnswer, got %v", err)
		}
	})

	t.Run("RejectsFabricatedLaneID", func(t *testing.T) {
		t.Parallel()
		clk, st, drv := setupSeededStore(t)
		fakeClient := model.NewFakeClient(model.Response{
			ID:         "msg_fabricated",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("On `run-1`, `lane-phantom` is currently running."),
			),
		})
		conc := concierge.New(concierge.Config{}, st, drv, clk, fakeClient)
		_, err := conc.Ask(t.Context(), "run-1", "Which lanes are running?")
		if !errors.Is(err, concierge.ErrUncitedAnswer) {
			t.Fatalf("expected ErrUncitedAnswer for fabricated lane-phantom, got %v", err)
		}
	})

	t.Run("RejectsFabricatedDirectiveID", func(t *testing.T) {
		t.Parallel()
		clk, st, drv := setupSeededStore(t)
		fakeClient := model.NewFakeClient(model.Response{
			ID:         "msg_fab_dir",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("On `run-1`, directive `dir-ghost` is active."),
			),
		})
		conc := concierge.New(concierge.Config{}, st, drv, clk, fakeClient)
		_, err := conc.Ask(t.Context(), "run-1", "Which directives are active?")
		if !errors.Is(err, concierge.ErrUncitedAnswer) {
			t.Fatalf("expected ErrUncitedAnswer for fabricated dir-ghost, got %v", err)
		}
	})
}

func TestMemoryWorkflow_ChatCorrectionReachesNextSnapshotAndWorkerBrief(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()
	const quote = "Correction: always use random ports in worker tests."
	client := model.NewFakeClient(
		model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("save", "memory.submit", json.RawMessage(`{"action":"create","id":"memory-ports","name":"Random ports","description":"Use random ports in worker tests","body":"Always use random ports in worker tests."}`)))},
		model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Saved memory-ports on run-1; undo to revert."))},
	)
	conc := concierge.New(concierge.Config{}, st, drv, clk, client)
	if _, err := conc.Ask(ctx, "run-1", quote); err != nil {
		t.Fatal(err)
	}
	m, err := st.Memories().Get(ctx, "", "memory-ports")
	if err != nil {
		t.Fatalf("correction was not saved: %v", err)
	}
	if m.Source != quote {
		t.Fatalf("source = %q, want owner quote %q", m.Source, quote)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{ID: "item-memory-worker", RunID: "run-1", Title: "Worker tests", Status: store.BacklogOpen, Priority: store.PriorityP1}); err != nil {
		t.Fatal(err)
	}
	workerClient := model.NewFakeClient(
		model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("spawn", dispatcher.ToolLaneSpawn, json.RawMessage(`{"item_id":"item-memory-worker","lane_id":"lane-memory-worker","brief":"Implement worker tests"}`)))},
		model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Started worker."))},
	)
	disp := dispatcher.New(dispatcher.Config{}, st, drv, clk, workerClient)
	result, err := disp.Dispatch(ctx, "run-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Snapshot.Text, "memory-ports") {
		t.Fatal("next dispatcher snapshot omitted correction")
	}
	if len(result.Rejected) != 0 {
		t.Fatalf("spawn rejected: %+v", result.Rejected)
	}
	if len(drv.Spawns) != 1 || !strings.Contains(drv.Spawns[0].Brief, "- [memory-ports] Random ports: Use random ports in worker tests") || strings.Contains(drv.Spawns[0].Brief, quote) {
		t.Fatalf("next worker brief omitted sourced correction: %+v", drv.Spawns)
	}
}

func TestConcierge_MemoryCorrectionsAndUndo(t *testing.T) {
	t.Parallel()
	for _, undoWord := range []string{"undo", "forget"} {
		t.Run(undoWord, func(t *testing.T) {
			t.Parallel()
			clk, st, drv := setupSeededStore(t)
			client := model.NewFakeClient()
			conc := concierge.New(concierge.Config{}, st, drv, clk, client)
			th, err := conc.StartThread(t.Context(), "run-1", "Owner corrections")
			if err != nil {
				t.Fatal(err)
			}
			submit := func(quote, args string) string {
				t.Helper()
				client.Enqueue(model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("memory-change", "memory.submit", json.RawMessage(args)))}, nil)
				client.Enqueue(model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("arbitrary model answer"))}, nil)
				res, err := conc.AskInThread(t.Context(), "run-1", th.ID, quote)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(res.Answer, "\n") || !strings.Contains(res.Answer, "undo") {
					t.Fatalf("missing one-line echo/undo: %q", res.Answer)
				}
				return res.Answer
			}
			undo := func() {
				t.Helper()
				n := len(client.Requests)
				res, err := conc.AskInThread(t.Context(), "run-1", th.ID, undoWord)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(res.Answer, "Undid memory") || len(client.Requests) != n {
					t.Fatalf("undo did not revert deterministically: %+v", res)
				}
			}
			submit("Use ephemeral ports.", `{"action":"create","id":"memory-test","name":"Ports","body":"Use ephemeral ports."}`)
			m, err := st.Memories().Get(t.Context(), "", "memory-test")
			if err != nil || m.Source != "Use ephemeral ports." {
				t.Fatalf("memory/source: %+v, %v", m, err)
			}
			if !strings.Contains(client.Requests[0].System, "starter-inner-loop") {
				t.Fatal("canonical index missing")
			}
			undo()
			if _, err := st.Memories().Get(t.Context(), "", "memory-test"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("create undo: %v", err)
			}
			submit("Use ephemeral ports.", `{"action":"create","id":"memory-test","name":"Ports","body":"Use ephemeral ports."}`)
			submit("Correction: use Unix sockets.", `{"action":"update","id":"memory-test","body":"Use Unix sockets."}`)
			m, err = st.Memories().Get(t.Context(), "", "memory-test")
			if err != nil || m.Body != "Use Unix sockets." || m.Source != "Correction: use Unix sockets." {
				t.Fatalf("updated memory: %+v, %v", m, err)
			}
			if !strings.Contains(client.Requests[len(client.Requests)-2].System, "memory-test") {
				t.Fatal("next canonical index missing created memory")
			}
			undo()
			m, err = st.Memories().Get(t.Context(), "", "memory-test")
			if err != nil || m.Body != "Use ephemeral ports." || m.Source != "Use ephemeral ports." {
				t.Fatalf("update undo: %+v, %v", m, err)
			}
			submit("Delete the ports memory.", `{"action":"delete","id":"memory-test"}`)
			if _, err := st.Memories().Get(t.Context(), "", "memory-test"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("delete: %v", err)
			}
			undo()
			m, err = st.Memories().Get(t.Context(), "", "memory-test")
			if err != nil || m.Body != "Use ephemeral ports." || m.Source != "Use ephemeral ports." {
				t.Fatalf("delete undo: %+v, %v", m, err)
			}
		})
	}
}

func TestConcierge_DirectiveSubmitWritesSourcedMemoryAndUndoRevokesAlias(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()
	const quote = "Standing rule: use random ports."
	client := model.NewFakeClient(
		model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("directive", "directive.submit", json.RawMessage(`{"id":"dir-memory-ports","kind":"rule","text":"Use random ports."}`)))},
		model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Saved dir-memory-ports on run-1."))},
	)
	concierge := concierge.New(concierge.Config{}, st, drv, clk, client)
	result, err := concierge.Ask(ctx, "run-1", quote)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := st.Memories().Get(ctx, "", "dir-memory-ports")
	if err != nil || saved.Source != quote || saved.Body != "Use random ports." {
		t.Fatalf("directive memory: %+v, %v", saved, err)
	}
	if _, err := concierge.AskInThread(ctx, "run-1", result.ThreadID, "undo"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Memories().Get(ctx, "", "dir-memory-ports"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("undo kept directive memory: %v", err)
	}
	alias, err := st.Directives().Get(ctx, "run-1", "dir-memory-ports")
	if err != nil || alias.Status != store.DirectiveRevoked {
		t.Fatalf("undo kept directive alias active: %+v, %v", alias, err)
	}
}

func TestConcierge_DirectiveRevokeRemovesDirectiveAndDecisionRecorded(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	if err := st.Directives().Add(ctx, store.Directive{
		ID:        "dir-hold-m4",
		RunID:     "run-1",
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceOwner,
		Text:      "hold M4 until M3 finishes",
		Status:    store.DirectiveActive,
		CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	client := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(model.NewToolUseBlock(
				"call-revoke",
				concierge.ToolDirectiveRevoke,
				json.RawMessage(`{"id":"dir-hold-m4","reason":"owner cleared the hold on M4"}`),
			)),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Revoked dir-hold-m4 on run-1.")),
		},
	)
	c := concierge.New(concierge.Config{DisableCitations: true}, st, drv, clk, client)
	if _, err := c.Ask(ctx, "run-1", "clear the hold on M4"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	dir, err := st.Directives().Get(ctx, "run-1", "dir-hold-m4")
	if err != nil || dir.Status != store.DirectiveRevoked {
		t.Fatalf("dir-hold-m4 after revoke = %+v, %v, want revoked", dir, err)
	}
	active, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.ID == "dir-hold-m4" {
			t.Fatalf("active directives still include dir-hold-m4: %+v", active)
		}
	}
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1", Actor: concierge.ActorConcierge})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range decs {
		if d.Tool == concierge.ToolDirectiveRevoke && d.Vetted {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected vetted %q decision in %+v", concierge.ToolDirectiveRevoke, decs)
	}

	// Revoking a nonexistent directive must return a not-found error to the model
	// rather than recording a vetted success decision.
	clientNotFound := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_revoke_missing", concierge.ToolDirectiveRevoke, json.RawMessage(`{"id":"does-not-exist","reason":"test missing"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Directive was not found.")),
		},
	)
	cNotFound := concierge.New(concierge.Config{DisableCitations: true}, st, drv, clk, clientNotFound)
	if _, err := cNotFound.Ask(ctx, "run-1", "clear does-not-exist"); err != nil {
		t.Fatalf("Ask missing directive: %v", err)
	}
	if len(clientNotFound.Requests) != 2 || !clientNotFound.Requests[1].Messages[2].Content[0].IsError {
		t.Fatalf("expected error tool_result for nonexistent directive.revoke, got %+v", clientNotFound.Requests)
	}
}

func TestConcierge_KeepTheHoldOnAdoptedRunCreatesNoStandingNoNewLanesRule(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	const adoptedRunID = "20261114-trial-m1"
	if err := st.Runs().Create(ctx, store.Run{
		ID:            adoptedRunID,
		Repository:    "bazelment/yoloswe",
		Objective:     "Goal: trial m1\nNext: when #5786 merges, staff a lane for 6-7",
		PlanRef:       "ORCHESTRATOR: orca since 2026-11-14T18:18:00Z (orca-chat:front-desk)",
		DefaultRecipe: "adopted",
	}); err != nil {
		t.Fatal(err)
	}

	const ownerQuote = "for run 20261114-trial-m1: keep the hold; when m2-gaps2 finishes swe, carry on with its normal phases"

	// 1. Even if a model tries to submit a standing rule or an invented no-new-lanes
	// directive/memory from "keep the hold", it is rejected and only the legitimate
	// one-time order to carry on with normal phases is accepted (without creating a
	// standing rule in Directives or Memories).
	badRule, _ := json.Marshal(concierge.DirectiveSubmitParams{
		ID:   "dir-invented-hold",
		Kind: concierge.KindRule,
		Text: "Standing owner hold: start no new lanes until you say otherwise.",
	})
	badMemory, _ := json.Marshal(concierge.MemorySubmitParams{
		Action: "create",
		ID:     "mem-invented-hold",
		Name:   "Standing owner hold",
		Type:   "rule",
		Body:   "No new lanes until the owner says otherwise.",
	})
	goodOrder, _ := json.Marshal(concierge.DirectiveSubmitParams{
		ID:    "dir-carry-on",
		Kind:  concierge.KindOrder,
		Scope: "m2-gaps2",
		Text:  "Keep the lease on 20261114-trial-m1; when m2-gaps2 finishes swe, carry on with its normal phases.",
	})

	client := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_bad_rule", concierge.ToolDirectiveSubmit, badRule),
				model.NewToolUseBlock("tu_bad_mem", "memory.submit", badMemory),
				model.NewToolUseBlock("tu_good_order", concierge.ToolDirectiveSubmit, goodOrder),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Kept the lease on `20261114-trial-m1` and queued order `dir-carry-on` so `m2-gaps2` continues through its normal phases."),
			),
		},
	)

	conc := concierge.New(concierge.Config{}, st, drv, clk, client)
	if _, err := conc.Ask(ctx, adoptedRunID, ownerQuote); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Verify no standing rule directive exists on the adopted run.
	standingDirs, err := st.Directives().List(ctx, store.DirectiveFilter{
		RunID:      adoptedRunID,
		Kinds:      []store.DirectiveKind{store.DirectiveStanding},
		ActiveOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(standingDirs) != 0 {
		t.Fatalf("expected 0 standing rule directives on %s, got %+v", adoptedRunID, standingDirs)
	}

	// Verify no standing rule memory was created on the adopted run (not even from the valid one-time order).
	ruleMems, err := st.Memories().List(ctx, store.MemoryFilter{RunID: adoptedRunID, Type: "rule"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ruleMems {
		if m.Source != "orca starter set" {
			t.Fatalf("expected 0 non-starter rule memories on %s, got %+v", adoptedRunID, m)
		}
	}

	// 2. Also verify the Dispatcher cannot invent a standing no-new-lanes note from the same owner message.
	dispClient := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_disp_note", dispatcher.ToolDirectiveNote, json.RawMessage(`{"id":"note-hold","fact":"The standing owner hold is active: no new lanes until you say otherwise"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Acknowledged.")),
		},
	)
	disp := dispatcher.New(dispatcher.Config{}, st, drv, clk, dispClient)
	dispRes, err := disp.Dispatch(ctx, adoptedRunID, []store.Event{
		{
			ID:      "ev-owner-hold",
			RunID:   adoptedRunID,
			Kind:    store.EventOwnerMessage,
			Class:   store.EventClassJudgment,
			Source:  "owner",
			Summary: ownerQuote,
		},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(dispRes.Rejected) != 1 {
		t.Fatalf("expected dispatcher invented hold note to be rejected, got Rejected=%+v", dispRes.Rejected)
	}
	ruleMems, _ = st.Memories().List(ctx, store.MemoryFilter{RunID: adoptedRunID, Type: "rule"})
	for _, m := range ruleMems {
		if m.Source != "orca starter set" {
			t.Fatalf("expected 0 non-starter rule memories after dispatcher turn, got %+v", m)
		}
	}

	// 3. Even if a model-authored EventOwnerMessage (Source="concierge" or "planner") or
	// DirectiveSourceConcierge directive in the store contains "no new lanes" text,
	// validateDispatcherNoteFact must still reject a dispatcher no-new-lanes note because
	// the owner never stated it.
	_, _ = st.Events().Append(ctx, store.Event{
		ID:      "ev-concierge-synthetic",
		RunID:   adoptedRunID,
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Source:  "concierge",
		Summary: "concierge directive: no new lanes",
	})
	_ = st.Directives().Add(ctx, store.Directive{
		ID:     "dir-concierge-synthetic",
		RunID:  adoptedRunID,
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceConcierge,
		Text:   "no new lanes",
		Status: store.DirectiveActive,
	})
	dispClient2 := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_disp_note_2", dispatcher.ToolDirectiveNote, json.RawMessage(`{"id":"note-hold-2","fact":"Standing hold: start no new lanes"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Acknowledged.")),
		},
	)
	disp2 := dispatcher.New(dispatcher.Config{}, st, drv, clk, dispClient2)
	dispRes2, err := disp2.Dispatch(ctx, adoptedRunID, []store.Event{
		{
			ID:      "ev-owner-hold-2",
			RunID:   adoptedRunID,
			Kind:    store.EventOwnerMessage,
			Class:   store.EventClassJudgment,
			Source:  "owner",
			Summary: ownerQuote,
		},
	})
	if err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	if len(dispRes2.Rejected) != 1 {
		t.Fatalf("expected dispatcher note to be rejected despite non-owner store records, got Rejected=%+v", dispRes2.Rejected)
	}

	// 4. Negated or quoted no-new-lanes substrings in an owner message must NOT authorize a standing rule or dispatcher hold note.
	negatedOwnerQuote := `I do not want a "no new lanes" rule; remove the 'do not start new lanes' note.`
	if err := concierge.ValidateStandingRule("Start no new lanes until told otherwise", negatedOwnerQuote); err == nil {
		t.Fatal("expected ValidateStandingRule to reject no-new-lanes rule when owner message only mentions it inside quotes/negation")
	}
	// An unrelated standing rule is allowed even when the owner quote mentions "keep the hold"
	// or when the rule contains words with "hold" as a substring (e.g., "threshold", "placeholder").
	if err := concierge.ValidateStandingRule("Always run lint before pushing.", ownerQuote); err != nil {
		t.Fatalf("expected unrelated standing rule to be allowed alongside 'keep the hold', got: %v", err)
	}
	if err := concierge.ValidateStandingRule("Set stall threshold to 30m and avoid placeholder text.", ownerQuote); err != nil {
		t.Fatalf("expected rule containing 'threshold'/'placeholder' to be allowed alongside 'keep the hold', got: %v", err)
	}
	// A factual dispatcher note that mentions "keep the hold" (orca lease) without claiming a work/lane block is allowed.
	dispClientLeaseNote := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_disp_lease_note", dispatcher.ToolDirectiveNote, json.RawMessage(`{"id":"note-lease-hold","fact":"Owner asked to keep the hold (orca lease) on run 20261114-trial-m1 while m2-gaps2 finishes."}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Recorded lease note.")),
		},
	)
	dispLease := dispatcher.New(dispatcher.Config{}, st, drv, clk, dispClientLeaseNote)
	dispResLease, err := dispLease.Dispatch(ctx, adoptedRunID, []store.Event{
		{
			ID:      "ev-owner-lease-note",
			RunID:   adoptedRunID,
			Kind:    store.EventOwnerMessage,
			Class:   store.EventClassJudgment,
			Source:  "owner",
			Summary: ownerQuote,
		},
	})
	if err != nil {
		t.Fatalf("Dispatch lease note: %v", err)
	}
	if len(dispResLease.Rejected) != 0 {
		t.Fatalf("expected factual lease hold note to be allowed, got Rejected=%+v", dispResLease.Rejected)
	}
	// A dispatcher note that uses "hold" as a work block ("Keep the hold: don't advance m3") is rejected when the owner only said "keep the hold".
	dispClientBlockHold := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_disp_block_hold", dispatcher.ToolDirectiveNote, json.RawMessage(`{"id":"note-block-hold","fact":"Keep the hold: don't advance m3"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Acknowledged.")),
		},
	)
	dispBlockHold := dispatcher.New(dispatcher.Config{}, st, drv, clk, dispClientBlockHold)
	dispResBlockHold, err := dispBlockHold.Dispatch(ctx, adoptedRunID, []store.Event{
		{
			ID:      "ev-owner-block-hold",
			RunID:   adoptedRunID,
			Kind:    store.EventOwnerMessage,
			Class:   store.EventClassJudgment,
			Source:  "owner",
			Summary: ownerQuote,
		},
	})
	if err != nil {
		t.Fatalf("Dispatch block hold note: %v", err)
	}
	if len(dispResBlockHold.Rejected) != 1 {
		t.Fatalf("expected work-blocking 'Keep the hold: don't advance m3' note to be rejected, got Rejected=%+v", dispResBlockHold.Rejected)
	}
	dispClient3 := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_disp_note_3", dispatcher.ToolDirectiveNote, json.RawMessage(`{"id":"note-hold-3","fact":"Standing hold: start no new lanes"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Acknowledged.")),
		},
	)
	disp3 := dispatcher.New(dispatcher.Config{}, st, drv, clk, dispClient3)
	dispRes3, err := disp3.Dispatch(ctx, adoptedRunID, []store.Event{
		{
			ID:      "ev-owner-negated",
			RunID:   adoptedRunID,
			Kind:    store.EventOwnerMessage,
			Class:   store.EventClassJudgment,
			Source:  "owner",
			Summary: negatedOwnerQuote,
		},
	})
	if err != nil {
		t.Fatalf("Dispatch 3: %v", err)
	}
	if len(dispRes3.Rejected) != 1 {
		t.Fatalf("expected dispatcher note to be rejected for negated/quoted owner message, got Rejected=%+v", dispRes3.Rejected)
	}

	// 5. When the current batch's owner event plainly states a no-new-lanes hold, dispatcher directive.note is allowed.
	dispClient4 := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_disp_note_4", dispatcher.ToolDirectiveNote, json.RawMessage(`{"id":"note-explicit-hold","fact":"Owner requested: do not start new lanes while auditing."}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Recorded explicit hold.")),
		},
	)
	disp4 := dispatcher.New(dispatcher.Config{}, st, drv, clk, dispClient4)
	dispRes4, err := disp4.Dispatch(ctx, adoptedRunID, []store.Event{
		{
			ID:      "ev-owner-explicit-hold",
			RunID:   adoptedRunID,
			Kind:    store.EventOwnerMessage,
			Class:   store.EventClassJudgment,
			Source:  "owner",
			Summary: "Please do not start new lanes until I finish the audit.",
		},
	})
	if err != nil {
		t.Fatalf("Dispatch 4: %v", err)
	}
	if len(dispRes4.Rejected) != 0 {
		t.Fatalf("expected explicit owner no-new-lanes batch event to allow directive.note, got Rejected=%+v", dispRes4.Rejected)
	}
}

func TestConcierge_StatusListsStandingRules(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "rule-max-store-lanes",
		RunID:  "run-1",
		Kind:   store.DirectiveStanding,
		Source: store.DirectiveSourceOwner,
		Scope:  "store/",
		Text:   "Never spawn more than 2 concurrent lanes touching store/.",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Memories().Put(ctx, store.MemoryEntry{
		ID:          "rule-random-ports",
		RunID:       "run-1",
		Scope:       "run",
		Type:        "rule",
		ProposedBy:  "owner",
		Name:        "Random ports",
		Description: "Always use random ports in tests.",
		Body:        "Always use random ports in tests.",
		Source:      "Standing rule: always use random ports in tests.",
	}); err != nil {
		t.Fatal(err)
	}

	client := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_status", concierge.ToolStatus, json.RawMessage(`{}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("On `run-1`, active standing rules are `rule-max-store-lanes` and `rule-random-ports` (say `undo` or `forget` to remove)."),
			),
		},
	)
	conc := concierge.New(concierge.Config{}, st, drv, clk, client)
	res, err := conc.Ask(ctx, "run-1", "/status")
	if err != nil {
		t.Fatalf("Ask(/status): %v", err)
	}
	if !strings.Contains(res.Answer, "rule-max-store-lanes") || !strings.Contains(res.Answer, "rule-random-ports") {
		t.Fatalf("answer missing standing rule IDs: %q", res.Answer)
	}
	statusOutput := client.Requests[1].Messages[2].Content[0].Content
	for _, want := range []string{
		"Standing Rules (2):",
		"rule-max-store-lanes [scope=store/]: Never spawn more than 2 concurrent lanes touching store/.",
		"rule-random-ports [scope=run]: Always use random ports in tests.",
		"forget rule-max-store-lanes",
	} {
		if !strings.Contains(statusOutput, want) {
			t.Fatalf("status output missing %q:\n%s", want, statusOutput)
		}
	}
}

func TestConcierge_ProgressAnswerCarriesFullObjectiveMilestonesAndNextStep(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupSeededStore(t)
	ctx := t.Context()

	const runID = "20261114-trial-m1"
	const fullObjective = `# 20261114-trial-m1

Goal: Complete slim-prompt trial across M1, M2 (items 6-7), and M4 metrics.
Plan: docs/design/slim-prompts-plan.md

Milestones:
- [x] M1: slim-prompt trial mode (2 real code lanes; PR #5604 and PR #5786 merged)
- [ ] M2 (items 6-7): live write-back to state.json/EVENTS and planner handoff on drained backlog
- [ ] M4: lane_metrics day vs baseline comparison and PLAN.md / Nebula post update

Next: when #5786 merges, staff a lane for 6-7 if not covered; then M4 (lane_metrics day vs baseline), update PLAN.md log and the Nebula post`

	if err := st.Runs().Create(ctx, store.Run{
		ID:            runID,
		Repository:    "bazelment/yoloswe",
		Objective:     fullObjective,
		PlanRef:       "ORCHESTRATOR: orca since 2026-11-14T18:18:00Z (orca-chat:front-desk)",
		DefaultRecipe: "adopted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "m2-gaps2",
		RunID:    runID,
		LaneID:   "m2-gaps2",
		Title:    "m2 gaps2",
		Priority: store.PriorityP1,
		Status:   store.BacklogDone,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:            "m2-gaps2",
		RunID:         runID,
		ItemID:        "m2-gaps2",
		Title:         "m2 gaps2",
		RecipeName:    "adopted",
		RecipeVersion: 1,
		Phase:         recipe.PhaseIntegrate,
		Round:         1,
		Status:        lane.StatusDone,
		PR:            5786,
		PRHead:        "fb6c5bae",
		MergeSHA:      "999aaaa",
	}); err != nil {
		t.Fatal(err)
	}

	client := model.NewFakeClient(
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_prog_status", concierge.ToolStatus, json.RawMessage(`{}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock(
					"On `20261114-trial-m1`, the goal is **not** met yet (1 of 3 milestones met, 2 open):\n" +
						"- **Met**: `M1` (slim-prompt trial mode; `m2-gaps2` `PR #5786` merged at `fb6c5bae`).\n" +
						"- **Open**: `M2` (items 6-7: live write-back to `state.json`/`EVENTS` and planner handoff on drained backlog) and `M4` (`lane_metrics` day vs baseline comparison and `PLAN.md` / Nebula post update).\n" +
						"- **Next**: staff a lane for items 6-7 now that `#5786` is merged, then run `M4` (`lane_metrics` day vs baseline) and update `docs/design/slim-prompts-plan.md` / `PLAN.md`.",
				),
			),
		},
	)

	conc := concierge.New(concierge.Config{}, st, drv, clk, client)
	res, err := conc.Ask(ctx, runID, "What's our goal? Have we met the goal, and what is our progress?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Assert the snapshot/status tool result given to the model carries all 3 milestones, the plan path, and Next:.
	if len(client.Requests) != 2 {
		t.Fatalf("expected 2 model requests, got %d", len(client.Requests))
	}
	snapshotGiven := client.Requests[1].Messages[2].Content[0].Content
	for _, wantInSnapshot := range []string{
		"M1: slim-prompt trial mode",
		"M2 (items 6-7): live write-back to state.json/EVENTS and planner handoff on drained backlog",
		"M4: lane_metrics day vs baseline comparison",
		"docs/design/slim-prompts-plan.md",
		"Next: when #5786 merges, staff a lane for 6-7 if not covered; then M4",
	} {
		if !strings.Contains(snapshotGiven, wantInSnapshot) {
			t.Fatalf("status snapshot given to model missing %q:\n%s", wantInSnapshot, snapshotGiven)
		}
	}

	// Assert the concierge answer names the open milestones and the next step.
	for _, wantInAnswer := range []string{
		"not",
		"M1",
		"M2",
		"M4",
		"6-7",
		"#5786",
	} {
		if !strings.Contains(res.Answer, wantInAnswer) {
			t.Fatalf("concierge progress answer missing %q:\n%s", wantInAnswer, res.Answer)
		}
	}
}
