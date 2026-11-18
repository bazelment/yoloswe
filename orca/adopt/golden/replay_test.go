package golden

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

// The same synthetic run participates in both the G60 preview corpus and this
// multi-tick replay through the adopted controller, dispatcher and G61 recorder.
func TestG62AdoptedFixtureReplay(t *testing.T) {
	ctx := t.Context()
	source := filepath.Join("testdata", "g62_adopted")
	dir := filepath.Join(t.TempDir(), "g62_adopted")
	if err := fs.WalkDir(os.DirFS(source), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, path)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(filepath.Join(source, path))
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	clk := clock.NewFakeClock(now)
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	out := t.TempDir()
	rec, err := adopt.NewActionRecorder(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	drv := lane.NewFakeDriver()
	drv.AgentStates["held-terminal"] = "working"
	drv.AgentStates["draft-terminal"] = "working"
	drv.AgentStates["review-terminal"] = "unknown"
	drv.AgentStates["moving-swe"] = "exited"
	drv.AgentStates["moving-review"] = "working"
	drv.AgentStates["moving-integrate"] = "exited"
	end := model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock("Hold host decisions."))}
	fake := model.NewFakeClient()
	// Script deliberately proposes unsafe actions. Deterministic guards must
	// reject these even when a strong model makes the wrong call.
	for _, call := range []struct{ tool, args string }{
		{dispatcher.ToolLaneStop, `{"lane_id":"done-override","reason":"stale review"}`},
		{dispatcher.ToolLaneSteer, `{"lane_id":"held-review","note":"proceed to integrate"}`},
		{dispatcher.ToolLaneRetry, `{"lane_id":"parked-draft","phase":"swe","note":"rework"}`},
		{dispatcher.ToolLaneSpawn, `{"item_id":"planned-no-brief-a","brief":"invented funding"}`},
		{dispatcher.ToolLaneSteer, `{"lane_id":"moving","note":"keep coding"}`},
	} {
		fake.Enqueue(model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(model.NewToolUseBlock("unsafe", call.tool, json.RawMessage(call.args)))}, nil)
		fake.Enqueue(end, nil)
	}
	for range 20 {
		fake.Enqueue(end, nil)
	}
	var disp *dispatcher.Dispatcher
	bg, cancel := context.WithCancel(ctx)
	cancel()
	d := adopt.NewControllers(bg, st, drv, clk, func(_ string, _ func(context.Context, string, string, string) error) controller.Judge {
		disp = dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
		return disp
	})
	d.Shadow, d.Recorder = true, rec
	r, err := adopt.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Repo = "fixture/repo"
	if err := st.Runs().Create(ctx, store.Run{ID: r.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.Runs().UpdateSettings(ctx, r.ID, store.RunSettings{Paused: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, r); err != nil {
		t.Fatal(err)
	}
	if item, err := st.Backlog().Get(ctx, r.ID, "planned-no-brief-a"); err != nil || strings.TrimSpace(item.Description) != "" {
		t.Fatalf("unfunded fixture unexpectedly has mission brief: item=%+v err=%v", item, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for d.Active(r.ID) {
		if time.Now().After(deadline) {
			t.Fatal("canceled controller did not quiesce")
		}
		runtime.Gosched()
	}
	t.Cleanup(func() { _ = d.Stop(context.Background(), r); _ = disp.Close() })
	if err := st.Runs().UpdateSettings(ctx, r.ID, store.RunSettings{}); err != nil {
		t.Fatal(err)
	}
	// Start with 60 minutes of quiet review and unknown health. It may never be
	// replaced, even after enough ticks cross the former 45-minute timeout.
	slow, err := st.Lanes().Get(ctx, r.ID, "review-60m")
	if err != nil {
		t.Fatal(err)
	}
	slow.LastEventAt = now.Add(-time.Hour)
	if err := st.Lanes().Upsert(ctx, *slow); err != nil {
		t.Fatal(err)
	}
	for tick := 0; tick < 5; tick++ {
		if err := d.Step(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := disp.Dispatch(ctx, r.ID, []store.Event{{ID: "script", Kind: store.EventTimer, Summary: "synthetic tick"}}); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Minute)
	}
	// The host changes phase twice and adds funded work during adoption.
	mutateState(t, dir, func(state map[string]any) {
		for _, raw := range state["tasks"].([]any) {
			row := raw.(map[string]any)
			if row["id"] == "moving" {
				row["phase"] = "review"
				row["terminal_state"] = "working"
				row["terminal"] = "moving-review"
			}
		}
	})
	if err := d.Step(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	moving, err := st.Lanes().Get(ctx, r.ID, "moving")
	if err != nil || moving.Phase != "review" || moving.TerminalID != "moving-review" {
		t.Fatalf("host phase not followed: %+v (%v)", moving, err)
	}
	mutateState(t, dir, func(state map[string]any) {
		for _, raw := range state["tasks"].([]any) {
			row := raw.(map[string]any)
			if row["id"] == "moving" {
				row["phase"] = "integrate"
				row["terminal_state"] = "exited"
				row["terminal"] = "moving-integrate"
			}
		}
		state["tasks"] = append(state["tasks"].([]any), map[string]any{"id": "funded-mid-window", "title": "Funded mid-window", "status": "planned", "brief": "Implement the funded synthetic task", "depends_on": []string{"done-override"}})
	})
	if err := d.Step(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	moving, err = st.Lanes().Get(ctx, r.ID, "moving")
	if err != nil || moving.Phase != "integrate" || moving.TerminalID != "moving-integrate" {
		t.Fatalf("host integrate not followed: %+v (%v)", moving, err)
	}
	// A clean handback must start review, retaining the standing host asks.
	payload, _ := json.Marshal(lane.PhaseReport{Phase: "clean", Round: 1, Outcome: lane.OutcomeDone, TerminalID: "clean-terminal"})
	if _, err := st.Events().Append(ctx, store.Event{ID: "clean-handback", RunID: r.ID, LaneID: "clean-report", Kind: store.EventLaneReport, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := d.Step(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(out, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var got []string
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		var action adopt.Action
		if err := json.Unmarshal(scan.Bytes(), &action); err != nil {
			t.Fatal(err)
		}
		switch action.Kind {
		case "stop", "spawn", "start_phase", "steer":
			got = append(got, action.Lane+":"+action.Kind)
			if action.Lane == "clean-report" && action.Kind == "start_phase" {
				var req lane.StartPhaseRequest
				if err := json.Unmarshal(action.Arguments, &req); err != nil {
					t.Fatal(err)
				}
				if req.Phase != "review" {
					t.Fatalf("clean skipped review: %+v", req)
				}
				for _, ask := range []string{"merge after #7002", "required eviction run", "eviction-canary"} {
					if !strings.Contains(req.Brief, ask) {
						t.Errorf("review brief dropped %q: %s", ask, req.Brief)
					}
				}
			}
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	wantBytes, err := fs.ReadFile(os.DirFS(source), "actions.golden.json")
	if err != nil {
		t.Logf("Observed: recorded actions %v", got)
		t.Fatal(err)
	}
	var want []string
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded actions mismatch\n got: %v\nwant: %v", got, want)
	}
	for _, laneID := range []string{"done-expired-wait", "done-override", "finish-first-other-pr", "clean-done"} {
		dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: r.ID, ActiveOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range dirs {
			if dir.Scope == laneID && strings.HasPrefix(dir.Ref, controller.NamedWaitRefPrefix) {
				t.Errorf("terminal lane %s imported named wait: %+v", laneID, dir)
			}
		}
		events, err := st.Events().List(ctx, store.EventFilter{RunID: r.ID, LaneID: laneID})
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if ev.Class == store.EventClassJudgment {
				t.Errorf("terminal lane %s raised attention: %+v", laneID, ev)
			}
		}
	}
	t.Logf("Observed: 8 synthetic adopted ticks -> recorded actions %v", got)
}

func mutateState(t *testing.T, dir string, fn func(map[string]any)) {
	t.Helper()
	p := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	fn(state)
	b, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}
