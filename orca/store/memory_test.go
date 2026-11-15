package store_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestMemoryStoreContract(t *testing.T) {
	t.Parallel()

	store.RunContractTests(t, func(_ *testing.T, clk clock.Clock) store.Store {
		return store.NewMemoryStore(clk)
	})
}

func TestMemoryStoreConcurrency(t *testing.T) {
	t.Parallel()

	st := store.NewMemoryStore(nil)
	defer st.Close()
	ctx := t.Context()

	if err := st.Runs().Create(ctx, store.Run{ID: "run-conc", Objective: "concurrency test"}); err != nil {
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

func TestIsNoMergeTextAndHelpers(t *testing.T) {
	t.Parallel()

	positives := []string{
		"Do not merge anything; this is a measurement/report task",
		"Report-only benchmark analysis",
		"Run audit without merging",
		"Do not merge; this is a measurement item",
		"no-merge investigation lane",
	}
	for _, text := range positives {
		if !store.IsNoMergeText(text) {
			t.Errorf("IsNoMergeText(%q) = false, want true", text)
		}
	}

	negatives := []string{
		"Open a PR; do not merge.",
		"Push and open a draft PR, do not merge",
		"Keep PRs ready; do not merge.",
		"Rebase cleanly without merge conflicts",
		"Avoid merge commits when rebasing",
		"Ask before merging if migration fails",
		"Don't merge if CI is red",
		"Implement feature X and merge when green",
	}
	for _, text := range negatives {
		if store.IsNoMergeText(text) {
			t.Errorf("IsNoMergeText(%q) = true, want false", text)
		}
	}

	// Once a persisted backlog item has NoMerge explicitly cleared to false via Update,
	// IsNoMergeLane and IsNoMergeSpawn must respect item.NoMerge == false.
	clk := clock.NewFakeClock(clock.NewRealClock().Now())
	st := store.NewMemoryStore(clk)
	defer st.Close()
	ctx := t.Context()
	if err := st.Runs().Create(ctx, store.Run{ID: "run-nm", Objective: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:          "m4-metrics",
		RunID:       "run-nm",
		Title:       "Measurement/report task",
		Description: "Do not merge anything; this is a measurement/report task",
		Priority:    store.PriorityP0,
		Status:      store.BacklogOpen,
	}); err != nil {
		t.Fatal(err)
	}
	added, err := st.Backlog().Get(ctx, "run-nm", "m4-metrics")
	if err != nil || added == nil || !added.NoMerge {
		t.Fatalf("added = %+v, err = %v; want NoMerge=true on Add", added, err)
	}
	added.NoMerge = false
	if err := st.Backlog().Update(ctx, *added, "dispatcher", "clear no_merge"); err != nil {
		t.Fatal(err)
	}
	clearedItem, err := st.Backlog().Get(ctx, "run-nm", "m4-metrics")
	if err != nil || clearedItem == nil || clearedItem.NoMerge {
		t.Fatalf("clearedItem = %+v, err = %v; want NoMerge=false after Update", clearedItem, err)
	}
	if store.IsNoMergeLane(store.Lane{ID: "m4-metrics", ItemID: "m4-metrics", Title: "M4 metrics"}, clearedItem) {
		t.Fatal("IsNoMergeLane should be false when existing item has NoMerge=false and lane title is normal")
	}
	if store.IsNoMergeSpawn(false, clearedItem, "Implement M4 metrics", "") {
		t.Fatal("IsNoMergeSpawn should be false when existing item has NoMerge=false and spawn params are normal")
	}
	if !store.IsNoMergeSpawn(true, clearedItem, "Implement M4 metrics", "") {
		t.Fatal("IsNoMergeSpawn should be true when noMergeParam=true")
	}
}

func TestNormalizeDirectiveIDAndActiveDirectiveListing(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"[note-7]":        "note-7",
		"[[note-7]]":      "note-7",
		" [chat-123] ":    "chat-123",
		"dir-concierge-1": "dir-concierge-1",
		"   ":             "",
	}
	for in, want := range cases {
		if got := store.NormalizeDirectiveID(in); got != want {
			t.Errorf("NormalizeDirectiveID(%q) = %q, want %q", in, got, want)
		}
	}

	clk := clock.NewFakeClock(clock.NewRealClock().Now())
	st := store.NewMemoryStore(clk)
	defer st.Close()
	ctx := t.Context()

	if err := st.Runs().Create(ctx, store.Run{ID: "run-1", Objective: "test"}); err != nil {
		t.Fatal(err)
	}

	// Repo-scoped standalone memory (RunID == ""), run-scoped rule memory, and starter-set rule memory.
	for _, mem := range []store.MemoryEntry{
		{
			ID:         "repo-rule-1",
			Name:       "Repo rule",
			Type:       "rule",
			Scope:      "repo",
			RunID:      "",
			Source:     "owner repo rule",
			Body:       "Always run lint before pushing",
			ProposedBy: "owner",
		},
		{
			ID:         "dir-run-1",
			Name:       "Run hold rule",
			Type:       "rule",
			Scope:      "run",
			RunID:      "run-1",
			Source:     "owner run rule",
			Body:       "Hold m4-metrics",
			ProposedBy: "owner",
		},
		{
			ID:         "starter-1",
			Name:       "Starter rule",
			Type:       "rule",
			Scope:      "run",
			RunID:      "run-1",
			Source:     store.StarterSetMemorySource,
			Body:       "Starter rule",
			ProposedBy: "owner",
		},
	} {
		if _, err := st.Memories().Put(ctx, mem); err != nil {
			t.Fatalf("Memories.Put(%s): %v", mem.ID, err)
		}
	}

	dirs, err := store.ListActiveDirectives(ctx, st, "run-1", clk.Now())
	if err != nil {
		t.Fatalf("ListActiveDirectives: %v", err)
	}
	gotIDs := make(map[string]store.Directive, len(dirs))
	for _, d := range dirs {
		gotIDs[d.ID] = d
	}
	if _, ok := gotIDs["repo-rule-1"]; ok {
		t.Fatalf("repo-scoped standalone memory repo-rule-1 must not be synthesized as a run directive, got %+v", dirs)
	}
	if _, ok := gotIDs["dir-run-1"]; !ok {
		t.Fatalf("expected run rule memory dir-run-1 in active directives, got %+v", dirs)
	}
	if _, ok := gotIDs["starter-1"]; ok {
		t.Fatalf("starter-set rule memory should be excluded from active directives, got %+v", dirs)
	}

	// If dir-run-1 has a revoked directive entry in Directives(), both ListActiveDirectives
	// and FilterActiveMemories must exclude it.
	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "dir-run-1",
		RunID:  "run-1",
		Kind:   store.DirectiveStanding,
		Source: store.DirectiveSourceConcierge,
		Scope:  "run",
		Text:   "Hold m4-metrics",
		Status: store.DirectiveRevoked,
	}); err != nil {
		t.Fatal(err)
	}

	dirsAfter, err := store.ListActiveDirectives(ctx, st, "run-1", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirsAfter {
		if d.ID == "dir-run-1" {
			t.Fatalf("revoked directive dir-run-1 still returned by ListActiveDirectives: %+v", dirsAfter)
		}
	}
	allMems, err := st.Memories().List(ctx, store.MemoryFilter{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	filteredMems := store.FilterActiveMemories(ctx, st, "run-1", allMems, clk.Now())
	for _, m := range filteredMems {
		if m.ID == "dir-run-1" {
			t.Fatalf("revoked directive memory dir-run-1 still returned by FilterActiveMemories: %+v", filteredMems)
		}
	}
}
