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
	if !slices.Equal(before.AppliedMigrations, []string{"0001_init.sql", "0002_memory_notes.sql", "0003_decision_trace.sql", "0004_lane_base_commit.sql"}) {
		t.Fatalf("AppliedMigrations = %v, want [0001_init.sql 0002_memory_notes.sql 0003_decision_trace.sql]", before.AppliedMigrations)
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
