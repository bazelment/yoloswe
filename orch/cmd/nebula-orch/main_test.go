package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
)

func TestInnerLoop_SingleLaneFakeDriverToCompletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "single.db")

	st, err := store.OpenSQLite(dbPath, clock.NewRealClock())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-single",
		Objective: "Single-lane inner loop verification",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:         "item-1",
		RunID:      "run-single",
		Title:      "Implement single lane feature",
		Acceptance: "lint and tests pass",
		Priority:   store.PriorityP0,
		Status:     store.BacklogOpen,
	}); err != nil {
		t.Fatalf("Add backlog item: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close seed store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := Execute(ctx, []string{
		"run",
		"--run", "run-single",
		"--store", "sqlite:" + dbPath,
		"--driver", "fake",
		"--replay", "testdata/single_lane_replay.json",
		"--once",
	}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("Execute run: %v\nstderr:\n%s", err, stderr.String())
	}

	verifySt, err := store.OpenSQLite(dbPath, clock.NewRealClock())
	if err != nil {
		t.Fatalf("OpenSQLite verify: %v", err)
	}
	defer func() { _ = verifySt.Close() }()

	gotRun, err := verifySt.Runs().Get(ctx, "run-single")
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if gotRun.Lease != nil {
		t.Fatalf("expected run lease to be released after graceful shutdown, got %+v", gotRun.Lease)
	}

	gotItem, err := verifySt.Backlog().Get(ctx, "run-single", "item-1")
	if err != nil {
		t.Fatalf("Get item-1: %v", err)
	}
	if gotItem.Status != store.BacklogDone {
		t.Fatalf("item-1 status = %q, want %q", gotItem.Status, store.BacklogDone)
	}

	gotLane, err := verifySt.Lanes().Get(ctx, "run-single", "lane-1")
	if err != nil {
		t.Fatalf("Get lane-1: %v", err)
	}
	if gotLane.Status != lane.StatusDone {
		t.Fatalf("lane-1 status = %q, want %q", gotLane.Status, lane.StatusDone)
	}

	metrics, err := LatestMetricsFromDecisions(ctx, verifySt, clock.NewRealClock(), "run-single")
	if err != nil {
		t.Fatalf("LatestMetricsFromDecisions: %v", err)
	}
	if metrics.AutoDecisions == 0 || metrics.JudgmentDecisions == 0 {
		t.Fatalf("expected both auto and judgment decisions in metrics, got %+v", metrics)
	}
	if metrics.TotalTokens != 1280 || metrics.TotalCostUSD <= 0 {
		t.Fatalf("unexpected token/cost metrics: %+v", metrics)
	}
	if !strings.Contains(stderr.String(), `"msg":"recorded run metrics to decision log"`) {
		t.Fatalf("stderr missing structured metrics log:\n%s", stderr.String())
	}
}

func TestSmoke_ThreeLaneScenarioAndConciergeChat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "three_lane.db")

	customRecipe := recipe.Default()
	customRecipe.Name = "default"
	recipeBytes, err := json.MarshalIndent(customRecipe, "", "  ")
	if err != nil {
		t.Fatalf("Marshal recipe: %v", err)
	}
	recipePath := filepath.Join(tmpDir, "default.json")
	if err := os.WriteFile(recipePath, recipeBytes, 0o644); err != nil {
		t.Fatalf("WriteFile recipe: %v", err)
	}

	scenario := FakeScenario{
		Behaviors: []FakePhaseBehavior{
			{
				LaneID: "lane-2",
				Phase:  recipe.PhaseIntegrate,
				Round:  1,
				Stall:  true,
				Reason: "lane-2 stalled waiting on merge check in integrate round 1",
			},
		},
	}
	scenarioBytes, err := json.MarshalIndent(scenario, "", "  ")
	if err != nil {
		t.Fatalf("Marshal scenario: %v", err)
	}
	scenarioPath := filepath.Join(tmpDir, "scenario.json")
	if err := os.WriteFile(scenarioPath, scenarioBytes, 0o644); err != nil {
		t.Fatalf("WriteFile scenario: %v", err)
	}

	st, err := store.OpenSQLite(dbPath, clock.NewRealClock())
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-3lane",
		Objective: "Wire nebula-orch run and chat across 3 lanes",
		PlanRef:   "post:5aaec193-a1f1-4009-aa55-f6174d583653",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	for _, item := range []store.BacklogItem{
		{
			ID:         "item-1",
			RunID:      "run-3lane",
			Title:      "Wire store and driver flags",
			Acceptance: "flags parsed and validated",
			Priority:   store.PriorityP0,
			Status:     store.BacklogOpen,
			Order:      1,
		},
		{
			ID:         "item-2",
			RunID:      "run-3lane",
			Title:      "Write decision log metrics",
			Acceptance: "metrics snapshot persisted in DecisionStore",
			Priority:   store.PriorityP1,
			Status:     store.BacklogOpen,
			Order:      2,
		},
		{
			ID:         "item-3",
			RunID:      "run-3lane",
			Title:      "Wire concierge chat command",
			Acceptance: "chat answers status question from SQLite store",
			Priority:   store.PriorityP2,
			Status:     store.BacklogOpen,
			DependsOn:  []string{"item-1"},
			Order:      3,
		},
	} {
		if err := st.Backlog().Add(ctx, item); err != nil {
			t.Fatalf("Add backlog %s: %v", item.ID, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close seed store: %v", err)
	}

	var runOut, runErr bytes.Buffer
	if err := Execute(ctx, []string{
		"run",
		"--run", "run-3lane",
		"--store", "sqlite:" + dbPath,
		"--driver", "fake",
		"--fake-scenario", scenarioPath,
		"--replay", "testdata/three_lane_replay.json",
		"--recipe-file", recipePath,
		"--concurrency-cap", "2",
		"--once",
	}, strings.NewReader(""), &runOut, &runErr); err != nil {
		t.Fatalf("Execute run: %v\nstderr:\n%s", err, runErr.String())
	}

	verifySt, err := store.OpenSQLite(dbPath, clock.NewRealClock())
	if err != nil {
		t.Fatalf("OpenSQLite verify: %v", err)
	}
	defer func() { _ = verifySt.Close() }()

	for _, itemID := range []string{"item-1", "item-2", "item-3"} {
		it, err := verifySt.Backlog().Get(ctx, "run-3lane", itemID)
		if err != nil {
			t.Fatalf("Get backlog %s: %v", itemID, err)
		}
		if it.Status != store.BacklogDone {
			t.Fatalf("backlog %s status = %q, want done", itemID, it.Status)
		}
	}
	for _, laneID := range []string{"lane-1", "lane-2", "lane-3"} {
		ln, err := verifySt.Lanes().Get(ctx, "run-3lane", laneID)
		if err != nil {
			t.Fatalf("Get lane %s: %v", laneID, err)
		}
		if ln.Status != lane.StatusDone {
			t.Fatalf("lane %s status = %q, want done", laneID, ln.Status)
		}
		if ln.MergeSHA == "" || ln.ApprovalSHA == "" {
			t.Fatalf("lane %s missing ApprovalSHA=%q or MergeSHA=%q", laneID, ln.ApprovalSHA, ln.MergeSHA)
		}
	}

	lane2, err := verifySt.Lanes().Get(ctx, "run-3lane", "lane-2")
	if err != nil {
		t.Fatalf("Get lane-2: %v", err)
	}
	if lane2.MaxRound(recipe.PhaseIntegrate) != 2 {
		t.Fatalf("lane-2 MaxRound(integrate) = %d, want 2", lane2.MaxRound(recipe.PhaseIntegrate))
	}

	decs, err := verifySt.Decisions().List(ctx, store.DecisionFilter{RunID: "run-3lane"})
	if err != nil {
		t.Fatalf("List decisions: %v", err)
	}
	var metricDec *store.Decision
	for i := range decs {
		if decs[i].Tool == ToolMetricsSnapshot {
			metricDec = &decs[i]
		}
	}
	if metricDec == nil {
		t.Fatalf("expected %s decision in DecisionStore, got %d decisions", ToolMetricsSnapshot, len(decs))
	}
	var recordedMetrics RunMetrics
	if err := json.Unmarshal(metricDec.Args, &recordedMetrics); err != nil {
		t.Fatalf("Unmarshal metrics decision Args: %v", err)
	}
	if recordedMetrics.AutoDecisions == 0 || recordedMetrics.JudgmentDecisions != 4 {
		t.Fatalf("recordedMetrics auto=%d judgment=%d, want >0 auto and 4 judgment",
			recordedMetrics.AutoDecisions, recordedMetrics.JudgmentDecisions)
	}
	if len(recordedMetrics.Days) == 0 || recordedMetrics.TokensPerDecision <= 0 || recordedMetrics.CostPerDecisionUSD <= 0 {
		t.Fatalf("expected populated per-decision and per-day token/cost metrics, got %+v", recordedMetrics)
	}

	var chatOut, chatErr bytes.Buffer
	if err := Execute(ctx, []string{
		"chat",
		"--run", "run-3lane",
		"--store", "sqlite:" + dbPath,
		"--replay", "testdata/concierge_status_replay.json",
	}, strings.NewReader("Where are we on run-3lane?\n"), &chatOut, &chatErr); err != nil {
		t.Fatalf("Execute chat: %v\nstderr:\n%s", err, chatErr.String())
	}

	answer := strings.TrimSpace(chatOut.String())
	for _, wantSubstr := range []string{"run-3lane", "3/3", "lane-1", "lane-2", "lane-3", "done"} {
		if !strings.Contains(answer, wantSubstr) {
			t.Fatalf("chat answer %q missing expected substring %q", answer, wantSubstr)
		}
	}

	threads, err := verifySt.Threads().ListThreads(ctx, store.ThreadFilter{
		RunID: "run-3lane",
		Role:  store.ThreadRoleConcierge,
	})
	if err != nil {
		t.Fatalf("ListThreads concierge: %v", err)
	}
	if len(threads) != 1 || threads[0].Status != store.ThreadStatusArchived {
		t.Fatalf("concierge threads = %+v, want 1 archived thread", threads)
	}
}

func TestConfig_RoleModelsAndRecipeFiles(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tmpDir := t.TempDir()

	cfgFile := filepath.Join(tmpDir, "orch-config.json")
	rawCfg := `{
		"run_id": "run-cfg",
		"llm_gateway_url": "https://gateway.example.internal/v1",
		"concurrency_cap": 6,
		"models": {
			"dispatcher": "claude-sonnet-custom",
			"dispatcher_hard": "claude-opus-custom",
			"concierge": "claude-haiku-custom",
			"planner": "claude-opus-planner"
		}
	}`
	if err := os.WriteFile(cfgFile, []byte(rawCfg), 0o644); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}

	nlRecipeFile := filepath.Join(tmpDir, "fast-docs.txt")
	if err := os.WriteFile(nlRecipeFile, []byte("skip clean and skip review; lanes merge themselves"), 0o644); err != nil {
		t.Fatalf("WriteFile nl recipe: %v", err)
	}

	var errBuf bytes.Buffer
	cfg, _, err := parseFlags("run", []string{
		"--config", cfgFile,
		"--recipe-file", nlRecipeFile,
		"--store", "memory",
		"--driver", "fake",
	}, &errBuf)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.RunID != "run-cfg" || cfg.LLMGatewayURL != "https://gateway.example.internal/v1" || cfg.ConcurrencyCap != 6 {
		t.Fatalf("unexpected parsed Config: %+v", cfg)
	}
	if cfg.Models.Dispatcher != "claude-sonnet-custom" ||
		cfg.Models.DispatcherHard != "claude-opus-custom" ||
		cfg.Models.Concierge != "claude-haiku-custom" ||
		cfg.Models.Planner != "claude-opus-planner" {
		t.Fatalf("unexpected RoleModels: %+v", cfg.Models)
	}

	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()

	drv := NewScriptedFakeDriver(lane.NewFakeDriver(), st.Events(), clk, true)
	runner := NewRunner(cfg, st, drv, clk, nil, nil)
	if err := runner.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	gotRec, err := st.Recipes().Get(ctx, "run-cfg", "fast-docs", 0)
	if err != nil {
		t.Fatalf("Get compiled recipe fast-docs: %v", err)
	}
	if gotRec.Limits.MaxConcurrentLanes != 6 {
		t.Fatalf("MaxConcurrentLanes = %d, want 6", gotRec.Limits.MaxConcurrentLanes)
	}
	if gotRec.MergePolicy.Mode != recipe.MergeModeSelf {
		t.Fatalf("MergePolicy.Mode = %q, want self", gotRec.MergePolicy.Mode)
	}

	_, _, storeErr := openStore(Config{RunID: "run-cfg", StoreSpec: "nebula"}, clk)
	if !errors.Is(storeErr, ErrNebulaStoreUnsupported) {
		t.Fatalf("openStore(nebula) error = %v, want %v", storeErr, ErrNebulaStoreUnsupported)
	}
}

func TestPlannerDirectivesAndConciergeSubmit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()

	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-plan",
		Objective: "Initial objective",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}

	conciergeClient := model.NewFakeClient(
		model.Response{
			ID:         "msg-c-1",
			Model:      DefaultConciergeModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("call-dir-1", ToolDirectiveSubmit, json.RawMessage(`{
					"kind": "recipe_edit",
					"ref": "default",
					"text": "skip clean"
				}`)),
				model.NewToolUseBlock("call-dir-2", ToolDirectiveSubmit, json.RawMessage(`{
					"kind": "plan_import",
					"ref": "post:new-plan",
					"text": "{\"objective\":\"Updated objective from plan\",\"add_items\":[{\"id\":\"item-plan-1\",\"title\":\"Imported item\",\"priority\":\"p0\"}],\"conflicts\":[{\"id\":\"conf-1\",\"question\":\"Keep legacy endpoint?\",\"options\":[\"drop\",\"keep\"],\"default\":\"drop\"}]}"
				}`)),
			),
			Usage: model.Usage{InputTokens: 400, OutputTokens: 90, CostUSD: 0.00068},
		},
		model.Response{
			ID:         "msg-c-2",
			Model:      DefaultConciergeModel,
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Submitted recipe_edit and plan_import directives.")),
			Usage:      model.Usage{InputTokens: 500, OutputTokens: 25, CostUSD: 0.0005},
		},
	)

	concierge := NewConcierge("run-plan", st, clk, conciergeClient, DefaultConciergeModel)
	reply, err := concierge.Ask(ctx, "Skip clean from now on and import post:new-plan")
	if err != nil {
		t.Fatalf("concierge.Ask: %v", err)
	}
	if !strings.Contains(reply, "Submitted") {
		t.Fatalf("unexpected reply: %q", reply)
	}

	planner := NewPlanner(st, clk, nil, DefaultPlannerModel, nil)
	if err := planner.ReconcileDirectives(ctx, "run-plan", 4); err != nil {
		t.Fatalf("ReconcileDirectives: %v", err)
	}

	latestDefault, err := st.Recipes().Get(ctx, "run-plan", recipe.DefaultName, 0)
	if err != nil {
		t.Fatalf("Get latest default recipe: %v", err)
	}
	if _, hasClean := latestDefault.Phase(recipe.PhaseClean); hasClean {
		t.Fatalf("expected clean phase to be removed in compiled recipe edit, got phases %v", latestDefault.PhaseNames())
	}

	gotRun, err := st.Runs().Get(ctx, "run-plan")
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if gotRun.Objective != "Updated objective from plan" || gotRun.PlanRef != "post:new-plan" {
		t.Fatalf("run after plan import = %+v", gotRun)
	}
	if _, err := st.Backlog().Get(ctx, "run-plan", "item-plan-1"); err != nil {
		t.Fatalf("expected imported backlog item-plan-1: %v", err)
	}
	if _, err := st.Directives().Get(ctx, "run-plan", "ask-conf-1"); err != nil {
		t.Fatalf("expected conflict owner ask directive ask-conf-1: %v", err)
	}
}

func TestGracefulShutdown_ReleasesLeaseAndWritesMetrics(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()

	drv := NewScriptedFakeDriver(lane.NewFakeDriver(), st.Events(), clk, true)
	cfg := Config{
		RunID:        "run-graceful",
		HolderID:     "holder-graceful",
		StoreSpec:    "memory",
		DriverKind:   "fake",
		PollInterval: 50 * time.Millisecond,
	}.withDefaults()

	runner := NewRunner(cfg, st, drv, clk, nil, nil)

	errCh := make(chan error, 1)
	go func() {
		errCh <- runner.Run(ctx)
	}()

	// Wait until the controller acquires the lease and blocks on PollInterval timer.
	deadline := time.Now().Add(5 * time.Second)
	for clk.Waiters() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Runner to acquire lease and enter poll wait")
		}
		time.Sleep(time.Millisecond)
	}

	runWhileActive, err := st.Runs().Get(t.Context(), "run-graceful")
	if err != nil {
		t.Fatalf("Get run while active: %v", err)
	}
	if runWhileActive.Lease == nil || runWhileActive.Lease.HolderID != "holder-graceful" {
		t.Fatalf("expected active lease held by holder-graceful, got %+v", runWhileActive.Lease)
	}

	cancel()
	if runErr := <-errCh; runErr != nil {
		t.Fatalf("runner.Run returned error on cancel: %v", runErr)
	}

	runAfterCancel, err := st.Runs().Get(t.Context(), "run-graceful")
	if err != nil {
		t.Fatalf("Get run after cancel: %v", err)
	}
	if runAfterCancel.Lease != nil {
		t.Fatalf("expected lease to be nil after graceful shutdown, got %+v", runAfterCancel.Lease)
	}

	decs, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: "run-graceful"})
	if err != nil {
		t.Fatalf("List decisions: %v", err)
	}
	if len(decs) == 0 || decs[len(decs)-1].Tool != ToolMetricsSnapshot {
		t.Fatalf("expected final decision to be %s, got %+v", ToolMetricsSnapshot, decs)
	}
}
