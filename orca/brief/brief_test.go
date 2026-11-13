package brief_test

import (
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/brief"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestMemoryDriver_FreshPhasesUseCurrentRunMemories(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := store.NewMemoryStore(nil)
	defer st.Close()
	for _, runID := range []string{"run-a", "run-b"} {
		if err := st.Runs().Create(ctx, store.Run{ID: runID}); err != nil {
			t.Fatal(err)
		}
	}
	save := func(id, scope, runID, body string) {
		t.Helper()
		if _, err := st.Memories().Put(ctx, store.MemoryEntry{ID: id, Name: id, Description: body, Type: "rule", Scope: scope, RunID: runID, Body: body, Source: "owner correction: " + body, ProposedBy: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	save("memory-owner", "owner", "", "Owner guidance")
	save("memory-repo", "repo", "", "Repository guidance")
	save("memory-run", "run", "run-a", "First instruction")
	save("memory-other", "run", "run-b", "OTHER RUN PRIVATE")
	raw := lane.NewFakeDriver()
	driver := brief.WithMemories(st, raw)
	if _, err := driver.SpawnSession(ctx, lane.SpawnRequest{RunID: "run-a", LaneID: "lane-a", Brief: "Implement task"}); err != nil {
		t.Fatal(err)
	}
	first := raw.Spawns[0].Brief
	for _, want := range []string{"Implement task", "- [memory-owner] memory-owner: Owner guidance", "- [memory-repo] memory-repo: Repository guidance"} {
		if !strings.Contains(first, want) {
			t.Fatalf("spawn brief missing %q: %s", want, first)
		}
	}
	if strings.Contains(first, "OTHER RUN PRIVATE") {
		t.Fatal("another run leaked into spawn brief")
	}
	save("memory-run", "run", "run-a", "Updated instruction")
	if _, err := driver.StartPhaseTerminal(ctx, lane.StartPhaseRequest{RunID: "run-a", LaneID: "lane-a", Brief: "/pr-polish-nebula\n\nGoal: Review task"}); err != nil {
		t.Fatal(err)
	}
	next := raw.PhaseStarts[0].Brief
	if next != "/pr-polish-nebula\n\nGoal: Review task" {
		t.Fatalf("next phase has stale or foreign guidance: %s", next)
	}
	save("memory-too-large", "run", "run-a", strings.Repeat("x", brief.DefaultMaxBytes))
	if _, err := driver.SpawnSession(ctx, lane.SpawnRequest{RunID: "run-a", LaneID: "too-large", Brief: "Task"}); err != nil {
		t.Fatalf("oversize memory should be truncated, got %v", err)
	}
	if len(raw.Spawns) != 2 || len(raw.Spawns[1].Brief) > brief.DefaultMaxBytes {
		t.Fatal("oversize worker should launch with a capped brief")
	}
}
