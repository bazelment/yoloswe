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
	if !strings.Contains(raw.Spawns[0].Brief, "Worker tests use random ports.") || !strings.Contains(raw.Spawns[0].Brief, "Use random ports") {
		t.Fatalf("worker brief missing correction/source: %s", raw.Spawns[0].Brief)
	}
	if _, err := c.Ask(ctx, "run-1", "forget"); err != nil {
		t.Fatalf("forget correction: %v", err)
	}
	if _, err := st.Memories().Get(ctx, "", "memory-random-ports"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("forgotten memory still exists, err=%v", err)
	}
}

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
	if len(drv.Spawns) != 1 || !strings.Contains(drv.Spawns[0].Brief, m.Body) || !strings.Contains(drv.Spawns[0].Brief, quote) {
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
