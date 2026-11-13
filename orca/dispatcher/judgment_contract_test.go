package dispatcher_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

func toolUseResponse(calls ...model.ToolCall) model.Response {
	blocks := make([]model.ContentBlock, len(calls))
	for i, c := range calls {
		blocks[i] = model.NewToolUseBlock(c.ID, c.Name, c.Input)
	}
	return model.Response{StopReason: model.StopReasonToolUse, Message: model.NewAssistantMessage(blocks...)}
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
