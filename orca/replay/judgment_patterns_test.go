package replay_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/replay"
)

// recordedOutputClient replays recorded tool calls as one parallel tool_use
// turn; any later reply ends the turn. It keeps every request so tests can read
// the snapshot and tools the model was shown.
type recordedOutputClient struct {
	calls []model.ToolCall
	reqs  []model.Request
	mu    sync.Mutex
}

func (c *recordedOutputClient) CreateMessage(_ context.Context, req model.Request) (*model.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req.Clone())
	if len(c.reqs) == 1 && len(c.calls) > 0 {
		blocks := make([]model.ContentBlock, 0, len(c.calls))
		for _, tc := range c.calls {
			blocks = append(blocks, model.NewToolUseBlock(tc.ID, tc.Name, tc.Input))
		}
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message:    model.NewAssistantMessage(blocks...),
			Usage:      model.Usage{InputTokens: 3000, OutputTokens: 100},
		}, nil
	}
	return &model.Response{
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock("done")),
		Usage:      model.Usage{InputTokens: 3000, OutputTokens: 10},
	}, nil
}

func call(id, name, args string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Input: json.RawMessage(args)}
}

// replayRecorded runs a dataset moment through the real Dispatcher with the
// recorded tool calls as the model's output.
func replayRecorded(t *testing.T, momentID string, calls ...model.ToolCall) (replay.MomentResult, *recordedOutputClient) {
	t.Helper()
	ds, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatalf("LoadDefaultDataset: %v", err)
	}
	client := &recordedOutputClient{calls: calls}
	res, err := replay.EvaluateMoment(t.Context(), momentByID(t, ds, momentID), client, dispatcher.Config{MaxTurns: 4})
	if err != nil {
		t.Fatalf("EvaluateMoment(%s): %v", momentID, err)
	}
	return res, client
}

func snapshotOf(c *recordedOutputClient) string { return c.reqs[0].Messages[0].Text() }

// Pattern 1: extra mutating actions on top of the expected one. The recorded
// outputs are the pre-fix live calls; the grader scores them as misses, and the
// prompt's one-primary-action rule is pinned here.
func TestPattern_ExtraMutatingActionsAreMissesAndPromptForbidsThem(t *testing.T) {
	t.Parallel()

	// planner.request already stores its reason as a directive, so the trailing
	// directive.note is a redundant second action: rejected, and counted as a
	// violation by the grader.
	extra, _ := replayRecorded(t, "cicd-300-g14",
		call("t1", "planner.request", `{"reason":"owner wants a v3 replan about work, not capacity","urgent":true}`),
		call("t2", "directive.note", `{"fact":"capacity is not the dispatcher's concern; keep lanes full","reason":"owner rule"}`),
	)
	if extra.Agreed || len(extra.AcceptedTools) != 1 || extra.InvariantViolations != 1 ||
		!strings.Contains(strings.Join(extra.RejectedReasons, "|"), "one primary action per batch") {
		t.Fatalf("planner.request + directive.note: agreed=%v tools=%v violations=%d rejected=%q, want one applied action and one contract rejection",
			extra.Agreed, extra.AcceptedTools, extra.InvariantViolations, extra.RejectedReasons)
	}
	single, _ := replayRecorded(t, "cicd-300-g14",
		call("t1", "planner.request", `{"reason":"owner wants a v3 replan about work, not capacity","urgent":true}`),
	)
	if !single.Agreed || single.InvariantViolations != 0 {
		t.Fatalf("single planner.request: agreed=%v violations=%d rejected=%v", single.Agreed, single.InvariantViolations, single.RejectedReasons)
	}

	for _, want := range []string{
		"One primary action per batch",
		"redundant `directive.note`",
		"stores it as an active directive",
		"Act first, stay terse",
	} {
		if !strings.Contains(dispatcher.SystemPrompt, want) {
			t.Errorf("system prompt lost %q", want)
		}
	}
	if dispatcher.SystemPromptVersion == "dispatcher-v1" {
		t.Errorf("prompt changed but SystemPromptVersion is still %s", dispatcher.SystemPromptVersion)
	}
}

// Pattern 2: backlog.update to in_progress before lane.spawn, after which the
// spawn is rejected because the item is already taken.
func TestPattern_SpawnImpliesInProgress(t *testing.T) {
	t.Parallel()

	const spawn = `{"item_id":"item-fb407-kernel-bom-grant","brief":"Implement the BOM grant as the owner specified.","reason":"owner ordered it"}`
	before, _ := replayRecorded(t, "feedback-g01",
		call("t1", "backlog.update", `{"id":"item-fb407-kernel-bom-grant","status":"in_progress","reason":"claim before spawn"}`),
		call("t2", "lane.spawn", spawn),
	)
	if before.Agreed || before.InvariantViolations == 0 {
		t.Fatalf("update-then-spawn: agreed=%v violations=%d, want a counted rejection", before.Agreed, before.InvariantViolations)
	}
	if !slices.ContainsFunc(before.RejectedReasons, func(r string) bool {
		return strings.Contains(r, "backlog.update") && strings.Contains(r, "lane.spawn")
	}) {
		t.Errorf("rejection must name lane.spawn as the owner of in_progress, got %q", before.RejectedReasons)
	}

	after, client := replayRecorded(t, "feedback-g01", call("t1", "lane.spawn", spawn))
	if !after.Agreed || after.InvariantViolations != 0 {
		t.Fatalf("spawn alone: agreed=%v violations=%d rejected=%v", after.Agreed, after.InvariantViolations, after.RejectedReasons)
	}

	// The contract the model sees: backlog.update cannot even express in_progress,
	// and lane.spawn's item_id says that spawning claims the item itself.
	for _, def := range client.reqs[0].Tools {
		raw, err := json.Marshal(def.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", def.Name, err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string   `json:"description"`
				Enum        []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode %s schema: %v", def.Name, err)
		}
		switch def.Name {
		case dispatcher.ToolBacklogUpdate:
			if status := schema.Properties["status"]; len(status.Enum) == 0 || slices.Contains(status.Enum, "in_progress") {
				t.Errorf("backlog.update status enum = %v, want a non-empty list without in_progress", status.Enum)
			}
		case dispatcher.ToolLaneSpawn:
			if desc := schema.Properties["item_id"].Description; !strings.Contains(desc, "do not call backlog.update first") {
				t.Errorf("lane.spawn item_id description lost the spawn-claims-the-item rule: %q", desc)
			}
		case dispatcher.ToolOwnerAsk:
			// Descriptions are cut at the first comma unless they use jsonschema_description.
			if desc := schema.Properties["default"].Description; !strings.Contains(desc, "can be passed as is") {
				t.Errorf("owner.ask default description truncated: %q", desc)
			}
			if desc := schema.Properties["deadline"].Description; !strings.Contains(desc, "05:30Z") {
				t.Errorf("owner.ask deadline description truncated: %q", desc)
			}
		}
	}
	if !strings.Contains(snapshotOf(client), "open ready") {
		t.Errorf("snapshot does not mark spawnable backlog items as ready:\n%s", snapshotOf(client))
	}
}

// Pattern 3: no action (or an owner.ask) on an owner order to fund a lane. The
// order's ASK line sat beyond the 512-byte event-summary cut, so the model
// never saw it.
func TestPattern_OwnerOrderIsVisibleAndActedOn(t *testing.T) {
	t.Parallel()

	ask, client := replayRecorded(t, "cicd-300-g04",
		call("t1", "owner.ask", `{"question":"Pivot to the direct-merge model?","options":["continue","pivot"],"default":"continue","deadline":"2h"}`),
	)
	if ask.Agreed {
		t.Fatalf("asking the owner to confirm a funded order must be a miss: %+v", ask)
	}
	snap := snapshotOf(client)
	if !strings.Contains(snap, "ASK: fund one P1 lane") {
		t.Fatalf("the owner's order is cut out of the snapshot:\n%s", snap)
	}
	if strings.Contains(snap, "lane=-") {
		t.Errorf("run-level event still prints a lane=- placeholder")
	}

	spawn, _ := replayRecorded(t, "cicd-300-g04",
		call("t1", "lane.spawn", `{"item_id":"item-merge-model-1k","brief":"Analysis only: produce the proposal the owner described; no PR, no live changes.","reason":"owner funded this lane"}`),
	)
	if !spawn.Agreed || spawn.InvariantViolations != 0 {
		t.Fatalf("lane.spawn for the funded item: agreed=%v violations=%d rejected=%v", spawn.Agreed, spawn.InvariantViolations, spawn.RejectedReasons)
	}
	for _, want := range []string{"Owner words are orders", "Do not ask the owner to confirm an order they already gave"} {
		if !strings.Contains(dispatcher.SystemPrompt, want) {
			t.Errorf("system prompt lost %q", want)
		}
	}
}

// Pattern 4: owner.ask where decide-don't-ask applies.
func TestPattern_DecideDontAskIsAStandingDirective(t *testing.T) {
	t.Parallel()

	asked, _ := replayRecorded(t, "cicd-300-g18",
		call("t1", "owner.ask", `{"question":"Approve the Stage A flip?","options":["approve","keep queue"],"default":"keep queue","deadline":"1h"}`),
	)
	if asked.Agreed || !asked.AskedOwner {
		t.Fatalf("owner.ask on a decide-don't-ask moment: agreed=%v asked=%v, want a counted unnecessary ask", asked.Agreed, asked.AskedOwner)
	}
	noted, _ := replayRecorded(t, "cicd-300-g18",
		call("t1", "directive.note", `{"fact":"Stage A fast merge is agreed; flip with the stated stop rule and rollback.","reason":"own outgoing report; decision already agreed"}`),
	)
	if !noted.Agreed || noted.AskedOwner {
		t.Fatalf("directive.note: agreed=%v asked=%v rejected=%v", noted.Agreed, noted.AskedOwner, noted.RejectedReasons)
	}
	for _, want := range []string{"standing owner directive", "never re-ask", "is your own outgoing report"} {
		if !strings.Contains(dispatcher.SystemPrompt, want) {
			t.Errorf("system prompt lost %q", want)
		}
	}
}

// A decision card relays through owner.ask even when the card's own stated
// default is not one of its listed options, and with a clock-time deadline: the
// two shapes that used to be rejected as invariant violations.
func TestPattern_OwnerCardRelayShapesAreAccepted(t *testing.T) {
	t.Parallel()

	res, _ := replayRecorded(t, "s1-main-g16",
		call("t1", "owner.ask", `{"question":"Require finite idle ceilings on new paid sessions?","options":["Apply 10/30 defaults and 30/60 ceilings","Keep unrestricted opt-out","Observation only"],"default":"Design only; existing behavior remains","deadline":"23:00Z"}`),
	)
	if !res.Agreed || res.InvariantViolations != 0 {
		t.Fatalf("card relay: agreed=%v violations=%d rejected=%v", res.Agreed, res.InvariantViolations, res.RejectedReasons)
	}
}

func TestSplitMomentIDsIsDeterministicAndBalanced(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatalf("LoadDefaultDataset: %v", err)
	}
	ids := make([]string, len(ds.Moments))
	for i := range ds.Moments {
		ids[i] = ds.Moments[i].ID
	}
	tune, holdout := replay.SplitMomentIDs(ids)
	if len(tune) != len(ids)/2 || len(holdout) != len(ids)-len(ids)/2 {
		t.Fatalf("split = %d/%d of %d moments, want a half each", len(tune), len(holdout), len(ids))
	}
	for _, id := range tune {
		if slices.Contains(holdout, id) {
			t.Fatalf("%s is in both halves", id)
		}
	}
	rev := slices.Clone(ids)
	slices.Reverse(rev)
	tune2, holdout2 := replay.SplitMomentIDs(rev)
	if !slices.Equal(tune, tune2) || !slices.Equal(holdout, holdout2) {
		t.Fatalf("split depends on dataset order")
	}

	sub, err := ds.Subset(replay.SplitTune)
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if len(sub.Moments) != len(tune) || len(sub.Questions) != 0 {
		t.Fatalf("tune subset has %d moments / %d questions, want %d / 0", len(sub.Moments), len(sub.Questions), len(tune))
	}
	if _, err := ds.Subset("nope"); err == nil {
		t.Fatalf("Subset(unknown split) must fail")
	}
}
