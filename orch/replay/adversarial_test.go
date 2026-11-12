package replay_test

// Adversarial tests for the Gate 2 replay grader. Every fake dispatcher here
// is deliberately wrong or deliberately dumb; the pass bars must fail it. If a
// regression makes the grader lenient (or the dataset leak its own answers),
// a test in this file fails.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/dispatcher"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/replay"
	"github.com/bazelment/yoloswe/orch/store"
)

// leakedToolRE matches any dispatcher tool name. The tool names are legitimate
// in the system prompt's tool list, never in the per-moment user message.
var leakedToolRE = regexp.MustCompile(`(?i)\b(?:lane[._](?:spawn|steer|retry|stop)|backlog[._](?:add|update|reorder|close)|planner[._]request|directive[._]note|owner[._]ask|read[._](?:lane_report|pr|ci))\b`)

func hasToolResult(req model.Request) bool {
	if len(req.Messages) == 0 {
		return false
	}
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type == model.BlockTypeToolResult {
			return true
		}
	}
	return false
}

func endTurn(text string) *model.Response {
	return &model.Response{
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock(text)),
		Usage:      model.Usage{InputTokens: 2000, OutputTokens: 20},
	}
}

// clientFunc adapts a function to model.ModelClient.
type clientFunc func(context.Context, model.Request) (*model.Response, error)

func (f clientFunc) CreateMessage(ctx context.Context, req model.Request) (*model.Response, error) {
	return f(ctx, req)
}

// noopClient never proposes an action ("empty decision").
func noopClient() model.ModelClient {
	return clientFunc(func(context.Context, model.Request) (*model.Response, error) {
		return endTurn("No action needed."), nil
	})
}

// labelTarget returns the strongest possible "wrong tool, right place" target:
// the label's own first expected target, else the event lane, else any lane.
func labelTarget(m replay.JudgmentMoment) string {
	if len(m.OwnerFollowUp.ExpectedTargets) > 0 {
		return m.OwnerFollowUp.ExpectedTargets[0]
	}
	for i := range m.Events {
		if m.Events[i].LaneID != "" {
			return m.Events[i].LaneID
		}
	}
	if len(m.State.Lanes) > 0 {
		return m.State.Lanes[0].ID
	}
	return "unknown-lane"
}

func marshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func alwaysStop(t *testing.T) func(replay.JudgmentMoment) model.ModelClient {
	return func(m replay.JudgmentMoment) model.ModelClient {
		return &staticActionClient{calls: []model.ToolCall{{
			ID:    "stop-1",
			Name:  dispatcher.ToolLaneStop,
			Input: marshal(t, dispatcher.LaneStopParams{LaneID: labelTarget(m), Reason: "adversarial: always stop"}),
		}}, usage: model.Usage{InputTokens: 3000}}
	}
}

func alwaysAsk(t *testing.T) func(replay.JudgmentMoment) model.ModelClient {
	return func(m replay.JudgmentMoment) model.ModelClient {
		return &staticActionClient{calls: []model.ToolCall{{
			ID:   "ask-1",
			Name: dispatcher.ToolOwnerAsk,
			Input: marshal(t, dispatcher.OwnerAskParams{
				LaneID:   labelTarget(m),
				Question: "Should I proceed?",
				Options:  []string{"yes", "no"},
				Default:  "yes",
				Deadline: "1h",
				Reason:   "adversarial: always ask",
			}),
		}}, usage: model.Usage{InputTokens: 3000}}
	}
}

func evalMoments(t *testing.T, ds *replay.Dataset, mk func(replay.JudgmentMoment) model.ModelClient) *replay.EvalReport {
	t.Helper()
	results := make([]replay.MomentResult, len(ds.Moments))
	for i := range ds.Moments {
		mr, err := replay.EvaluateMoment(t.Context(), ds.Moments[i], mk(ds.Moments[i]), dispatcher.Config{})
		if err != nil {
			t.Fatalf("EvaluateMoment(%s): %v", ds.Moments[i].ID, err)
		}
		results[i] = mr
	}
	return replay.ComputeReport(results, nil)
}

func logScore(t *testing.T, name string, rep *replay.EvalReport) {
	t.Helper()
	t.Logf("ADVERSARIAL %-28s moments=%d agreement=%d/%d (%.1f%%) baseline=%.1f%% unnecessary_asks=%d/%d total_asks=%d agreementPass=%v askPass=%v allPass=%v",
		name, rep.TotalMoments, rep.DispatcherAgreed, rep.TotalMoments, rep.AgreementRate*100,
		rep.BaselineAgreementRate*100, rep.DispatcherUnnecessaryAsks, rep.DecideDontAskMoments,
		rep.TotalDispatcherAsks, rep.AgreementPass, rep.AskRatePass, rep.AllPass())
}

type adversary struct {
	mk func(*testing.T) func(replay.JudgmentMoment) model.ModelClient
	// maxAgreed is the most moments the adversary may legitimately agree on:
	// e.g. always-ask is *correct* on moments whose label is owner.ask.
	maxAgreed func(*replay.Dataset) int
	name      string
}

func countExpectedTool(ds *replay.Dataset, tool string) int {
	n := 0
	for i := range ds.Moments {
		for _, et := range ds.Moments[i].OwnerFollowUp.ExpectedTools {
			if dispatcher.NormalizeToolName(et) == tool {
				n++
				break
			}
		}
	}
	return n
}

func adversaries() []adversary {
	return []adversary{
		{
			name: "always-lane.stop",
			mk:   alwaysStop,
			maxAgreed: func(ds *replay.Dataset) int {
				return countExpectedTool(ds, dispatcher.ToolLaneStop)
			},
		},
		{
			name: "always-owner.ask",
			mk:   alwaysAsk,
			maxAgreed: func(ds *replay.Dataset) int {
				return countExpectedTool(ds, dispatcher.ToolOwnerAsk)
			},
		},
		{
			name: "never-acts",
			mk: func(*testing.T) func(replay.JudgmentMoment) model.ModelClient {
				return func(replay.JudgmentMoment) model.ModelClient { return noopClient() }
			},
			maxAgreed: func(*replay.Dataset) int { return 0 },
		},
	}
}

// TestAdversarialWrongDispatchersFailTheBars is the inner-loop case: the
// 5-moment set with always-lane.stop / always-owner.ask / never-acts.
func TestAdversarialWrongDispatchersFailTheBars(t *testing.T) {
	t.Parallel()
	for _, set := range []struct {
		load func() (*replay.Dataset, error)
		name string
	}{
		{replay.LoadInnerLoopDataset, "inner-loop-5"},
		{replay.LoadDefaultDataset, "gate2-full"},
	} {
		ds, err := set.load()
		if err != nil {
			t.Fatalf("%s: load: %v", set.name, err)
		}
		for _, adv := range adversaries() {
			t.Run(set.name+"/"+adv.name, func(t *testing.T) {
				t.Parallel()
				rep := evalMoments(t, ds, adv.mk(t))
				logScore(t, set.name+"/"+adv.name, rep)

				if limit := adv.maxAgreed(ds); rep.DispatcherAgreed > limit {
					t.Errorf("%s agreed on %d moments, want <= %d (only moments whose label is that tool may agree): grader is lenient",
						adv.name, rep.DispatcherAgreed, limit)
				}
				if rep.AllPass() {
					t.Errorf("%s: AllPass() = true, the Gate 2 bar must fail a deliberately wrong dispatcher", adv.name)
				}
				if rep.AgreementPass {
					t.Errorf("%s: AgreementPass = true (agreement %.1f%% vs baseline %.1f%%)", adv.name, rep.AgreementRate*100, rep.BaselineAgreementRate*100)
				}
				if adv.name == "always-owner.ask" && rep.AskRatePass {
					t.Errorf("always-owner.ask: AskRatePass = true (unnecessary %d/%d, total asks %d)",
						rep.DispatcherUnnecessaryAsks, rep.DecideDontAskMoments, rep.TotalDispatcherAsks)
				}
			})
		}
	}
}

// TestAdversarialRightToolWrongTarget: the label's tool on a different lane
// must not agree, for every moment that names targets.
func TestAdversarialRightToolWrongTarget(t *testing.T) {
	t.Parallel()
	ds, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ds.Moments {
		if len(m.OwnerFollowUp.ExpectedTargets) == 0 || m.OwnerFollowUp.ExpectedTools[0] != dispatcher.ToolLaneRetry {
			continue
		}
		client := &staticActionClient{calls: []model.ToolCall{{
			ID:    "c1",
			Name:  dispatcher.ToolLaneRetry,
			Input: marshal(t, dispatcher.LaneRetryParams{LaneID: "some-other-lane", Reason: "wrong lane"}),
		}}}
		mr, err := replay.EvaluateMoment(t.Context(), m, client, dispatcher.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if mr.Agreed {
			t.Errorf("%s: lane.retry on some-other-lane agreed with label %v", m.ID, m.OwnerFollowUp.ExpectedTargets)
		}
	}
}

// TestAdversarialNoZeroOverZeroOrEmptyPass: the report must not PASS when
// nothing was measured or nothing agreed.
func TestAdversarialNoZeroOverZeroOrEmptyPass(t *testing.T) {
	t.Parallel()
	if rep := replay.ComputeReport(nil, nil); rep.AllPass() {
		t.Error("ComputeReport(nil, nil).AllPass() = true: an empty eval must not PASS")
	}
	// 0% agreement against a 0% baseline is 0 >= 0; that must not be a PASS.
	zero := []replay.MomentResult{{ID: "a", Verdict: replay.VerdictCorrected, InputTokens: 1000}, {ID: "b", Verdict: replay.VerdictCorrected, InputTokens: 1000}}
	if rep := replay.ComputeReport(zero, nil); rep.AgreementPass || rep.AllPass() {
		t.Errorf("0 agreed / 0 baseline passed: AgreementPass=%v AllPass=%v", rep.AgreementPass, rep.AllPass())
	}
}

// TestAdversarialPromptDoesNotContainTheAnswer captures what the model is
// actually shown for every moment and checks it for the label's tool name, the
// owner's follow-up text and the baseline's own summary.
func TestAdversarialPromptDoesNotContainTheAnswer(t *testing.T) {
	t.Parallel()
	for _, set := range []struct {
		load func() (*replay.Dataset, error)
		name string
	}{
		{replay.LoadInnerLoopDataset, "inner-loop-5"},
		{replay.LoadDefaultDataset, "gate2-full"},
	} {
		ds, err := set.load()
		if err != nil {
			t.Fatalf("%s: %v", set.name, err)
		}
		t.Run(set.name, func(t *testing.T) {
			t.Parallel()
			var leaked, total int
			for _, m := range ds.Moments {
				var mu sync.Mutex
				var seen []string
				capture := clientFunc(func(_ context.Context, req model.Request) (*model.Response, error) {
					mu.Lock()
					for _, msg := range req.Messages {
						seen = append(seen, msg.Text())
					}
					mu.Unlock()
					return endTurn("No action needed."), nil
				})
				if _, err := replay.EvaluateMoment(t.Context(), m, capture, dispatcher.Config{}); err != nil {
					t.Fatalf("%s: %v", m.ID, err)
				}
				total++
				prompt := strings.Join(seen, "\n")
				var why []string
				if loc := leakedToolRE.FindString(prompt); loc != "" {
					why = append(why, fmt.Sprintf("tool name %q", loc))
				}
				for _, secret := range []string{m.OwnerFollowUp.OwnerNote, m.Baseline.Summary} {
					if s := strings.TrimSpace(secret); len(s) >= 12 && strings.Contains(strings.ToLower(prompt), strings.ToLower(s)) {
						why = append(why, fmt.Sprintf("verbatim %q", s))
					}
				}
				if len(why) > 0 {
					leaked++
					if leaked <= 5 {
						t.Errorf("%s: model prompt contains the answer: %s", m.ID, strings.Join(why, "; "))
					}
				}
			}
			t.Logf("LEAKAGE %s: %d/%d moments' prompts contain a tool name or the owner/baseline text", set.name, leaked, total)
			if leaked > 0 {
				t.Errorf("%d/%d moment prompts leak the expected answer", leaked, total)
			}
		})
	}
}

var (
	scrapeToolRE = regexp.MustCompile(`(?i)\b(lane\.(?:retry|spawn|steer|stop)|backlog\.(?:add|close|update)|planner\.request|directive\.note|owner\.ask)\b`)
	scrapeIDRE   = regexp.MustCompile(`(?:lane_id|item_id|\bid)=([A-Za-z0-9._:\-]+)`)
	scrapeRefRE  = regexp.MustCompile(`post:[A-Za-z0-9\-]+`)
)

// scrapingDispatcher has no model: it copies whatever tool call and id the
// event text instructs. Its score is the leakage score of the dataset.
func scrapingDispatcher(t *testing.T) clientFunc {
	return func(_ context.Context, req model.Request) (*model.Response, error) {
		if hasToolResult(req) || len(req.Messages) == 0 {
			return endTurn("done"), nil
		}
		text := req.Messages[0].Text()
		if i := strings.Index(text, "## Judgment Event Batch"); i >= 0 {
			text = text[i:]
		}
		tool := scrapeToolRE.FindString(text)
		if tool == "" {
			return endTurn("No action needed."), nil
		}
		id := ""
		if m := scrapeIDRE.FindStringSubmatch(text); m != nil {
			id = strings.TrimRight(m[1], ".")
		}
		var input any
		switch strings.ToLower(tool) {
		case dispatcher.ToolLaneRetry:
			input = dispatcher.LaneRetryParams{LaneID: id, Reason: "scraped"}
		case dispatcher.ToolLaneSpawn:
			input = dispatcher.LaneSpawnParams{ItemID: id, Brief: "scraped brief", Reason: "scraped"}
		case dispatcher.ToolLaneSteer:
			input = dispatcher.LaneSteerParams{LaneID: id, Note: "scraped", Reason: "scraped"}
		case dispatcher.ToolBacklogClose:
			input = dispatcher.BacklogCloseParams{ID: id, Status: "done", Reason: "scraped"}
		case dispatcher.ToolBacklogAdd:
			input = dispatcher.BacklogAddParams{ID: id, Title: "scraped", Priority: "p1", Reason: "scraped"}
		case dispatcher.ToolPlannerRequest:
			input = dispatcher.PlannerRequestParams{Reason: "scraped", PlanRef: scrapeRefRE.FindString(text)}
		case dispatcher.ToolDirectiveNote:
			input = dispatcher.DirectiveNoteParams{Fact: "scraped", Scope: id}
		default:
			return endTurn("No action needed."), nil
		}
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		return &model.Response{
			StopReason: model.StopReasonToolUse,
			Message:    model.NewAssistantMessage(model.NewToolUseBlock("scrape-1", strings.ToLower(tool), raw)),
			Usage:      model.Usage{InputTokens: 2500},
		}, nil
	}
}

// TestAdversarialScraperCannotPassTheBar: a regex over the event text, with no
// model and no understanding, must not clear the agreement bar. At the audited
// HEAD it agreed with the label on most moments because event summaries told
// the dispatcher which tool to call and with which id.
func TestAdversarialScraperCannotPassTheBar(t *testing.T) {
	t.Parallel()
	for _, set := range []struct {
		load func() (*replay.Dataset, error)
		name string
	}{
		{replay.LoadInnerLoopDataset, "inner-loop-5"},
		{replay.LoadDefaultDataset, "gate2-full"},
	} {
		ds, err := set.load()
		if err != nil {
			t.Fatalf("%s: %v", set.name, err)
		}
		t.Run(set.name, func(t *testing.T) {
			t.Parallel()
			rep := evalMoments(t, ds, func(replay.JudgmentMoment) model.ModelClient { return scrapingDispatcher(t) })
			logScore(t, set.name+"/event-text-scraper", rep)
			// 20%: a handful of mechanical moments may be guessable from facts
			// alone, but nothing close to the 70%+ a leaked answer yields.
			if rep.AgreementRate > 0.20 {
				t.Errorf("a model-free event-text scraper agrees on %.1f%% (%d/%d) of moments: the event text contains the answer",
					rep.AgreementRate*100, rep.DispatcherAgreed, rep.TotalMoments)
			}
			if rep.AgreementPass {
				t.Errorf("scraper cleared the agreement bar (%.1f%% vs baseline %.1f%%)", rep.AgreementRate*100, rep.BaselineAgreementRate*100)
			}
		})
	}
}

// TestAdversarialConciergeNeedsTheStore: a concierge that answers from the
// question text alone must not clear the accuracy bar, and one that never
// calls a Store tool must not count as accurate.
func TestAdversarialConciergeNeedsTheStore(t *testing.T) {
	t.Parallel()
	ds, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var echoAccurate, prompted int
	for _, q := range ds.Questions {
		qr, err := replay.EvaluateQuestion(ctx, q, echoQuestionConcierge{})
		if err != nil {
			t.Fatal(err)
		}
		if qr.Accurate {
			echoAccurate++
		}
		prompted++
	}
	t.Logf("ADVERSARIAL concierge echoes question, never calls a tool: accurate %d/%d (bar 90%%)", echoAccurate, prompted)
	if float64(echoAccurate)/float64(prompted) >= replay.PassBarMinConciergeAccuracy {
		t.Errorf("question-echo concierge scored %d/%d accurate (>= %.0f%% bar): required facts are answerable without the Store",
			echoAccurate, prompted, replay.PassBarMinConciergeAccuracy*100)
	}

	// Same through the real StoreConcierge with a model that answers from the
	// prompt only (no tool use): it must not be accurate.
	var promptOnly, n int
	sc := replay.NewStoreConcierge(clientFunc(func(_ context.Context, req model.Request) (*model.Response, error) {
		return endTurn(req.Messages[0].Text()), nil
	}))
	for _, q := range ds.Questions {
		qr, err := replay.EvaluateQuestion(ctx, q, sc)
		if err != nil {
			t.Fatal(err)
		}
		if qr.Accurate {
			promptOnly++
		}
		n++
	}
	t.Logf("ADVERSARIAL concierge parrots its prompt, no Store tool call: accurate %d/%d", promptOnly, n)
	if promptOnly != 0 {
		t.Errorf("a prompt-parroting concierge that never queried the Store was accurate on %d/%d questions", promptOnly, n)
	}
}

type echoQuestionConcierge struct{}

func (echoQuestionConcierge) Answer(_ context.Context, _ store.Store, _ lane.LaneDriver, _ clock.Clock, _ string, question string) (replay.ConciergeAnswer, error) {
	return replay.ConciergeAnswer{Answer: question}, nil
}

// TestSyntheticMomentsNeverCountTowardBars: label-dependent bars use verified
// moments only, and a pass without an evidenced owner correction is INCONCLUSIVE.
func TestSyntheticMomentsNeverCountTowardBars(t *testing.T) {
	t.Parallel()
	acc := []replay.QuestionResult{{ID: "q", Accurate: true}}

	var synthetic []replay.MomentResult
	for i := range 10 {
		synthetic = append(synthetic, replay.MomentResult{
			ID: fmt.Sprintf("s%d", i), Verdict: replay.VerdictCorrected, Agreed: true, InputTokens: 1000,
			ForbidOwnerAsk: true,
		})
	}
	rep := replay.ComputeReport(synthetic, acc)
	if rep.AgreementPass || rep.AskRatePass || rep.AllPass() || rep.Verdict() != replay.VerdictFail {
		t.Errorf("10 synthetic agreed moments produced agreementPass=%v askPass=%v verdict=%s; synthetic must not count",
			rep.AgreementPass, rep.AskRatePass, rep.Verdict())
	}
	if rep.SyntheticAgreed != 10 || rep.GroundedMoments != 0 {
		t.Errorf("synthetic=%d grounded=%d, want 10/0", rep.SyntheticAgreed, rep.GroundedMoments)
	}

	endorsed := []replay.MomentResult{{ID: "g", Grounded: true, Verdict: replay.VerdictEndorsed, Agreed: true, BaselineAgreed: true, InputTokens: 1000}}
	rep = replay.ComputeReport(endorsed, acc)
	if !rep.BarsPass() || rep.Verdict() != replay.VerdictInconclusive || rep.AllPass() {
		t.Errorf("verified+endorsed only: bars=%v verdict=%s, want bars pass but INCONCLUSIVE", rep.BarsPass(), rep.Verdict())
	}

	corrected := []replay.MomentResult{
		{ID: "g1", Grounded: true, Verdict: replay.VerdictEndorsed, Agreed: true, BaselineAgreed: true, InputTokens: 1000},
		{ID: "g2", Grounded: true, Verdict: replay.VerdictCorrected, Agreed: true, DecideDontAsk: true, ForbidOwnerAsk: true, BaselineAskedOwner: true, InputTokens: 1000},
	}
	rep = replay.ComputeReport(corrected, acc)
	if rep.Verdict() != replay.VerdictPass || !rep.AllPass() {
		t.Errorf("verified moments with an evidenced correction: verdict=%s\n%s", rep.Verdict(), rep.FormatReport())
	}

	// A verified moment where the dispatcher asked anyway fails the ask bar.
	corrected[1].AskedOwner = true
	corrected[1].Agreed = false
	if rep = replay.ComputeReport(corrected, acc); rep.AskRatePass || rep.Verdict() != replay.VerdictFail {
		t.Errorf("unnecessary ask on a verified moment: askPass=%v verdict=%s", rep.AskRatePass, rep.Verdict())
	}
}

// TestDatasetRejectsLeakyOrUngroundedMoments: the dataset loader is the first
// line of defence; the prompt-capture test above is the second.
func TestDatasetRejectsLeakyOrUngroundedMoments(t *testing.T) {
	t.Parallel()
	base, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(m *replay.JudgmentMoment)) error {
		var ds replay.Dataset
		if err := json.Unmarshal(raw, &ds); err != nil {
			t.Fatal(err)
		}
		fn(&ds.Moments[0])
		return ds.Validate()
	}
	cases := map[string]func(m *replay.JudgmentMoment){
		"tool name in event summary": func(m *replay.JudgmentMoment) { m.Events[0].Summary += " Call lane.retry for lane_id=x." },
		"owner note echoed in event": func(m *replay.JudgmentMoment) {
			m.OwnerFollowUp.OwnerNote = "retry it now please"
			m.Events[0].Summary += " retry it now please"
		},
		"per-moment planted directive": func(m *replay.JudgmentMoment) {
			m.State.Directives = append(m.State.Directives, store.Directive{ID: "dir-" + m.ID + "-decide", Text: "do the thing"})
		},
		"missing grounding":          func(m *replay.JudgmentMoment) { m.Grounding = "" },
		"missing grounding evidence": func(m *replay.JudgmentMoment) { m.GroundingEvidence = " " },
	}
	for name, fn := range cases {
		if err := mutate(fn); err == nil {
			t.Errorf("%s: Validate accepted the moment", name)
		}
	}
	if err := mutate(func(*replay.JudgmentMoment) {}); err != nil {
		t.Errorf("unmutated dataset must validate: %v", err)
	}
}
