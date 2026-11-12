package replay_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orch/dispatcher"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/replay"
)

type scriptedEvalClient struct {
	momentTools     map[string]model.ToolCall
	questionAnswers map[string]string
}

func (s *scriptedEvalClient) CreateMessage(_ context.Context, req model.Request) (*model.Response, error) {
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1]
		for _, b := range last.Content {
			if b.Type == model.BlockTypeToolResult {
				return &model.Response{
					StopReason: model.StopReasonEndTurn,
					Message:    model.NewAssistantMessage(model.NewTextBlock("Completed judgment action.")),
					Usage:      model.Usage{InputTokens: 2400, OutputTokens: 60},
				}, nil
			}
		}
	}

	userText := ""
	if len(req.Messages) > 0 {
		userText = req.Messages[0].Text()
	}

	if strings.Contains(req.System, replay.ConciergeSystemPromptVersion) {
		for qID, ans := range s.questionAnswers {
			if strings.Contains(userText, qID) {
				return &model.Response{
					StopReason: model.StopReasonEndTurn,
					Message:    model.NewAssistantMessage(model.NewTextBlock(ans)),
					Usage:      model.Usage{InputTokens: 1200, OutputTokens: 80},
				}, nil
			}
		}
		return &model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock(userText)),
			Usage:      model.Usage{InputTokens: 1200, OutputTokens: 80},
		}, nil
	}

	for evID, tc := range s.momentTools {
		if strings.Contains(userText, evID) {
			return &model.Response{
				StopReason: model.StopReasonToolUse,
				Message:    model.NewAssistantMessage(model.NewToolUseBlock(tc.ID, tc.Name, tc.Input)),
				Usage:      model.Usage{InputTokens: 3200, OutputTokens: 110},
			}, nil
		}
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

func TestInnerLoop5MomentsEndToEnd(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}
	if len(ds.Moments) != 5 {
		t.Fatalf("len(ds.Moments) = %d, want 5", len(ds.Moments))
	}
	if len(ds.Questions) != 5 {
		t.Fatalf("len(ds.Questions) = %d, want 5", len(ds.Questions))
	}

	client := &scriptedEvalClient{
		momentTools: map[string]model.ToolCall{
			"ev-cicd-m001": {
				ID:   "call-1",
				Name: dispatcher.ToolLaneRetry,
				Input: mustJSON(t, dispatcher.LaneRetryParams{
					LaneID: "cd-deploy-dev",
					Phase:  "swe",
					Round:  2,
					Note:   "Retry swe on round 2 after wake cooldown exit 75.",
					Reason: "Wake cooldown ended; retry on fresh round 2.",
				}),
			},
			"ev-cicd-m002": {
				ID:   "call-2",
				Name: dispatcher.ToolLaneSteer,
				Input: mustJSON(t, dispatcher.LaneSteerParams{
					LaneID: "cd-deploy-dev",
					Note:   "Adopt cd-publish (#5124) shared manifest validator schema.",
					Reason: "Per standing rule, align with merged shared schema without asking owner.",
				}),
			},
			"ev-feedback-m001": {
				ID:   "call-3",
				Name: dispatcher.ToolLaneSpawn,
				Input: mustJSON(t, dispatcher.LaneSpawnParams{
					ItemID: "fb880-release-backup",
					LaneID: "fb880-release-backup",
					Brief:  "Fix code release --backup so release aborts when backup fails.",
					Reason: "P0 data-safety item ready with available lane capacity.",
				}),
			},
			"ev-feedback-m002": {
				ID:   "call-4",
				Name: dispatcher.ToolPlannerRequest,
				Input: mustJSON(t, dispatcher.PlannerRequestParams{
					PlanRef: "post:feedback-reliability-v2",
					Reason:  "Owner requested goal reassessment and backlog replan.",
				}),
			},
			"ev-s1-m001": {
				ID:   "call-5",
				Name: dispatcher.ToolBacklogAdd,
				Input: mustJSON(t, dispatcher.BacklogAddParams{
					ID:         "fsck-refusal-branches",
					Title:      "Clean up fsck refusal branches on sleep copy rejection",
					Priority:   "p1",
					Acceptance: "Refusal branches cleaned up and covered by unit test",
					Reason:     "Split separate defect discovered by sleep-copy-fsck-gate into its own item.",
				}),
			},
		},
	}

	rep, err := replay.Evaluate(t.Context(), ds, replay.EvalConfig{
		Client:      client,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if !rep.AllPass() {
		t.Fatalf("AllPass() = false, want true:\n%s", rep.FormatReport())
	}
	if rep.AgreementRate != 1.0 {
		t.Fatalf("AgreementRate = %.2f, want 1.0", rep.AgreementRate)
	}
	if rep.BaselineAgreementRate != 0.60 {
		t.Fatalf("BaselineAgreementRate = %.2f, want 0.60", rep.BaselineAgreementRate)
	}
	if rep.UnnecessaryAskRate != 0.0 {
		t.Fatalf("UnnecessaryAskRate = %.2f, want 0.0", rep.UnnecessaryAskRate)
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

func TestFullGate2DatasetStructureAndDeterministicEval(t *testing.T) {
	t.Parallel()

	ds, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatalf("LoadDefaultDataset: %v", err)
	}
	if len(ds.Moments) != 150 {
		t.Fatalf("len(ds.Moments) = %d, want 150", len(ds.Moments))
	}
	if len(ds.Questions) != 40 {
		t.Fatalf("len(ds.Questions) = %d, want 40", len(ds.Questions))
	}

	runCounts := make(map[string]int)
	correctedCount := 0
	for i := range ds.Moments {
		m := &ds.Moments[i]
		runCounts[m.RunID]++
		if m.OwnerFollowUp.Verdict == replay.VerdictCorrected {
			correctedCount++
		}
	}
	for _, wantRun := range []string{"20261103-cicd-300", "20261026-feedback", "20261023-s1-main"} {
		if runCounts[wantRun] == 0 {
			t.Fatalf("no moments extracted for run %q", wantRun)
		}
	}
	if correctedCount == 0 {
		t.Fatal("expected owner-corrected moments in dataset, got 0")
	}

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

	client := &scriptedEvalClient{momentTools: momentTools}
	rep, err := replay.Evaluate(t.Context(), ds, replay.EvalConfig{
		Client:      client,
		Concurrency: 8,
	})
	if err != nil {
		t.Fatalf("Evaluate full dataset: %v", err)
	}
	if !rep.AllPass() {
		t.Fatalf("AllPass() = false on deterministic full-set run:\n%s", rep.FormatReport())
	}
}
