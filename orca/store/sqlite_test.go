package store_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestSQLiteStoreContract(t *testing.T) {
	t.Parallel()

	store.RunContractTests(t, func(t *testing.T, clk clock.Clock) store.Store {
		t.Helper()
		st, err := store.OpenRunSQLite(t.TempDir(), "run-1", clk)
		if err != nil {
			t.Fatalf("OpenRunSQLite error: %v", err)
		}
		return st
	})
}

type storeSnapshot struct {
	Run               *store.Run
	AppliedMigrations []string
	Backlog           []store.BacklogItem
	Lanes             []store.Lane
	Directives        []store.Directive
	Decisions         []store.Decision
	Events            []store.Event
	Threads           []store.Thread
	ParentTurns       []store.Turn
	ParentItems       []store.Item
	ForkTurns         []store.Turn
	ForkItems         []store.Item
	RecipeVersions    []store.Recipe
	RecipeLatest      []store.Recipe
	ControllerCursor  store.EventCursor
}

func captureSnapshot(t *testing.T, st *store.SQLiteStore, runID string) storeSnapshot {
	t.Helper()
	ctx := t.Context()

	migrations, err := st.AppliedMigrations(ctx)
	if err != nil {
		t.Fatalf("AppliedMigrations: %v", err)
	}
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("Runs().Get: %v", err)
	}
	backlog, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		t.Fatalf("Backlog().List: %v", err)
	}
	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		t.Fatalf("Lanes().List: %v", err)
	}
	directives, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID})
	if err != nil {
		t.Fatalf("Directives().List: %v", err)
	}
	decisions, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
	if err != nil {
		t.Fatalf("Decisions().List: %v", err)
	}
	events, err := st.Events().List(ctx, store.EventFilter{RunID: runID})
	if err != nil {
		t.Fatalf("Events().List: %v", err)
	}
	cursor, err := st.Events().GetCursor(ctx, runID, "controller")
	if err != nil {
		t.Fatalf("Events().GetCursor: %v", err)
	}
	threads, err := st.Threads().ListThreads(ctx, store.ThreadFilter{RunID: runID})
	if err != nil {
		t.Fatalf("Threads().ListThreads: %v", err)
	}
	parentTurns, err := st.Threads().ListTurns(ctx, "th-main")
	if err != nil {
		t.Fatalf("Threads().ListTurns(th-main): %v", err)
	}
	parentItems, err := st.Threads().ListItems(ctx, "th-main", "", 0, 0)
	if err != nil {
		t.Fatalf("Threads().ListItems(th-main): %v", err)
	}
	forkTurns, err := st.Threads().ListTurns(ctx, "th-fork")
	if err != nil {
		t.Fatalf("Threads().ListTurns(th-fork): %v", err)
	}
	forkItems, err := st.Threads().ListItems(ctx, "th-fork", "", 0, 0)
	if err != nil {
		t.Fatalf("Threads().ListItems(th-fork): %v", err)
	}
	recVersions, err := st.Recipes().ListVersions(ctx, runID, recipe.DefaultName)
	if err != nil {
		t.Fatalf("Recipes().ListVersions: %v", err)
	}
	recLatest, err := st.Recipes().ListLatest(ctx, runID)
	if err != nil {
		t.Fatalf("Recipes().ListLatest: %v", err)
	}

	return storeSnapshot{
		AppliedMigrations: migrations,
		Run:               run,
		Backlog:           backlog,
		Lanes:             lanes,
		Directives:        directives,
		Decisions:         decisions,
		Events:            events,
		ControllerCursor:  cursor,
		Threads:           threads,
		ParentTurns:       parentTurns,
		ParentItems:       parentItems,
		ForkTurns:         forkTurns,
		ForkItems:         forkItems,
		RecipeVersions:    recVersions,
		RecipeLatest:      recLatest,
	}
}

func TestSQLiteStoreReopenIdenticalState(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	const runID = "run-reopen"
	start := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	ctx := t.Context()

	st1, err := store.OpenRunSQLite(baseDir, runID, clk)
	if err != nil {
		t.Fatalf("OpenRunSQLite(initial) error: %v", err)
	}

	if err := st1.Runs().Create(ctx, store.Run{
		ID:        runID,
		Objective: "Initial objective",
		PlanRef:   "post:init",
	}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	clk.Advance(time.Second)
	if err := st1.Runs().UpdateObjective(ctx, runID, "Persisted objective", "post:5aaec193"); err != nil {
		t.Fatalf("UpdateObjective: %v", err)
	}
	if _, err := st1.Runs().AcquireLease(ctx, runID, "ctrl-1", 5*time.Minute); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	clk.Advance(10 * time.Second)
	if _, err := st1.Runs().HeartbeatLease(ctx, runID, "ctrl-1", 5*time.Minute); err != nil {
		t.Fatalf("HeartbeatLease: %v", err)
	}

	if err := st1.Backlog().Add(ctx, store.BacklogItem{
		ID:          "item-a",
		RunID:       runID,
		Title:       "Implement SQLite store",
		Description: "Pure-Go modernc.org/sqlite backend",
		Acceptance:  "Contract suite + reopen test pass",
		Priority:    store.PriorityP0,
		Order:       1,
		Tags:        []string{"store", "sqlite"},
	}); err != nil {
		t.Fatalf("Backlog.Add(item-a): %v", err)
	}
	if err := st1.Backlog().Add(ctx, store.BacklogItem{
		ID:         "item-b",
		RunID:      runID,
		Title:      "Wire CLI",
		Acceptance: "CLI runs",
		Priority:   store.PriorityP1,
		Order:      2,
		NoMerge:    true,
		DependsOn:  []string{"item-a"},
	}); err != nil {
		t.Fatalf("Backlog.Add(item-b): %v", err)
	}
	itemA, err := st1.Backlog().Get(ctx, runID, "item-a")
	if err != nil {
		t.Fatalf("Backlog.Get(item-a): %v", err)
	}
	itemA.Status = store.BacklogInProgress
	itemA.LaneID = "sqlite"
	itemA.RecipeName = recipe.DefaultName
	clk.Advance(time.Second)
	if err := st1.Backlog().Update(ctx, *itemA, "dispatcher", "spawned sqlite lane"); err != nil {
		t.Fatalf("Backlog.Update(item-a): %v", err)
	}
	clk.Advance(time.Second)
	if err := st1.Backlog().Close(ctx, runID, "item-a", store.BacklogDone, "controller", "merged PR"); err != nil {
		t.Fatalf("Backlog.Close(item-a): %v", err)
	}

	ln := store.Lane{
		ID:            "sqlite",
		RunID:         runID,
		ItemID:        "item-a",
		Title:         "Implement SQLite store",
		Branch:        "swarm-lane/sqlite",
		BaseBranch:    "main",
		Worktree:      "/workspace",
		Status:        lane.StatusDone,
		RecipeName:    recipe.DefaultName,
		RecipeVersion: 2,
		Attempt:       1,
		PR:            363,
		PRHead:        "abc1234",
		ApprovalSHA:   "abc1234",
		MergeSHA:      "def5678",
		Checks:        "green",
		LastEventID:   "ev-2",
		LastEventAt:   clk.Now(),
	}
	ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-1", "term-1")
	ln.RecordAttempt(recipe.PhaseReview, 1, "sess-2", "term-2")
	if err := st1.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatalf("Lanes.Upsert: %v", err)
	}

	expiry := clk.Now().Add(time.Hour)
	if err := st1.Directives().Add(ctx, store.Directive{
		ID:        "dir-1",
		RunID:     runID,
		Kind:      store.DirectiveStanding,
		Source:    store.DirectiveSourceOwner,
		Scope:     "run",
		Text:      "Always run lint before test",
		ExpiresAt: &expiry,
	}); err != nil {
		t.Fatalf("Directives.Add(dir-1): %v", err)
	}
	if err := st1.Directives().Add(ctx, store.Directive{
		ID:     "dir-2",
		RunID:  runID,
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceConcierge,
		Scope:  "lane:sqlite",
		Ref:    "post:5aaec193",
		Text:   "Focus on reopen test",
	}); err != nil {
		t.Fatalf("Directives.Add(dir-2): %v", err)
	}
	if err := st1.Directives().UpdateStatus(ctx, runID, "dir-2", store.DirectiveHandled); err != nil {
		t.Fatalf("Directives.UpdateStatus(dir-2): %v", err)
	}

	if _, err := st1.Decisions().Append(ctx, store.Decision{
		RunID:        runID,
		Category:     store.DecisionJudgment,
		Actor:        "dispatcher",
		ThreadID:     "th-main",
		TurnID:       "turn-1",
		LaneID:       "sqlite",
		Tool:         "lane.spawn",
		Args:         json.RawMessage(`{"lane":"sqlite","item":"item-a"}`),
		Reason:       "P0 ready",
		Result:       "spawned",
		Vetted:       true,
		InputTokens:  8500,
		OutputTokens: 240,
		CostUSD:      0.031,
		LatencyMs:    920,
	}); err != nil {
		t.Fatalf("Decisions.Append: %v", err)
	}

	if _, err := st1.Events().Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventLaneReport,
		Class:   store.EventClassAuto,
		LaneID:  "sqlite",
		Source:  "term-1",
		Summary: "swe phase done",
		Payload: json.RawMessage(`{"outcome":"done","pr":363}`),
	}); err != nil {
		t.Fatalf("Events.Append(1): %v", err)
	}
	ev2, err := st1.Events().Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventCI,
		Class:   store.EventClassAuto,
		LaneID:  "sqlite",
		Source:  "github",
		Summary: "checks green",
		Urgent:  true,
	})
	if err != nil {
		t.Fatalf("Events.Append(2): %v", err)
	}
	if err := st1.Events().SetCursor(ctx, runID, "controller", ev2.Seq); err != nil {
		t.Fatalf("Events.SetCursor(2): %v", err)
	}
	if err := st1.Events().SetCursor(ctx, runID, "controller", 1); err != nil {
		t.Fatalf("Events.SetCursor(monotonic regression check): %v", err)
	}

	if _, err := st1.Threads().StartThread(ctx, store.Thread{
		ID:    "th-main",
		RunID: runID,
		Role:  store.ThreadRoleDispatcher,
		Goal:  "Dispatch run-reopen",
	}); err != nil {
		t.Fatalf("Threads.StartThread: %v", err)
	}
	if err := st1.Threads().SetThreadResumeID(ctx, "th-main", "sess-reopen-1"); err != nil {
		t.Fatalf("Threads.SetThreadResumeID: %v", err)
	}
	if err := st1.Threads().SteerTurn(ctx, "th-main", "", "Queued steer before turn 1"); err != nil {
		t.Fatalf("Threads.SteerTurn(queued): %v", err)
	}
	if _, err := st1.Threads().StartTurn(ctx, store.Turn{
		ID:       "turn-1",
		ThreadID: "th-main",
		Input:    "Batch 1",
	}); err != nil {
		t.Fatalf("Threads.StartTurn: %v", err)
	}
	if err := st1.Threads().SteerTurn(ctx, "th-main", "turn-1", "Live steer during turn 1"); err != nil {
		t.Fatalf("Threads.SteerTurn(live): %v", err)
	}
	if _, err := st1.Threads().AppendItem(ctx, store.Item{
		ID:       "it-call-1",
		ThreadID: "th-main",
		TurnID:   "turn-1",
		Kind:     store.ItemKindToolCall,
		Name:     "lane.spawn",
		CallID:   "call_1",
		Content:  "spawn sqlite",
		Payload:  json.RawMessage(`{"lane":"sqlite"}`),
	}); err != nil {
		t.Fatalf("Threads.AppendItem: %v", err)
	}
	if err := st1.Threads().CompleteTurn(ctx, "th-main", "turn-1", store.TurnStatusCompleted, model.Usage{
		InputTokens:  8500,
		OutputTokens: 240,
		CostUSD:      0.031,
	}, ""); err != nil {
		t.Fatalf("Threads.CompleteTurn: %v", err)
	}
	if _, err := st1.Threads().ForkThread(ctx, "th-main", "th-fork", "turn-1"); err != nil {
		t.Fatalf("Threads.ForkThread: %v", err)
	}
	if err := st1.Threads().SteerTurn(ctx, "th-main", "", "Queued steer for next turn"); err != nil {
		t.Fatalf("Threads.SteerTurn(post-complete queued): %v", err)
	}

	r1 := recipe.Default()
	r1.RunID = runID
	r1.Version = 0
	if _, err := st1.Recipes().Save(ctx, r1); err != nil {
		t.Fatalf("Recipes.Save(v1): %v", err)
	}
	r2 := r1.Clone()
	r2.Source = "Custom v2 source"
	r2.Unmapped = []string{"custom note"}
	if _, err := st1.Recipes().Save(ctx, r2); err != nil {
		t.Fatalf("Recipes.Save(v2): %v", err)
	}

	before := captureSnapshot(t, st1, runID)
	if before.ControllerCursor.Seq != 2 {
		t.Fatalf("ControllerCursor.Seq = %d, want 2 (monotonic)", before.ControllerCursor.Seq)
	}
	if !slices.Equal(before.AppliedMigrations, []string{"0001_init.sql", "0002_memory_notes.sql", "0003_decision_trace.sql", "0004_lane_base_commit.sql", "0005_run_settings.sql", "0006_run_repository.sql", "0007_thread_resume_id.sql", "0008_cost_guard.sql", "0009_backlog_no_merge.sql", "0010_lane_answers.sql"}) {
		t.Fatalf("AppliedMigrations = %v, want all migrations through 0010", before.AppliedMigrations)
	}

	if err := st1.Close(); err != nil {
		t.Fatalf("st1.Close() error: %v", err)
	}

	st2, err := store.OpenRunSQLite(baseDir, runID, clk)
	if err != nil {
		t.Fatalf("OpenRunSQLite(reopen) error: %v", err)
	}
	defer st2.Close()

	after := captureSnapshot(t, st2, runID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot mismatch after close+reopen:\nbefore: %+v\nafter:  %+v", before, after)
	}

	ev3, err := st2.Events().Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventTimer,
		Summary: "post-reopen tick",
	})
	if err != nil {
		t.Fatalf("Events.Append after reopen: %v", err)
	}
	if ev3.Seq != 3 {
		t.Fatalf("ev3.Seq after reopen = %d, want 3", ev3.Seq)
	}

	dec2, err := st2.Decisions().Append(ctx, store.Decision{
		RunID: runID,
		Actor: "controller",
		Tool:  "noop",
	})
	if err != nil {
		t.Fatalf("Decisions.Append after reopen: %v", err)
	}
	if dec2.Seq != 2 {
		t.Fatalf("dec2.Seq after reopen = %d, want 2", dec2.Seq)
	}
}

func TestSQLiteStoreConcurrency(t *testing.T) {
	t.Parallel()

	st, err := store.OpenRunSQLite(t.TempDir(), "run-conc", nil)
	if err != nil {
		t.Fatalf("OpenRunSQLite error: %v", err)
	}
	defer st.Close()
	ctx := t.Context()

	if err := st.Runs().Create(ctx, store.Run{ID: "run-conc", Objective: "sqlite concurrency test"}); err != nil {
		t.Fatalf("Create run error: %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			_, err := st.Events().Append(ctx, store.Event{
				RunID:   "run-conc",
				Kind:    store.EventTimer,
				Summary: fmt.Sprintf("tick-%d", idx),
			})
			if err != nil {
				t.Errorf("Append event %d: %v", idx, err)
			}
			_, err = st.Decisions().Append(ctx, store.Decision{
				RunID: "run-conc",
				Actor: "controller",
				Tool:  "noop",
			})
			if err != nil {
				t.Errorf("Append decision %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	evs, err := st.Events().List(ctx, store.EventFilter{RunID: "run-conc"})
	if err != nil {
		t.Fatalf("List events: %v", err)
	}
	if len(evs) != workers {
		t.Fatalf("len(evs) = %d, want %d", len(evs), workers)
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event[%d].Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestSQLiteStorePerRunFiles(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	ctx := t.Context()

	for _, badRunID := range []string{"", " ", ".", "..", "a/b", `a\b`} {
		if _, err := store.RunDBPath(baseDir, badRunID); err == nil {
			t.Fatalf("RunDBPath(%q) succeeded, want error", badRunID)
		}
	}

	stA, err := store.OpenRunSQLite(baseDir, "run-alpha", nil)
	if err != nil {
		t.Fatalf("OpenRunSQLite(run-alpha): %v", err)
	}
	defer stA.Close()

	stB, err := store.OpenRunSQLite(baseDir, "run-beta", nil)
	if err != nil {
		t.Fatalf("OpenRunSQLite(run-beta): %v", err)
	}
	defer stB.Close()

	if stA.Path() == stB.Path() {
		t.Fatalf("expected distinct database files per run, got same path %q", stA.Path())
	}

	evA, err := stA.Events().Append(ctx, store.Event{RunID: "run-alpha", Kind: store.EventTimer, Summary: "alpha-1"})
	if err != nil {
		t.Fatalf("stA.Events().Append: %v", err)
	}
	evB, err := stB.Events().Append(ctx, store.Event{RunID: "run-beta", Kind: store.EventTimer, Summary: "beta-1"})
	if err != nil {
		t.Fatalf("stB.Events().Append: %v", err)
	}
	if evA.Seq != 1 || evB.Seq != 1 {
		t.Fatalf("seqs = (%d, %d), want (1, 1)", evA.Seq, evB.Seq)
	}
}

func TestMemoryUpgradeAndDurability(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "store.db")
	st, err := store.OpenSQLite(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a database from before memory existed, with 0001 already applied.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `DROP TABLE memories; DROP TABLE run_notes; DROP TABLE memory_seed_state; DELETE FROM schema_migrations WHERE version='0002_memory_notes.sql';`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.OpenSQLite(path, nil)
	if err != nil {
		t.Fatalf("upgrade legacy database: %v", err)
	}
	starters := store.StarterMemories()
	changed := starters[0]
	changed.Body = "Owner correction must persist"
	if _, err := st.Memories().Put(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if err := st.Memories().Delete(ctx, "", starters[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Notes().Append(ctx, store.RunNote{RunID: "run-1", Text: "upgrade works"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.OpenSQLite(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Memories().Get(ctx, "", changed.ID)
	if err != nil || got.Body != changed.Body {
		t.Fatalf("owner edit after reopen = %+v, %v", got, err)
	}
	if _, err := st.Memories().Get(ctx, "", starters[1].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted starter resurrected: %v", err)
	}
	notes, err := st.Notes().List(ctx, store.NoteFilter{RunID: "run-1"})
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes after reopen = %+v, %v", notes, err)
	}
}

func TestObjectiveDoneAndCostGuardHelpers(t *testing.T) {
	t.Parallel()
	for _, doneNext := range []string{
		"",
		"none",
		"done.",
		"N/A",
		"complete",
		"completed",
		"nothing left",
		"nothing left to do",
		"No open work remaining",
		"Done — all merged",
		"Done; PR merged",
		"Run DONE 22:25Z",
		"none, all merged",
		"Complete (all lanes merged)",
		"none. Run DONE 22:25Z — all lanes merged (#5604, #5811). PLAN.md log + post updated. No loop armed.",
		"Run complete — all PRs merged",
	} {
		if !store.IsDoneOrEmptyNext(doneNext) {
			t.Fatalf("IsDoneOrEmptyNext(%q) = false, want true", doneNext)
		}
	}
	for _, activeNext := range []string{
		"Complete the M3 review",
		"Completed M1; start M2",
		"Finished X; start M4",
		"(a) staff M4",
		".github workflow fix",
		"run complete; deploy pending",
		"when #5786 merges, staff a lane for 6-7",
	} {
		if store.IsDoneOrEmptyNext(activeNext) {
			t.Fatalf("IsDoneOrEmptyNext(%q) = true, want false", activeNext)
		}
		obj := "# Run\nGoal: test\nNext: " + activeNext + "\n"
		if store.ObjectiveIsDone(obj) {
			t.Fatalf("ObjectiveIsDone(%q) = true, want false", obj)
		}
	}
	if got := store.ExtractNextSection("Next: staff M4\n\nNext:\n"); got != "staff M4" {
		t.Fatalf("ExtractNextSection with trailing bare Next: = %q, want %q", got, "staff M4")
	}
	if got := store.ExtractNextSection("Next:\n", "Next: fallback step\n"); got != "fallback step" {
		t.Fatalf("ExtractNextSection multi-source fallthrough = %q, want %q", got, "fallback step")
	}

	ctx := t.Context()
	clk := clock.NewFakeClock(time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "cost.db")
	st, err := store.OpenSQLite(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Runs().Create(ctx, store.Run{
		ID:        "run-cost",
		Objective: "Goal: cost test\nNext: Complete the M3 review",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Runs().UpdateSettings(ctx, "run-cost", store.RunSettings{
		ConcurrencyCap:    2,
		StallTimeout:      15 * time.Minute,
		MaxCostPerHourUSD: 7.5,
	}); err != nil {
		t.Fatal(err)
	}
	gotSettings, err := st.Runs().GetSettings(ctx, "run-cost")
	if err != nil {
		t.Fatal(err)
	}
	if gotSettings.MaxCostPerHourUSD != 7.5 || gotSettings.String() != "concurrency_cap=2 stall_timeout=15m0s paused=false max_cost_per_hour_usd=$7.50" {
		t.Fatalf("GetSettings = %+v (%q), want MaxCostPerHourUSD=7.5", gotSettings, gotSettings.String())
	}

	// Reset MaxCostPerHourUSD to 0 so EnforceCostGuard uses fallbackLimit without persisting it.
	gotSettings.MaxCostPerHourUSD = 0
	if err := st.Runs().UpdateSettings(ctx, "run-cost", gotSettings); err != nil {
		t.Fatal(err)
	}

	// Record a turn 2 hours ago (outside 1h window) and a turn 10 minutes ago (inside 1h window)
	// that has both a thread turn and a decision with the same TurnID.
	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID:    "run-cost",
		Category: store.DecisionJudgment,
		Actor:    "dispatcher",
		Tool:     "dispatcher.turn",
		CostUSD:  10.0,
		Vetted:   true,
	}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Hour)
	if _, err := st.Threads().StartThread(ctx, store.Thread{ID: "thr-1", RunID: "run-cost", Role: store.ThreadRoleDispatcher}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Threads().StartTurn(ctx, store.Turn{ID: "turn-1", ThreadID: "thr-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Threads().CompleteTurn(ctx, "thr-1", "turn-1", store.TurnStatusCompleted, model.Usage{CostUSD: 6.0}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(ctx, store.Decision{
		RunID:    "run-cost",
		ThreadID: "thr-1",
		TurnID:   "turn-1",
		Category: store.DecisionJudgment,
		Actor:    "dispatcher",
		Tool:     "lane.noop",
		CostUSD:  6.0,
		Vetted:   true,
	}); err != nil {
		t.Fatal(err)
	}

	spent, err := store.HourlyRunCostUSD(ctx, st, "run-cost", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if spent != 6.0 {
		t.Fatalf("HourlyRunCostUSD = %.2f, want 6.00 (old turn excluded, same-turn decision deduplicated)", spent)
	}

	exceeded, _, _, msg, err := store.EnforceCostGuard(ctx, st, "run-cost", clk.Now(), 5.0)
	if err != nil || !exceeded || msg == "" {
		t.Fatalf("EnforceCostGuard(1) = (%v, %q, %v), want (true, non-empty, nil)", exceeded, msg, err)
	}
	afterGuard, err := st.Runs().GetSettings(ctx, "run-cost")
	if err != nil {
		t.Fatal(err)
	}
	if !afterGuard.Paused || afterGuard.MaxCostPerHourUSD != 0 {
		t.Fatalf("settings after fallback cost guard = %+v, want Paused=true and MaxCostPerHourUSD=0", afterGuard)
	}

	// Owner unpauses while still over the hourly limit; next EnforceCostGuard re-pauses and emits a new notice.
	afterGuard.Paused = false
	if err := st.Runs().UpdateSettings(ctx, "run-cost", afterGuard); err != nil {
		t.Fatal(err)
	}
	exceeded2, _, _, msg2, err := store.EnforceCostGuard(ctx, st, "run-cost", clk.Now(), 5.0)
	if err != nil || !exceeded2 || msg2 == "" {
		t.Fatalf("EnforceCostGuard(after unpause) = (%v, %q, %v), want (true, non-empty, nil)", exceeded2, msg2, err)
	}

	// Owner explicitly disables the cost guard (max_cost_per_hour_usd=0 -> CostGuardDisabled=true);
	// verify SQLite round-trip and that EnforceCostGuard does not substitute fallbackLimit.
	if err := st.Runs().UpdateSettings(ctx, "run-cost", store.RunSettings{
		ConcurrencyCap:    2,
		StallTimeout:      15 * time.Minute,
		MaxCostPerHourUSD: 0,
		CostGuardDisabled: true,
		Paused:            false,
	}); err != nil {
		t.Fatal(err)
	}
	disabledSettings, err := st.Runs().GetSettings(ctx, "run-cost")
	if err != nil {
		t.Fatal(err)
	}
	if !disabledSettings.CostGuardDisabled || disabledSettings.MaxCostPerHourUSD != 0 {
		t.Fatalf("disabledSettings = %+v, want CostGuardDisabled=true MaxCostPerHourUSD=0", disabledSettings)
	}
	exceeded3, _, _, _, err := store.EnforceCostGuard(ctx, st, "run-cost", clk.Now(), 5.0)
	if err != nil || exceeded3 {
		t.Fatalf("EnforceCostGuard with CostGuardDisabled=true = (%v, %v), want (false, nil)", exceeded3, err)
	}

	// Contract directive with explicit non-contract Kind and non-empty Scope round-trips as Contract=true in both stores.
	contractDir := store.Directive{
		ID:       "dir-contract-scoped",
		RunID:    "run-cost",
		Kind:     store.DirectiveStanding,
		Scope:    "run",
		Contract: true,
		Text:     "Do not edit go.mod manually",
	}
	if err := st.Directives().Add(ctx, contractDir); err != nil {
		t.Fatal(err)
	}
	gotSQLite, err := st.Directives().Get(ctx, "run-cost", "dir-contract-scoped")
	if err != nil {
		t.Fatal(err)
	}
	if !gotSQLite.IsContract() || !gotSQLite.Contract || gotSQLite.Kind != store.DirectiveContract || gotSQLite.Scope != "run" {
		t.Fatalf("gotSQLite = %+v, want Contract=true Kind=contract Scope=run", gotSQLite)
	}
	memSt := store.NewMemoryStore(clk)
	defer memSt.Close()
	if err := memSt.Runs().Create(ctx, store.Run{ID: "run-cost"}); err != nil {
		t.Fatal(err)
	}
	if err := memSt.Directives().Add(ctx, contractDir); err != nil {
		t.Fatal(err)
	}
	gotMem, err := memSt.Directives().Get(ctx, "run-cost", "dir-contract-scoped")
	if err != nil {
		t.Fatal(err)
	}
	if !gotMem.IsContract() || !gotMem.Contract || gotMem.Kind != store.DirectiveContract || gotMem.Scope != "run" {
		t.Fatalf("gotMem = %+v, want Contract=true Kind=contract Scope=run", gotMem)
	}
}

func TestSQLiteLaneAnswerStateSurvivesReopen(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clk := clock.NewFakeClock(time.Time{})
	path := filepath.Join(t.TempDir(), "answers.db")
	st, err := store.OpenSQLite(path, clk)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Runs().Create(ctx, store.Run{ID: "run-1", Objective: "Answer lane questions"}); err != nil {
		t.Fatal(err)
	}
	ln := lane.Lane{ID: "question", RunID: "run-1", Status: lane.StatusBlocked, TerminalID: "t-1", Question: "Restore main?", AnswerAttemptedAt: clk.Now(), AwaitingInputSince: clk.Now()}
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatal(err)
	}
	for _, confirmed := range []bool{false, true} {
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st, err = store.OpenSQLite(path, clk)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Lanes().Get(ctx, "run-1", ln.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Question != ln.Question || !got.AnswerAttemptedAt.Equal(ln.AnswerAttemptedAt) || !got.AnswerConfirmedAt.Equal(ln.AnswerConfirmedAt) || !got.AwaitingInputSince.Equal(ln.AwaitingInputSince) || got.AnswerEpoch != ln.AnswerEpoch {
			t.Fatalf("reopened lane=%+v, want answer state from %+v", got, ln)
		}
		if !got.DuplicateQuestion(ln.Question, clk.Now().Add(time.Second)) {
			t.Fatalf("reopened answer lost duplicate suppression: %+v", got)
		}
		if !confirmed {
			clk.Advance(time.Second)
			ln.Status = lane.StatusRunning
			ln.AnswerConfirmedAt = clk.Now()
			ln.AnswerEpoch = 1
			ln.AwaitingInputSince = time.Time{}
			if err := st.Lanes().Upsert(ctx, ln); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}
