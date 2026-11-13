package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/concierge"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/planner"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

func seedPlannerStore(t *testing.T) (*clock.FakeClock, *store.MemoryStore, *lane.FakeDriver) {
	t.Helper()
	ctx := t.Context()
	start := time.Date(2026, 4, 15, 13, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	drv := lane.NewFakeDriver()

	defRecipe := recipe.Default()
	defRecipe.RunID = "run-1"
	defRecipe.CreatedAt = start
	if _, err := st.Recipes().Save(ctx, defRecipe); err != nil {
		t.Fatalf("Save default recipe: %v", err)
	}

	if err := st.Runs().Create(ctx, store.Run{
		ID:            "run-1",
		Objective:     "Initial prototype objective",
		PlanRef:       "docs/design/old-plan.md",
		DefaultRecipe: recipe.DefaultName,
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}

	// Active standing directive that conflicts with 6 concurrent store/ lanes in plan_v2.md.
	if err := st.Directives().Add(ctx, store.Directive{
		ID:        "dir-rule-store-cap",
		RunID:     "run-1",
		Kind:      store.DirectiveStanding,
		Source:    store.DirectiveSourceConcierge,
		Scope:     "store/",
		Text:      "Never spawn more than 2 concurrent lanes touching store/.",
		Status:    store.DirectiveActive,
		CreatedAt: start.Add(-15 * time.Minute),
	}); err != nil {
		t.Fatalf("Add directive: %v", err)
	}

	for _, it := range []store.BacklogItem{
		{
			ID:          "item-alpha",
			RunID:       "run-1",
			Title:       "Implement Store and Controller",
			Description: "Core store and controller loop",
			Priority:    store.PriorityP0,
			Status:      store.BacklogInProgress,
			LaneID:      "lane-alpha",
			Order:       1,
		},
		{
			ID:          "item-beta",
			RunID:       "run-1",
			Title:       "Implement Dispatcher thread",
			Description: "Dispatcher judgment routing",
			Priority:    store.PriorityP2,
			Status:      store.BacklogOpen,
			Order:       2,
		},
		{
			ID:          "item-legacy-poller",
			RunID:       "run-1",
			Title:       "Implement legacy bash poller",
			Description: "Old bash polling loop superseded by controller",
			Priority:    store.PriorityP1,
			Status:      store.BacklogOpen,
			Order:       3,
		},
	} {
		if err := st.Backlog().Add(ctx, it); err != nil {
			t.Fatalf("Add backlog %s: %v", it.ID, err)
		}
	}

	for _, ln := range []lane.Lane{
		{
			ID:            "lane-alpha",
			RunID:         "run-1",
			ItemID:        "item-alpha",
			Title:         "Implement Store and Controller",
			RecipeName:    recipe.DefaultName,
			RecipeVersion: 1,
			Phase:         recipe.PhaseSWE,
			Round:         1,
			Status:        lane.StatusRunning,
			Branch:        "swarm-lane/run-1/alpha",
			PR:            101,
			PRHead:        "abc1234567890",
			SessionID:     "sess-alpha",
			TerminalID:    "term-alpha-swe-1",
		},
		{
			ID:            "lane-legacy",
			RunID:         "run-1",
			ItemID:        "item-legacy-poller",
			Title:         "Implement legacy bash poller",
			RecipeName:    recipe.DefaultName,
			RecipeVersion: 1,
			Phase:         recipe.PhaseSWE,
			Round:         1,
			Status:        lane.StatusRunning,
			Branch:        "swarm-lane/run-1/legacy",
			SessionID:     "sess-legacy",
			TerminalID:    "term-legacy-swe-1",
		},
	} {
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			t.Fatalf("Upsert lane %s: %v", ln.ID, err)
		}
	}

	return clk, st, drv
}

func TestRecorded_PlanImportFromFile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk, st, drv := seedPlannerStore(t)

	client, err := model.LoadFakeClient(filepath.Join("testdata", "plan_import_replay.json"))
	if err != nil {
		t.Fatalf("LoadFakeClient: %v", err)
	}

	pln := planner.New(planner.Config{}, st, drv, clk, client)
	planPath := filepath.Join("testdata", "plan_v2.md")

	res, err := pln.ImportPlan(ctx, "run-1", planPath)
	if err != nil {
		t.Fatalf("ImportPlan: %v", err)
	}
	if res == nil || res.Reconciliation == nil || res.Plan == nil {
		t.Fatalf("unexpected nil fields on Result: %+v", res)
	}
	if res.Mode != planner.ModePlanImport {
		t.Fatalf("Mode = %q, want %q", res.Mode, planner.ModePlanImport)
	}
	if res.Plan.SourceKind != planner.PlanSourceFile {
		t.Fatalf("Plan.SourceKind = %q, want %q", res.Plan.SourceKind, planner.PlanSourceFile)
	}
	if res.TurnCount != 2 {
		t.Fatalf("TurnCount = %d, want 2", res.TurnCount)
	}

	th, err := st.Threads().GetThread(ctx, res.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Role != store.ThreadRolePlanner || th.RunID != "run-1" {
		t.Fatalf("unexpected thread: %+v", th)
	}
	turns, err := st.Threads().ListTurns(ctx, res.ThreadID)
	if err != nil {
		t.Fatalf("ListTurns: %v", err)
	}
	if len(turns) != 1 || turns[0].Status != store.TurnStatusCompleted {
		t.Fatalf("unexpected turns: %+v", turns)
	}

	run, err := st.Runs().Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	wantObj := "Ship the Nebula swarm orchestrator harness in orca/ with Concierge, Planner, and SQLite persistence ready for shadow evaluation."
	if run.Objective != wantObj {
		t.Fatalf("run.Objective = %q, want %q", run.Objective, wantObj)
	}
	if run.PlanRef != planPath {
		t.Fatalf("run.PlanRef = %q, want %q", run.PlanRef, planPath)
	}
	if !res.Applied.ObjectiveUpdated {
		t.Fatalf("expected ObjectiveUpdated=true")
	}

	if !slices.Equal(res.Applied.AddedItems, []string{"item-concierge", "item-planner"}) {
		t.Fatalf("AddedItems = %v, want [item-concierge item-planner]", res.Applied.AddedItems)
	}
	if !slices.Equal(res.Applied.UpdatedItems, []string{"item-beta"}) {
		t.Fatalf("UpdatedItems = %v, want [item-beta]", res.Applied.UpdatedItems)
	}
	if !slices.Equal(res.Applied.DroppedItems, []string{"item-legacy-poller"}) {
		t.Fatalf("DroppedItems = %v, want [item-legacy-poller]", res.Applied.DroppedItems)
	}
	if !slices.Equal(res.Applied.ReorderedItems, []string{"item-alpha", "item-concierge", "item-planner", "item-beta"}) {
		t.Fatalf("ReorderedItems = %v", res.Applied.ReorderedItems)
	}

	conciergeItem, err := st.Backlog().Get(ctx, "run-1", "item-concierge")
	if err != nil {
		t.Fatalf("Get item-concierge: %v", err)
	}
	if conciergeItem.Title != "Implement concierge chat threads" || conciergeItem.Priority != store.PriorityP0 || conciergeItem.Status != store.BacklogOpen {
		t.Fatalf("item-concierge = %+v", conciergeItem)
	}

	plannerItem, err := st.Backlog().Get(ctx, "run-1", "item-planner")
	if err != nil {
		t.Fatalf("Get item-planner: %v", err)
	}
	if !slices.Equal(plannerItem.DependsOn, []string{"item-concierge"}) || plannerItem.Priority != store.PriorityP0 {
		t.Fatalf("item-planner = %+v", plannerItem)
	}

	betaItem, err := st.Backlog().Get(ctx, "run-1", "item-beta")
	if err != nil {
		t.Fatalf("Get item-beta: %v", err)
	}
	if betaItem.Priority != store.PriorityP0 || betaItem.Title != "Complete end-to-end replay suite across concierge and planner" {
		t.Fatalf("item-beta = %+v", betaItem)
	}

	droppedItem, err := st.Backlog().Get(ctx, "run-1", "item-legacy-poller")
	if err != nil {
		t.Fatalf("Get item-legacy-poller: %v", err)
	}
	if droppedItem.Status != store.BacklogDropped {
		t.Fatalf("item-legacy-poller status = %q, want %q", droppedItem.Status, store.BacklogDropped)
	}

	if !slices.Equal(res.Applied.StoppedLanes, []string{"lane-legacy"}) {
		t.Fatalf("StoppedLanes = %v, want [lane-legacy]", res.Applied.StoppedLanes)
	}
	if !slices.Equal(res.Applied.RebriefedLanes, []string{"lane-alpha"}) {
		t.Fatalf("RebriefedLanes = %v, want [lane-alpha]", res.Applied.RebriefedLanes)
	}

	if len(drv.Stops) != 1 || drv.Stops[0].LaneID != "lane-legacy" {
		t.Fatalf("drv.Stops = %+v, want 1 stop on lane-legacy", drv.Stops)
	}
	legacyLane, err := st.Lanes().Get(ctx, "run-1", "lane-legacy")
	if err != nil {
		t.Fatalf("Get lane-legacy: %v", err)
	}
	if legacyLane.Status != lane.StatusFailed {
		t.Fatalf("lane-legacy Status = %q, want %q", legacyLane.Status, lane.StatusFailed)
	}

	if len(drv.Steers) != 1 || drv.Steers[0].LaneID != "lane-alpha" {
		t.Fatalf("drv.Steers = %+v, want 1 steer on lane-alpha", drv.Steers)
	}
	if !strings.Contains(drv.Steers[0].Message, "Include Planner trigger hooks") {
		t.Fatalf("drv.Steers[0].Message = %q", drv.Steers[0].Message)
	}

	if len(res.Applied.OwnerAsks) != 2 {
		t.Fatalf("len(OwnerAsks) = %d, want 2: %+v", len(res.Applied.OwnerAsks), res.Applied.OwnerAsks)
	}
	for _, ask := range res.Applied.OwnerAsks {
		if err := ask.Validate(); err != nil {
			t.Fatalf("OwnerAsk %s failed Validate: %v", ask.ID, err)
		}
		storedAskDir, err := st.Directives().Get(ctx, "run-1", "ask-"+ask.ID)
		if err != nil {
			t.Fatalf("Get directive ask-%s: %v", ask.ID, err)
		}
		if storedAskDir.Status != store.DirectiveActive || storedAskDir.ExpiresAt == nil {
			t.Fatalf("expected active owner ask directive with deadline, got %+v", storedAskDir)
		}
	}
	if res.Applied.OwnerAsks[0].ID != "ask-conflict-store-cap" || res.Applied.OwnerAsks[0].Default != "keep_cap_2" {
		t.Fatalf("unexpected conflict ask: %+v", res.Applied.OwnerAsks[0])
	}
	if res.Applied.OwnerAsks[1].Default != "skip_unmappable" {
		t.Fatalf("unexpected unmappable ask: %+v", res.Applied.OwnerAsks[1])
	}

	for _, substr := range []string{
		"added 2 backlog item(s) [item-concierge, item-planner]",
		"updated 1 backlog item(s) [item-beta]",
		"dropped 1 backlog item(s) [item-legacy-poller]",
		"stopped 1 lane(s) [lane-legacy]",
		"re-briefed 1 lane(s) [lane-alpha]",
		"owner asks [ask-conflict-store-cap",
	} {
		if !strings.Contains(res.Summary, substr) {
			t.Fatalf("Summary missing %q:\n%s", substr, res.Summary)
		}
	}

	if len(res.Decisions) == 0 {
		t.Fatalf("expected planner decisions to be logged")
	}
	for _, dec := range res.Decisions {
		if dec.Actor != planner.ActorPlanner {
			t.Fatalf("dec.Actor = %q, want %q", dec.Actor, planner.ActorPlanner)
		}
	}
}

func TestRecorded_PlanImportFromNebulaPostID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk, st, drv := seedPlannerStore(t)

	planBytes, err := os.ReadFile(filepath.Join("testdata", "plan_v2.md"))
	if err != nil {
		t.Fatalf("ReadFile plan_v2.md: %v", err)
	}

	const postID = "5aaec193-a1f1-4009-aa55-f6174d583653"
	var fetchedPostID string
	reader := &planner.DefaultPlanReader{
		FetchPost: func(_ context.Context, id string) (string, error) {
			fetchedPostID = id
			return string(planBytes), nil
		},
	}

	client, err := model.LoadFakeClient(filepath.Join("testdata", "plan_import_replay.json"))
	if err != nil {
		t.Fatalf("LoadFakeClient: %v", err)
	}

	pln := planner.New(planner.Config{
		PlanReader: reader,
	}, st, drv, clk, client)

	res, err := pln.ImportPlan(ctx, "run-1", "post:"+postID)
	if err != nil {
		t.Fatalf("ImportPlan from Nebula post: %v", err)
	}
	if fetchedPostID != postID {
		t.Fatalf("fetchedPostID = %q, want %q", fetchedPostID, postID)
	}
	if res.Plan == nil || res.Plan.SourceKind != planner.PlanSourceNebulaPost || res.Plan.Target != postID {
		t.Fatalf("unexpected PlanDocument: %+v", res.Plan)
	}

	run, err := st.Runs().Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if run.PlanRef != "post:"+postID {
		t.Fatalf("run.PlanRef = %q, want post:%s", run.PlanRef, postID)
	}
	if !slices.Equal(res.Applied.AddedItems, []string{"item-concierge", "item-planner"}) {
		t.Fatalf("AddedItems = %v", res.Applied.AddedItems)
	}
}

func TestRecorded_RecipeEdit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk, st, drv := seedPlannerStore(t)

	client, err := model.LoadFakeClient(filepath.Join("testdata", "recipe_edit_replay.json"))
	if err != nil {
		t.Fatalf("LoadFakeClient: %v", err)
	}

	pln := planner.New(planner.Config{}, st, drv, clk, client)

	editText := "Skip clean for docs-only changes: route swe directly to review when judgment_rule matches 'if the change only modifies documentation or markdown files'."
	res, err := pln.EditRecipe(ctx, "run-1", "default", editText)
	if err != nil {
		t.Fatalf("EditRecipe: %v", err)
	}
	if res == nil || res.RecipeResult == nil {
		t.Fatalf("unexpected nil RecipeResult: %+v", res)
	}
	if res.Mode != planner.ModeRecipeEdit {
		t.Fatalf("Mode = %q, want %q", res.Mode, planner.ModeRecipeEdit)
	}
	if res.RecipeResult.Recipe.Name != "default" || res.RecipeResult.Recipe.Version != 2 {
		t.Fatalf("Recipe = %s@v%d, want default@v2", res.RecipeResult.Recipe.Name, res.RecipeResult.Recipe.Version)
	}
	if res.RecipeResult.Recipe.BaseRecipe != "default" || res.RecipeResult.Recipe.BaseVersion != 1 {
		t.Fatalf("Base = %s@v%d, want default@v1", res.RecipeResult.Recipe.BaseRecipe, res.RecipeResult.Recipe.BaseVersion)
	}
	if !strings.Contains(res.Summary, "Compiled recipe default@v2 (base default@v1") {
		t.Fatalf("unexpected Summary: %q", res.Summary)
	}
	if !strings.Contains(res.RecipeResult.EchoTable, "### Recipe `default` (v2, base `default` v1)") {
		t.Fatalf("unexpected EchoTable:\n%s", res.RecipeResult.EchoTable)
	}

	th, err := st.Threads().GetThread(ctx, res.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.Role != store.ThreadRolePlanner {
		t.Fatalf("th.Role = %q, want planner", th.Role)
	}

	storedV2, err := st.Recipes().Get(ctx, "run-1", "default", 2)
	if err != nil {
		t.Fatalf("Get default@v2: %v", err)
	}
	mech, judgment := storedV2.MatchTransitions("swe", "done", false, 1)
	if mech == nil || mech.To != "clean" {
		t.Fatalf("expected mechanical transition swe->clean, got %+v", mech)
	}
	if len(judgment) != 1 || judgment[0].To != "review" || judgment[0].On.JudgmentRule != "if the change only modifies documentation or markdown files" {
		t.Fatalf("expected judgment transition swe->review, got %+v", judgment)
	}

	// Existing v1 recipe and in-flight lane remain pinned to v1.
	storedV1, err := st.Recipes().Get(ctx, "run-1", "default", 1)
	if err != nil {
		t.Fatalf("Get default@v1: %v", err)
	}
	if storedV1.Version != 1 {
		t.Fatalf("storedV1.Version = %d, want 1", storedV1.Version)
	}
	alphaLane, err := st.Lanes().Get(ctx, "run-1", "lane-alpha")
	if err != nil {
		t.Fatalf("Get lane-alpha: %v", err)
	}
	if alphaLane.RecipeVersion != 1 {
		t.Fatalf("alphaLane.RecipeVersion = %d, want 1", alphaLane.RecipeVersion)
	}

	if len(res.Decisions) != 1 || res.Decisions[0].Tool != planner.ToolRecipeCompile {
		t.Fatalf("Decisions = %+v, want 1 recipe.compile decision", res.Decisions)
	}
}

func TestConciergeToPlanner_EndToEndDirectives(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk, st, drv := seedPlannerStore(t)

	planPath := filepath.Join("testdata", "plan_v2.md")

	// 1. Plan import via Concierge directive.submit -> Planner DirectiveHandler.
	plannerPlanClient, err := model.LoadFakeClient(filepath.Join("testdata", "plan_import_replay.json"))
	if err != nil {
		t.Fatalf("LoadFakeClient plan_import_replay: %v", err)
	}
	pln := planner.New(planner.Config{}, st, drv, clk, plannerPlanClient)

	planDirInput, _ := json.Marshal(concierge.DirectiveSubmitParams{
		ID:   "dir-import-v2",
		Kind: concierge.KindPlanImport,
		Ref:  planPath,
		Text: "import updated plan v2",
	})
	conciergePlanClient := model.NewFakeClient(
		model.Response{
			ID:         "msg_conc_plan_1",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_plan_1", concierge.ToolDirectiveSubmit, planDirInput),
			),
		},
		model.Response{
			ID:         "msg_conc_plan_2",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Submitted `plan_import` directive `dir-import-v2` on `run-1` and reconciled `" + planPath + "`."),
			),
		},
	)

	conc := concierge.New(concierge.Config{
		OnDirective: pln.DirectiveHandler(),
	}, st, drv, clk, conciergePlanClient)

	resp, err := conc.Ask(ctx, "run-1", "Please import the updated plan at "+planPath)
	if err != nil {
		t.Fatalf("conc.Ask plan_import: %v", err)
	}
	if len(resp.SubmittedDirectives) != 1 || resp.SubmittedDirectives[0].Kind != store.DirectivePlanImport {
		t.Fatalf("SubmittedDirectives = %+v", resp.SubmittedDirectives)
	}

	dir1, err := st.Directives().Get(ctx, "run-1", "dir-import-v2")
	if err != nil {
		t.Fatalf("Get dir-import-v2: %v", err)
	}
	if dir1.Status != store.DirectiveHandled {
		t.Fatalf("dir1.Status = %q, want %q", dir1.Status, store.DirectiveHandled)
	}

	// 2. Recipe edit via Concierge directive.submit -> Planner DirectiveHandler.
	plannerRecipeClient, err := model.LoadFakeClient(filepath.Join("testdata", "recipe_edit_replay.json"))
	if err != nil {
		t.Fatalf("LoadFakeClient recipe_edit_replay: %v", err)
	}
	plnRecipe := planner.New(planner.Config{}, st, drv, clk, plannerRecipeClient)

	editText := "Skip clean for docs-only changes: route swe directly to review when judgment_rule matches 'if the change only modifies documentation or markdown files'."
	recipeDirInput, _ := json.Marshal(concierge.DirectiveSubmitParams{
		ID:    "dir-recipe-v2",
		Kind:  concierge.KindRecipeEdit,
		Ref:   "default",
		Scope: "default",
		Text:  editText,
	})
	conciergeRecipeClient := model.NewFakeClient(
		model.Response{
			ID:         "msg_conc_recipe_1",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_recipe_1", concierge.ToolDirectiveSubmit, recipeDirInput),
			),
		},
		model.Response{
			ID:         "msg_conc_recipe_2",
			Model:      concierge.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Submitted `recipe_edit` directive `dir-recipe-v2` on `run-1` and compiled `default` v2."),
			),
		},
	)

	concRecipe := concierge.New(concierge.Config{
		OnDirective: plnRecipe.DirectiveHandler(),
	}, st, drv, clk, conciergeRecipeClient)

	resp2, err := concRecipe.Ask(ctx, "run-1", "Skip clean for docs-only changes on the default recipe")
	if err != nil {
		t.Fatalf("concRecipe.Ask recipe_edit: %v", err)
	}
	if len(resp2.SubmittedDirectives) != 1 || resp2.SubmittedDirectives[0].Kind != store.DirectiveRecipeEdit {
		t.Fatalf("SubmittedDirectives = %+v", resp2.SubmittedDirectives)
	}

	dir2, err := st.Directives().Get(ctx, "run-1", "dir-recipe-v2")
	if err != nil {
		t.Fatalf("Get dir-recipe-v2: %v", err)
	}
	if dir2.Status != store.DirectiveHandled {
		t.Fatalf("dir2.Status = %q, want %q", dir2.Status, store.DirectiveHandled)
	}

	storedV2, err := st.Recipes().Get(ctx, "run-1", "default", 2)
	if err != nil {
		t.Fatalf("Get default@v2: %v", err)
	}
	if storedV2.Version != 2 {
		t.Fatalf("storedV2.Version = %d, want 2", storedV2.Version)
	}
}

func TestHandleSignal_ReplanOnControllerSignal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk, st, drv := seedPlannerStore(t)

	reconcileInput, _ := json.Marshal(planner.Reconciliation{
		Summary: "Added follow-up integration verification task after milestone completion.",
		BacklogDiff: planner.BacklogDiff{
			Add: []planner.BacklogAddItem{
				{
					ID:       "item-verify",
					Title:    "Run milestone verification suite",
					Priority: "p0",
				},
			},
		},
	})
	client := model.NewFakeClient(
		model.Response{
			ID:         "msg_replan_1",
			Model:      planner.DefaultModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_replan_1", planner.ToolSubmitReconciliation, reconcileInput),
			),
		},
		model.Response{
			ID:         "msg_replan_2",
			Model:      planner.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewTextBlock("Added follow-up integration verification task after milestone completion."),
			),
		},
	)

	pln := planner.New(planner.Config{}, st, drv, clk, client)
	res, err := pln.HandleSignal(ctx, planner.Signal{
		RunID:  "run-1",
		Kind:   planner.SignalEmptyBacklog,
		Reason: "no ready backlog items remain",
	})
	if err != nil {
		t.Fatalf("HandleSignal: %v", err)
	}
	if res.Mode != planner.ModeReplan {
		t.Fatalf("Mode = %q, want %q", res.Mode, planner.ModeReplan)
	}
	if !slices.Equal(res.Applied.AddedItems, []string{"item-verify"}) {
		t.Fatalf("AddedItems = %v, want [item-verify]", res.Applied.AddedItems)
	}

	verifyItem, err := st.Backlog().Get(ctx, "run-1", "item-verify")
	if err != nil {
		t.Fatalf("Get item-verify: %v", err)
	}
	if verifyItem.Title != "Run milestone verification suite" || verifyItem.Priority != store.PriorityP0 {
		t.Fatalf("verifyItem = %+v", verifyItem)
	}

	var _ []controller.OwnerAsk = res.Applied.OwnerAsks
}

func TestReconcile_ApplyFailureAndRetry(t *testing.T) {
	t.Parallel()

	t.Run("FailurePropagatesAndKeepsDirectiveActive", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clk, st, drv := seedPlannerStore(t)

		badInput, _ := json.Marshal(planner.Reconciliation{
			Summary: "Stop nonexistent lane",
			LanesToStop: []planner.LaneStopSpec{
				{LaneID: "lane-does-not-exist", Reason: "obsolete"},
			},
		})
		client := model.NewFakeClient(
			model.Response{
				ID:         "msg_bad_1",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("tu_bad_1", planner.ToolSubmitReconciliation, badInput),
				),
			},
			model.Response{
				ID:         "msg_bad_2",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonEndTurn,
				Message: model.NewAssistantMessage(
					model.NewTextBlock("Unable to stop unknown lane."),
				),
			},
		)

		dir := store.Directive{
			ID:     "dir-plan-fail",
			RunID:  "run-1",
			Kind:   store.DirectivePlanImport,
			Source: store.DirectiveSourceConcierge,
			Ref:    filepath.Join("testdata", "plan_v2.md"),
			Status: store.DirectiveActive,
		}
		if err := st.Directives().Add(ctx, dir); err != nil {
			t.Fatalf("Add directive: %v", err)
		}

		pln := planner.New(planner.Config{}, st, drv, clk, client)
		res, err := pln.HandleDirective(ctx, "run-1", dir)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected store.ErrNotFound from failing apply, got %v", err)
		}
		if res == nil || res.Reconciliation != nil {
			t.Fatalf("expected nil Reconciliation on failed apply, got %+v", res)
		}
		storedDir, err := st.Directives().Get(ctx, "run-1", "dir-plan-fail")
		if err != nil {
			t.Fatalf("Get directive: %v", err)
		}
		if storedDir.Status != store.DirectiveActive {
			t.Fatalf("expected directive to stay active on failed apply, got %q", storedDir.Status)
		}
	})

	t.Run("ModelRetryAppliesSecondAttempt", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		clk, st, drv := seedPlannerStore(t)

		badInput, _ := json.Marshal(planner.Reconciliation{
			LanesToStop: []planner.LaneStopSpec{
				{LaneID: "lane-does-not-exist", Reason: "typo"},
			},
		})
		goodInput, _ := json.Marshal(planner.Reconciliation{
			Summary: "Stopped lane-legacy on retry.",
			LanesToStop: []planner.LaneStopSpec{
				{LaneID: "lane-legacy", Reason: "superseded"},
			},
		})
		client := model.NewFakeClient(
			model.Response{
				ID:         "msg_retry_1",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("tu_retry_1", planner.ToolSubmitReconciliation, badInput),
				),
			},
			model.Response{
				ID:         "msg_retry_2",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("tu_retry_2", planner.ToolSubmitReconciliation, goodInput),
				),
			},
			model.Response{
				ID:         "msg_retry_3",
				Model:      planner.DefaultModel,
				StopReason: model.StopReasonEndTurn,
				Message: model.NewAssistantMessage(
					model.NewTextBlock("Stopped lane-legacy on retry."),
				),
			},
		)

		pln := planner.New(planner.Config{}, st, drv, clk, client)
		res, err := pln.Replan(ctx, "run-1", "retry test")
		if err != nil {
			t.Fatalf("Replan retry: %v", err)
		}
		if !slices.Equal(res.Applied.StoppedLanes, []string{"lane-legacy"}) {
			t.Fatalf("StoppedLanes = %v, want [lane-legacy]", res.Applied.StoppedLanes)
		}
	})
}

func TestReconcile_SkipsTerminalLaneOnStopAndBlocksLaneOnAsk(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk, st, drv := seedPlannerStore(t)

	doneLane := lane.Lane{
		ID:     "lane-done",
		RunID:  "run-1",
		Status: lane.StatusDone,
	}
	if err := st.Lanes().Upsert(ctx, doneLane); err != nil {
		t.Fatalf("Upsert lane-done: %v", err)
	}

	reconcileInput, _ := json.Marshal(planner.Reconciliation{
		Summary: "Check terminal lane stop and duplicate ask ID.",
		LanesToStop: []planner.LaneStopSpec{
			{LaneID: "lane-done", Reason: "already done"},
		},
		Conflicts: []planner.ConflictPoint{
			{
				ID:          "ask-dup",
				Description: "first conflict on lane-alpha",
				LaneID:      "lane-alpha",
			},
			{
				ID:          "ask-dup",
				Description: "duplicate ask ID should not fail",
				LaneID:      "lane-alpha",
			},
		},
	})
	client := model.NewFakeClient(
		model.Response{
			ID:         "msg_term_1",
			Model:      planner.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_term_1", planner.ToolSubmitReconciliation, reconcileInput),
			),
		},
	)

	pln := planner.New(planner.Config{}, st, drv, clk, client)
	res, err := pln.Replan(ctx, "run-1", "terminal lane and ask test")
	if err != nil {
		t.Fatalf("Replan: %v", err)
	}
	if len(res.Applied.StoppedLanes) != 0 {
		t.Fatalf("expected terminal lane-done not to be stopped, got %v", res.Applied.StoppedLanes)
	}
	gotDone, err := st.Lanes().Get(ctx, "run-1", "lane-done")
	if err != nil {
		t.Fatalf("Get lane-done: %v", err)
	}
	if gotDone.Status != lane.StatusDone {
		t.Fatalf("lane-done Status = %q, want %q", gotDone.Status, lane.StatusDone)
	}
	gotAlpha, err := st.Lanes().Get(ctx, "run-1", "lane-alpha")
	if err != nil {
		t.Fatalf("Get lane-alpha: %v", err)
	}
	if gotAlpha.Status != lane.StatusBlocked {
		t.Fatalf("expected conflict ask on lane-alpha to block lane, got %q", gotAlpha.Status)
	}
}

func TestPlanner_DirectiveLifecycleAndOnPlannerRequest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk, st, drv := seedPlannerStore(t)

	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "planreq-1",
		RunID:  "run-1",
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceDispatcher,
		Ref:    "planner_request:planreq-1",
		Text:   "dispatcher requested replan",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatalf("Add planreq-1: %v", err)
	}

	reconcileInput, _ := json.Marshal(planner.Reconciliation{
		Summary: "Replanned for dispatcher request.",
	})
	client := model.NewFakeClient(
		model.Response{
			ID:         "msg_req_1",
			Model:      planner.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("tu_req_1", planner.ToolSubmitReconciliation, reconcileInput),
			),
		},
	)

	pln := planner.New(planner.Config{}, st, drv, clk, client)
	if err := pln.OnPlannerRequest(ctx, "run-1", "dispatcher requested replan", ""); err != nil {
		t.Fatalf("OnPlannerRequest: %v", err)
	}
	reqDir, err := st.Directives().Get(ctx, "run-1", "planreq-1")
	if err != nil {
		t.Fatalf("Get planreq-1: %v", err)
	}
	if reqDir.Status != store.DirectiveHandled {
		t.Fatalf("planreq-1 Status = %q, want %q", reqDir.Status, store.DirectiveHandled)
	}

	standingDir, err := st.Directives().Get(ctx, "run-1", "dir-rule-store-cap")
	if err != nil {
		t.Fatalf("Get standing directive: %v", err)
	}
	if _, err := pln.HandleDirective(ctx, "run-1", *standingDir); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument when passing standing directive to HandleDirective, got %v", err)
	}
}

func TestPlanReader_ValidationAndConfinement(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for _, tc := range []struct {
		ref      string
		wantKind string
		wantTgt  string
	}{
		{"post:5aaec193-a1f1-4009-aa55-f6174d583653", planner.PlanSourceNebulaPost, "5aaec193-a1f1-4009-aa55-f6174d583653"},
		{"nebula://post/abc-123", planner.PlanSourceNebulaPost, "abc-123"},
		{"5aaec193-a1f1-4009-aa55-f6174d583653", planner.PlanSourceNebulaPost, "5aaec193-a1f1-4009-aa55-f6174d583653"},
		{"file://testdata/plan_v2.md", planner.PlanSourceFile, "testdata/plan_v2.md"},
		{"testdata/plan_v2.md", planner.PlanSourceFile, "testdata/plan_v2.md"},
	} {
		kind, tgt := planner.ParsePlanRef(tc.ref)
		if kind != tc.wantKind || tgt != tc.wantTgt {
			t.Fatalf("ParsePlanRef(%q) = (%q, %q), want (%q, %q)", tc.ref, kind, tgt, tc.wantKind, tc.wantTgt)
		}
	}

	reader := &planner.DefaultPlanReader{
		BaseDir:     "testdata",
		DisableExec: true,
	}
	doc, err := reader.ReadPlan(ctx, "plan_v2.md")
	if err != nil {
		t.Fatalf("ReadPlan within BaseDir: %v", err)
	}
	if doc.SourceKind != planner.PlanSourceFile || doc.Title == "" {
		t.Fatalf("unexpected doc: %+v", doc)
	}

	if _, err := reader.ReadPlan(ctx, "../planner.go"); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for path escaping BaseDir, got %v", err)
	}
	if _, err := reader.ReadPlan(ctx, "post:--help"); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for flag-like post target, got %v", err)
	}

	smallReader := &planner.DefaultPlanReader{
		BaseDir:  "testdata",
		MaxBytes: 16,
	}
	if _, err := smallReader.ReadPlan(ctx, "plan_v2.md"); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument when plan exceeds MaxBytes, got %v", err)
	}
}
