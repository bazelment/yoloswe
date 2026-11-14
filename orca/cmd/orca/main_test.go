package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxfake "github.com/bazelment/yoloswe/orca/mailbox/fake"
	"github.com/bazelment/yoloswe/orca/model"
	planreader "github.com/bazelment/yoloswe/orca/planner"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
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

func TestRepoInference_GitHubOrigins(t *testing.T) {
	for _, tc := range []struct{ name, origin, want string }{
		{"https", "https://github.com/a/b.git", "a/b"},
		{"ssh", "git@github.com:a/b.git", "a/b"},
		{"ssh-url", "ssh://git@github.com/a/b", "a/b"},
		{"missing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repositoryFromOrigin(tc.origin); got != tc.want {
				t.Fatalf("repositoryFromOrigin(%q) = %q, want %q", tc.origin, got, tc.want)
			}
		})
	}
	if cfg, _, err := parseFlags("run", []string{"--goal", "report", "--repo", "override/repo"}, io.Discard); err != nil {
		t.Fatalf("parseFlags: %v", err)
	} else if cfg.Repository != "override/repo" {
		t.Fatalf("--repo override = %q", cfg.Repository)
	}
}

func TestRepoInference_NoOriginAndNoRepoIsClearError(t *testing.T) {
	cfg := Config{RunID: "run-no-repo", Goal: "report", DriverKind: "nebula", StoreSpec: "memory"}.withDefaults()
	if err := cfg.Validate(true); err != nil {
		t.Fatalf("config validation unexpectedly failed before repo check: %v", err)
	}
	var stderr bytes.Buffer
	err := executeRun(t.Context(), cfg, newLogger(&stderr, "error", "text"))
	if err == nil || !strings.Contains(err.Error(), "--repo is required") || !strings.Contains(err.Error(), "could not infer origin") {
		t.Fatalf("executeRun error = %v, want clear missing-origin error", err)
	}
}

func TestChatNewRunIDCreatesRunRecord(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "chat.db")
	var stdout, stderr bytes.Buffer
	err := Execute(t.Context(), []string{
		"chat", "--run", "new-chat-run", "--store", "sqlite:" + dbPath,
		"--replay", "testdata/concierge_status_replay.json",
	}, strings.NewReader("/status\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Execute chat: %v\nstderr:\n%s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "orca/store: not found") {
		t.Fatalf("new run chat reported missing store record:\n%s", stdout.String())
	}
	st, err := store.OpenSQLite(dbPath, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.Runs().Get(t.Context(), "new-chat-run")
	if err != nil || run.Objective != "Owner chat for run new-chat-run" {
		t.Fatalf("created run = %+v, err=%v", run, err)
	}
}

func TestConciergeRunSettingsChatPersistsOwnerWordsAndStatus(t *testing.T) {
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()
	if err := st.Runs().Create(ctx, store.Run{ID: "chat-settings", Objective: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Runs().UpdateSettings(ctx, "chat-settings", store.RunSettings{ConcurrencyCap: 4, StallTimeout: 45 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	client := model.NewFakeClient(
		model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("settings", ToolRunSettings, json.RawMessage(`{"action":"set","concurrency_cap":2,"words":"run at most 2 lanes at a time"}`)))},
		model.Response{Message: model.NewAssistantMessage(model.NewTextBlock("updated"))},
	)
	c := NewConcierge("chat-settings", st, clk, client, DefaultConciergeModel)
	if _, err := c.Ask(ctx, "run at most 2 lanes at a time"); err != nil {
		t.Fatal(err)
	}
	got, err := st.Runs().GetSettings(ctx, "chat-settings")
	if err != nil || got.ConcurrencyCap != 2 {
		t.Fatalf("settings=%+v err=%v", got, err)
	}
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "chat-settings"})
	if err != nil || len(decs) != 1 || decs[0].Reason != "run at most 2 lanes at a time" {
		t.Fatalf("decisions=%+v err=%v", decs, err)
	}
	status, err := c.FormatStatusReport(ctx)
	if err != nil || !strings.Contains(status, "Settings: concurrency_cap=2") {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestConciergeRunSettingsRejectsInvalidValues(t *testing.T) {
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()
	if err := st.Runs().Create(ctx, store.Run{ID: "chat-invalid"}); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]string{
		"cap":     `{"action":"set","concurrency_cap":17}`,
		"timeout": `{"action":"set","stall_timeout":"1m"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := NewConcierge("chat-invalid", st, clk, nil, DefaultConciergeModel)
			result := c.buildToolRegistry("thread", "turn", "", nil).Execute(ctx, model.ToolCall{ID: "bad", Name: ToolRunSettings, Input: json.RawMessage(args)})
			if !result.IsError || !strings.Contains(result.Content, "run.settings") {
				t.Fatalf("tool result=%+v, want run.settings validation message", result)
			}
		})
	}
}

func TestGoalRunDefaults_OnceStandingAndExplicitOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		once bool
	}{
		{"goal", []string{"--goal", "report", "--driver", "fake"}, true},
		{"standing", []string{"--goal", "report", "--standing", "--driver", "fake", "--max-steps", "1"}, false},
		{"explicit once", []string{"--goal", "report", "--once", "--driver", "fake"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := parseFlags("run", tc.args, io.Discard)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if cfg.Once != tc.once {
				t.Fatalf("Once = %t, want %t (standing=%t)", cfg.Once, tc.once, cfg.Standing)
			}
		})
	}
}

func TestExecuteNebulaEnsuresRunBeforeRunnerStartup(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "startup.db")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/code-runs/run-startup" {
			calls.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":"FORBIDDEN","message":"denied"}`)
			return
		}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("NEBULA_API_URL", srv.URL)
	t.Setenv("NEBULA_TOKEN", "test-token")

	var stderr bytes.Buffer
	err := Execute(t.Context(), []string{
		"run", "--run", "run-startup", "--store", "sqlite:" + dbPath,
		"--driver", "nebula",
	}, strings.NewReader(""), io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unable to create Nebula run") {
		t.Fatalf("Execute error = %v, want startup EnsureRun failure", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("run-create requests = %d, want exactly one before runner startup", calls.Load())
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
		Objective: "Wire orca run and chat across 3 lanes",
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

	var promptOut, promptErr bytes.Buffer
	if err := Execute(ctx, []string{
		"chat", "--run", "run-3lane", "--store", "sqlite:" + dbPath,
		"--replay", "testdata/concierge_status_replay.json", "--prompt", "where are we?",
	}, strings.NewReader(""), &promptOut, &promptErr); err != nil {
		t.Fatalf("Execute chat --prompt: %v\nstderr:\n%s", err, promptErr.String())
	}
	if !strings.Contains(promptOut.String(), "→ concierge: where are we?") || !strings.Contains(promptOut.String(), "Run run-3lane is complete") {
		t.Fatalf("prompt output = %q", promptOut.String())
	}
	decs, err = verifySt.Decisions().List(ctx, store.DecisionFilter{RunID: "run-3lane"})
	if err != nil {
		t.Fatal(err)
	}
	foundRouting := false
	for _, d := range decs {
		if d.Actor == "router" && d.Tool == "routing" && d.Input == "where are we?" {
			foundRouting = true
		}
	}
	if !foundRouting {
		t.Fatalf("--prompt did not record routing decision: %+v", decs)
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
	runner := NewRunner(cfg, st, drv, clk, model.NewFakeClient(compiledRecipeResponse(t, "skip clean and skip review; lanes merge themselves")), nil)
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

	planner := NewPlanner(st, clk, model.NewFakeClient(compiledRecipeResponse(t, "skip clean"), model.Response{Message: model.NewAssistantMessage(model.NewToolUseBlock("plan", SubmitPlanReconcileTool, json.RawMessage(`{"objective":"Updated objective from plan","add_items":[{"id":"item-plan-1","title":"Imported item","priority":"p0"}],"conflicts":[{"id":"conf-1","question":"Keep legacy endpoint?","options":["drop","keep"],"default":"drop"}]}`)))}), DefaultPlannerModel, nil)
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

func TestRestart_UniqueIDsAcrossInstances(t *testing.T) {
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()

	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-restart",
		Objective: "Verify unique IDs across restarted instances",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}

	makeSubmitClient := func(text string) model.ModelClient {
		return model.NewFakeClient(
			model.Response{
				ID:         "msg-1",
				Model:      DefaultConciergeModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("call-1", ToolDirectiveSubmit, json.RawMessage(`{"kind":"instruction","text":"`+text+`"}`)),
				),
			},
			model.Response{
				ID:         "msg-2",
				Model:      DefaultConciergeModel,
				StopReason: model.StopReasonEndTurn,
				Message:    model.NewAssistantMessage(model.NewTextBlock("ok")),
			},
		)
	}

	globalIDSeq.Store(0)
	processNonce = "proc1aaa"
	c1 := NewConcierge("run-restart", st, clk, makeSubmitClient("first instruction"), DefaultConciergeModel)
	if _, err := c1.Ask(ctx, "submit first"); err != nil {
		t.Fatalf("c1.Ask: %v", err)
	}
	p1 := NewPlanner(st, clk, model.NewFakeClient(compiledRecipeResponse(t, "skip clean")), DefaultPlannerModel, nil)
	if _, err := p1.CompileAndSaveRecipe(ctx, "run-restart", "default", "skip clean", nil, 2); err != nil {
		t.Fatalf("p1.CompileAndSaveRecipe: %v", err)
	}

	// Simulate a process restart: reset sequence counter to 0 and rotate processNonce at the same clock timestamp.
	globalIDSeq.Store(0)
	processNonce = "proc2bbb"
	c2 := NewConcierge("run-restart", st, clk, makeSubmitClient("second instruction"), DefaultConciergeModel)
	if _, err := c2.Ask(ctx, "submit second"); err != nil {
		t.Fatalf("c2.Ask: %v", err)
	}
	p2 := NewPlanner(st, clk, model.NewFakeClient(compiledRecipeResponse(t, "lanes merge themselves")), DefaultPlannerModel, nil)
	if _, err := p2.CompileAndSaveRecipe(ctx, "run-restart", "default", "lanes merge themselves", nil, 2); err != nil {
		t.Fatalf("p2.CompileAndSaveRecipe: %v", err)
	}

	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: "run-restart"})
	if err != nil {
		t.Fatalf("List directives: %v", err)
	}
	if len(dirs) != 2 || dirs[0].ID == dirs[1].ID {
		t.Fatalf("expected 2 distinct directives across Concierge instances, got %+v", dirs)
	}

	metrics, err := ComputeMetrics(ctx, st, clk, "run-restart")
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if metrics.OwnerNudges != 2 {
		t.Fatalf("OwnerNudges = %d, want 2 (no double count with EventOwnerMessage)", metrics.OwnerNudges)
	}
}

func TestStandbyRunner_DoesNotMutateWithoutLeaseAndRevokesBadDirective(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 19, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()

	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-lease",
		Objective: "Verify lease gating before store mutations",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	if _, err := st.Runs().AcquireLease(ctx, "run-lease", "primary-holder", 30*time.Second); err != nil {
		t.Fatalf("AcquireLease primary: %v", err)
	}
	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "dir-edit-1",
		RunID:  "run-lease",
		Kind:   store.DirectiveRecipeEdit,
		Source: store.DirectiveSourceOwner,
		Ref:    "default",
		Text:   "skip clean",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatalf("Add directive: %v", err)
	}

	drv := NewScriptedFakeDriver(lane.NewFakeDriver(), st.Events(), clk, true)
	standbyCfg := Config{
		RunID:      "run-lease",
		Objective:  "standby-should-not-overwrite",
		HolderID:   "standby-holder",
		StoreSpec:  "memory",
		DriverKind: "fake",
		Once:       true,
		MaxSteps:   1,
	}.withDefaults()
	standby := NewRunner(standbyCfg, st, drv, clk, nil, nil)

	if err := standby.Step(ctx); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("standby.Step error = %v, want %v", err, store.ErrLeaseHeld)
	}
	if err := standby.Run(ctx); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("standby.Run error = %v, want %v", err, store.ErrLeaseHeld)
	}

	standbyPollCfg := standbyCfg
	standbyPollCfg.Once = false
	standbyPoll := NewRunner(standbyPollCfg, st, drv, clk, nil, nil)
	if err := standbyPoll.Run(ctx); err != nil {
		t.Fatalf("non-Once standbyPoll.Run should wait on lease rather than fail in Bootstrap, got %v", err)
	}

	runAfterStandby, err := st.Runs().Get(ctx, "run-lease")
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if runAfterStandby.Objective != "Verify lease gating before store mutations" {
		t.Fatalf("expected run objective to remain %q when standby lacks lease, got %q", "Verify lease gating before store mutations", runAfterStandby.Objective)
	}

	dirAfterStandby, err := st.Directives().Get(ctx, "run-lease", "dir-edit-1")
	if err != nil {
		t.Fatalf("Get directive: %v", err)
	}
	if dirAfterStandby.Status != store.DirectiveActive {
		t.Fatalf("expected directive to remain active when standby lacks lease, got %q", dirAfterStandby.Status)
	}
	decsAfterStandby, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-lease"})
	if err != nil {
		t.Fatalf("List decisions: %v", err)
	}
	if len(decsAfterStandby) != 0 {
		t.Fatalf("expected standby runner to record 0 decisions on shutdown, got %+v", decsAfterStandby)
	}

	// Add an invalid empty plan_import directive and verify primary runner revokes it without failing Step.
	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "dir-bad-import",
		RunID:  "run-lease",
		Kind:   store.DirectivePlanImport,
		Source: store.DirectiveSourceConcierge,
		Ref:    "/etc/passwd",
		Text:   "",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatalf("Add bad directive: %v", err)
	}

	primaryCfg := Config{
		RunID:      "run-lease",
		HolderID:   "primary-holder",
		StoreSpec:  "memory",
		DriverKind: "fake",
	}.withDefaults()
	primary := NewRunner(primaryCfg, st, drv, clk, nil, nil)
	if err := primary.Bootstrap(ctx); err != nil {
		t.Fatalf("primary.Bootstrap: %v", err)
	}
	if err := primary.Step(ctx); err != nil {
		t.Fatalf("primary.Step: %v", err)
	}

	badDir, err := st.Directives().Get(ctx, "run-lease", "dir-bad-import")
	if err != nil {
		t.Fatalf("Get bad directive: %v", err)
	}
	if badDir.Status != store.DirectiveRevoked {
		t.Fatalf("bad directive status = %q, want %q", badDir.Status, store.DirectiveRevoked)
	}
}

func TestConfig_BoolMergePrecedenceAndNegativeCapValidation(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "cfg.json")
	if err := os.WriteFile(cfgFile, []byte(`{"run_id":"run-bool","once":true,"fake_auto_complete":false}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var errBuf bytes.Buffer
	fileOnlyCfg, _, err := parseFlags("run", []string{"--config", cfgFile}, &errBuf)
	if err != nil {
		t.Fatalf("parseFlags fileOnly: %v", err)
	}
	if !fileOnlyCfg.Once {
		t.Fatalf("expected config file once=true to take effect when --once flag is omitted")
	}
	if fileOnlyCfg.FakeAutoComplete {
		t.Fatalf("expected config file fake_auto_complete=false to override default true when --fake-auto flag is omitted")
	}

	if err := os.WriteFile(cfgFile, []byte(`{"run_id":"run-bool","once":true,"fake_auto_complete":true}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, _, err := parseFlags("run", []string{
		"--config", cfgFile,
		"--once=false",
		"--fake-auto=false",
		"--concurrency-cap", "-2",
	}, &errBuf)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.Once {
		t.Fatalf("expected explicit --once=false to override config file once=true")
	}
	if cfg.FakeAutoComplete {
		t.Fatalf("expected explicit --fake-auto=false to override config file fake_auto_complete=true")
	}
	if err := cfg.Validate(true); err == nil {
		t.Fatalf("expected Validate to reject negative concurrency cap %d", cfg.ConcurrencyCap)
	}
}

type watchedDriver struct {
	*lane.FakeDriver
	watches int
}

func (d *watchedDriver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	d.watches++
	return d.FakeDriver.Watch(ctx, runID)
}

func TestRunnerStartsLiveWatches(t *testing.T) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := &watchedDriver{FakeDriver: lane.NewFakeDriver()}
	box := mailboxfake.New(st.Events(), "watch-run")
	r := NewRunner(Config{RunID: "watch-run"}, st, drv, clk, nil, nil, box)
	if err := r.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	rec := recipe.Default()
	rec.RunID = "watch-run"
	rec.Name = "watch-three"
	rec.Phases = []recipe.Phase{rec.Phases[0], rec.Phases[2], rec.Phases[3]}
	rec.Transitions = []recipe.Transition{
		{From: "swe", On: recipe.Condition{Verdict: "done"}, To: "review"},
		{From: "review", On: recipe.Condition{Verdict: "done"}, To: "integrate"},
		{From: "integrate", On: recipe.Condition{Verdict: "done"}, To: recipe.TargetComplete},
	}
	if _, err := st.Recipes().Save(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{ID: "worker", RunID: "watch-run", Status: lane.StatusPlanned, RecipeName: rec.Name}); err != nil {
		t.Fatal(err)
	}
	if err := r.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"swe", "review", "integrate"} {
		before, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "watch-run"})
		drv.Emit(lane.Event{RunID: "watch-run", LaneID: "worker", Kind: lane.EventPhaseReported, Report: &lane.PhaseReport{Phase: phase, Outcome: lane.OutcomeDone, HeadSHA: "head", Summary: "phase rationale"}})
		runnerWaitFor(t, func() bool {
			events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "watch-run"})
			return len(events) > len(before)
		})
		if err := r.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	ln, err := st.Lanes().Get(t.Context(), "watch-run", "worker")
	if err != nil || ln.Status != lane.StatusDone {
		t.Fatalf("Watch progression: %+v %v", ln, err)
	}
	before, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "watch-run"})
	box.Inject(mailbox.Message{LaneID: "worker", Body: "mailbox inbound"})
	runnerWaitFor(t, func() bool {
		events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "watch-run"})
		return len(events) > len(before)
	})
	if err := r.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	cur, err := st.Events().GetCursor(t.Context(), "watch-run", "controller")
	events, _ := st.Events().List(t.Context(), store.EventFilter{RunID: "watch-run"})
	if err != nil || cur.Seq != events[len(events)-1].Seq {
		t.Fatalf("mailbox not consumed: %+v %v", cur, err)
	}

	if drv.watches != 1 {
		t.Fatalf("Watch calls = %d, want 1", drv.watches)
	}
}

func runnerWaitFor(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !condition() {
		select {
		case <-timer.C:
			t.Fatal("condition timed out")
		default:
			runtime.Gosched()
		}
	}
}

func TestDriverWatchDoesNotReplayForwardedEvents(t *testing.T) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	for _, source := range []string{"nebula:terminal:worker", "lane.watch"} {
		if _, err := st.Events().Append(t.Context(), store.Event{RunID: "run", Kind: store.EventPhaseExit, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := (driverEventStore{st.Events()}).List(t.Context(), store.EventFilter{RunID: "run"})
	if err != nil || len(events) != 1 || events[0].Source != "nebula:terminal:worker" {
		t.Fatalf("driver sees controller feedback: %+v %v", events, err)
	}
}

func TestRunnerWorkerQuestionUsesRouterModel(t *testing.T) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()
	box := mailboxfake.New(st.Events(), "question-run")
	client := model.NewFakeClient(model.Response{Message: model.Message{Role: model.RoleAssistant, Content: []model.ContentBlock{model.NewTextBlock("Use the repository merge queue.")}}})
	r := NewRunner(Config{RunID: "question-run", Models: RoleModels{Roles: map[string]RoleSelection{"router": {Provider: "openai", Model: "question-model"}}}}, st, drv, clk, client, nil, box)
	if err := r.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{ID: "worker", RunID: "question-run", Status: lane.StatusPlanned}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Notes().Append(t.Context(), store.RunNote{RunID: "question-run", TaskID: "worker", Text: "Use the repository merge queue."}); err != nil {
		t.Fatal(err)
	}
	if err := r.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.AppendEvent(t.Context(), st.Events(), "question-run", mailbox.Message{LaneID: "worker", Question: true, Body: "Which merge path?"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(box.Sent()) != 0 || len(drv.Steers) != 1 || drv.Steers[0].Message != "Use the task text and notes:\nUse the repository merge queue." {
		t.Fatalf("owner=%+v steers=%+v", box.Sent(), drv.Steers)
	}
	if len(client.Requests) != 1 || client.Requests[0].Model != "question-model" || !strings.Contains(client.Requests[0].Messages[0].Text(), "repository merge queue") {
		t.Fatalf("router requests: %+v", client.Requests)
	}
}

func TestRoleResolution(t *testing.T) {
	t.Parallel()
	cfg, _, err := parseFlags("run", []string{"--model-provider", "openai", "--store", "memory", "--goal", "resolve roles", "--planner-provider", "anthropic", "--planner-model", "claude-opus-4-6", "--critic-provider", "openai", "--critic-model", "gpt-6.1-sol", "--router-model", "router-test"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(true); err != nil {
		t.Fatal(err)
	}
	if cfg.RunID == "" || cfg.Models.Dispatcher != "gpt-6.1-sol" || cfg.Models.Roles["planner"].Effort != "high" || cfg.Models.Roles["critic"].Provider != "openai" {
		t.Fatalf("resolved config: %+v", cfg)
	}
	fake := model.NewFakeClient(model.Response{Message: model.NewAssistantMessage(model.NewTextBlock("ok"))})
	_, err = roleClient(fake, cfg, "planner").CreateMessage(t.Context(), model.Request{Model: "ignored", MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := fake.Requests[0]
	if got.Model != "claude-opus-4-6" || got.Metadata["provider"] != "anthropic" || got.Metadata["role"] != "planner" || got.ReasoningEffort != "high" {
		t.Fatalf("request: %+v", got)
	}
	defaults := Config{ModelProvider: "openai"}.withDefaults()
	if defaults.Models.Roles["planner"].Provider == defaults.Models.Roles["critic"].Provider || defaults.Models.Dispatcher != "gpt-6.1-sol" {
		t.Fatalf("defaults: %+v", defaults.Models)
	}
}

func TestDefaultRoleModelsUseCurrentProviderIDs(t *testing.T) {
	for provider, want := range map[string]map[string]string{
		"anthropic": {
			"planner": "claude-opus-4-6", "critic": "gpt-6.1-sol",
			"dispatcher": "claude-sonnet-5-5", "concierge": "claude-haiku-5-5", "router": "claude-haiku-5-5",
		},
		"openai": {
			"planner": "gpt-6-astra", "critic": "claude-sonnet-5-5",
			"dispatcher": "gpt-6.1-sol", "concierge": "gpt-6.1-sol", "router": "gpt-6.1-sol",
		},
	} {
		t.Run(provider, func(t *testing.T) {
			cfg := Config{ModelProvider: provider}.withDefaults()
			for role, want := range want {
				if got := cfg.Models.Roles[role].Model; got != want {
					t.Errorf("%s model = %q, want %q", role, got, want)
				}
			}
		})
	}
}

func TestRoleConfigPrecedenceAndValidation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "roles.json")
	if err := os.WriteFile(path, []byte(`{"run_id":"roles","models":{"roles":{"planner":{"provider":"anthropic","model":"old","effort":"medium"},"critic":{"provider":"openai"},"router":{"provider":"anthropic","model":"cheap"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := parseFlags("run", []string{"--store", "memory", "--config", path, "--planner-model", "new", "--planner-effort", "high"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(true); err != nil {
		t.Fatal(err)
	}
	if cfg.Models.Roles["planner"] != (RoleSelection{Provider: "anthropic", Model: "new", Effort: "high"}) || cfg.Models.Roles["router"].Model != "cheap" {
		t.Fatalf("roles: %+v", cfg.Models.Roles)
	}
	cfg.Models.Roles["critic"] = RoleSelection{Provider: "anthropic", Model: "same"}
	if err := cfg.Validate(true); err == nil || !strings.Contains(err.Error(), "different providers") {
		t.Fatalf("same-provider critic: %v", err)
	}
}

func TestRecipeCompileFailureIsHardError(t *testing.T) {
	t.Parallel()
	clk := clock.NewRealClock()
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "bad-recipe"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bad.txt")
	if err := os.WriteFile(path, []byte("skip clean"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := model.NewFakeClient(model.Response{Message: model.NewAssistantMessage(model.NewTextBlock("cannot compile this"))})
	planner := NewPlanner(st, clk, fake, "test", nil)
	if _, err := LoadRecipeFile(t.Context(), path, "bad-recipe", 4, st, planner); err == nil {
		t.Fatal("expected compiler error")
	}
	if _, err := st.Recipes().Get(t.Context(), "bad-recipe", "bad", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recipe saved despite compile failure: %v", err)
	}
	fake = model.NewFakeClient(model.Response{Message: model.NewAssistantMessage(model.NewTextBlock("cannot compile this"))})
	planner = NewPlanner(st, clk, fake, "test", nil)
	if _, err := planner.CompileOverride(t.Context(), recipe.Default(), "skip clean"); err == nil {
		t.Fatal("expected override compiler error")
	}
	if _, err := LoadRecipeFile(t.Context(), path, "bad-recipe", 4, st, nil); err == nil {
		t.Fatal("expected missing compiler error")
	}
}

func compiledRecipeResponse(t *testing.T, delta string) model.Response {
	t.Helper()
	r := dispatcher.CompileRecipeOverride(recipe.Default(), delta)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return model.Response{Message: model.NewAssistantMessage(model.NewToolUseBlock("compile", recipe.SubmitRecipeToolName, raw))}
}

func TestCLIRepositoryReachesDriver(t *testing.T) {
	t.Parallel()
	var selected atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/code-repositories":
			_, _ = io.WriteString(w, `{"repositories":[{"id":"wrong","fullName":"other/repo"},{"id":"chosen","fullName":"owner/project"}]}`)
		case r.URL.Path == "/api/code-sessions" && r.Method == http.MethodPost:
			var body struct {
				RepositoryID string `json:"repositoryId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			selected.Store(body.RepositoryID)
			_, _ = io.WriteString(w, `{"session":{"id":"new-session","status":"ready"}}`)
		case r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[{"id":"wrong-reuse","repositoryId":"wrong","status":"ready","metadata":{"swarmRun":"repo-test","swarmLane":"test"}}]}`)
		default:
			_, _ = io.WriteString(w, `{"session":{"id":"current","status":"ready","repositoryId":"wrong"}}`)
		}
	}))
	defer server.Close()
	cfg, _, err := parseFlags("run", []string{"--run", "repo-test", "--repo", "owner/project"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NebulaAPIURL = server.URL
	cfg.NebulaToken = "test-token"
	st := store.NewMemoryStore(clock.NewRealClock())
	defer st.Close()
	drv, err := openNebulaDriver(cfg, st, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.EnsureSession(t.Context(), lane.SpawnRequest{RunID: cfg.RunID, LaneID: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := selected.Load(); got != "chosen" {
		t.Fatalf("repositoryId = %v", got)
	}
	cfg.Repository = "absent/repo"
	drv, err = openNebulaDriver(cfg, st, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.EnsureSession(t.Context(), lane.SpawnRequest{RunID: cfg.RunID, LaneID: "absent"}); err == nil {
		t.Fatal("expected missing repository error")
	}
}

func TestCLIStartupPlanningRecorded(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"goal", "file", "json-file", "post", "plan-ref"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			args := []string{"--run", "startup-" + source, "--store", "memory", "--goal", "Find the smallest test", "--repo", "owner/project", "--driver", "fake"}
			ref := ""
			const post = "286c9d4f-9da3-4787-aa66-bd97aa21f947"
			content := "Investigate the assumption before rolling out."
			if source == "json-file" {
				content = `{"goal":"Investigate before rolling out"}`
			}
			switch source {
			case "file", "json-file":
				ref = filepath.Join(t.TempDir(), "plan.txt")
				if err := os.WriteFile(ref, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--plan", ref)
			case "post":
				ref = post
				args = append(args, "--plan", ref)
			case "plan-ref":
				ref = "post:" + post
				args = append(args, "--plan-ref", ref)
			}
			cfg, _, err := parseFlags("run", args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			fake, err := model.LoadFakeClient("testdata/critic_revise_replay.json")
			if err != nil {
				t.Fatal(err)
			}
			clk := clock.NewRealClock()
			st := store.NewMemoryStore(clk)
			defer st.Close()
			drv := lane.NewFakeDriver()
			// Every model call must happen before either the backlog or driver is mutated.
			guarded := modelClientFunc(func(ctx context.Context, req model.Request) (*model.Response, error) {
				items, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: cfg.RunID})
				if err != nil {
					t.Fatal(err)
				}
				if len(items) != 0 {
					t.Fatalf("backlog applied before revision: %+v", items)
				}
				resp, err := fake.CreateMessage(ctx, req)
				if err == nil && source == "json-file" && len(fake.Requests) == 3 {
					var rec PlanReconciliation
					if err := json.Unmarshal(resp.Message.Content[0].Input, &rec); err != nil {
						t.Fatal(err)
					}
					rec.PlanRef = "post:canonical"
					resp.Message.Content[0].Input, err = json.Marshal(rec)
				}
				return resp, err
			})
			runner := NewRunner(cfg, st, drv, clk, guarded, nil)
			runner.planner.planReader = planreader.NewPlanReader(map[string]string{post: content})
			if err := runner.Bootstrap(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := runner.Bootstrap(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := runner.controller.ReleaseLease(t.Context()); err != nil {
				t.Fatal(err)
			}
			resumed := NewRunner(cfg, st, drv, clk, guarded, nil)
			resumed.planner.planReader = runner.planner.planReader
			if err := resumed.Bootstrap(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(fake.Requests) != 3 {
				t.Fatalf("calls=%d", len(fake.Requests))
			}
			for i, role := range []string{"planner", "critic", "planner"} {
				req := fake.Requests[i]
				sel := cfg.Models.Roles[role]
				if req.Model != sel.Model || req.Metadata["provider"] != sel.Provider || req.Metadata["role"] != role {
					t.Fatalf("request %d: %+v", i, req)
				}
			}
			if !strings.Contains(fake.Requests[2].Messages[0].Text(), "small role resolution test") {
				t.Fatal("revision missing critique")
			}
			if ref != "" && !strings.Contains(fake.Requests[0].Messages[0].Text(), content) {
				t.Fatal("planner did not receive fetched plan")
			}
			if !strings.Contains(fake.Requests[0].Messages[0].Text(), cfg.Goal) {
				t.Fatal("planner did not receive goal")
			}
			items, err := st.Backlog().List(t.Context(), store.BacklogFilter{RunID: cfg.RunID})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].ID != "small" {
				t.Fatalf("backlog: %+v", items)
			}
			run, err := st.Runs().Get(t.Context(), cfg.RunID)
			if err != nil {
				t.Fatal(err)
			}
			wantRef := ref
			if source == "json-file" {
				wantRef = "post:canonical"
			}
			if run.Objective != cfg.Goal || run.PlanRef != wantRef {
				t.Fatalf("run: %+v", run)
			}
			decisions, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: cfg.RunID})
			if err != nil {
				t.Fatal(err)
			}
			if len(decisions) != 2 || decisions[0].Actor != "critic" {
				t.Fatalf("decisions: %+v", decisions)
			}
			var inputTokens, outputTokens int64
			for i := range decisions {
				inputTokens += decisions[i].InputTokens
				outputTokens += decisions[i].OutputTokens
			}
			if inputTokens != 300 || outputTokens != 150 {
				t.Fatalf("decision usage double counted: input=%d output=%d", inputTokens, outputTokens)
			}
		})
	}
}

func TestClosedPlanSkipsCritic(t *testing.T) {
	t.Parallel()
	fake, err := model.LoadFakeClient("testdata/closed_plan_replay.json")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewRealClock()
	st := store.NewMemoryStore(clk)
	defer st.Close()
	runner := NewRunner(Config{RunID: "closed", Goal: "Apply a known fix", StoreSpec: "memory"}, st, lane.NewFakeDriver(), clk, fake, nil)
	if err := runner.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests) != 2 {
		t.Fatalf("calls=%d", len(fake.Requests))
	}
	decisions, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) < 2 || decisions[0].Actor != "critic" || decisions[0].Provider == decisions[len(decisions)-1].Provider {
		t.Fatalf("startup decisions = %+v", decisions)
	}
}

func TestPlanningFailureDoesNotPublishDraft(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"planner", "critic", "revision"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			fake, err := model.LoadFakeClient("testdata/critic_revise_replay.json")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			client := modelClientFunc(func(ctx context.Context, req model.Request) (*model.Response, error) {
				calls++
				if stage == "critic" && req.Metadata["role"] == "critic" {
					return nil, &model.APIError{StatusCode: http.StatusTooManyRequests, Message: "rate limited", Attempts: 1}
				}
				if stage != "critic" && calls == map[string]int{"planner": 1, "revision": 3}[stage] {
					return nil, errors.New("recorded failure")
				}
				return fake.CreateMessage(ctx, req)
			})
			clk := clock.NewRealClock()
			st := store.NewMemoryStore(clk)
			defer st.Close()
			runner := NewRunner(Config{RunID: "failure", Goal: "Investigate", StoreSpec: "memory"}, st, lane.NewFakeDriver(), clk, client, nil)
			if err := runner.Bootstrap(t.Context()); stage == "critic" {
				if err != nil {
					t.Fatalf("critic failure should fall back to draft: %v", err)
				}
			} else if err == nil {
				t.Fatal("expected error")
			}
			items, err := st.Backlog().List(t.Context(), store.BacklogFilter{RunID: "failure"})
			if err != nil {
				t.Fatal(err)
			}
			if stage != "critic" && len(items) != 0 {
				t.Fatalf("draft published: %+v", items)
			}
			if stage == "critic" {
				if calls != 2 || len(items) != 1 || items[0].ID != "broad" || items[0].Title != "Roll out the change" {
					t.Fatalf("critic failure should publish planner draft without revision: calls=%d items=%+v", calls, items)
				}
				decisions, err := st.Decisions().List(t.Context(), store.DecisionFilter{RunID: "failure"})
				if err != nil {
					t.Fatal(err)
				}
				if len(decisions) != 2 || decisions[0].Actor != "critic" || decisions[0].Tool != "planner.critique" || decisions[0].Vetted || !strings.Contains(decisions[0].Result, "critic unavailable") || !strings.Contains(decisions[0].Reason, "429") {
					t.Fatalf("critic fallback decision = %+v", decisions)
				}
			}
		})
	}
}

func TestCLIOverridesOppositeConfigAlias(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ config, flag, value string }{
		{`{"goal":"old"}`, "--objective", "new"},
		{`{"objective":"old"}`, "--goal", "new"},
		{`{"plan":"old"}`, "--plan-ref", "new"},
		{`{"plan_ref":"old"}`, "--plan", "new"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := parseFlags("run", []string{"--run", "alias", "--store", "memory", "--config", path, tc.flag, tc.value}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.flag {
			case "--goal", "--objective":
				if cfg.Objective != "new" {
					t.Fatalf("objective=%q", cfg.Objective)
				}
			case "--plan":
				if cfg.Plan != "new" || cfg.PlanRef != "" {
					t.Fatalf("plan=%q ref=%q", cfg.Plan, cfg.PlanRef)
				}
			case "--plan-ref":
				if cfg.PlanRef != "new" || cfg.Plan != "" {
					t.Fatalf("plan=%q ref=%q", cfg.Plan, cfg.PlanRef)
				}
			}
		})
	}
}

func TestRoleProviderRoutingAndExplicitGateway(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			_, _ = io.WriteString(w, `{"id":"openai","output":[{"type":"message","text":"OpenAI"}]}`)
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"anthropic","role":"assistant","content":[{"type":"text","text":"Anthropic"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("ORCH_MODEL_PROVIDER", "")
	cfg := Config{LLMGatewayURL: server.URL}.withDefaults()
	client, err := openModelClient(cfg, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "critic", "dispatcher", "concierge", "router"} {
		got, err := roleClient(client, cfg, role).CreateMessage(t.Context(), model.Request{Model: cfg.Models.Roles[role].Model, Messages: []model.Message{model.NewUserMessage("test")}})
		if err != nil {
			t.Fatal(err)
		}
		want := "OpenAI"
		if role == "critic" {
			want = "Anthropic"
		}
		if got.Text() != want {
			t.Fatalf("role %s got %q want %q", role, got.Text(), want)
		}
	}
}

func TestPlannerRequestFetchesReference(t *testing.T) {
	t.Parallel()
	fake, err := model.LoadFakeClient("testdata/closed_plan_replay.json")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewRealClock()
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "replan"}); err != nil {
		t.Fatal(err)
	}
	planner := NewPlanner(st, clk, fake, "planner", nil)
	planner.planReader = planreader.NewPlanReader(map[string]string{"post:test": "A new plan from the referenced post"})
	if err := planner.HandlePlannerRequest(t.Context(), "replan", "Reconcile", "post:test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fake.Requests[0].Messages[0].Text(), "A new plan from the referenced post") {
		t.Fatal("reference not fetched")
	}
}

func TestPlannerRequestRejectsLocalFiles(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"/etc/passwd", "relative-plan.txt", "file:///etc/passwd"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			planner := NewPlanner(nil, nil, nil, "", nil)
			planner.planReader = planreader.PlanReaderFunc(func(context.Context, string) (planreader.PlanDocument, error) {
				t.Fatal("dispatcher-selected file reached the plan reader")
				return planreader.PlanDocument{}, nil
			})
			if err := planner.HandlePlannerRequest(t.Context(), "replan", "Reconcile", ref); err == nil || !strings.Contains(err.Error(), "post reference") {
				t.Fatalf("expected post reference restriction, got %v", err)
			}
		})
	}
}

func TestStartupGoalDoesNotMatchDispatcherReplan(t *testing.T) {
	t.Parallel()
	fake, err := model.LoadFakeClient("testdata/closed_plan_replay.json")
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewRealClock()
	st := store.NewMemoryStore(clk)
	defer st.Close()
	cfg := Config{RunID: "startup-provenance", Goal: "Apply a known fix", StoreSpec: "memory"}
	if err := st.Runs().Create(t.Context(), store.Run{ID: cfg.RunID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(t.Context(), store.Decision{
		RunID: cfg.RunID, Actor: ActorPlanner, Tool: ToolPlannerReplan, Reason: "Replan: " + cfg.Goal,
	}); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(cfg, st, lane.NewFakeDriver(), clk, fake, nil)
	if err := runner.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests) != 2 {
		t.Fatalf("startup calls=%d; dispatcher replan is not startup", len(fake.Requests))
	}
}

func TestConciergeRunControl(t *testing.T) {
	t.Parallel()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "run-control"}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"pause", "resume", "stop"} {
		client := model.NewFakeClient(
			model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("control", "run.control", json.RawMessage(`{"action":"`+action+`"}`)))},
			model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Run " + action))},
		)
		c := NewConcierge("run-control", st, nil, client, DefaultConciergeModel)
		if _, err := c.Ask(t.Context(), action+" the run"); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run-control", ActiveOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(dirs) != 1 || dirs[0].Ref != "chat:control" || dirs[0].Text != action {
			t.Fatalf("%s: %+v", action, dirs)
		}
	}
}

func TestCredentiallessRoles(t *testing.T) {
	for _, key := range []string{"ORCH_MODEL_PROVIDER", "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "LLM_GATEWAY_URL", "NEBULA_LLM_KEY"} {
		t.Setenv(key, "")
	}
	cfg := Config{DriverKind: "fake"}.withDefaults()
	for _, role := range []string{"planner", "critic", "dispatcher", "concierge", "router"} {
		want := "claude-cli"
		if role == "planner" {
			want = "codex-cli"
		}
		if got := cfg.Models.Roles[role].Provider; got != want {
			t.Errorf("%s provider = %s, want %s", role, got, want)
		}
	}
}

func TestConfigStallTimeoutExplicitPrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		args       []string
		explicit   bool
		timeout    time.Duration
	}{
		{name: "default", body: "{}", timeout: 45 * time.Minute},
		{name: "flag", args: []string{"--stall-timeout", "5m"}, explicit: true, timeout: 5 * time.Minute},
		{name: "file", body: `{"stall_timeout":1800000000000}`, explicit: true, timeout: 30 * time.Minute},
		{name: "flag with file", body: "{}", args: []string{"--stall-timeout", "5m"}, explicit: true, timeout: 5 * time.Minute},
		{name: "flag overrides file", body: `{"stall_timeout":1800000000000}`, args: []string{"--stall-timeout", "5m"}, explicit: true, timeout: 5 * time.Minute},
		{name: "zero flag resets file", body: `{"stall_timeout":1800000000000}`, args: []string{"--stall-timeout", "0"}, timeout: 45 * time.Minute},
		{name: "zero file", body: `{"stall_timeout":0}`, timeout: 45 * time.Minute},
		{name: "null file", body: `{"stall_timeout":null}`, timeout: 45 * time.Minute},
		{name: "negative flag", args: []string{"--stall-timeout", "-1m"}, timeout: 45 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string(nil), tc.args...)
			if tc.body != "" {
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--config", path)
			}
			cfg, _, err := parseFlags("run", args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			cfg = cfg.withDefaults()
			if cfg.HasExplicitStallTimeout() != tc.explicit || cfg.StallTimeout != tc.timeout {
				t.Fatalf("timeout=%s explicit=%v, want %s explicit=%v", cfg.StallTimeout, cfg.HasExplicitStallTimeout(), tc.timeout, tc.explicit)
			}
		})
	}
}
