package store

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
)

// Factory constructs a fresh Store wired to clk for a single contract subtest.
type Factory func(t *testing.T, clk clock.Clock) Store

// RunContractTests executes the shared Store contract test suite against
// a backend Factory. Every Store implementation (memory, SQLite, Nebula API)
// must pass this suite.
func RunContractTests(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("MemoriesAndNotes", func(t *testing.T) {
		t.Parallel()
		testMemoriesAndNotes(t, factory)
	})
	t.Run("RunsAndLeases", func(t *testing.T) {
		t.Parallel()
		testRunsAndLeases(t, factory)
	})
	t.Run("Backlog", func(t *testing.T) {
		t.Parallel()
		testBacklog(t, factory)
	})
	t.Run("Lanes", func(t *testing.T) {
		t.Parallel()
		testLanes(t, factory)
	})
	t.Run("Directives", func(t *testing.T) {
		t.Parallel()
		testDirectives(t, factory)
	})
	t.Run("Decisions", func(t *testing.T) {
		t.Parallel()
		testDecisions(t, factory)
	})
	t.Run("EventsAndCursors", func(t *testing.T) {
		t.Parallel()
		testEventsAndCursors(t, factory)
	})
	t.Run("ThreadsTurnsAndItems", func(t *testing.T) {
		t.Parallel()
		testThreadsTurnsAndItems(t, factory)
	})
	t.Run("Recipes", func(t *testing.T) {
		t.Parallel()
		testRecipes(t, factory)
	})
}

func testRunsAndLeases(t *testing.T, factory Factory) {
	start := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	if _, err := st.Runs().Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}

	run := Run{
		ID:        "run-1",
		Objective: "Build native swarm orchestrator",
		PlanRef:   "post:5aaec193",
	}
	if err := st.Runs().Create(ctx, run); err != nil {
		t.Fatalf("Create error: %v", err)
	}
	if err := st.Runs().Create(ctx, run); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Create error = %v, want ErrAlreadyExists", err)
	}

	got, err := st.Runs().Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Objective != run.Objective || got.PlanRef != run.PlanRef || got.DefaultRecipe != recipe.DefaultName {
		t.Fatalf("Get = %+v, want objective/planRef/defaultRecipe populated", got)
	}
	if err := st.Runs().Create(ctx, Run{ID: "run-0", Repository: "o/r", Objective: "earlier"}); err != nil {
		t.Fatalf("Create run-0 error: %v", err)
	}
	listed, err := st.Runs().List(ctx)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(listed) != 2 || listed[0].ID != "run-0" || listed[0].Repository != "o/r" || listed[1].ID != "run-1" || listed[1].Objective != run.Objective {
		t.Fatalf("List = %+v, want run-0 then run-1", listed)
	}

	clk.Advance(5 * time.Second)
	if err := st.Runs().UpdateObjective(ctx, "run-1", "Updated objective", "post:new-plan"); err != nil {
		t.Fatalf("UpdateObjective error: %v", err)
	}
	got, err = st.Runs().Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get after UpdateObjective error: %v", err)
	}
	if got.Objective != "Updated objective" || got.PlanRef != "post:new-plan" {
		t.Fatalf("Get after UpdateObjective = %+v", got)
	}

	ttl := 30 * time.Second
	l1, err := st.Runs().AcquireLease(ctx, "run-1", "holder-a", ttl)
	if err != nil {
		t.Fatalf("AcquireLease(holder-a) error: %v", err)
	}
	if l1.HolderID != "holder-a" || l1.Expired(clk.Now()) {
		t.Fatalf("AcquireLease = %+v", l1)
	}

	if _, err := st.Runs().AcquireLease(ctx, "run-1", "holder-b", ttl); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("AcquireLease(holder-b) while held = %v, want ErrLeaseHeld", err)
	}
	if _, err := st.Runs().HeartbeatLease(ctx, "run-1", "holder-b", ttl); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("HeartbeatLease(holder-b) = %v, want ErrLeaseNotHeld", err)
	}

	clk.Advance(10 * time.Second)
	l1Hb, err := st.Runs().HeartbeatLease(ctx, "run-1", "holder-a", ttl)
	if err != nil {
		t.Fatalf("HeartbeatLease(holder-a) error: %v", err)
	}
	if !l1Hb.ExpiresAt.Equal(clk.Now().Add(ttl)) || !l1Hb.AcquiredAt.Equal(l1.AcquiredAt) {
		t.Fatalf("HeartbeatLease = %+v, want extended ExpiresAt and preserved AcquiredAt", l1Hb)
	}

	clk.Advance(ttl + time.Second)
	if _, err := st.Runs().HeartbeatLease(ctx, "run-1", "holder-a", ttl); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("HeartbeatLease after expiry = %v, want ErrLeaseNotHeld", err)
	}
	l2, err := st.Runs().AcquireLease(ctx, "run-1", "holder-b", ttl)
	if err != nil {
		t.Fatalf("AcquireLease(holder-b) after expiry error: %v", err)
	}
	if l2.HolderID != "holder-b" {
		t.Fatalf("HolderID = %q, want holder-b", l2.HolderID)
	}

	if err := st.Runs().ReleaseLease(ctx, "run-1", "holder-a"); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("ReleaseLease(holder-a) = %v, want ErrLeaseNotHeld", err)
	}
	if err := st.Runs().ReleaseLease(ctx, "run-1", "holder-b"); err != nil {
		t.Fatalf("ReleaseLease(holder-b) error: %v", err)
	}
	got, err = st.Runs().Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get after ReleaseLease error: %v", err)
	}
	if got.Lease != nil {
		t.Fatalf("expected nil Lease after release, got %+v", got.Lease)
	}
}

func testBacklog(t *testing.T, factory Factory) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC))
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	items := []BacklogItem{
		{
			ID:         "item-p2",
			RunID:      "run-1",
			Title:      "Secondary polish",
			Acceptance: "lint green",
			Priority:   PriorityP2,
			Tags:       []string{"cleanup"},
		},
		{
			ID:         "item-p0-b",
			RunID:      "run-1",
			Title:      "Core store",
			Acceptance: "contract tests pass",
			Priority:   PriorityP0,
			Order:      2,
		},
		{
			ID:         "item-p0-a",
			RunID:      "run-1",
			Title:      "Domain types",
			Acceptance: "types compile",
			Priority:   PriorityP0,
			Order:      1,
		},
	}
	for i := range items {
		if err := st.Backlog().Add(ctx, items[i]); err != nil {
			t.Fatalf("Add(%s) error: %v", items[i].ID, err)
		}
	}
	if err := st.Backlog().Add(ctx, items[0]); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Add error = %v, want ErrAlreadyExists", err)
	}

	listed, err := st.Backlog().List(ctx, BacklogFilter{RunID: "run-1"})
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	wantOrder := []string{"item-p0-a", "item-p0-b", "item-p2"}
	var gotOrder []string
	for i := range listed {
		gotOrder = append(gotOrder, listed[i].ID)
	}
	if !slices.Equal(gotOrder, wantOrder) {
		t.Fatalf("List order = %v, want %v", gotOrder, wantOrder)
	}

	if err := st.Backlog().Reorder(ctx, "run-1", []string{"item-p0-b", "item-p0-a"}, "dispatcher"); err != nil {
		t.Fatalf("Reorder error: %v", err)
	}
	listed, err = st.Backlog().List(ctx, BacklogFilter{RunID: "run-1", Limit: 2})
	if err != nil {
		t.Fatalf("List after Reorder error: %v", err)
	}
	if len(listed) != 2 || listed[0].ID != "item-p0-b" || listed[1].ID != "item-p0-a" {
		t.Fatalf("List after Reorder = %+v", listed)
	}

	got, err := st.Backlog().Get(ctx, "run-1", "item-p2")
	if err != nil {
		t.Fatalf("Get(item-p2) error: %v", err)
	}
	got.Tags[0] = "mutated-locally"
	fresh, err := st.Backlog().Get(ctx, "run-1", "item-p2")
	if err != nil {
		t.Fatalf("Get(item-p2) second time error: %v", err)
	}
	if fresh.Tags[0] != "cleanup" {
		t.Fatalf("store state was mutated by caller slice edit: %v", fresh.Tags)
	}

	fresh.Status = BacklogInProgress
	fresh.LaneID = "lane-core"
	if err := st.Backlog().Update(ctx, *fresh, "dispatcher", "assigned to lane-core"); err != nil {
		t.Fatalf("Update error: %v", err)
	}

	inProg, err := st.Backlog().List(ctx, BacklogFilter{
		RunID:    "run-1",
		Statuses: []BacklogStatus{BacklogInProgress},
		LaneID:   "lane-core",
	})
	if err != nil {
		t.Fatalf("List(in_progress) error: %v", err)
	}
	if len(inProg) != 1 || inProg[0].ID != "item-p2" || len(inProg[0].History) < 2 {
		t.Fatalf("List(in_progress) = %+v", inProg)
	}

	if err := st.Backlog().Close(ctx, "run-1", "item-p2", BacklogOpen, "dispatcher", "bad close"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Close with non-terminal status = %v, want ErrInvalidArgument", err)
	}
	if err := st.Backlog().Close(ctx, "run-1", "item-p2", BacklogDone, "dispatcher", "PR merged"); err != nil {
		t.Fatalf("Close(done) error: %v", err)
	}
	closed, err := st.Backlog().Get(ctx, "run-1", "item-p2")
	if err != nil {
		t.Fatalf("Get after Close error: %v", err)
	}
	if closed.Status != BacklogDone || len(closed.History) < 3 {
		t.Fatalf("closed item = %+v", closed)
	}
}

func testLanes(t *testing.T, factory Factory) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC))
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	ln := Lane{
		ID:            "core",
		RunID:         "run-1",
		ItemID:        "item-1",
		Title:         "Create orca/ module",
		Status:        lane.StatusRunning,
		Phase:         recipe.PhaseSWE,
		Round:         1,
		RecipeName:    recipe.DefaultName,
		RecipeVersion: 1,
		SessionID:     "sess-1",
		TerminalID:    "term-1",
		Sessions:      map[string]string{"swe": "sess-1"},
		Terminals:     map[string]string{"swe": "term-1"},
		LastEventID:   "ev-1",
	}
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatalf("Upsert error: %v", err)
	}

	got, err := st.Lanes().Get(ctx, "run-1", "core")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Phase != recipe.PhaseSWE || got.Round != 1 || got.RecipeVersion != 1 {
		t.Fatalf("Get = %+v", got)
	}
	got.Sessions["swe"] = "corrupted"
	again, err := st.Lanes().Get(ctx, "run-1", "core")
	if err != nil {
		t.Fatalf("Get second time error: %v", err)
	}
	if again.Sessions["swe"] != "sess-1" {
		t.Fatalf("Sessions map was mutated by caller: %v", again.Sessions)
	}

	again.RecordAttempt(recipe.PhaseReview, 1, "sess-2", "term-2")
	again.Status = lane.StatusDone
	if err := st.Lanes().Upsert(ctx, *again); err != nil {
		t.Fatalf("Upsert update error: %v", err)
	}

	doneLanes, err := st.Lanes().List(ctx, LaneFilter{
		RunID:    "run-1",
		Statuses: []LaneStatus{lane.StatusDone},
	})
	if err != nil {
		t.Fatalf("List(done) error: %v", err)
	}
	if len(doneLanes) != 1 || doneLanes[0].Phase != recipe.PhaseReview {
		t.Fatalf("List(done) = %+v", doneLanes)
	}

	if err := st.Lanes().Delete(ctx, "run-1", "core"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if _, err := st.Lanes().Get(ctx, "run-1", "core"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func testDirectives(t *testing.T, factory Factory) {
	start := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	expiry := start.Add(10 * time.Minute)
	d1 := Directive{
		ID:        "dir-1",
		RunID:     "run-1",
		Kind:      DirectiveStanding,
		Source:    DirectiveSourceOwner,
		Scope:     "run",
		Text:      "Never ask the owner when CI is red; send back to swe.",
		ExpiresAt: &expiry,
	}
	d2 := Directive{
		ID:     "dir-2",
		RunID:  "run-1",
		Kind:   DirectivePlanImport,
		Source: DirectiveSourceConcierge,
		Scope:  "run",
		Ref:    "post:5aaec193",
		Text:   "Import the swarm orchestrator design post",
	}
	if err := st.Directives().Add(ctx, d1); err != nil {
		t.Fatalf("Add(d1) error: %v", err)
	}
	if err := st.Directives().Add(ctx, d2); err != nil {
		t.Fatalf("Add(d2) error: %v", err)
	}

	active, err := st.Directives().List(ctx, DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil {
		t.Fatalf("List(active) error: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("len(active) = %d, want 2", len(active))
	}

	if err := st.Directives().UpdateStatus(ctx, "run-1", "dir-2", DirectiveHandled); err != nil {
		t.Fatalf("UpdateStatus error: %v", err)
	}
	active, err = st.Directives().List(ctx, DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil {
		t.Fatalf("List(active) after handled error: %v", err)
	}
	if len(active) != 1 || active[0].ID != "dir-1" {
		t.Fatalf("active directives = %+v, want only dir-1", active)
	}

	clk.Advance(11 * time.Minute)
	active, err = st.Directives().List(ctx, DirectiveFilter{RunID: "run-1", ActiveOnly: true})
	if err != nil {
		t.Fatalf("List(active) after expiry error: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active directives after expiry, got %+v", active)
	}

	all, err := st.Directives().List(ctx, DirectiveFilter{RunID: "run-1", ActiveOnly: false})
	if err != nil {
		t.Fatalf("List(all) error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}
}

func testDecisions(t *testing.T, factory Factory) {
	start := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(start)
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	dec1, err := st.Decisions().Append(ctx, Decision{
		RunID:     "run-1",
		Category:  DecisionAuto,
		Actor:     "controller",
		LaneID:    "core",
		Tool:      "phase.advance",
		Args:      json.RawMessage(`{"from":"swe","to":"clean"}`),
		Reason:    "swe reported done",
		Result:    "started clean",
		Vetted:    true,
		LatencyMs: 12,
	})
	if err != nil {
		t.Fatalf("Append(dec1) error: %v", err)
	}
	if dec1.Seq != 1 {
		t.Fatalf("dec1.Seq = %d, want 1", dec1.Seq)
	}

	clk.Advance(time.Minute)
	dec2, err := st.Decisions().Append(ctx, Decision{
		RunID:        "run-1",
		Category:     DecisionJudgment,
		Actor:        "dispatcher",
		ThreadID:     "th-disp",
		TurnID:       "turn-1",
		LaneID:       "core",
		Tool:         "lane.spawn",
		Args:         json.RawMessage(`{"item":"item-1"}`),
		Reason:       "slot available",
		Result:       "spawned core",
		Vetted:       true,
		InputTokens:  12000,
		OutputTokens: 350,
		CostUSD:      0.042,
		LatencyMs:    1850,
	})
	if err != nil {
		t.Fatalf("Append(dec2) error: %v", err)
	}
	if dec2.Seq != 2 {
		t.Fatalf("dec2.Seq = %d, want 2", dec2.Seq)
	}

	since1, err := st.Decisions().List(ctx, DecisionFilter{
		RunID:    "run-1",
		SinceSeq: 1,
		Category: DecisionJudgment,
	})
	if err != nil {
		t.Fatalf("List(SinceSeq=1) error: %v", err)
	}
	if len(since1) != 1 || since1[0].Seq != 2 || since1[0].Tool != "lane.spawn" {
		t.Fatalf("List(SinceSeq=1) = %+v", since1)
	}
}

func testEventsAndCursors(t *testing.T, factory Factory) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 5, 0, 0, 0, time.UTC))
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	cur, err := st.Events().GetCursor(ctx, "run-1", "controller")
	if err != nil {
		t.Fatalf("GetCursor initial error: %v", err)
	}
	if cur.Seq != 0 {
		t.Fatalf("initial cursor Seq = %d, want 0", cur.Seq)
	}

	ev1, err := st.Events().Append(ctx, Event{
		RunID:   "run-1",
		Kind:    EventLaneReport,
		Class:   EventClassAuto,
		LaneID:  "core",
		Summary: "core swe done",
		Payload: json.RawMessage(`{"outcome":"done"}`),
	})
	if err != nil {
		t.Fatalf("Append(ev1) error: %v", err)
	}
	ev2, err := st.Events().Append(ctx, Event{
		RunID:   "run-1",
		Kind:    EventOwnerMessage,
		Class:   EventClassJudgment,
		Urgent:  true,
		Summary: "prioritize sqlite store",
	})
	if err != nil {
		t.Fatalf("Append(ev2) error: %v", err)
	}
	if ev1.Seq != 1 || ev2.Seq != 2 {
		t.Fatalf("event seqs = (%d, %d), want (1, 2)", ev1.Seq, ev2.Seq)
	}

	if err := st.Events().SetCursor(ctx, "run-1", "controller", ev1.Seq); err != nil {
		t.Fatalf("SetCursor error: %v", err)
	}
	cur, err = st.Events().GetCursor(ctx, "run-1", "controller")
	if err != nil {
		t.Fatalf("GetCursor after SetCursor error: %v", err)
	}
	if cur.Seq != 1 {
		t.Fatalf("cursor Seq = %d, want 1", cur.Seq)
	}

	unread, err := st.Events().List(ctx, EventFilter{
		RunID:    "run-1",
		AfterSeq: cur.Seq,
	})
	if err != nil {
		t.Fatalf("List(AfterSeq=1) error: %v", err)
	}
	if len(unread) != 1 || unread[0].ID != ev2.ID || !unread[0].Urgent {
		t.Fatalf("unread events = %+v, want [ev2]", unread)
	}
}

func testThreadsTurnsAndItems(t *testing.T, factory Factory) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC))
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	th, err := st.Threads().StartThread(ctx, Thread{
		ID:    "th-1",
		RunID: "run-1",
		Role:  ThreadRoleConcierge,
		Goal:  "Answer owner questions",
	})
	if err != nil {
		t.Fatalf("StartThread error: %v", err)
	}
	if th.Status != ThreadStatusIdle {
		t.Fatalf("initial thread Status = %q, want idle", th.Status)
	}

	if err := st.Threads().SteerTurn(ctx, "th-1", "", "Check lane core first"); err != nil {
		t.Fatalf("SteerTurn(queued) error: %v", err)
	}

	turn1, err := st.Threads().StartTurn(ctx, Turn{
		ID:       "turn-1",
		ThreadID: "th-1",
		Input:    "Where are we?",
	})
	if err != nil {
		t.Fatalf("StartTurn(turn-1) error: %v", err)
	}
	if turn1.Seq != 1 || len(turn1.Steers) != 1 || turn1.Steers[0] != "Check lane core first" {
		t.Fatalf("turn1 = %+v, expected drained QueuedSteers", turn1)
	}

	if err := st.Threads().SteerTurn(ctx, "th-1", "turn-1", "Also check CI"); err != nil {
		t.Fatalf("SteerTurn(running) error: %v", err)
	}

	if _, err := st.Threads().AppendItem(ctx, Item{
		ID:       "it-1",
		ThreadID: "th-1",
		TurnID:   "turn-1",
		Kind:     ItemKindAssistantMessage,
		Content:  "Lane core is in swe.",
	}); err != nil {
		t.Fatalf("AppendItem(it-1) error: %v", err)
	}

	if err := st.Threads().CompleteTurn(ctx, "th-1", "turn-1", TurnStatusCompleted, model.Usage{
		InputTokens:  1000,
		OutputTokens: 200,
		CostUSD:      0.005,
	}, ""); err != nil {
		t.Fatalf("CompleteTurn(turn-1) error: %v", err)
	}

	if _, err := st.Threads().StartTurn(ctx, Turn{
		ID:       "turn-2",
		ThreadID: "th-1",
		Input:    "Second turn on parent",
	}); err != nil {
		t.Fatalf("StartTurn(turn-2) error: %v", err)
	}
	if _, err := st.Threads().AppendItem(ctx, Item{
		ID:       "it-2",
		ThreadID: "th-1",
		TurnID:   "turn-2",
		Kind:     ItemKindUserMessage,
		Content:  "Turn 2 message",
	}); err != nil {
		t.Fatalf("AppendItem(it-2) error: %v", err)
	}

	forked, err := st.Threads().ForkThread(ctx, "th-1", "th-fork", "turn-1")
	if err != nil {
		t.Fatalf("ForkThread error: %v", err)
	}
	if forked.ParentThreadID != "th-1" || forked.ForkedAtTurnID != "turn-1" {
		t.Fatalf("forked thread = %+v", forked)
	}

	forkTurns, err := st.Threads().ListTurns(ctx, "th-fork")
	if err != nil {
		t.Fatalf("ListTurns(th-fork) error: %v", err)
	}
	if len(forkTurns) != 1 || forkTurns[0].ID != "turn-1" {
		t.Fatalf("forkTurns = %+v, want only turn-1", forkTurns)
	}

	forkItems, err := st.Threads().ListItems(ctx, "th-fork", "", 0, 0)
	if err != nil {
		t.Fatalf("ListItems(th-fork) error: %v", err)
	}
	if len(forkItems) != 2 { // steer item + assistant message from turn-1
		t.Fatalf("len(forkItems) = %d, want 2: %+v", len(forkItems), forkItems)
	}

	parentThread, err := st.Threads().GetThread(ctx, "th-1")
	if err != nil {
		t.Fatalf("GetThread(th-1) error: %v", err)
	}
	if parentThread.InputTokens != 1000 || parentThread.OutputTokens != 200 {
		t.Fatalf("parentThread token rollup = (%d, %d), want (1000, 200)", parentThread.InputTokens, parentThread.OutputTokens)
	}
}

func testRecipes(t *testing.T, factory Factory) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC))
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()

	def := recipe.Default()
	def.RunID = "run-1"
	def.Version = 0 // let store assign version 1
	v1, err := st.Recipes().Save(ctx, def)
	if err != nil {
		t.Fatalf("Save(v1) error: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("v1.Version = %d, want 1", v1.Version)
	}

	v2In := def.Clone()
	v2In.Source = "Skip clean phase: swe -> review -> integrate"
	v2In.Phases = []recipe.Phase{def.Phases[0], def.Phases[2], def.Phases[3]}
	v2In.Transitions = []recipe.Transition{
		{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
		{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
	}
	v2, err := st.Recipes().Save(ctx, v2In)
	if err != nil {
		t.Fatalf("Save(v2) error: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("v2.Version = %d, want 2", v2.Version)
	}

	gotV1, err := st.Recipes().Get(ctx, "run-1", recipe.DefaultName, 1)
	if err != nil {
		t.Fatalf("Get(v1) error: %v", err)
	}
	if len(gotV1.Phases) != 4 {
		t.Fatalf("len(gotV1.Phases) = %d, want 4", len(gotV1.Phases))
	}

	gotLatest, err := st.Recipes().Get(ctx, "run-1", recipe.DefaultName, 0)
	if err != nil {
		t.Fatalf("Get(latest) error: %v", err)
	}
	if gotLatest.Version != 2 || len(gotLatest.Phases) != 3 {
		t.Fatalf("gotLatest = v%d (%d phases), want v2 (3 phases)", gotLatest.Version, len(gotLatest.Phases))
	}

	versions, err := st.Recipes().ListVersions(ctx, "run-1", recipe.DefaultName)
	if err != nil {
		t.Fatalf("ListVersions error: %v", err)
	}
	if len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("ListVersions = %+v", versions)
	}

	latestAll, err := st.Recipes().ListLatest(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListLatest error: %v", err)
	}
	if len(latestAll) != 1 || latestAll[0].Version != 2 {
		t.Fatalf("ListLatest = %+v", latestAll)
	}
}

func testMemoriesAndNotes(t *testing.T, factory Factory) {
	clk := clock.NewFakeClock(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC))
	st := factory(t, clk)
	defer st.Close()
	ctx := t.Context()
	m := MemoryEntry{ID: "correction", Name: "correction", Description: "Use scoped checks", Type: "rule", Scope: "run", RunID: "run-1", Source: "Use scoped checks, please", Body: "Why: fast feedback. How: test the changed package.", ProposedBy: "owner"}
	if _, err := st.Memories().Put(ctx, m); err != nil {
		t.Fatal(err)
	}
	invalid := m
	invalid.RunID = ""
	if _, err := st.Memories().Put(ctx, invalid); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("run memory without run_id = %v", err)
	}
	invalid = m
	invalid.Scope = "owner"
	if _, err := st.Memories().Put(ctx, invalid); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("owner memory with run_id = %v", err)
	}
	invalid = m
	invalid.Type = "unknown"
	if _, err := st.Memories().Put(ctx, invalid); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid memory type = %v", err)
	}
	clk.Advance(time.Second)
	m.Body = "Why: fast feedback. How: start with the smallest case."
	updated, err := st.Memories().Put(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.CreatedAt.Equal(clk.Now().Add(-time.Second)) || !updated.UpdatedAt.Equal(clk.Now()) {
		t.Fatalf("updated timestamps = %v/%v", updated.CreatedAt, updated.UpdatedAt)
	}
	got, err := st.Memories().Get(ctx, "run", m.ID)
	if err != nil || got.Body != m.Body || got.Source != m.Source {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if _, err := st.Memories().Get(ctx, "owner", m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong scope read = %v", err)
	}
	if err := st.Memories().Delete(ctx, "owner", m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong scope delete = %v", err)
	}
	memories, err := st.Memories().List(ctx, MemoryFilter{RunID: "other-run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != len(StarterMemories()) {
		t.Fatalf("other run memories = %d, want only starters", len(memories))
	}
	for i := 0; i < 3; i++ {
		if _, err := st.Notes().Append(ctx, RunNote{RunID: "run-1", TaskID: "task-1", Text: "phase done"}); err != nil {
			t.Fatalf("fixed-clock append %d: %v", i, err)
		}
	}
	notes, err := st.Notes().List(ctx, NoteFilter{RunID: "run-1", TaskID: "task-1"})
	if err != nil || len(notes) != 3 {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	ids := make(map[string]bool)
	for _, n := range notes {
		if ids[n.ID] {
			t.Fatalf("duplicate note ID %s", n.ID)
		}
		ids[n.ID] = true
	}
	if _, err := st.Notes().Append(ctx, RunNote{ID: notes[0].ID, RunID: "other-run", Text: "duplicate"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate note error = %v", err)
	}
}
