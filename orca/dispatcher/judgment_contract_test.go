package dispatcher_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

func toolUseResponse(calls ...model.ToolCall) model.Response {
	blocks := make([]model.ContentBlock, len(calls))
	for i, c := range calls {
		blocks[i] = model.NewToolUseBlock(c.ID, c.Name, c.Input)
	}
	return model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(blocks...)}
}

func TestOwnerAnswerVetterRequiresOpenAsk(t *testing.T) {
	clk, st, _ := setupTestEnv(t)
	view, err := dispatcher.LoadStateView(t.Context(), st, clk, "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	view.OwnerAsks["open-1"] = controller.OwnerAsk{ID: "open-1"}
	invs := dispatcher.DefaultInvariants()
	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{name: "unknown", id: "missing", want: "is not open"},
		{name: "closed", id: "closed", want: "is not open"},
		{name: "open", id: "open-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			act, err := dispatcher.ParseAction("call", dispatcher.ToolOwnerAnswer, json.RawMessage(`{"ask_id":"`+tc.id+`","answer":"go"}`))
			if err != nil {
				t.Fatal(err)
			}
			got := dispatcher.VetAction(view, act, invs)
			if tc.want == "" {
				if got != nil {
					t.Fatal(got)
				}
			} else if got == nil || !strings.Contains(got.Error(), tc.want) {
				t.Fatalf("VetAction=%v, want %q", got, tc.want)
			}
		})
	}
}

func endTurnResponse(text string) model.Response {
	return model.Response{StopReason: model.StopReasonEndTurn, Message: model.NewAssistantMessage(model.NewTextBlock(text))}
}

func toolNames(req model.Request) []string {
	names := make([]string, len(req.Tools))
	for i := range req.Tools {
		names[i] = req.Tools[i].Name
	}
	return names
}

// An owner order whose instruction comes after a long preamble must survive the
// snapshot; machine events stay under the tight cap.
func TestSnapshot_OwnerOriginSummariesKeepTheInstruction(t *testing.T) {
	t.Parallel()
	clk, st, _ := setupTestEnv(t)
	ctx := t.Context()

	long := strings.Repeat("Preamble about why this matters. ", 25) + "ASK: fund one P1 lane for the analysis."
	if len(long) <= dispatcher.DefaultMaxSummaryBytes {
		t.Fatalf("fixture must exceed the machine-event cap, len=%d", len(long))
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{ID: "item-ready", RunID: "run-1", Title: "ready", Status: store.BacklogOpen, Priority: store.PriorityP1}); err != nil {
		t.Fatalf("Add item-ready: %v", err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{ID: "item-blocked-on-ready", RunID: "run-1", Title: "waits", Status: store.BacklogOpen, Priority: store.PriorityP1, DependsOn: []string{"item-ready"}}); err != nil {
		t.Fatalf("Add dependent: %v", err)
	}

	events := []store.Event{
		{ID: "ev-owner", Seq: 1, Kind: store.EventOwnerMessage, Summary: long},
		{ID: "ev-ci", Seq: 2, Kind: store.EventCI, LaneID: "lane-a", Summary: long},
	}
	snap, err := dispatcher.BuildSnapshot(ctx, st, clk, "run-1", events, dispatcher.SnapshotConfig{})
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}

	ownerLine, ciLine := "", ""
	for _, line := range strings.Split(snap.Text, "\n") {
		switch {
		case strings.Contains(line, "**ev-owner**"):
			ownerLine = line
		case strings.Contains(line, "**ev-ci**"):
			ciLine = line
		}
	}
	if !strings.Contains(ownerLine, "ASK: fund one P1 lane") {
		t.Errorf("owner event lost its instruction: %q", ownerLine)
	}
	if strings.Contains(ciLine, "ASK: fund one P1 lane") || !strings.Contains(ciLine, "[truncated]") {
		t.Errorf("machine event should still be cut at the summary cap: %q", ciLine)
	}
	if strings.Contains(ownerLine, "lane=") {
		t.Errorf("run-level event must not carry a lane field: %q", ownerLine)
	}
	if !strings.Contains(ciLine, "lane=lane-a") {
		t.Errorf("lane event lost its lane: %q", ciLine)
	}
	if !strings.Contains(snap.Text, "**item-ready** [p1 #0 open ready]") && !strings.Contains(snap.Text, "**item-ready** [p1 #1 open ready]") {
		t.Errorf("open item with no dependencies is not marked ready:\n%s", snap.Text)
	}
	if strings.Contains(snap.Text, "**item-blocked-on-ready** [p1 #0 open ready]") || strings.Contains(snap.Text, "**item-blocked-on-ready** [p1 #1 open ready]") {
		t.Errorf("item with an unfinished dependency must not be marked ready:\n%s", snap.Text)
	}
}

// read.* tools only expand a lane's excerpt, so a batch that names no lane is
// not offered them.
func TestDispatch_ReadToolsOnlyOfferedWithALaneEvent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		laneID    string
		wantReads bool
	}{
		{name: "RunLevelBatch", laneID: "", wantReads: false},
		{name: "LaneBatch", laneID: "lane-a", wantReads: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk, st, drv := setupTestEnv(t)
			fake := model.NewFakeClient(endTurnResponse("nothing to do"))
			d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
			ev, err := st.Events().Append(t.Context(), store.Event{RunID: "run-1", Kind: store.EventMail, Class: store.EventClassJudgment, LaneID: tc.laneID, Summary: "hello"})
			if err != nil {
				t.Fatalf("Append event: %v", err)
			}
			if _, err := d.Dispatch(t.Context(), "run-1", []store.Event{ev}); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			var hasRead, hasSpawn bool
			for _, n := range toolNames(fake.Requests[0]) {
				hasRead = hasRead || strings.HasPrefix(n, "read.")
				hasSpawn = hasSpawn || n == dispatcher.ToolLaneSpawn
			}
			if hasRead != tc.wantReads || !hasSpawn {
				t.Errorf("tools = %v, wantReads=%v", toolNames(fake.Requests[0]), tc.wantReads)
			}
			if hasSnapNote := strings.Contains(fake.Requests[0].Messages[0].Text(), "read.* tools are not offered"); hasSnapNote == tc.wantReads {
				t.Errorf("snapshot read-tool note present=%v, want %v", hasSnapNote, !tc.wantReads)
			}
		})
	}
}

// A turn cut off at the output limit decided nothing: it is re-asked, and the
// recorded tool call from the second attempt is applied.
func TestDispatch_TruncatedTurnIsReAsked(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)

	cut := model.Response{StopReason: model.StopReasonMaxTokens, Message: model.NewAssistantMessage(model.NewTextBlock("## Analysis\nI need to parse this carefully ..."))}
	note := toolUseResponse(model.ToolCall{ID: "t1", Name: dispatcher.ToolDirectiveNote, Input: json.RawMessage(`{"fact":"owner ruling: option c","reason":"owner decision"}`)})
	fake := model.NewFakeClient(cut, note, endTurnResponse("recorded"))
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)

	ev, err := st.Events().Append(t.Context(), store.Event{RunID: "run-1", Kind: store.EventOwnerMessage, Class: store.EventClassJudgment, Summary: "OWNER DECISION option (c)"})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}
	res, err := d.Dispatch(t.Context(), "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolDirectiveNote {
		t.Fatalf("accepted = %+v, want one directive.note", res.Accepted)
	}
	if len(fake.Requests) != 3 {
		t.Fatalf("requests = %d, want 3 (cut, retry, final)", len(fake.Requests))
	}
	retry := fake.Requests[1].Messages
	if last := retry[len(retry)-1]; last.Role != model.RoleUser || !strings.Contains(last.Text(), "cut off") {
		t.Errorf("retry must end with the cut-off nudge, got %s %q", last.Role, last.Text())
	}
	if retry[len(retry)-2].Role != model.RoleAssistant {
		t.Errorf("retry must keep user/assistant alternation")
	}
}

// Truncation retries are bounded.
func TestDispatch_TruncationRetriesAreBounded(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)

	cut := model.Response{StopReason: model.StopReasonMaxTokens, Message: model.NewAssistantMessage(model.NewTextBlock("..."))}
	fake := model.NewFakeClient(cut, cut, cut, cut, cut)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
	ev, err := st.Events().Append(t.Context(), store.Event{RunID: "run-1", Kind: store.EventMail, Class: store.EventClassJudgment, Summary: "x"})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}
	res, err := d.Dispatch(t.Context(), "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(fake.Requests) != 3 || len(res.Accepted) != 0 {
		t.Fatalf("requests=%d accepted=%d, want 3 requests (1 + 2 retries) and no action", len(fake.Requests), len(res.Accepted))
	}
}

// Decision cards state a default and a deadline in their own words: the stated
// default becomes an option (so default-is-an-option still holds) and the
// deadline forms owners actually write are understood.
func TestParseAction_OwnerAskCardShapes(t *testing.T) {
	t.Parallel()
	clk, st, _ := setupTestEnv(t)

	view, err := dispatcher.LoadStateView(t.Context(), st, clk, "run-1", 0)
	if err != nil {
		t.Fatalf("LoadStateView: %v", err)
	}
	invs := dispatcher.DefaultInvariants()

	cases := []struct {
		name        string
		args        string
		wantDefault string
		wantErr     string
		wantOptions int
	}{
		{name: "DefaultOutsideOptionsBecomesOption", args: `{"question":"q","options":["a","b"],"default":"Design only","deadline":"30m"}`, wantDefault: "Design only", wantOptions: 3},
		{name: "DefaultMatchedCaseInsensitively", args: `{"question":"q","options":["Alpha","Beta"],"default":" alpha ","deadline":"30m"}`, wantDefault: "Alpha", wantOptions: 2},
		{name: "PaddedOptionIsTrimmedSoApplyAcceptsIt", args: `{"question":"q","options":[" Alpha ","Beta"],"default":"alpha","deadline":"30m"}`, wantDefault: "Alpha", wantOptions: 2},
		{name: "ClockTimeDeadline", args: `{"question":"q","options":["a","b"],"default":"a","deadline":"05:30Z"}`, wantDefault: "a", wantOptions: 2},
		{name: "DayDeadline", args: `{"question":"q","options":["a","b"],"default":"a","deadline":"2d"}`, wantDefault: "a", wantOptions: 2},
		{name: "NoOptionsStillRejected", args: `{"question":"q","default":"a","deadline":"30m"}`, wantErr: "options are required"},
		{name: "EmptyDefaultStillRejected", args: `{"question":"q","options":["a"],"default":" ","deadline":"30m"}`, wantErr: "default option is required"},
		{name: "GarbageDeadlineStillRejected", args: `{"question":"q","options":["a"],"default":"a","deadline":"soonish"}`, wantErr: "invalid deadline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			act, err := dispatcher.ParseAction("id", dispatcher.ToolOwnerAsk, json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("ParseAction: %v", err)
			}
			vErr := dispatcher.VetAction(view, act, invs)
			if tc.wantErr != "" {
				if vErr == nil || !strings.Contains(vErr.Error(), tc.wantErr) {
					t.Fatalf("VetAction err = %v, want %q", vErr, tc.wantErr)
				}
				return
			}
			if vErr != nil {
				t.Fatalf("VetAction: %v", vErr)
			}
			// applyOwnerAsk trims Default and controller.OwnerAsk.Validate then needs
			// an exact match among the options.
			if !slices.Contains(act.OwnerAsk.Options, strings.TrimSpace(act.OwnerAsk.Default)) {
				t.Fatalf("default %q is not exactly one of options %q", act.OwnerAsk.Default, act.OwnerAsk.Options)
			}
			if act.OwnerAsk.Default != tc.wantDefault || len(act.OwnerAsk.Options) != tc.wantOptions {
				t.Fatalf("default=%q options=%v, want default %q and %d options", act.OwnerAsk.Default, act.OwnerAsk.Options, tc.wantDefault, tc.wantOptions)
			}
		})
	}
}

// Spawning owns the in_progress transition: a hand-set in_progress is rejected
// with a message that names lane.spawn, and a spawn marks the item itself.
func TestBacklogUpdate_InProgressIsOwnedBySpawn(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()
	if err := st.Backlog().Add(ctx, store.BacklogItem{ID: "item-x", RunID: "run-1", Title: "x", Status: store.BacklogOpen, Priority: store.PriorityP1}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t1", Name: dispatcher.ToolBacklogUpdate, Input: json.RawMessage(`{"id":"item-x","status":"in_progress","reason":"claim"}`)},
			model.ToolCall{ID: "t2", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"item-x","brief":"do x","reason":"ordered"}`)},
		),
		endTurnResponse("done"),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
	ev, err := st.Events().Append(ctx, store.Event{RunID: "run-1", Kind: store.EventOwnerMessage, Class: store.EventClassJudgment, Summary: "start x"})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0], "lane.spawn") {
		t.Fatalf("rejected = %q, want one rejection naming lane.spawn", res.Rejected)
	}
	if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolLaneSpawn {
		t.Fatalf("accepted = %+v, want the spawn", res.Accepted)
	}
	item, err := st.Backlog().Get(ctx, "run-1", "item-x")
	if err != nil {
		t.Fatalf("Get item: %v", err)
	}
	if item.Status != store.BacklogInProgress || item.LaneID != "item-x" {
		t.Fatalf("item = %s lane=%q, want in_progress on lane item-x", item.Status, item.LaneID)
	}
	if ln, err := st.Lanes().Get(ctx, "run-1", "item-x"); err != nil || ln.Status != lane.StatusRunning {
		t.Fatalf("lane = %+v err=%v, want running", ln, err)
	}
}

// The batch contract: a second mutating action is rejected with an error the
// model sees and the result records; the first action stands. backlog.add
// followed by a spawn of that item is the one allowed pair.
func TestDispatch_OnePrimaryActionPerBatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		calls        []model.ToolCall
		wantAccepted []string
		wantRejected int
	}{
		{
			name: "NoteBesideSteerIsRejected",
			calls: []model.ToolCall{
				{ID: "t1", Name: dispatcher.ToolLaneSteer, Input: json.RawMessage(`{"lane_id":"lane-a","note":"fix it","reason":"r"}`)},
				{ID: "t2", Name: dispatcher.ToolDirectiveNote, Input: json.RawMessage(`{"fact":"restates the steer","reason":"r"}`)},
			},
			wantAccepted: []string{dispatcher.ToolLaneSteer},
			wantRejected: 1,
		},
		{
			name: "NoteFirstThenPlannerRequestIsRejected",
			calls: []model.ToolCall{
				{ID: "t1", Name: dispatcher.ToolDirectiveNote, Input: json.RawMessage(`{"fact":"a rule","reason":"r"}`)},
				{ID: "t2", Name: dispatcher.ToolPlannerRequest, Input: json.RawMessage(`{"reason":"replan"}`)},
			},
			wantAccepted: []string{dispatcher.ToolDirectiveNote},
			wantRejected: 1,
		},
		{
			name: "AddThenSpawnOfAddedItemIsAllowed",
			calls: []model.ToolCall{
				{ID: "t1", Name: dispatcher.ToolBacklogAdd, Input: json.RawMessage(`{"id":"item-new","title":"new","reason":"r"}`)},
				{ID: "t2", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"item-new","brief":"do it","reason":"r"}`)},
			},
			wantAccepted: []string{dispatcher.ToolBacklogAdd, dispatcher.ToolLaneSpawn},
		},
		{
			name: "AddThenSpawnOfOtherItemIsAllowed",
			calls: []model.ToolCall{
				{ID: "t1", Name: dispatcher.ToolBacklogAdd, Input: json.RawMessage(`{"id":"item-new","title":"new","reason":"r"}`)},
				{ID: "t2", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"item-other","brief":"do it","reason":"r"}`)},
			},
			wantAccepted: []string{dispatcher.ToolBacklogAdd, dispatcher.ToolLaneSpawn},
		},
		{
			name: "SpawnThenStopIsRejected",
			calls: []model.ToolCall{
				{ID: "t1", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"item-other","brief":"do it","reason":"r"}`)},
				{ID: "t2", Name: dispatcher.ToolLaneStop, Input: json.RawMessage(`{"lane_id":"lane-a","reason":"r"}`)},
			},
			wantAccepted: []string{dispatcher.ToolLaneSpawn},
			wantRejected: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk, st, drv := setupTestEnv(t)
			ctx := t.Context()
			if err := st.Backlog().Add(ctx, store.BacklogItem{ID: "item-other", RunID: "run-1", Title: "other", Status: store.BacklogOpen, Priority: store.PriorityP1}); err != nil {
				t.Fatalf("Add item: %v", err)
			}
			if err := st.Lanes().Upsert(ctx, lane.Lane{ID: "lane-a", RunID: "run-1", Status: lane.StatusRunning, SessionID: "s", TerminalID: "t"}); err != nil {
				t.Fatalf("Upsert lane: %v", err)
			}
			fake := model.NewFakeClient(toolUseResponse(tc.calls...), endTurnResponse("done"))
			d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
			ev, err := st.Events().Append(ctx, store.Event{RunID: "run-1", Kind: store.EventOwnerMessage, Class: store.EventClassJudgment, Summary: "x"})
			if err != nil {
				t.Fatalf("Append event: %v", err)
			}
			res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			var got []string
			for _, a := range res.Accepted {
				got = append(got, a.Tool)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantAccepted, ",") || len(res.Rejected) != tc.wantRejected {
				t.Fatalf("accepted=%v rejected=%q, want %v and %d rejection(s)", got, res.Rejected, tc.wantAccepted, tc.wantRejected)
			}
			if tc.wantRejected > 0 && !strings.Contains(res.Rejected[0], "one primary action per batch") {
				t.Errorf("rejection must name the contract: %q", res.Rejected[0])
			}
		})
	}
}

// The request shape per model: the default dispatcher model is
// claude-sonnet-5-5, the hard-batch model is separate, and no model is sent a
// temperature unless the caller sets one (sonnet-5-5 rejects the parameter).
func TestDispatch_RequestShapePerModel(t *testing.T) {
	t.Parallel()

	if dispatcher.DefaultModel != "claude-sonnet-5-5" {
		t.Fatalf("DefaultModel = %q, want claude-sonnet-5-5", dispatcher.DefaultModel)
	}
	temp := 0.2
	cases := []struct {
		setTemp  *float64
		wantTemp *float64
		name     string
		model    string
		wantModl string
		urgent   bool
	}{
		{name: "DefaultModel", wantModl: dispatcher.DefaultModel},
		{name: "HardBatchModel", urgent: true, wantModl: dispatcher.DefaultHardBatchModel},
		{name: "CustomModelNoTemperature", model: "claude-haiku-4-5", wantModl: "claude-haiku-4-5"},
		{name: "ExplicitTemperatureIsSent", model: "claude-sonnet-4-6", setTemp: &temp, wantModl: "claude-sonnet-4-6", wantTemp: &temp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk, st, drv := setupTestEnv(t)
			fake := model.NewFakeClient(endTurnResponse("ok"))
			d := dispatcher.New(dispatcher.Config{Model: tc.model, Temperature: tc.setTemp}, st, drv, clk, fake)
			ev, err := st.Events().Append(t.Context(), store.Event{RunID: "run-1", Kind: store.EventMail, Class: store.EventClassJudgment, Urgent: tc.urgent, Summary: "x"})
			if err != nil {
				t.Fatalf("Append event: %v", err)
			}
			if _, err := d.Dispatch(t.Context(), "run-1", []store.Event{ev}); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			req := fake.Requests[0]
			if req.Model != tc.wantModl {
				t.Errorf("Model = %q, want %q", req.Model, tc.wantModl)
			}
			switch {
			case tc.wantTemp == nil && req.Temperature != nil:
				t.Errorf("Temperature = %v, want it omitted", *req.Temperature)
			case tc.wantTemp != nil && (req.Temperature == nil || *req.Temperature != *tc.wantTemp):
				t.Errorf("Temperature = %v, want %v", req.Temperature, *tc.wantTemp)
			}
		})
	}
}

func TestDispatch_OwnerYesStartItSpawnsReadyP0(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "dir-hold-1",
		RunID:  "run-1",
		Kind:   store.DirectiveStanding,
		Source: store.DirectiveSourceOwner,
		Text:   "Hold starting M4 until I say so",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatalf("Add directive: %v", err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "m4-metrics",
		RunID:    "run-1",
		Title:    "M4 metrics lane",
		Priority: store.PriorityP0,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatalf("Add backlog item: %v", err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t1", Name: "directive.revoke", Input: json.RawMessage(`{"id":"dir-hold-1","reason":"owner lifted hold"}`)},
			model.ToolCall{ID: "t2", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"m4-metrics","brief":"Implement M4 metrics","reason":"owner ordered start"}`)},
		),
		endTurnResponse("Revoked dir-hold-1 and spawned m4-metrics."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "yes, start it",
	})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}

	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	offered := toolNames(fake.Requests[0])
	for _, wantTool := range []string{
		dispatcher.ToolBacklogAdd, dispatcher.ToolBacklogUpdate, dispatcher.ToolBacklogReorder, dispatcher.ToolBacklogClose,
		dispatcher.ToolLaneSpawn, dispatcher.ToolLaneSteer, dispatcher.ToolLaneRetry, dispatcher.ToolLaneStop,
		dispatcher.ToolOwnerAsk, dispatcher.ToolOwnerAnswer, dispatcher.ToolDirectiveNote, dispatcher.ToolDirectiveRevoke,
		dispatcher.ToolPlannerRequest, "memory.read",
	} {
		if !slices.Contains(offered, wantTool) {
			t.Fatalf("offered tools %v missing %q", offered, wantTool)
		}
	}
	if len(res.Rejected) > 0 {
		t.Fatalf("unexpected rejections: %v", res.Rejected)
	}
	if len(res.Accepted) != 2 || res.Accepted[1].Tool != dispatcher.ToolLaneSpawn {
		t.Fatalf("accepted = %+v, want directive.revoke + lane.spawn", res.Accepted)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "m4-metrics")
	if err != nil || ln.Status != lane.StatusRunning {
		t.Fatalf("lane m4-metrics = %+v, err=%v, want running", ln, err)
	}
	laterSnap, err := d.BuildSnapshot(ctx, "run-1", nil)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if strings.Contains(laterSnap.Text, "dir-hold-1") || strings.Contains(laterSnap.Text, "Hold starting M4") {
		t.Fatalf("later snapshot still lists revoked directive dir-hold-1:\n%s", laterSnap.Text)
	}
}

func TestDispatch_RevokeHoldAndFollowUpSpawnAfterBacklogAdd(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	for _, id := range []string{"hold-1", "hold-2"} {
		if err := st.Directives().Add(ctx, store.Directive{
			ID:     id,
			RunID:  "run-1",
			Kind:   store.DirectiveStanding,
			Source: store.DirectiveSourceConcierge,
			Text:   "Do not start M4 yet (" + id + ")",
			Status: store.DirectiveActive,
		}); err != nil {
			t.Fatalf("Add directive %s: %v", id, err)
		}
		if _, err := st.Memories().Put(ctx, store.MemoryEntry{
			ID: id, Name: "Directive: " + id, Description: "Do not start M4 yet", Type: "rule", Scope: "run",
			RunID: "run-1", Source: "hold", Body: "Do not start M4 yet", ProposedBy: "owner",
		}); err != nil {
			t.Fatalf("Put memory %s: %v", id, err)
		}
	}
	// Also seed the active DirectiveInstruction that StoreBackend.deliver writes
	// for the incoming owner chat message itself ("yes: clear both hold directives and start M4").
	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "dir-chat-msg",
		RunID:  "run-1",
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceOwner,
		Text:   "yes: clear both hold directives and start M4",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatalf("Add chat instruction directive: %v", err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t1", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"hold-1","reason":"owner cleared hold"}`)},
			model.ToolCall{ID: "t2", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"hold-2","reason":"owner cleared hold"}`)},
			model.ToolCall{ID: "t3", Name: dispatcher.ToolBacklogAdd, Input: json.RawMessage(`{"id":"m4-metrics","title":"M4 metrics","priority":"p0","reason":"owner ordered M4"}`)},
		),
		endTurnResponse("I could not spawn the M4 lane. The lane.spawn tool was not available in this session."),
		toolUseResponse(
			model.ToolCall{ID: "t4", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"m4-metrics","brief":"Implement M4 metrics","reason":"owner ordered M4"}`)},
		),
		endTurnResponse("Cleared both holds and spawned m4-metrics."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "yes: clear both hold directives and start M4",
	})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}

	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) > 0 {
		t.Fatalf("unexpected rejections: %v", res.Rejected)
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].LaneID != "m4-metrics" {
		t.Fatalf("spawns = %+v, want m4-metrics spawned in same turn", drv.Spawns)
	}
	laterSnap, err := d.BuildSnapshot(ctx, "run-1", nil)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	for _, id := range []string{"hold-1", "hold-2"} {
		if strings.Contains(laterSnap.Text, id) {
			t.Fatalf("later snapshot still lists revoked directive %s:\n%s", id, laterSnap.Text)
		}
	}
}

func TestDispatch_NegatedStartOrStatusQuestionDoesNotNudgeSpawn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		msg  string
	}{
		{"negated start", "do not start M4 yet"},
		{"negated spawn", "don't spawn any new lanes"},
		{"pause until resume", "pause until I say resume"},
		{"cancel and dont start", "cancel X and don't start Y"},
		{"substring unclear", "the M4 requirement is unclear"},
		{"status question", "can we start M4 or what is the status?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk, st, drv := setupTestEnv(t)
			ctx := t.Context()
			fake := model.NewFakeClient(
				toolUseResponse(
					model.ToolCall{ID: "t1", Name: dispatcher.ToolBacklogAdd, Input: json.RawMessage(`{"id":"m4-metrics","title":"M4 metrics","priority":"p0","reason":"record M4"}`)},
				),
				endTurnResponse("Recorded m4-metrics without spawning."),
			)
			d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
			ev, err := st.Events().Append(ctx, store.Event{
				RunID:   "run-1",
				Kind:    store.EventOwnerMessage,
				Class:   store.EventClassJudgment,
				Urgent:  true,
				Summary: tc.msg,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Dispatch(ctx, "run-1", []store.Event{ev}); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if len(fake.Requests) != 2 {
				t.Fatalf("Requests = %d, want 2 (no follow-up spawn nudge for %q)", len(fake.Requests), tc.msg)
			}
		})
	}
}

func TestVet_DirectMergeGuardNegationBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		brief      string
		wantReject bool
	}{
		{"explicit prohibition do not run", "Implement M4. Do not run gh pr merge.", false},
		{"explicit prohibition never backticked", "Implement M4. Never `gh pr merge` directly.", false},
		{"explicit prohibition do not merge yourself", "Implement M4. Do not merge the PR yourself.", false},
		{"explicit prohibition list with or", "Implement M4. Never run gh pr merge or git push origin main.", false},
		{"explicit prohibition list with commas", "Implement M4. Do not run gh pr merge, hub merge, or git push origin main.", false},
		{"explicit prohibition parenthetical interjection", "Implement M4. Do not, under any circumstances, run gh pr merge.", false},
		{"direct instruction", "Implement M4 and run gh pr merge --squash.", true},
		{"comma separated no issues before merge", "When done, no issues, gh pr merge.", true},
		{"know substring before merge", "Once you know CI is green, run gh pr merge.", true},
		{"cannot in earlier verb before merge", "We cannot wait to run gh pr merge.", true},
		{"do not hesitate before merge", "Do not hesitate to run gh pr merge now.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk, st, drv := setupTestEnv(t)
			ctx := t.Context()
			if err := st.Backlog().Add(ctx, store.BacklogItem{
				ID:       "m4",
				RunID:    "run-1",
				Title:    "M4",
				Priority: store.PriorityP0,
				Status:   store.BacklogOpen,
			}); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]string{
				"item_id": "m4",
				"brief":   tc.brief,
				"reason":  "test brief merge guard",
			})
			fake := model.NewFakeClient(
				toolUseResponse(model.ToolCall{ID: "t1", Name: dispatcher.ToolLaneSpawn, Input: input}),
				endTurnResponse("done"),
			)
			d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
			ev, _ := st.Events().Append(ctx, store.Event{
				RunID:   "run-1",
				Kind:    store.EventTimer,
				Class:   store.EventClassJudgment,
				Summary: "tick",
			})
			res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if tc.wantReject && len(res.Rejected) == 0 {
				t.Fatalf("brief %q was accepted, want rejected by direct-merge guard", tc.brief)
			}
			if !tc.wantReject && len(res.Rejected) > 0 {
				t.Fatalf("brief %q was rejected (%v), want accepted", tc.brief, res.Rejected)
			}
		})
	}
}

func TestDispatch_DirectiveRevokeNotFoundRejected(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	// Seed a non-directive memory entry and an open owner ask directive to verify
	// neither can be revoked/deleted via directive.revoke.
	if _, err := st.Memories().Put(ctx, store.MemoryEntry{
		ID:          "orphan-mem",
		Name:        "Repo rule",
		Description: "keep tests deterministic",
		Type:        "rule",
		Scope:       "repo",
		Source:      "test",
		Body:        "keep tests deterministic",
		ProposedBy:  "model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "ask-q1",
		RunID:  "run-1",
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceDispatcher,
		Ref:    "owner_ask:q1",
		Text:   `{"id":"q1","run_id":"run-1","question":"Proceed?","options":["yes","no"],"default":"yes"}`,
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Directives().Add(ctx, store.Directive{
		ID:     "dir-hold",
		RunID:  "run-1",
		Kind:   store.DirectiveStanding,
		Source: store.DirectiveSourceOwner,
		Text:   "hold new lanes",
		Status: store.DirectiveActive,
	}); err != nil {
		t.Fatal(err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t1", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"nonexistent-dir","reason":"clear nonexistent hold"}`)},
			model.ToolCall{ID: "t2", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"orphan-mem","reason":"try to delete non-directive memory"}`)},
			model.ToolCall{ID: "t3", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"ask-q1","reason":"try to revoke owner ask"}`)},
		),
		endTurnResponse("done"),
	)
	d := dispatcher.New(dispatcher.Config{AllowMultiAction: true}, st, drv, clk, fake)
	ev, _ := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "clear nonexistent-dir",
	})
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Accepted) != 0 || len(res.Rejected) != 3 {
		t.Fatalf("Accepted=%d Rejected=%d (%v), want 0 accepted and 3 rejected", len(res.Accepted), len(res.Rejected), res.Rejected)
	}
	if mem, mErr := st.Memories().Get(ctx, "", "orphan-mem"); mErr != nil || mem == nil {
		t.Fatalf("orphan-mem should remain intact after rejected directive.revoke, got %+v, %v", mem, mErr)
	}

	// Also verify directive.revoke is rejected when the batch has no owner_msg event.
	fakeNoOwner := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t4", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"dir-hold","reason":"lane asked to lift hold"}`)},
		),
		endTurnResponse("done"),
	)
	dNoOwner := dispatcher.New(dispatcher.Config{}, st, drv, clk, fakeNoOwner)
	laneEv, _ := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventMail,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "lane mail asking to lift hold",
	})
	resNoOwner, err := dNoOwner.Dispatch(ctx, "run-1", []store.Event{laneEv})
	if err != nil {
		t.Fatalf("Dispatch without owner_msg: %v", err)
	}
	if len(resNoOwner.Accepted) != 0 || len(resNoOwner.Rejected) != 1 {
		t.Fatalf("without owner_msg: Accepted=%d Rejected=%d (%v), want 0 accepted and 1 rejected", len(resNoOwner.Accepted), len(resNoOwner.Rejected), resNoOwner.Rejected)
	}
}

func TestDispatch_RawEvidenceInspectionSendsInvalidClaimBack(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	runDir := t.TempDir()
	laneDir := filepath.Join(runDir, "lanes", "lazyblk-perf")
	if err := os.MkdirAll(laneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rawSummary := `{"claim":"66x fewer bytes read","seed_bytes":32,"duplicated_blocks":2048,"note":"test files were one 32-byte seed repeated"}` + "\n"
	if err := os.WriteFile(filepath.Join(laneDir, "summary.json"), []byte(rawSummary), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "lazyblk-perf",
		RunID:    "run-1",
		Title:    "Benchmark lazy block device vs local disk",
		Priority: store.PriorityP0,
		Status:   store.BacklogInProgress,
		LaneID:   "lazyblk-perf",
	}); err != nil {
		t.Fatal(err)
	}
	ln := lane.Lane{
		ID:         "lazyblk-perf",
		RunID:      "run-1",
		ItemID:     "lazyblk-perf",
		Title:      "Benchmark lazy block device vs local disk",
		Status:     lane.StatusBlocked,
		Phase:      recipe.PhaseReview,
		Round:      1,
		RecipeName: recipe.DefaultName,
		Worktree:   laneDir,
	}
	ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-lazy-1", "term-lazy-swe-1")
	ln.RecordAttempt(recipe.PhaseReview, 1, "sess-lazy-1", "term-lazy-rev-1")
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatal(err)
	}
	drv.SetReport("term-lazy-rev-1", lane.PhaseReport{
		LaneID:       "lazyblk-perf",
		TerminalID:   "term-lazy-rev-1",
		Phase:        recipe.PhaseReview,
		Round:        1,
		Outcome:      lane.OutcomeDone,
		Summary:      "Reported win: lazy path reads 66x fewer bytes than local disk (see summary.json).",
		RiskiestStep: "verify benchmark payload entropy in summary.json",
		Observed:     "summary.json -> 66x fewer bytes read",
	})

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "inspect-1",
				Name:  "read.lane",
				Input: json.RawMessage(`{"lane_id":"lazyblk-perf","path":"summary.json","reason":"check raw evidence for 66x speedup claim before integrate"}`),
			},
		),
		toolUseResponse(
			model.ToolCall{
				ID:    "retry-1",
				Name:  dispatcher.ToolLaneRetry,
				Input: json.RawMessage(`{"lane_id":"lazyblk-perf","phase":"swe","round":2,"note":"Voided reported win: summary.json shows test files were one 32-byte seed repeated, so compression skewed lazy reads 66x.","reason":"raw evidence check failed: duplicated 32-byte seed in summary.json"}`),
			},
		),
		endTurnResponse("Voided reported win from raw evidence in summary.json and sent lazyblk-perf back for round 2."),
	)

	d := dispatcher.New(dispatcher.Config{
		Investigate: model.InvestigateConfig{RunDir: runDir},
	}, st, drv, clk, fake)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "lazyblk-perf",
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "lazyblk-perf review reported done: lazy path reads 66x fewer bytes than local disk",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("Rejected = %v, want 0 rejections", res.Rejected)
	}
	if len(fake.Requests) < 2 {
		t.Fatalf("Requests = %d, want at least 2", len(fake.Requests))
	}
	secondTurnMsgs := fake.Requests[1].Messages
	toolResultText := secondTurnMsgs[len(secondTurnMsgs)-1].Content[0].Content
	if !strings.Contains(toolResultText, "one 32-byte seed repeated") || !strings.Contains(toolResultText, `"duplicated_blocks":2048`) {
		t.Fatalf("inspection tool output did not reach model in turn 2: %q", toolResultText)
	}

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 2 {
		t.Fatalf("len(decisions) = %d, want 2 (read.lane + lane.retry): %+v", len(decs), decs)
	}
	if !decs[0].Vetted || decs[0].Tool != "read.lane" || !strings.Contains(decs[0].Result, "one 32-byte seed repeated") {
		t.Fatalf("decision[0] = %+v, want vetted read.lane with raw file output", decs[0])
	}
	if !decs[1].Vetted || decs[1].Tool != dispatcher.ToolLaneRetry || decs[1].LaneID != "lazyblk-perf" {
		t.Fatalf("decision[1] = %+v, want vetted lane.retry on lazyblk-perf", decs[1])
	}

	updatedLane, err := st.Lanes().Get(ctx, "run-1", "lazyblk-perf")
	if err != nil {
		t.Fatal(err)
	}
	if updatedLane.Status != lane.StatusRunning || updatedLane.Phase != recipe.PhaseSWE || updatedLane.Round != 2 {
		t.Fatalf("lane after raw evidence failure = %+v, want running in swe round 2", updatedLane)
	}
}

func TestDispatch_ContractDirectiveBroadcastsToRunningLanesAndNextBrief(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	for _, id := range []string{"lane-a", "lane-b"} {
		ln := lane.Lane{
			ID:         id,
			RunID:      "run-1",
			ItemID:     id,
			Title:      "Running work " + id,
			Status:     lane.StatusRunning,
			Phase:      recipe.PhaseSWE,
			Round:      1,
			RecipeName: recipe.DefaultName,
			SessionID:  "sess-" + id,
			TerminalID: "term-" + id,
		}
		ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-"+id, "term-"+id)
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "item-next",
		RunID:    "run-1",
		Title:    "Next worker lane",
		Priority: store.PriorityP0,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatal(err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "contract-1",
				Name:  dispatcher.ToolDirectiveNote,
				Input: json.RawMessage(`{"fact":"best performance, beat local disk; Bazel code may change","contract":true,"reason":"owner run contract decision"}`),
			},
		),
		endTurnResponse("Recorded run contract."),
		toolUseResponse(
			model.ToolCall{
				ID:    "spawn-1",
				Name:  dispatcher.ToolLaneSpawn,
				Input: json.RawMessage(`{"item_id":"item-next","brief":"Implement the next piece following the run contract.","reason":"staff ready work"}`),
			},
		),
		endTurnResponse("Spawned item-next."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)

	ownerEv, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "Owner contract: best performance, beat local disk; Bazel code may change",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, "run-1", []store.Event{ownerEv}); err != nil {
		t.Fatalf("Dispatch tick 1: %v", err)
	}

	if len(drv.Steers) != 2 {
		t.Fatalf("len(Steers) after new contract directive = %d, want 2 (one per running lane): %+v", len(drv.Steers), drv.Steers)
	}
	steeredSessions := []string{drv.Steers[0].SessionID, drv.Steers[1].SessionID}
	slices.Sort(steeredSessions)
	if !slices.Equal(steeredSessions, []string{"sess-lane-a", "sess-lane-b"}) {
		t.Fatalf("steered sessions = %v, want [sess-lane-a sess-lane-b]", steeredSessions)
	}
	for _, stCall := range drv.Steers {
		if !strings.Contains(stCall.Message, "best performance, beat local disk; Bazel code may change") {
			t.Fatalf("steer message missing contract directive: %q", stCall.Message)
		}
	}

	timerEv, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "tick 2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, "run-1", []store.Event{timerEv}); err != nil {
		t.Fatalf("Dispatch tick 2: %v", err)
	}

	if len(drv.Steers) != 2 {
		t.Fatalf("len(Steers) after tick 2 = %d, want 2 (no duplicate contract steers)", len(drv.Steers))
	}
	if len(drv.Spawns) != 1 {
		t.Fatalf("len(Spawns) = %d, want 1", len(drv.Spawns))
	}
	if !strings.Contains(drv.Spawns[0].Brief, "## Run Contract") || !strings.Contains(drv.Spawns[0].Brief, "best performance, beat local disk; Bazel code may change") {
		t.Fatalf("next brief missing run contract:\n%s", drv.Spawns[0].Brief)
	}
	if !strings.Contains(fake.Requests[2].Messages[0].Text(), "## Run Contract") || !strings.Contains(fake.Requests[2].Messages[0].Text(), "best performance, beat local disk; Bazel code may change") {
		t.Fatalf("next dispatcher snapshot missing run contract:\n%s", fake.Requests[2].Messages[0].Text())
	}
}

func TestDispatch_ForwardToolPostsExcerptToTargetLane(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	srcLane := lane.Lane{
		ID:         "perf-sweep",
		RunID:      "run-1",
		ItemID:     "perf-sweep",
		Title:      "Sweep per-knob numbers",
		Status:     lane.StatusDone,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		RecipeName: recipe.DefaultName,
		SessionID:  "sess-perf-sweep",
		TerminalID: "term-perf-sweep",
	}
	if err := st.Lanes().Upsert(ctx, srcLane); err != nil {
		t.Fatal(err)
	}
	dstLane := lane.Lane{
		ID:         "lazyblk-perf",
		RunID:      "run-1",
		ItemID:     "lazyblk-perf",
		Title:      "Optimize lazy block device",
		Status:     lane.StatusRunning,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		RecipeName: recipe.DefaultName,
		SessionID:  "sess-lazyblk",
		TerminalID: "term-lazyblk",
	}
	dstLane.RecordAttempt(recipe.PhaseSWE, 1, "sess-lazyblk", "term-lazyblk")
	if err := st.Lanes().Upsert(ctx, dstLane); err != nil {
		t.Fatal(err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "fwd-1",
				Name:  dispatcher.ToolLaneForward,
				Input: json.RawMessage(`{"from_lane_id":"perf-sweep","to_lane_id":"lazyblk-perf","excerpt":"readahead=256KiB cut p99 latency by 41%; queue_depth=32 saturated iops","reason":"perf-sweep per-knob numbers affect lazyblk-perf"}`),
			},
		),
		endTurnResponse("Forwarded perf-sweep findings to lazyblk-perf."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "perf-sweep",
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "perf-sweep reported per-knob latency numbers",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 0 || len(res.Accepted) != 1 {
		t.Fatalf("Accepted=%d Rejected=%v, want 1 accepted and 0 rejected", len(res.Accepted), res.Rejected)
	}
	if len(drv.Steers) != 1 {
		t.Fatalf("len(Steers) = %d, want 1", len(drv.Steers))
	}
	if drv.Steers[0].SessionID != "sess-lazyblk" {
		t.Fatalf("steer session = %q, want sess-lazyblk", drv.Steers[0].SessionID)
	}
	if !strings.Contains(drv.Steers[0].Message, "perf-sweep") || !strings.Contains(drv.Steers[0].Message, "readahead=256KiB cut p99 latency by 41%") {
		t.Fatalf("steer message missing forwarded excerpt: %q", drv.Steers[0].Message)
	}
}

func TestDispatch_SpawnWithModelOutsideAllowedListIsRefused(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "item-hard",
		RunID:    "run-1",
		Title:    "Hard novel architecture work",
		Priority: store.PriorityP0,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatal(err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "spawn-bad-model",
				Name:  dispatcher.ToolLaneSpawn,
				Input: json.RawMessage(`{"item_id":"item-hard","model":"gpt-4o-mini","brief":"Implement hard novel subsystem.","reason":"spawn with disallowed model"}`),
			},
		),
		toolUseResponse(
			model.ToolCall{
				ID:    "spawn-good-model",
				Name:  dispatcher.ToolLaneSpawn,
				Input: json.RawMessage(`{"item_id":"item-hard","model":"claude-opus-4-6","brief":"Implement hard novel subsystem.","reason":"hard/new work uses strongest allowed model"}`),
			},
		),
		endTurnResponse("Spawned item-hard with claude-opus-4-6."),
	)
	d := dispatcher.New(dispatcher.Config{
		AllowedModels: []string{"claude-opus-4-6", "claude-sonnet-4-6"},
	}, st, drv, clk, fake)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "staff open backlog",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0], "not in the allowed models list") {
		t.Fatalf("Rejected = %+v, want 1 rejection for model outside allowed list", res.Rejected)
	}
	if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolLaneSpawn {
		t.Fatalf("Accepted = %+v, want 1 accepted lane.spawn", res.Accepted)
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].Model != "claude-opus-4-6" {
		t.Fatalf("Spawns = %+v, want 1 spawn with Model=claude-opus-4-6", drv.Spawns)
	}

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 2 {
		t.Fatalf("len(decisions) = %d, want 2 (1 rejected + 1 accepted): %+v", len(decs), decs)
	}
	if decs[0].Vetted || !strings.Contains(decs[0].Result, "not in the allowed models list") {
		t.Fatalf("decision[0] = %+v, want unvetted rejection for disallowed model", decs[0])
	}
	if !decs[1].Vetted || decs[1].Model != "claude-opus-4-6" || !strings.Contains(decs[1].Result, "model=claude-opus-4-6") {
		t.Fatalf("decision[1] = %+v, want vetted spawn recording model=claude-opus-4-6", decs[1])
	}
}

func TestDispatch_IdleRunWithReadyWorkInObjectiveSpawns(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Runs().UpdateObjective(ctx, "run-1", "Deliver diskless sandbox.\n\nNext:\n- lazyblk-perf: benchmark lazy block device vs local disk", ""); err != nil {
		t.Fatal(err)
	}

	fake := model.NewFakeClient(
		endTurnResponse("Nothing needed a decision."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "periodic check: nothing needed a decision",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolLaneSpawn {
		t.Fatalf("Accepted = %+v, want 1 proactive lane.spawn decision", res.Accepted)
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].LaneID != "lazyblk-perf" {
		t.Fatalf("Spawns = %+v, want 1 spawn for lazyblk-perf", drv.Spawns)
	}

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 1 || !decs[0].Vetted || decs[0].Tool != dispatcher.ToolLaneSpawn || decs[0].LaneID != "lazyblk-perf" {
		t.Fatalf("decisions = %+v, want 1 vetted lane.spawn decision for lazyblk-perf", decs)
	}
}

func TestReadOnlyCommandAndReadLaneConfinement(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for _, tc := range []struct {
		name    string
		prog    string
		args    []string
		wantErr bool
	}{
		{name: "jq valid filter and file", prog: "jq", args: []string{"-r", ".claim", "summary.json"}, wantErr: false},
		{name: "jq field named env allowed", prog: "jq", args: []string{".env", "summary.json"}, wantErr: false},
		{name: "jq missing file arg rejected", prog: "jq", args: []string{"."}, wantErr: true},
		{name: "jq null-input env rejected", prog: "jq", args: []string{"-n", "env"}, wantErr: true},
		{name: "jq env builtin rejected", prog: "jq", args: []string{"env", "summary.json"}, wantErr: true},
		{name: "jq string interpolation env rejected", prog: "jq", args: []string{`"\(env)"`, "summary.json"}, wantErr: true},
		{name: "jq ENV var rejected", prog: "jq", args: []string{"$ENV.SECRET", "summary.json"}, wantErr: true},
		{name: "jq rawfile flag rejected", prog: "jq", args: []string{"--rawfile", "x", "/etc/passwd", ".", "summary.json"}, wantErr: true},
		{name: "jq first file traversal rejected", prog: "jq", args: []string{".", "../secret.json", "summary.json"}, wantErr: true},
		{name: "wc valid flag and file", prog: "wc", args: []string{"-l", "summary.json"}, wantErr: false},
		{name: "wc files0-from rejected", prog: "wc", args: []string{"--files0-from=/etc/passwd"}, wantErr: true},
		{name: "tail follow rejected", prog: "tail", args: []string{"-f", "summary.json"}, wantErr: true},
		{name: "cat without file rejected", prog: "cat", args: []string{"-n"}, wantErr: true},
		{name: "rg valid pattern and file", prog: "rg", args: []string{"-n", "seed_bytes", "summary.json"}, wantErr: false},
		{name: "rg without file rejected", prog: "rg", args: []string{"-n", "seed_bytes"}, wantErr: true},
		{name: "rg encoding flag -E rejected", prog: "rg", args: []string{"-E", "seed_bytes", "summary.json"}, wantErr: true},
		{name: "grep extended-regexp -E allowed", prog: "grep", args: []string{"-E", "seed_bytes", "summary.json"}, wantErr: false},
		{name: "grep recursive flag rejected", prog: "grep", args: []string{"-r", "seed_bytes", "summary.json"}, wantErr: true},
		{name: "rg pre flag rejected", prog: "rg", args: []string{"--pre=sh", "seed_bytes", "summary.json"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := model.ValidateReadOnlyCommand(tc.prog, tc.args)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateReadOnlyCommand(%q, %v) succeeded, want error", tc.prog, tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateReadOnlyCommand(%q, %v) unexpected error: %v", tc.prog, tc.args, err)
			}
		})
	}

	// Verify symlink confinement and read.lane execution in the lane worktree (not main RepoDir).
	mainRepoDir := t.TempDir()
	worktreeDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mainRepoDir, "summary.json"), []byte(`{"source":"main-repo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeDir, "summary.json"), []byte(`{"source":"lane-worktree","note":"hello world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(worktreeDir, "escape.txt")); err != nil {
		t.Fatal(err)
	}

	invCfg := model.InvestigateConfig{
		RunDir:  worktreeDir,
		RepoDir: worktreeDir,
	}
	if _, err := model.ExecuteReadCmd(ctx, invCfg, model.ReadCmdParams{
		Program: "cat",
		Args:    []string{"escape.txt"},
	}); err == nil {
		t.Fatal("ExecuteReadCmd(cat escape.txt) succeeded on symlink outside allowed roots, want error")
	}
	if _, err := model.ExecuteReadCmd(ctx, invCfg, model.ReadCmdParams{
		Program: "grep",
		Args:    []string{"hello", "."},
	}); err == nil {
		t.Fatal("ExecuteReadCmd(grep hello .) succeeded on directory path, want error")
	}
	dirListing, err := model.ExecuteReadFile(invCfg, model.ReadFileParams{
		Path:     ".",
		MaxBytes: 24,
	})
	if err != nil {
		t.Fatalf("ExecuteReadFile(. MaxBytes=24): %v", err)
	}
	if !strings.HasSuffix(dirListing, "...[truncated]") {
		t.Fatalf("ExecuteReadFile(. MaxBytes=24) = %q, want truncation suffix", dirListing)
	}

	clk, st, drv := setupTestEnv(t)
	ln := lane.Lane{
		ID:         "lane-wt",
		RunID:      "run-1",
		ItemID:     "lane-wt",
		Title:      "Worktree inspection lane",
		Status:     lane.StatusRunning,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		RecipeName: recipe.DefaultName,
		Worktree:   worktreeDir,
		Branch:     "swarm-lane/lane-wt",
		SessionID:  "sess-wt",
		TerminalID: "term-wt",
	}
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatal(err)
	}

	var seenDir, seenProg string
	var seenArgs []string
	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "rl-prog",
				Name:  dispatcher.ToolReadLane,
				Input: json.RawMessage(`{"lane_id":"lane-wt","program":"wc","args":["-l","summary.json"],"reason":"inspect line count via program+args"}`),
			},
			model.ToolCall{
				ID:    "rl-cmd",
				Name:  dispatcher.ToolReadLane,
				Input: json.RawMessage(`{"lane_id":"lane-wt","command":"grep -F \"hello world\" summary.json","reason":"inspect quoted command in lane worktree"}`),
			},
			model.ToolCall{
				ID:    "rl-ref",
				Name:  dispatcher.ToolReadLane,
				Input: json.RawMessage(`{"lane_id":"lane-wt","path":"only-on-ref.txt","ref":"refs/heads/custom-ref","reason":"inspect explicit ref"}`),
			},
		),
		endTurnResponse("Inspected lane."),
	)
	d := dispatcher.New(dispatcher.Config{
		Investigate: model.InvestigateConfig{
			RepoDir: mainRepoDir,
			CommandRunner: func(_ context.Context, dir, program string, args []string) ([]byte, error) {
				seenDir = dir
				seenProg = program
				seenArgs = slices.Clone(args)
				return []byte(dir + "|" + program + "|" + strings.Join(args, ",")), nil
			},
		},
	}, st, drv, clk, fake)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "lane-wt",
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Summary: "lane-wt report",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 0 || len(res.Accepted) != 3 {
		t.Fatalf("Accepted=%d Rejected=%v, want 3 accepted read.lane calls", len(res.Accepted), res.Rejected)
	}
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil || len(decs) != 3 {
		t.Fatalf("decisions = %+v, err=%v, want 3 decisions", decs, err)
	}
	if !strings.Contains(decs[0].Result, worktreeDir+"|wc|-l,summary.json") {
		t.Fatalf("decision[0] result = %q, want execution in worktreeDir with program=wc args=[-l summary.json]", decs[0].Result)
	}
	if !strings.Contains(decs[1].Result, worktreeDir+"|grep|-F,hello world,summary.json") {
		t.Fatalf("decision[1] result = %q, want quote-aware command split in worktreeDir", decs[1].Result)
	}
	if seenProg != "git" || !slices.Equal(seenArgs, []string{"show", "--no-ext-diff", "--no-textconv", "refs/heads/custom-ref:only-on-ref.txt"}) {
		t.Fatalf("ref call ran dir=%q prog=%q args=%v, want git show refs/heads/custom-ref:only-on-ref.txt", seenDir, seenProg, seenArgs)
	}
}

func TestProactiveStaffingAndObjectiveReadyItemRules(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Runs().UpdateObjective(ctx, "run-1", "Deliver diskless sandbox.\n\nNext:\n- Implement the lazy block device benchmark\n- lazyblk-perf: benchmark lazy block device vs local disk\n- cache-warmup: prewarm chunk index", ""); err != nil {
		t.Fatal(err)
	}

	view, err := dispatcher.LoadStateView(ctx, st, clk, "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := view.ObjectiveMentionsReadyItem("lazyblk-perf"); !ok {
		t.Fatal("ObjectiveMentionsReadyItem(lazyblk-perf) = false, want true")
	}
	for _, proseWord := range []string{"diskless", "sandbox", "local", "disk", "benchmark", "lazy", "block", "device"} {
		if _, ok := view.ObjectiveMentionsReadyItem(proseWord); ok {
			t.Fatalf("ObjectiveMentionsReadyItem(%q) = true, want false for prose word", proseWord)
		}
	}
	if firstReady, ok := view.FirstObjectiveReadyItem(); !ok || firstReady.ID != "lazyblk-perf" {
		t.Fatalf("FirstObjectiveReadyItem() = (%+v, %v), want lazyblk-perf (not prose word)", firstReady, ok)
	}

	// Seed one running lane; default MaxConcurrentLanes is 4, so idle tick should staff the next ready item.
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:         "lazyblk-perf",
		RunID:      "run-1",
		ItemID:     "lazyblk-perf",
		Status:     lane.StatusRunning,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		RecipeName: recipe.DefaultName,
		SessionID:  "sess-1",
		TerminalID: "term-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Runs().UpdateSettings(ctx, "run-1", store.RunSettings{ConcurrencyCap: 2}); err != nil {
		t.Fatal(err)
	}

	fake := model.NewFakeClient(
		endTurnResponse("No action."),
		endTurnResponse("Lane timer."),
		endTurnResponse("At cap."),
		endTurnResponse("Empty batch."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)

	// Lane-scoped timer event (LaneID != "") must not be treated as a run-level idle tick.
	evLaneTimer, _ := st.Events().Append(ctx, store.Event{RunID: "run-1", LaneID: "lazyblk-perf", Kind: store.EventTimer, Class: store.EventClassJudgment, Summary: "lane wait deadline"})
	resLaneTimer, err := d.Dispatch(ctx, "run-1", []store.Event{evLaneTimer})
	if err != nil {
		t.Fatalf("Dispatch lane timer: %v", err)
	}
	if len(resLaneTimer.Accepted) != 0 {
		t.Fatalf("resLaneTimer.Accepted = %+v, want 0 proactive spawns on lane-scoped timer", resLaneTimer.Accepted)
	}

	ev1, _ := st.Events().Append(ctx, store.Event{RunID: "run-1", Kind: store.EventTimer, Class: store.EventClassJudgment, Summary: "idle tick 1"})
	res1, err := d.Dispatch(ctx, "run-1", []store.Event{ev1})
	if err != nil {
		t.Fatalf("Dispatch 1: %v", err)
	}
	if len(res1.Accepted) != 1 || res1.Accepted[0].LaneID != "cache-warmup" {
		t.Fatalf("res1.Accepted = %+v, want proactive spawn of cache-warmup while 1/2 lanes running", res1.Accepted)
	}

	// Now 2/2 lanes are running (at ConcurrencyCap=2); next idle tick must not spawn.
	ev2, _ := st.Events().Append(ctx, store.Event{RunID: "run-1", Kind: store.EventTimer, Class: store.EventClassJudgment, Summary: "idle tick 2"})
	res2, err := d.Dispatch(ctx, "run-1", []store.Event{ev2})
	if err != nil {
		t.Fatalf("Dispatch 2: %v", err)
	}
	if len(res2.Accepted) != 0 {
		t.Fatalf("res2.Accepted = %+v, want 0 spawns when at ConcurrencyCap", res2.Accepted)
	}

	// Empty events slice must not be treated as an idle timer tick.
	res3, err := d.Dispatch(ctx, "run-1", nil)
	if err != nil {
		t.Fatalf("Dispatch 3: %v", err)
	}
	if len(res3.Accepted) != 0 {
		t.Fatalf("res3.Accepted = %+v, want 0 spawns on empty events slice", res3.Accepted)
	}
}

func TestContractOrderingRevokeAndModelPolicyInvariants(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	runDir := t.TempDir()
	t0 := clk.Now().UTC()
	for _, dir := range []store.Directive{
		{ID: "c-late", RunID: "run-1", Kind: store.DirectiveContract, Contract: true, Source: store.DirectiveSourceOwner, Text: "second contract rule", Status: store.DirectiveActive, CreatedAt: t0.Add(2 * time.Minute)},
		{ID: "c-early", RunID: "run-1", Kind: store.DirectiveContract, Contract: true, Source: store.DirectiveSourceOwner, Text: "first contract rule", Status: store.DirectiveActive, CreatedAt: t0.Add(1 * time.Minute)},
	} {
		if err := st.Directives().Add(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:         "lane-run",
		RunID:      "run-1",
		ItemID:     "lane-run",
		Status:     lane.StatusRunning,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		RecipeName: recipe.DefaultName,
		SessionID:  "sess-run",
		TerminalID: "term-run",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:         "lane-review",
		RunID:      "run-1",
		ItemID:     "lane-review",
		Status:     lane.StatusBlocked,
		Phase:      recipe.PhaseReview,
		Round:      1,
		RecipeName: recipe.DefaultName,
		SessionID:  "sess-rev",
		TerminalID: "term-rev",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "item-tier",
		RunID:    "run-1",
		Title:    "Tier alias spawn item",
		Priority: store.PriorityP0,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatal(err)
	}

	view, err := dispatcher.LoadStateView(ctx, st, clk, "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	invs := dispatcher.DefaultInvariants()

	// 1. DefaultAllowedModels is enforced even when AllowedModels is unset, and directive.note decodes kind="contract".
	noteKindAct, err := dispatcher.ParseAction("dn-kind", dispatcher.ToolDirectiveNote, json.RawMessage(`{"fact":"contract rule via kind","kind":"contract"}`))
	if err != nil {
		t.Fatal(err)
	}
	if noteKindAct.DirectiveNote == nil || !noteKindAct.DirectiveNote.Contract {
		t.Fatalf("ParseAction(directive.note kind=contract) = %+v, want Contract=true", noteKindAct.DirectiveNote)
	}
	badModelAct, err := dispatcher.ParseAction("m1", dispatcher.ToolLaneSpawn, json.RawMessage(`{"item_id":"item-tier","model":"unknown-model-xyz","brief":"do work"}`))
	if err != nil {
		t.Fatal(err)
	}
	if vErr := dispatcher.VetAction(view, badModelAct, invs); vErr == nil || !strings.Contains(vErr.Error(), "not in the allowed models list") {
		t.Fatalf("VetAction(unknown-model-xyz) = %v, want allowed-models rejection", vErr)
	}

	// 2. Fastest model (haiku / fastest / gpt-5.4-mini) is rejected for verification/review phase on lane.retry.
	for _, fastModel := range []string{"fastest", "gpt-5.4-mini"} {
		fastRevAct, pErr := dispatcher.ParseAction("m2-"+fastModel, dispatcher.ToolLaneRetry, json.RawMessage(`{"lane_id":"lane-review","phase":"review","model":"`+fastModel+`","note":"re-verify"}`))
		if pErr != nil {
			t.Fatal(pErr)
		}
		if vErr := dispatcher.VetAction(view, fastRevAct, invs); vErr == nil || !strings.Contains(vErr.Error(), "never use fastest model for verification") {
			t.Fatalf("VetAction(retry review with %s) = %v, want fastest-model verification rejection", fastModel, vErr)
		}
	}
	// "cheaper" maps to sonnet/gpt-5.4 (not fastest) and is allowed.
	if got := dispatcher.ResolveWorkerModel("cheaper", "claude"); got != "sonnet" {
		t.Fatalf("ResolveWorkerModel(cheaper, claude) = %q, want sonnet", got)
	}
	if got := dispatcher.ResolveWorkerModel("cheaper", "codex"); got != "gpt-5.4" {
		t.Fatalf("ResolveWorkerModel(cheaper, codex) = %q, want gpt-5.4", got)
	}

	// 3. lane.forward self-forward and unknown from_lane_id are rejected by vet.
	selfFwdAct, err := dispatcher.ParseAction("m3", dispatcher.ToolLaneForward, json.RawMessage(`{"from_lane_id":"lane-run","to_lane_id":"lane-run","excerpt":"same lane"}`))
	if err != nil {
		t.Fatal(err)
	}
	if vErr := dispatcher.VetAction(view, selfFwdAct, invs); vErr == nil || !strings.Contains(vErr.Error(), "to itself") {
		t.Fatalf("VetAction(self forward) = %v, want self-forward rejection", vErr)
	}
	unknownFromAct, err := dispatcher.ParseAction("m3b", dispatcher.ToolLaneForward, json.RawMessage(`{"from_lane_id":"unknown-src","to_lane_id":"lane-run","excerpt":"from ghost"}`))
	if err != nil {
		t.Fatal(err)
	}
	if vErr := dispatcher.VetAction(view, unknownFromAct, invs); vErr == nil || !strings.Contains(vErr.Error(), "source lane") {
		t.Fatalf("VetAction(unknown from_lane_id) = %v, want unknown source lane rejection", vErr)
	}

	// 4. Revoking a contract directive steers running lanes with revocation notice,
	// spawning with tier alias "strongest" resolves to "opus", and brief contract lines are sorted by CreatedAt.
	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "rev-1", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"c-late","reason":"owner revoked second rule"}`)},
			model.ToolCall{ID: "sp-1", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"item-tier","model":"strongest","brief":"Build tier item.","reason":"staff P0"}`)},
		),
		endTurnResponse("Done."),
	)
	d := dispatcher.New(dispatcher.Config{
		AllowMultiAction: true,
		Investigate:      model.InvestigateConfig{RunDir: runDir},
	}, st, drv, clk, fake)

	snapBefore, err := d.BuildSnapshot(ctx, "run-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	earlyIdx := strings.Index(snapBefore.Text, "first contract rule")
	lateIdx := strings.Index(snapBefore.Text, "second contract rule")
	if earlyIdx < 0 || lateIdx < 0 || earlyIdx > lateIdx {
		t.Fatalf("snapshot contract rules out of CreatedAt order (early=%d late=%d):\n%s", earlyIdx, lateIdx, snapBefore.Text)
	}

	ev, _ := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "revoke c-late and start item-tier",
	})
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 0 || len(res.Accepted) != 2 {
		t.Fatalf("Accepted=%d Rejected=%v, want 2 accepted", len(res.Accepted), res.Rejected)
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].Model != "opus" {
		t.Fatalf("Spawns = %+v, want Model=opus resolved from strongest tier alias", drv.Spawns)
	}
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	foundResolvedDecisionModel := false
	for _, dec := range decs {
		if dec.Tool == dispatcher.ToolLaneSpawn && dec.Model == "opus" && strings.Contains(dec.Result, "model=opus") {
			foundResolvedDecisionModel = true
			break
		}
	}
	if !foundResolvedDecisionModel {
		t.Fatalf("decisions = %+v, want lane.spawn decision with resolved Model=opus and result containing model=opus", decs)
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "CONTEXT.md")); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected CONTEXT.md written to RunDir: err=%v", statErr)
	}
	foundRevokeSteer := false
	for _, stCall := range drv.Steers {
		if strings.Contains(stCall.Message, "directive c-late was revoked") {
			foundRevokeSteer = true
			break
		}
	}
	if !foundRevokeSteer {
		t.Fatalf("drv.Steers = %+v, want revocation steer for c-late", drv.Steers)
	}
}

func TestDispatch_NoMergeReportOnlyItemSkipsIntegrateAndNeverMerges(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	adopted := recipe.Default().Clone()
	adopted.RunID = "run-1"
	adopted.Name = "adopted"
	adopted.MergePolicy.Mode = recipe.MergeModeController
	if _, err := st.Recipes().Save(ctx, adopted); err != nil {
		t.Fatalf("Save recipe: %v", err)
	}

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:         "m4-metrics",
		RunID:      "run-1",
		Title:      "M4 metrics",
		Priority:   store.PriorityP0,
		Status:     store.BacklogOpen,
		RecipeName: "adopted",
	}); err != nil {
		t.Fatalf("Add backlog item: %v", err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "t1",
				Name:  dispatcher.ToolBacklogAdd,
				Input: json.RawMessage(`{"id":"audit-item","title":"Audit report","priority":"p1","no_merge":true,"reason":"add explicit no-merge item"}`),
			},
			model.ToolCall{
				ID:    "t2",
				Name:  dispatcher.ToolLaneSpawn,
				Input: json.RawMessage(`{"item_id":"m4-metrics","recipe":"adopted","brief":"Measure M4 latency. Do not merge anything; this is a measurement/report task.","reason":"spawn report-only m4-metrics"}`),
			},
		),
		endTurnResponse("Spawned m4-metrics as no-merge."),
	)
	d := dispatcher.New(dispatcher.Config{AllowMultiAction: true}, st, drv, clk, fake)
	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "staff m4-metrics",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 0 || len(res.Accepted) != 2 {
		t.Fatalf("Accepted=%d Rejected=%v, want 2 accepted and 0 rejected", len(res.Accepted), res.Rejected)
	}

	// Verify both items are explicitly recorded as NoMerge in the store.
	auditItem, err := st.Backlog().Get(ctx, "run-1", "audit-item")
	if err != nil || !auditItem.NoMerge {
		t.Fatalf("audit-item = %+v, err=%v; want NoMerge=true", auditItem, err)
	}
	m4Item, err := st.Backlog().Get(ctx, "run-1", "m4-metrics")
	if err != nil || !m4Item.NoMerge {
		t.Fatalf("m4-metrics = %+v, err=%v; want NoMerge=true recorded on backlog item from report-only brief", m4Item, err)
	}

	snap, err := d.BuildSnapshot(ctx, "run-1", nil)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if !strings.Contains(snap.Text, "no_merge") {
		t.Fatalf("snapshot missing no_merge badge on backlog item:\n%s", snap.Text)
	}

	// Drive the spawned m4-metrics lane through SWE -> Clean -> Review via Controller; verify
	// it completes after Review without ever running Integrate or merging.
	ctrl := controller.New(controller.Config{RunID: "run-1"}, st, drv, clk, nil)
	for _, rep := range []lane.PhaseReport{
		{Phase: recipe.PhaseSWE, Outcome: lane.OutcomeDone, HeadSHA: "sha-m4-1", PR: 5826, Summary: "Collected M4 metrics report."},
		{Phase: recipe.PhaseClean, Outcome: lane.OutcomeDone, HeadSHA: "sha-m4-1", PR: 5826},
	} {
		raw, _ := json.Marshal(rep)
		if _, err := st.Events().Append(ctx, store.Event{
			RunID: "run-1", LaneID: "m4-metrics", Kind: store.EventLaneReport, Class: store.EventClassAuto, Payload: raw,
		}); err != nil {
			t.Fatal(err)
		}
		if err := ctrl.Step(ctx); err != nil {
			t.Fatalf("ctrl.Step after %s: %v", rep.Phase, err)
		}
	}
	if _, err := st.Events().Append(ctx, store.Event{
		RunID: "run-1", LaneID: "m4-metrics", Kind: store.EventCI, Class: store.EventClassAuto, Summary: "green", Payload: json.RawMessage(`{"status":"green","head_sha":"sha-m4-1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	revRaw, _ := json.Marshal(lane.PhaseReport{
		Phase: recipe.PhaseReview, Outcome: lane.OutcomeDone, HeadSHA: "sha-m4-1", PR: 5826, Summary: "LGTM on measurement report.",
	})
	if _, err := st.Events().Append(ctx, store.Event{
		RunID: "run-1", LaneID: "m4-metrics", Kind: store.EventLaneReport, Class: store.EventClassAuto, Payload: revRaw,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Step(ctx); err != nil {
		t.Fatalf("ctrl.Step after Review: %v", err)
	}

	ln, err := st.Lanes().Get(ctx, "run-1", "m4-metrics")
	if err != nil {
		t.Fatalf("Lanes.Get(m4-metrics): %v", err)
	}
	if ln.Status != lane.StatusDone {
		t.Fatalf("lane status = %q, want %q after review", ln.Status, lane.StatusDone)
	}
	if ln.Phase != recipe.PhaseReview {
		t.Fatalf("lane phase = %q, want %q (must never enter integrate)", ln.Phase, recipe.PhaseReview)
	}
	if ln.MergeSHA != "" {
		t.Fatalf("lane MergeSHA = %q, want empty", ln.MergeSHA)
	}
	for _, sp := range drv.Spawns {
		if sp.Phase == recipe.PhaseIntegrate {
			t.Fatalf("unexpected integrate spawn for no-merge lane: %+v", sp)
		}
	}
}

func TestDispatch_G35ActiveDirectivesRevokableAndUndoStaleHoldsInOneTurn(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	// Reproduce G35 (live 11-15 18:15Z): run is closed out ("Next: done") and all
	// lanes/items are terminal, while note-7, dir-concierge-...-5, and chat-... are active.
	if err := st.Runs().UpdateObjective(ctx, "run-1", "# Objective\n\nNext: done (closed out)", ""); err != nil {
		t.Fatalf("UpdateObjective: %v", err)
	}
	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "m4-metrics",
		RunID:    "run-1",
		Title:    "M4 metrics",
		Priority: store.PriorityP0,
		Status:   store.BacklogDone,
	}); err != nil {
		t.Fatalf("Backlog.Add: %v", err)
	}

	const (
		noteID      = "note-7"
		conciergeID = "dir-concierge-1794641883102318034-1435a60f-5"
		chatID      = "chat-EBEMBAM3PGYIVRBAW22H46DF5M"
	)
	for _, d := range []store.Directive{
		{
			ID:     noteID,
			RunID:  "run-1",
			Kind:   store.DirectiveNote,
			Source: store.DirectiveSourceDispatcher,
			Scope:  "run",
			Text:   "Hold off on spawning m4-metrics until the owner lifts the hold",
			Status: store.DirectiveActive,
		},
		{
			ID:     conciergeID,
			RunID:  "run-1",
			Kind:   store.DirectiveStanding,
			Source: store.DirectiveSourceConcierge,
			Scope:  "run",
			Text:   "Do not start m4-metrics yet",
			Status: store.DirectiveActive,
		},
		{
			ID:     chatID,
			RunID:  "run-1",
			Kind:   store.DirectiveInstruction,
			Source: store.DirectiveSourceOwner,
			Scope:  "run",
			Text:   "undo the stale hold directives",
			Status: store.DirectiveActive,
		},
	} {
		if err := st.Directives().Add(ctx, d); err != nil {
			t.Fatalf("Directives.Add(%s): %v", d.ID, err)
		}
	}
	for _, id := range []string{noteID, conciergeID} {
		if _, err := st.Memories().Put(ctx, store.MemoryEntry{
			ID:          id,
			Name:        "Directive: " + id,
			Description: "Hold m4-metrics",
			Type:        "rule",
			Scope:       "run",
			RunID:       "run-1",
			Source:      "hold m4-metrics",
			Body:        "Do not start m4-metrics yet",
			ProposedBy:  "owner",
		}); err != nil {
			t.Fatalf("Memories.Put(%s): %v", id, err)
		}
	}

	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, nil)
	snapBefore, err := d.BuildSnapshot(ctx, "run-1", nil)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	for _, wantID := range []string{noteID, conciergeID, chatID} {
		if !strings.Contains(snapBefore.Text, "["+wantID+"]") {
			t.Fatalf("snapshot before revoke missing %s:\n%s", wantID, snapBefore.Text)
		}
	}

	// Verify dispatcher revokes both explicitly requested IDs (including bracketed [note-7])
	// without rejecting note-7 as "does not exist in the snapshot", and clears every hold
	// the owner asked about in one turn.
	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t1", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"` + chatID + `","reason":"clear owner chat instruction"}`)},
			model.ToolCall{ID: "t2", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"[` + noteID + `]","reason":"owner lifted stale hold"}`)},
		),
		endTurnResponse("Cleared stale hold directives."),
	)
	dWithFake := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "undo the stale hold directives",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := dWithFake.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) > 0 {
		t.Fatalf("unexpected rejections on directive.revoke: %v", res.Rejected)
	}
	if len(res.Accepted) != 2 {
		t.Fatalf("Accepted = %+v, want 2 directive.revoke calls accepted", res.Accepted)
	}

	activeAfter, err := store.ListUserFacingActiveDirectives(ctx, st, "run-1", clk.Now())
	if err != nil {
		t.Fatalf("ListUserFacingActiveDirectives: %v", err)
	}
	if len(activeAfter) != 0 {
		t.Fatalf("active directives after one-turn revoke = %+v, want empty", activeAfter)
	}
	snapAfter, err := dWithFake.BuildSnapshot(ctx, "run-1", nil)
	if err != nil {
		t.Fatalf("BuildSnapshot after: %v", err)
	}
	for _, clearedID := range []string{noteID, conciergeID, chatID} {
		if strings.Contains(snapAfter.Text, clearedID) {
			t.Fatalf("snapshot after revoke still lists %s:\n%s", clearedID, snapAfter.Text)
		}
	}
}

func TestDispatch_BacklogUpdateCanClearNoMerge(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:          "m4-metrics",
		RunID:       "run-1",
		Title:       "M4 metrics report",
		Description: "Do not merge anything; this is a measurement/report task",
		Priority:    store.PriorityP0,
		Status:      store.BacklogOpen,
	}); err != nil {
		t.Fatalf("Backlog.Add: %v", err)
	}
	itemBefore, err := st.Backlog().Get(ctx, "run-1", "m4-metrics")
	if err != nil || itemBefore == nil || !itemBefore.NoMerge {
		t.Fatalf("itemBefore = %+v, err = %v; want NoMerge=true", itemBefore, err)
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "t1",
				Name:  dispatcher.ToolBacklogUpdate,
				Input: json.RawMessage(`{"id":"m4-metrics","no_merge":false,"reason":"owner asked to merge this item"}`),
			},
		),
		endTurnResponse("Cleared no_merge on m4-metrics."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "m4-metrics should merge after review; clear no_merge",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) > 0 || len(res.Accepted) != 1 {
		t.Fatalf("Accepted=%+v Rejected=%v, want 1 accepted and 0 rejected", res.Accepted, res.Rejected)
	}
	itemAfter, err := st.Backlog().Get(ctx, "run-1", "m4-metrics")
	if err != nil || itemAfter == nil {
		t.Fatalf("Backlog.Get after update: %v", err)
	}
	if itemAfter.NoMerge {
		t.Fatalf("itemAfter.NoMerge = true, want false after explicit no_merge:false update")
	}
	snap, err := d.BuildSnapshot(ctx, "run-1", nil)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if strings.Contains(snap.Text, "no_merge") {
		t.Fatalf("snapshot still tags m4-metrics as no_merge after clearing:\n%s", snap.Text)
	}
}

func TestDispatch_G35HoldSweepAllowsSameTurnSpawnAndSelectiveKeep(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "item-bookkeeping",
		RunID:    "run-1",
		Title:    "Bookkeeping cleanup",
		Priority: store.PriorityP0,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatalf("Backlog.Add: %v", err)
	}
	for _, dir := range []store.Directive{
		{
			ID:     "note-1",
			RunID:  "run-1",
			Kind:   store.DirectiveNote,
			Source: store.DirectiveSourceDispatcher,
			Scope:  "run",
			Text:   "Hold all new lanes until owner confirms",
			Status: store.DirectiveActive,
		},
		{
			ID:     "note-2",
			RunID:  "run-1",
			Kind:   store.DirectiveStanding,
			Source: store.DirectiveSourceConcierge,
			Scope:  "run",
			Text:   "Pause new lanes while bookkeeping is pending",
			Status: store.DirectiveActive,
		},
		{
			ID:     "note-lane",
			RunID:  "run-1",
			Kind:   store.DirectiveStanding,
			Source: store.DirectiveSourceOwner,
			Scope:  "other-lane",
			Text:   "Hold other-lane until migration finishes",
			Status: store.DirectiveActive,
		},
	} {
		if err := st.Directives().Add(ctx, dir); err != nil {
			t.Fatalf("Directives.Add(%s): %v", dir.ID, err)
		}
	}

	// Part 1: Owner message says "keep note-2" -> revoking note-1 must NOT sweep note-2.
	fakeSelective := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t1", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"note-1","reason":"owner lifted note-1 only"}`)},
		),
		endTurnResponse("Revoked note-1 and kept note-2."),
	)
	dSelective := dispatcher.New(dispatcher.Config{}, st, drv, clk, fakeSelective)
	ev1, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "lift the hold in note-1, but keep note-2 active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dSelective.Dispatch(ctx, "run-1", []store.Event{ev1}); err != nil {
		t.Fatalf("Dispatch selective: %v", err)
	}
	d1, err := st.Directives().Get(ctx, "run-1", "note-1")
	if err != nil || d1.Status != store.DirectiveRevoked {
		t.Fatalf("note-1 status = %+v, err = %v; want revoked after explicit revoke", d1, err)
	}
	d2, err := st.Directives().Get(ctx, "run-1", "note-2")
	if err != nil || d2.Status != store.DirectiveActive {
		t.Fatalf("note-2 status = %+v, err = %v; want still active when owner said 'keep'", d2, err)
	}

	// Re-add note-1 so both note-1 and note-2 are active holds, then lift all holds
	// with a message containing "bookkeeping" (must not match whole-word "keep"),
	// also explicitly revoke note-2 in the same turn (testing RevokedInTurn after
	// note-1 already swept note-2), and spawn item-bookkeeping in the same turn.
	if err := st.Directives().UpdateStatus(ctx, "run-1", "note-1", store.DirectiveActive); err != nil {
		t.Fatal(err)
	}
	fakeSweepAndSpawn := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{ID: "t1", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"note-1","reason":"owner lifted holds"}`)},
			model.ToolCall{ID: "t2", Name: dispatcher.ToolDirectiveRevoke, Input: json.RawMessage(`{"id":"note-2","reason":"owner lifted holds"}`)},
			model.ToolCall{ID: "t3", Name: dispatcher.ToolLaneSpawn, Input: json.RawMessage(`{"item_id":"item-bookkeeping","brief":"Implement bookkeeping cleanup.","reason":"owner lifted holds and asked to start"}`)},
		),
		endTurnResponse("Lifted holds and spawned item-bookkeeping."),
	)
	dSweep := dispatcher.New(dispatcher.Config{}, st, drv, clk, fakeSweepAndSpawn)
	ev2, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "undo the stale hold directives and start bookkeeping cleanup now",
	})
	if err != nil {
		t.Fatal(err)
	}
	res2, err := dSweep.Dispatch(ctx, "run-1", []store.Event{ev2})
	if err != nil {
		t.Fatalf("Dispatch sweep+spawn: %v", err)
	}
	if len(res2.Rejected) > 0 {
		t.Fatalf("unexpected rejections in sweep+spawn turn: %v", res2.Rejected)
	}
	if len(res2.Accepted) != 3 {
		t.Fatalf("Accepted = %+v, want 3 (2x directive.revoke + lane.spawn)", res2.Accepted)
	}
	for _, revokedID := range []string{"note-1", "note-2"} {
		gotDir, gErr := st.Directives().Get(ctx, "run-1", revokedID)
		if gErr != nil || gotDir.Status != store.DirectiveRevoked {
			t.Fatalf("%s after sweep = %+v, err = %v; want revoked", revokedID, gotDir, gErr)
		}
	}
	laneScopedDir, err := st.Directives().Get(ctx, "run-1", "note-lane")
	if err != nil || laneScopedDir.Status != store.DirectiveActive {
		t.Fatalf("note-lane status = %+v, err = %v; want lane-scoped hold untouched by run sweep", laneScopedDir, err)
	}
}

func TestModelVetterAcceptsConcreteHarnessModelsAndRejectsUnknown(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:       "mem-overhead",
		RunID:    "run-1",
		Title:    "Measure memory overhead",
		Priority: store.PriorityP0,
		Status:   store.BacklogOpen,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(ctx, lane.Lane{
		ID:         "lane-blocked",
		RunID:      "run-1",
		ItemID:     "lane-blocked",
		Status:     lane.StatusBlocked,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		RecipeName: recipe.DefaultName,
		SessionID:  "sess-blocked",
		TerminalID: "term-blocked",
	}); err != nil {
		t.Fatal(err)
	}

	view, err := dispatcher.LoadStateView(ctx, st, clk, "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	invs := dispatcher.DefaultInvariants()

	for _, acceptedModel := range []string{
		"gemini-4-argon-high",
		"gemini-4-argon",
		"gemini-4-carbon-high",
		"gemini-4-hydrogen-high",
		"gemini-4-flash",
		"gemini-3.8-flash-high",
		"gemini-3.1-pro-high",
		"claude-opus-4-6",
		"claude-opus-4-8",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
		"claude-fable-5",
		"gpt-6.1-sol",
		"gpt-6-sol",
		"gpt-5.6-terra",
		"gpt-5.4",
		"opus[200k]",
		"sonnet[200k]",
	} {
		spawnAct, pErr := dispatcher.ParseAction("sp-"+acceptedModel, dispatcher.ToolLaneSpawn, json.RawMessage(`{"item_id":"mem-overhead","model":"`+acceptedModel+`","brief":"measure overhead","reason":"staff lane"}`))
		if pErr != nil {
			t.Fatalf("ParseAction(%s): %v", acceptedModel, pErr)
		}
		if vErr := dispatcher.VetAction(view, spawnAct, invs); vErr != nil {
			t.Fatalf("VetAction(lane.spawn model=%q) unexpected error: %v", acceptedModel, vErr)
		}
		retryAct, pErr := dispatcher.ParseAction("rt-"+acceptedModel, dispatcher.ToolLaneRetry, json.RawMessage(`{"lane_id":"lane-blocked","phase":"swe","round":2,"model":"`+acceptedModel+`","note":"retry with concrete model","reason":"retry lane"}`))
		if pErr != nil {
			t.Fatalf("ParseAction(retry %s): %v", acceptedModel, pErr)
		}
		if vErr := dispatcher.VetAction(view, retryAct, invs); vErr != nil {
			t.Fatalf("VetAction(lane.retry model=%q) unexpected error: %v", acceptedModel, vErr)
		}
	}

	for _, rejectedModel := range []string{"gemini-9-bogus", "unknown-model-xyz"} {
		badSpawn, pErr := dispatcher.ParseAction("bad-sp-"+rejectedModel, dispatcher.ToolLaneSpawn, json.RawMessage(`{"item_id":"mem-overhead","model":"`+rejectedModel+`","brief":"measure overhead","reason":"staff lane"}`))
		if pErr != nil {
			t.Fatalf("ParseAction(%s): %v", rejectedModel, pErr)
		}
		vErr := dispatcher.VetAction(view, badSpawn, invs)
		if vErr == nil || !strings.Contains(vErr.Error(), "not in the allowed models list") || !strings.Contains(vErr.Error(), "gemini-4-argon-high") {
			t.Fatalf("VetAction(lane.spawn model=%q) = %v, want allowed-models rejection naming accepted set", rejectedModel, vErr)
		}
		badRetry, pErr := dispatcher.ParseAction("bad-rt-"+rejectedModel, dispatcher.ToolLaneRetry, json.RawMessage(`{"lane_id":"lane-blocked","phase":"swe","round":2,"model":"`+rejectedModel+`","note":"retry","reason":"retry lane"}`))
		if pErr != nil {
			t.Fatalf("ParseAction(retry %s): %v", rejectedModel, pErr)
		}
		if rErr := dispatcher.VetAction(view, badRetry, invs); rErr == nil || !strings.Contains(rErr.Error(), "not in the allowed models list") || !strings.Contains(rErr.Error(), "gemini-4-argon-high") {
			t.Fatalf("VetAction(lane.retry model=%q) = %v, want allowed-models rejection naming accepted set", rejectedModel, rErr)
		}
	}

	fake := model.NewFakeClient(
		toolUseResponse(
			model.ToolCall{
				ID:    "spawn-bogus",
				Name:  dispatcher.ToolLaneSpawn,
				Input: json.RawMessage(`{"item_id":"mem-overhead","model":"gemini-9-bogus","brief":"Measure memory overhead.","reason":"try bogus model"}`),
			},
		),
		toolUseResponse(
			model.ToolCall{
				ID:    "spawn-argon",
				Name:  dispatcher.ToolLaneSpawn,
				Input: json.RawMessage(`{"item_id":"mem-overhead","model":"gemini-4-argon-high","brief":"Measure memory overhead.","reason":"owner named gemini-4-argon-high"}`),
			},
		),
		endTurnResponse("Spawned mem-overhead with gemini-4-argon-high."),
	)
	d := dispatcher.New(dispatcher.Config{}, st, drv, clk, fake)
	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "fund lane mem-overhead on gemini-4-argon-high",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0], `"gemini-9-bogus" is not in the allowed models list`) {
		t.Fatalf("Rejected = %+v, want rejection for gemini-9-bogus", res.Rejected)
	}
	if len(res.Accepted) != 1 || res.Accepted[0].Tool != dispatcher.ToolLaneSpawn {
		t.Fatalf("Accepted = %+v, want 1 accepted lane.spawn", res.Accepted)
	}
	if len(drv.Spawns) != 1 || drv.Spawns[0].Model != "gemini-4-argon-high" || drv.Spawns[0].Harness != "agy" {
		t.Fatalf("Spawns = %+v, want 1 spawn with Harness=agy Model=gemini-4-argon-high", drv.Spawns)
	}
}

func TestResolveWorkerAgentHarnessPreservesCompatibleConfiguredHarness(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model, configured, want string
	}{
		{"gpt-oss-120b-medium", "agy", "agy"},
		{"claude-sonnet-4-6", "agy", "agy"},
		{"gpt-6.1-sol", "claude", "codex"},
		{"gemini-4-argon-high", "codex", "agy"},
	} {
		t.Run(tc.model+"/"+tc.configured, func(t *testing.T) {
			if got := dispatcher.ResolveWorkerAgentHarness(tc.model, tc.configured); got != tc.want {
				t.Fatalf("ResolveWorkerAgentHarness(%q, %q) = %q, want %q", tc.model, tc.configured, got, tc.want)
			}
		})
	}
}
