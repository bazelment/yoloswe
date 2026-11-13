package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/replay"
)

type scriptedEvalClient struct {
	momentTools     map[string]model.ToolCall
	questionAnswers map[string]string
}

func lastToolResult(req model.Request) (string, bool) {
	if len(req.Messages) == 0 {
		return "", false
	}
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type == model.BlockTypeToolResult {
			return b.Content, true
		}
	}
	return "", false
}

func (s *scriptedEvalClient) CreateMessage(_ context.Context, req model.Request) (*model.Response, error) {
	result, haveResult := lastToolResult(req)

	userText := ""
	if len(req.Messages) > 0 {
		userText = req.Messages[0].Text()
	}

	// The concierge is only handed the run id and question, so a scripted
	// concierge must read the Store through the status tool and relay it.
	if strings.Contains(req.System, replay.ConciergeSystemPromptVersion) {
		if !haveResult {
			return &model.Response{
				StopReason: model.StopReasonToolUse,
				Message:    model.NewAssistantMessage(model.NewToolUseBlock("status-1", "status", json.RawMessage(`{}`))),
				Usage:      model.Usage{InputTokens: 900, OutputTokens: 20},
			}, nil
		}
		for qID, ans := range s.questionAnswers {
			if strings.Contains(userText, qID) {
				result = ans
			}
		}
		return &model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock(result)),
			Usage:      model.Usage{InputTokens: 1200, OutputTokens: 80},
		}, nil
	}

	if haveResult {
		return &model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Completed judgment action.")),
			Usage:      model.Usage{InputTokens: 2400, OutputTokens: 60},
		}, nil
	}

	// Event ids are prefixes of one another (ev-x-g1, ev-x-g10): the longest match wins.
	best := ""
	for evID := range s.momentTools {
		if strings.Contains(userText, evID) && len(evID) > len(best) {
			best = evID
		}
	}
	if tc, ok := s.momentTools[best]; ok {
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message:    model.NewAssistantMessage(model.NewToolUseBlock(tc.ID, tc.Name, tc.Input)),
			Usage:      model.Usage{InputTokens: 3200, OutputTokens: 110},
		}, nil
	}

	return &model.Response{
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock("No action needed.")),
		Usage:      model.Usage{InputTokens: 2500, OutputTokens: 40},
	}, nil
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestScrubbingValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{
			name:    "clean payload",
			payload: `{"id":"cicd-300-m001","session":"sess-cd-deploy-dev-1"}`,
			wantErr: "",
		},
		{
			name:    "leaked email",
			payload: `{"author":"owner@example.com"}`,
			wantErr: "unscrubbed email",
		},
		{
			name:    "leaked github token",
			payload: `{"token":"ghp_1234567890abcdef1234567890abcdef"}`,
			wantErr: "unscrubbed token",
		},
		{
			name:    "leaked raw uuid",
			payload: `{"session":"6cac9dc7-9196-4610-a102-8049ad6f460b"}`,
			wantErr: "unscrubbed raw UUID",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := replay.ValidateScrubbedBytes([]byte(tc.payload))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateScrubbedBytes unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateScrubbedBytes error = %v, want substring %q", err, tc.wantErr)
			}
			scrubbed := replay.ScrubText(tc.payload)
			if err := replay.ValidateScrubbedBytes([]byte(scrubbed)); err != nil {
				t.Fatalf("ScrubText output still failed validation: %v (scrubbed=%s)", err, scrubbed)
			}
		})
	}
}

func momentByID(t *testing.T, ds *replay.Dataset, id string) replay.JudgmentMoment {
	t.Helper()
	for i := range ds.Moments {
		if ds.Moments[i].ID == id {
			return ds.Moments[i]
		}
	}
	t.Fatalf("moment %q not in dataset", id)
	return replay.JudgmentMoment{}
}

// failedMoments lists the moments a report did not agree on, with the reasons.
func failedMoments(rep *replay.EvalReport) string {
	var b strings.Builder
	for _, mr := range rep.MomentResults {
		if !mr.Agreed || mr.InvariantViolations > 0 {
			fmt.Fprintf(&b, "  %s want %v%v got %v%v rejected=%v summary=%q\n", mr.ID, mr.ExpectedTools, mr.ExpectedTargets, mr.AcceptedTools, mr.AcceptedTargets, mr.RejectedReasons, mr.Summary)
		}
	}
	return b.String()
}

// oracleClient answers every moment with its own label: the first expected
// tool on the first expected target. It exercises the plumbing and the grader;
// it says nothing about a real model.
func oracleClient(t *testing.T, ds *replay.Dataset) *scriptedEvalClient {
	t.Helper()
	momentTools := make(map[string]model.ToolCall, len(ds.Moments))
	for i := range ds.Moments {
		m := &ds.Moments[i]
		evID := m.Events[0].ID
		tool := m.OwnerFollowUp.ExpectedTools[0]
		target := ""
		if len(m.OwnerFollowUp.ExpectedTargets) > 0 {
			target = m.OwnerFollowUp.ExpectedTargets[0]
		}
		var input json.RawMessage
		switch tool {
		case dispatcher.ToolLaneRetry:
			input = mustJSON(t, dispatcher.LaneRetryParams{
				LaneID: target,
				Phase:  "swe",
				Round:  2,
				Note:   "Retry on round 2.",
				Reason: "Resolve stall or rework on round 2.",
			})
		case dispatcher.ToolLaneSpawn:
			input = mustJSON(t, dispatcher.LaneSpawnParams{
				ItemID: target,
				LaneID: target,
				Brief:  "Implement and test item " + target,
				Reason: "Open priority backlog item ready with free capacity.",
			})
		case dispatcher.ToolLaneSteer:
			input = mustJSON(t, dispatcher.LaneSteerParams{
				LaneID: target,
				Note:   "Align onto merged shared contract on main.",
				Reason: "Standing rule: resolve contract alignment directly.",
			})
		case dispatcher.ToolBacklogAdd:
			input = mustJSON(t, dispatcher.BacklogAddParams{
				ID:         target,
				Title:      "Follow-up item " + target,
				Priority:   "p1",
				Acceptance: "Tests pass",
				Reason:     "Split out-of-scope defect into separate item.",
			})
		case dispatcher.ToolBacklogClose:
			input = mustJSON(t, dispatcher.BacklogCloseParams{
				ID:     target,
				Status: "done",
				Reason: "Lane PR merged into main.",
			})
		case dispatcher.ToolPlannerRequest:
			input = mustJSON(t, dispatcher.PlannerRequestParams{
				PlanRef: m.State.Run.PlanRef,
				Reason:  "Owner requested backlog replan.",
			})
		case dispatcher.ToolDirectiveNote:
			input = mustJSON(t, dispatcher.DirectiveNoteParams{
				Fact:   "Upstream CI queue latency elevated.",
				Scope:  "ci-queue",
				Expiry: "6h",
				Reason: "Record operational observation.",
			})
		case dispatcher.ToolOwnerAsk:
			input = mustJSON(t, dispatcher.OwnerAskParams{
				LaneID:   target,
				Question: "Grant GitHub App org permissions now or defer until impl merges?",
				Options:  []string{"defer-until-impl-merged", "grant-now"},
				Default:  "defer-until-impl-merged",
				Deadline: "4h",
				Reason:   "Requires owner browser approval on production org.",
			})
		default:
			t.Fatalf("unexpected expected_tool %q in moment %s", tool, m.ID)
		}
		momentTools[evID] = model.ToolCall{
			ID:    "call-" + m.ID,
			Name:  tool,
			Input: input,
		}
	}

	return &scriptedEvalClient{momentTools: momentTools}
}

// TestInnerLoop10MomentsEndToEnd is the smallest end-to-end case: 10 grounded
// moments and 2 owner status questions through the strict grader.
func TestInnerLoop10MomentsEndToEnd(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}
	if len(ds.Moments) != 10 || len(ds.Questions) != 2 {
		t.Fatalf("inner loop has %d moments / %d questions, want 10 / 2", len(ds.Moments), len(ds.Questions))
	}
	client := oracleClient(t, ds)

	rep, err := replay.Evaluate(t.Context(), ds, replay.EvalConfig{Client: client, Concurrency: 2})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if rep.GroundedMoments != 10 || rep.SyntheticMoments != 0 {
		t.Fatalf("grounded/synthetic = %d/%d, want 10/0", rep.GroundedMoments, rep.SyntheticMoments)
	}
	if rep.DispatcherAgreed != 10 {
		t.Fatalf("label oracle agreed on %d/10:\n%s%s", rep.DispatcherAgreed, failedMoments(rep), rep.FormatReport())
	}
	if rep.EvidencedCorrections != 2 || rep.DecideDontAskMoments != 3 {
		t.Fatalf("corrections=%d decide-dont-ask=%d, want 2 and 3", rep.EvidencedCorrections, rep.DecideDontAskMoments)
	}
	if rep.InvariantViolations != 0 {
		t.Fatalf("InvariantViolations = %d, want 0", rep.InvariantViolations)
	}
	if rep.ConciergeAccuracy != 1.0 {
		t.Fatalf("ConciergeAccuracy = %.2f, want 1.0", rep.ConciergeAccuracy)
	}
	if rep.MedianInputTokens <= 0 || rep.MedianInputTokens >= replay.PassBarMaxMedianInputTokens {
		t.Fatalf("MedianInputTokens = %d, want in (0, %d)", rep.MedianInputTokens, replay.PassBarMaxMedianInputTokens)
	}
}

// Moment and question counts of the committed Gate 2 dataset. They are what
// the owner export supports and not a target: the export holds too few owner
// status questions to reach 30 (see the provenance rubric).
const (
	wantFullMoments   = 40
	wantFullQuestions = 4
)

func TestFullGate2DatasetStructureAndDeterministicEval(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatalf("LoadDefaultDataset: %v", err)
	}
	if len(ds.Moments) != wantFullMoments || len(ds.Questions) != wantFullQuestions {
		t.Fatalf("moments/questions = %d/%d, want %d/%d (recount with the rubric before changing)", len(ds.Moments), len(ds.Questions), wantFullMoments, wantFullQuestions)
	}

	runCounts := make(map[string]int)
	for i := range ds.Moments {
		m := &ds.Moments[i]
		runCounts[m.RunID]++
		if m.Grounding != replay.GroundingVerified {
			t.Fatalf("moment %s is %q; the dataset may only hold verified moments", m.ID, m.Grounding)
		}
	}
	for _, wantRun := range []string{"20261103-cicd-300", "20261026-feedback", "20261023-s1-main"} {
		if runCounts[wantRun] == 0 {
			t.Fatalf("no moments extracted for run %q", wantRun)
		}
	}

	rep, err := replay.Evaluate(t.Context(), ds, replay.EvalConfig{Client: oracleClient(t, ds), Concurrency: 8})
	if err != nil {
		t.Fatalf("Evaluate full dataset: %v", err)
	}
	// An oracle that replays each label satisfies every measured bar, and the
	// verdict is PASS only because the owner corrections and decide-don't-ask
	// moments are evidenced in the export.
	if !rep.BarsPass() {
		t.Fatalf("BarsPass() = false for the label oracle:\n%s%s", failedMoments(rep), rep.FormatReport())
	}
	if rep.AgreementRate != 1.0 || rep.GroundedMoments != wantFullMoments || rep.SyntheticMoments != 0 {
		t.Fatalf("oracle agreement=%.2f grounded/synthetic=%d/%d, want 1.0 and %d/0:\n%s", rep.AgreementRate, rep.GroundedMoments, rep.SyntheticMoments, wantFullMoments, rep.FormatReport())
	}
	if rep.EvidencedCorrections != 2 || rep.DecideDontAskMoments != 3 || rep.Verdict() != replay.VerdictPass {
		t.Fatalf("verdict %s, corrections %d, decide-dont-ask %d: want PASS/2/3", rep.Verdict(), rep.EvidencedCorrections, rep.DecideDontAskMoments)
	}
}

type multiTurnWireClient struct {
	laneID string
	turns  int
}

func (m *multiTurnWireClient) CreateMessage(_ context.Context, req model.Request) (*model.Response, error) {
	m.turns++
	for _, t := range req.Tools {
		if strings.ContainsRune(t.Name, '.') {
			return nil, errors.New("tool definition name on wire still contains '.': " + t.Name)
		}
	}
	switch m.turns {
	case 1:
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(model.NewToolUseBlock(
				"call-read-1",
				"read_lane_report",
				json.RawMessage(`{"lane_id":"`+m.laneID+`"}`),
			)),
			Usage: model.Usage{
				InputTokens:              1000,
				CacheCreationInputTokens: 500,
				CacheReadInputTokens:     200,
				OutputTokens:             40,
			},
		}, nil
	case 2:
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(model.NewToolUseBlock(
				"call-retry-2",
				"lane_steer",
				json.RawMessage(`{"lane_id":"`+m.laneID+`","note":"Hold the upload path off until the switch lands.","reason":"Owner order relayed by the host."}`),
			)),
			Usage: model.Usage{
				InputTokens:          1100,
				CacheReadInputTokens: 300,
				OutputTokens:         50,
			},
		}, nil
	default:
		return &model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Steered lane " + m.laneID + " after reading its report.")),
			Usage: model.Usage{
				InputTokens:  900,
				OutputTokens: 30,
			},
		}, nil
	}
}

func TestWireToolClientReadLaneReportThenMutateAndCacheTokens(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}
	steer := momentByID(t, ds, "cicd-300-g15")
	rawClient := &multiTurnWireClient{laneID: steer.OwnerFollowUp.ExpectedTargets[0]}
	wireClient := replay.NewWireToolClient(rawClient)

	mr, err := replay.EvaluateMoment(t.Context(), steer, wireClient, dispatcher.Config{MaxTurns: 4})
	if err != nil {
		t.Fatalf("EvaluateMoment: %v", err)
	}
	if rawClient.turns != 3 {
		t.Fatalf("rawClient.turns = %d, want 3 (read.lane_report -> lane.steer -> final summary)", rawClient.turns)
	}
	if !mr.Agreed {
		t.Fatalf("mr.Agreed = false, want true (acceptedTools=%v acceptedTargets=%v)", mr.AcceptedTools, mr.AcceptedTargets)
	}
	wantTokens := int64((1000 + 500 + 200) + (1100 + 300) + 900)
	if mr.InputTokens != wantTokens {
		t.Fatalf("mr.InputTokens = %d, want %d (including cache creation and cache read tokens)", mr.InputTokens, wantTokens)
	}
}

type staticActionClient struct {
	calls []model.ToolCall
	usage model.Usage
}

func (s *staticActionClient) CreateMessage(_ context.Context, req model.Request) (*model.Response, error) {
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1]
		for _, b := range last.Content {
			if b.Type == model.BlockTypeToolResult {
				return &model.Response{
					StopReason: model.StopReasonEndTurn,
					Message:    model.NewAssistantMessage(model.NewTextBlock("Done.")),
				}, nil
			}
		}
	}
	var blocks []model.ContentBlock
	for _, tc := range s.calls {
		blocks = append(blocks, model.NewToolUseBlock(tc.ID, tc.Name, tc.Input))
	}
	return &model.Response{
		StopReason: model.StopReasonToolUse,
		Message:    model.NewAssistantMessage(blocks...),
		Usage:      s.usage,
	}, nil
}

func TestNegativeAgreementAndComputeReportBars(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}

	t.Run("substring target mismatch rejected", func(t *testing.T) {
		t.Parallel()
		var m replay.JudgmentMoment
		raw := `{
			"id": "sub-1",
			"run_id": "run-sub",
			"description": "Ensure lane-1 does not match lane-10",
			"state": {
				"run": {"id": "run-sub", "objective": "test"},
				"lanes": [
					{"id": "lane-1", "run_id": "run-sub", "status": "blocked", "phase": "swe", "round": 1},
					{"id": "lane-10", "run_id": "run-sub", "status": "blocked", "phase": "swe", "round": 1}
				]
			},
			"events": [{"id": "ev-sub", "run_id": "run-sub", "kind": "stall", "class": "judgment", "lane_id": "lane-10", "summary": "stall"}],
			"baseline": {"tool": "lane.retry", "target": "lane-10", "summary": "retry"},
			"owner_follow_up": {
				"verdict": "endorsed",
				"expected_tools": ["lane.retry"],
				"expected_targets": ["lane-10"]
			}
		}`
		if uErr := json.Unmarshal([]byte(raw), &m); uErr != nil {
			t.Fatalf("json.Unmarshal: %v", uErr)
		}
		client := &staticActionClient{
			calls: []model.ToolCall{{
				ID:    "c1",
				Name:  dispatcher.ToolLaneRetry,
				Input: mustJSON(t, dispatcher.LaneRetryParams{LaneID: "lane-1", Phase: "swe", Round: 2, Reason: "wrong lane"}),
			}},
		}
		mr, evalErr := replay.EvaluateMoment(t.Context(), m, client, dispatcher.Config{})
		if evalErr != nil {
			t.Fatalf("EvaluateMoment: %v", evalErr)
		}
		if mr.Agreed {
			t.Fatalf("mr.Agreed = true for target lane-1 against expected lane-10, want false")
		}
	})

	t.Run("unexpected extra mutating action rejected", func(t *testing.T) {
		t.Parallel()
		m := momentByID(t, ds, "cicd-300-g15")
		client := &staticActionClient{
			calls: []model.ToolCall{
				{
					ID:    "c1",
					Name:  dispatcher.ToolLaneSteer,
					Input: mustJSON(t, dispatcher.LaneSteerParams{LaneID: m.OwnerFollowUp.ExpectedTargets[0], Note: "Hold the upload path off.", Reason: "expected steer"}),
				},
				{
					ID:    "c2",
					Name:  dispatcher.ToolBacklogAdd,
					Input: mustJSON(t, dispatcher.BacklogAddParams{ID: "unexpected-item", Title: "Unrelated", Priority: "p2", Reason: "spurious"}),
				},
			},
		}
		mr, evalErr := replay.EvaluateMoment(t.Context(), m, client, dispatcher.Config{})
		if evalErr != nil {
			t.Fatalf("EvaluateMoment: %v", evalErr)
		}
		if mr.Agreed {
			t.Fatalf("mr.Agreed = true when an unexpected second mutating action was accepted, want false")
		}
	})

	t.Run("owner.ask on decide_dont_ask fails agreement and ask rate bar", func(t *testing.T) {
		t.Parallel()
		m := momentByID(t, ds, "cicd-300-g18") // decide, don't ask
		client := &staticActionClient{
			calls: []model.ToolCall{{
				ID:   "c-ask",
				Name: dispatcher.ToolOwnerAsk,
				Input: mustJSON(t, dispatcher.OwnerAskParams{
					LaneID:   m.State.Lanes[0].ID,
					Question: "Should the split go ahead as proposed?",
					Options:  []string{"go", "hold"},
					Default:  "go",
					Deadline: "1h",
					Reason:   "Unnecessary ask",
				}),
			}},
			usage: model.Usage{InputTokens: 35000},
		}
		mr, evalErr := replay.EvaluateMoment(t.Context(), m, client, dispatcher.Config{})
		if evalErr != nil {
			t.Fatalf("EvaluateMoment: %v", evalErr)
		}
		if mr.Agreed {
			t.Fatal("mr.Agreed = true on forbidden owner.ask, want false")
		}
		if !mr.AskedOwner {
			t.Fatal("mr.AskedOwner = false, want true")
		}

		rep := replay.ComputeReport([]replay.MomentResult{mr}, []replay.QuestionResult{{
			ID:                "q-bad",
			MissingFacts:      []string{"missing-fact"},
			ViolatedForbidden: []string{"hallucinated-pr"},
			Accurate:          false,
		}})
		if rep.AllPass() {
			t.Fatal("rep.AllPass() = true when bars failed, want false")
		}
		if rep.AskRatePass {
			t.Fatal("rep.AskRatePass = true when unnecessary ask rate is 100%, want false")
		}
		if rep.ConciergePass {
			t.Fatal("rep.ConciergePass = true when accuracy is 0%, want false")
		}
		if rep.TokensPass {
			t.Fatal("rep.TokensPass = true when median tokens is 35000 >= 30000, want false")
		}
	})
}

func TestAuditAcceptedActionsViolations(t *testing.T) {
	t.Parallel()

	var state replay.MomentState
	rawState := `{
		"run": {"id": "run-audit", "objective": "audit test", "default_recipe": "default"},
		"backlog": [
			{"id": "item-new", "run_id": "run-audit", "title": "New item", "priority": "p1", "status": "open"}
		],
		"lanes": [
			{"id": "ln-1", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-2", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-3", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-4", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-5", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-6", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-7", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-8", "run_id": "run-audit", "status": "running", "phase": "swe", "round": 1},
			{"id": "ln-stale", "run_id": "run-audit", "status": "blocked", "phase": "review", "round": 1, "pr": 101, "pr_head": "sha-new", "approval_sha": "sha-old", "checks": "pending"}
		]
	}`
	if err := json.Unmarshal([]byte(rawState), &state); err != nil {
		t.Fatalf("json.Unmarshal state: %v", err)
	}

	violations := replay.AuditAcceptedActions(state, []dispatcher.ProposedAction{
		{
			Tool: dispatcher.ToolLaneSpawn,
			LaneSpawn: &dispatcher.LaneSpawnParams{
				ItemID: "item-new",
				LaneID: "ln-new",
				Brief:  "Use token ghp_1234567890abcdef1234567890abcdef to push.",
			},
		},
		{
			Tool: dispatcher.ToolLaneRetry,
			LaneRetry: &dispatcher.LaneRetryParams{
				LaneID: "ln-stale",
				Phase:  "integrate",
				Round:  2,
				Note:   "Retry integrate.",
			},
		},
	})
	if len(violations) != 3 {
		t.Fatalf("len(violations) = %d (%v), want 3 (concurrency cap, secret token leak, stale approval integrate)", len(violations), violations)
	}
	joinedViolations := strings.Join(violations, " | ")
	for _, wantSub := range []string{"concurrency limit", "secret token leaked", "approval"} {
		if !strings.Contains(strings.ToLower(joinedViolations), wantSub) {
			t.Errorf("violations %v missing expected substring %q", violations, wantSub)
		}
	}
}

func TestDatasetValidateRejections(t *testing.T) {
	t.Parallel()

	var nilDS *replay.Dataset
	if err := nilDS.Validate(); err == nil {
		t.Fatal("nil Dataset.Validate() = nil, want error")
	}

	emptyDS := &replay.Dataset{}
	if err := emptyDS.Validate(); err == nil {
		t.Fatal("empty Dataset.Validate() = nil, want error")
	}

	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}

	dupMoments := *ds
	dupMoments.Moments = append([]replay.JudgmentMoment{ds.Moments[0]}, ds.Moments...)
	if err := dupMoments.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate moment id") {
		t.Fatalf("duplicate moment Validate() = %v, want duplicate moment id error", err)
	}

	badRunID := *ds
	badRunID.Moments = append([]replay.JudgmentMoment(nil), ds.Moments...)
	badRunID.Moments[0].RunID = "mismatch-run"
	if err := badRunID.Validate(); err == nil || !strings.Contains(err.Error(), "mismatched run_id") {
		t.Fatalf("mismatched run_id Validate() = %v, want mismatched run_id error", err)
	}

	badQuestion := *ds
	badQuestion.Questions = append([]replay.ConciergeQuestion(nil), ds.Questions...)
	badQuestion.Questions[0].RequiredFacts = nil
	if err := badQuestion.Validate(); err == nil || !strings.Contains(err.Error(), "no required_facts") {
		t.Fatalf("empty required_facts Validate() = %v, want no required_facts error", err)
	}
}

type cancellingClient struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *cancellingClient) CreateMessage(ctx context.Context, _ model.Request) (*model.Response, error) {
	if c.calls.Add(1) == 1 {
		c.cancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &model.Response{
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock("ok")),
	}, nil
}

func TestEvaluateContextCancellation(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}

	for _, workers := range []int{1, 4} {
		ctx, cancel := context.WithCancel(t.Context())
		client := &cancellingClient{cancel: cancel}
		_, evalErr := replay.Evaluate(ctx, ds, replay.EvalConfig{
			Client:      client,
			Concurrency: workers,
		})
		if evalErr == nil {
			t.Fatalf("Evaluate(workers=%d) with cancelled context returned nil error, want non-nil", workers)
		}
	}
}

type conciergeToolExerciserClient struct {
	laneIDs []string
	turn    int
}

func (c *conciergeToolExerciserClient) CreateMessage(_ context.Context, req model.Request) (*model.Response, error) {
	c.turn++
	switch c.turn {
	case 1:
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("t1", "status", json.RawMessage(`{}`)),
				model.NewToolUseBlock("t2", "lane_show", json.RawMessage(`{"lane_id":"`+c.laneIDs[0]+`"}`)),
				model.NewToolUseBlock("t3", "pr_status", json.RawMessage(`{"lane_id":"`+c.laneIDs[0]+`"}`)),
				model.NewToolUseBlock("t4", "ci_status", json.RawMessage(`{"lane_id":"`+c.laneIDs[0]+`"}`)),
				model.NewToolUseBlock("t5", "decisions_since", json.RawMessage(`{"since":"0","limit":5}`)),
				model.NewToolUseBlock("t6", "directive_submit", json.RawMessage(`{"kind":"instruction","text":"Hold merges on `+c.laneIDs[0]+`"}`)),
				model.NewToolUseBlock("t7", "directive_submit", json.RawMessage(`{"kind":"note","text":"Second directive under same FakeClock timestamp"}`)),
			),
			Usage: model.Usage{InputTokens: 800, CacheReadInputTokens: 200, OutputTokens: 100},
		}, nil
	default:
		var directiveIDs []string
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == model.BlockTypeToolResult {
					if b.IsError {
						return nil, errors.New("unexpected tool error in concierge: " + b.Content)
					}
					if strings.HasPrefix(b.Content, "submitted directive ") {
						directiveIDs = append(directiveIDs, strings.TrimPrefix(b.Content, "submitted directive "))
					}
				}
			}
		}
		if len(directiveIDs) != 2 || directiveIDs[0] == directiveIDs[1] {
			return nil, errors.New("expected 2 distinct directive IDs under FakeClock")
		}
		return &model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Running now: " + strings.Join(c.laneIDs, ", ") + ".")),
			Usage:      model.Usage{InputTokens: 900, OutputTokens: 60},
		}, nil
	}
}

func TestStoreConciergeToolsAndDirectiveSubmit(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}
	q := ds.Questions[0]
	var laneIDs []string
	for _, fg := range q.RequiredFacts {
		laneIDs = append(laneIDs, fg.AnyOf...)
	}
	client := &conciergeToolExerciserClient{laneIDs: laneIDs}
	concierge := replay.NewStoreConcierge(client)

	qr, err := replay.EvaluateQuestion(t.Context(), q, concierge)
	if err != nil {
		t.Fatalf("EvaluateQuestion: %v", err)
	}
	if !qr.Accurate {
		t.Fatalf("qr.Accurate = false (missing=%v violated=%v answer=%q)", qr.MissingFacts, qr.ViolatedForbidden, qr.Answer)
	}
	if len(qr.ToolCalls) != 7 {
		t.Fatalf("len(qr.ToolCalls) = %d (%v), want 7", len(qr.ToolCalls), qr.ToolCalls)
	}
	if qr.InputTokens != 1900 {
		t.Fatalf("qr.InputTokens = %d, want 1900", qr.InputTokens)
	}
}
