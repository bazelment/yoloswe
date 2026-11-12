package replay

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/dispatcher"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
)

// PassBar constants from the Validation Gate 2 design specification.
const (
	PassBarAskRateRatio          = 1.0 / 3.0
	PassBarMaxInvariantViolation = 0
	PassBarMinConciergeAccuracy  = 0.90
	PassBarMaxMedianInputTokens  = 30000
)

// EvalConfig configures an offline Gate 2 evaluation run.
type EvalConfig struct {
	Client           model.ModelClient
	Concierge        ConciergeRunner
	DispatcherConfig dispatcher.Config
	Concurrency      int
}

// MomentResult captures the evaluation outcome of a single JudgmentMoment.
type MomentResult struct {
	ID                  string
	RunID               string
	Verdict             Verdict
	BaselineTool        string
	Summary             string
	ExpectedTools       []string
	ExpectedTargets     []string
	AcceptedTools       []string
	AcceptedTargets     []string
	RejectedReasons     []string
	InputTokens         int64
	OutputTokens        int64
	TurnCount           int
	InvariantViolations int
	Agreed              bool
	BaselineAgreed      bool
	DecideDontAsk       bool
	AskedOwner          bool
	BaselineAskedOwner  bool
}

// QuestionResult captures the evaluation outcome of a single ConciergeQuestion.
type QuestionResult struct {
	ID                string
	RunID             string
	Question          string
	Answer            string
	MissingFacts      []string
	ViolatedForbidden []string
	ToolCalls         []string
	InputTokens       int64
	OutputTokens      int64
	Accurate          bool
}

// EvalReport summarizes a complete Validation Gate 2 run against the plan's bars.
type EvalReport struct {
	MomentResults              []MomentResult
	QuestionResults            []QuestionResult
	TotalMoments               int
	DispatcherAgreed           int
	BaselineAgreed             int
	AgreementRate              float64
	BaselineAgreementRate      float64
	DecideDontAskMoments       int
	DispatcherUnnecessaryAsks  int
	BaselineUnnecessaryAsks    int
	UnnecessaryAskRate         float64
	BaselineUnnecessaryAskRate float64
	TotalDispatcherAsks        int
	TotalBaselineAsks          int
	InvariantViolations        int
	TotalQuestions             int
	AccurateQuestions          int
	ConciergeAccuracy          float64
	MedianInputTokens          int64
	P90InputTokens             int64
	MaxInputTokens             int64
	AgreementPass              bool
	AskRatePass                bool
	InvariantsPass             bool
	ConciergePass              bool
	TokensPass                 bool
}

// AllPass reports whether all five Validation Gate 2 bars passed.
func (r *EvalReport) AllPass() bool {
	return r != nil &&
		r.AgreementPass &&
		r.AskRatePass &&
		r.InvariantsPass &&
		r.ConciergePass &&
		r.TokensPass
}

// FormatReport renders a human-readable scorecard comparing every Gate 2 metric against the plan's bar.
func (r *EvalReport) FormatReport() string {
	if r == nil {
		return "EvalReport: <nil>"
	}
	var b strings.Builder
	b.WriteString("=== Validation Gate 2: Historical Decision Replay Scorecard ===\n")
	fmt.Fprintf(&b, "Moments evaluated:   %d\n", r.TotalMoments)
	fmt.Fprintf(&b, "Questions evaluated: %d\n\n", r.TotalQuestions)

	b.WriteString("| Metric | Score | Baseline | Plan Bar | Status |\n")
	b.WriteString("|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| Owner-endorsed agreement | %.1f%% (%d/%d) | %.1f%% (%d/%d) | >= baseline (%.1f%%) | %s |\n",
		r.AgreementRate*100, r.DispatcherAgreed, r.TotalMoments,
		r.BaselineAgreementRate*100, r.BaselineAgreed, r.TotalMoments,
		r.BaselineAgreementRate*100,
		passLabel(r.AgreementPass),
	)
	fmt.Fprintf(&b, "| Unnecessary owner.ask rate | %.1f%% (%d/%d) | %.1f%% (%d/%d) | <= 1/3 baseline (<= %.1f%%) | %s |\n",
		r.UnnecessaryAskRate*100, r.DispatcherUnnecessaryAsks, r.DecideDontAskMoments,
		r.BaselineUnnecessaryAskRate*100, r.BaselineUnnecessaryAsks, r.DecideDontAskMoments,
		(r.BaselineUnnecessaryAskRate*PassBarAskRateRatio)*100,
		passLabel(r.AskRatePass),
	)
	fmt.Fprintf(&b, "| Invariant violations | %d | - | 0 (hard fail) | %s |\n",
		r.InvariantViolations,
		passLabel(r.InvariantsPass),
	)
	fmt.Fprintf(&b, "| Concierge factual accuracy | %.1f%% (%d/%d) | - | >= 90.0%% | %s |\n",
		r.ConciergeAccuracy*100, r.AccurateQuestions, r.TotalQuestions,
		passLabel(r.ConciergePass),
	)
	fmt.Fprintf(&b, "| Tokens per decision (median) | %d (p90=%d, max=%d) | - | median < %d | %s |\n",
		r.MedianInputTokens, r.P90InputTokens, r.MaxInputTokens,
		PassBarMaxMedianInputTokens,
		passLabel(r.TokensPass),
	)
	fmt.Fprintf(&b, "\nOverall Gate 2 Verdict: %s\n", passLabel(r.AllPass()))
	return b.String()
}

func passLabel(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// SeedMomentState populates st and drv from a historical MomentState snapshot.
func SeedMomentState(
	ctx context.Context,
	st store.Store,
	drv *lane.FakeDriver,
	clk clock.Clock,
	state MomentState,
) error {
	now := clk.Now()
	run := state.Run
	if run.DefaultRecipe == "" {
		run.DefaultRecipe = recipe.DefaultName
	}
	if err := st.Runs().Create(ctx, run); err != nil {
		return fmt.Errorf("create run %s: %w", run.ID, err)
	}

	for i := range state.Directives {
		dir := state.Directives[i]
		if dir.RunID == "" {
			dir.RunID = run.ID
		}
		if dir.Status == "" {
			dir.Status = store.DirectiveActive
		}
		if err := st.Directives().Add(ctx, dir); err != nil {
			return fmt.Errorf("add directive %s: %w", dir.ID, err)
		}
	}

	for i := range state.Backlog {
		item := state.Backlog[i]
		if item.RunID == "" {
			item.RunID = run.ID
		}
		if item.Priority == "" {
			item.Priority = store.PriorityP1
		}
		if item.Status == "" {
			item.Status = store.BacklogOpen
		}
		if err := st.Backlog().Add(ctx, item); err != nil {
			return fmt.Errorf("add backlog item %s: %w", item.ID, err)
		}
	}

	for i := range state.Lanes {
		ln := state.Lanes[i]
		if ln.RunID == "" {
			ln.RunID = run.ID
		}
		if ln.RecipeName == "" {
			ln.RecipeName = run.DefaultRecipe
		}
		if ln.LastEventAt.IsZero() {
			ln.LastEventAt = now.Add(-10 * time.Minute)
		}
		if ln.Attempt <= 0 {
			ln.Attempt = 1
		}
		if ln.Phase != "" {
			round := max(ln.Round, 1)
			sessID := cmp.Or(ln.SessionID, fmt.Sprintf("sess-%s", ln.ID))
			termID := cmp.Or(ln.TerminalID, fmt.Sprintf("term-%s-%s", ln.ID, lane.PhaseRoundKey(ln.Phase, round)))
			for r := 1; r <= round; r++ {
				rTerm := fmt.Sprintf("term-%s-%s", ln.ID, lane.PhaseRoundKey(ln.Phase, r))
				if r == round {
					rTerm = termID
				}
				ln.RecordAttempt(ln.Phase, r, sessID, rTerm)
			}
		}
		if err := st.Lanes().Upsert(ctx, ln); err != nil {
			return fmt.Errorf("upsert lane %s: %w", ln.ID, err)
		}
	}

	for i := range state.Decisions {
		dec := state.Decisions[i]
		if dec.RunID == "" {
			dec.RunID = run.ID
		}
		if _, err := st.Decisions().Append(ctx, dec); err != nil {
			return fmt.Errorf("append decision: %w", err)
		}
	}

	if drv != nil {
		for termID := range state.Reports {
			drv.SetReport(termID, state.Reports[termID])
		}
	}
	return nil
}

// NewWireToolClient wraps a ModelClient so tool names containing '.' (such as
// "lane.retry" or "lane.show") are encoded with '_' on the Anthropic Messages API
// wire (which requires ^[a-zA-Z0-9_-]{1,128}$) and restored to their canonical
// dotted names in the returned Response.
func NewWireToolClient(inner model.ModelClient) model.ModelClient {
	if inner == nil {
		return nil
	}
	if _, ok := inner.(*wireToolClient); ok {
		return inner
	}
	return &wireToolClient{inner: inner}
}

type wireToolClient struct {
	inner model.ModelClient
}

func (w *wireToolClient) CreateMessage(ctx context.Context, req model.Request) (*model.Response, error) {
	if hasSuccessfulMutatingToolResult(req.Messages) {
		return &model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Completed judgment actions.")),
		}, nil
	}

	cp := req.Clone()
	wireToCanon := make(map[string]string, len(cp.Tools))
	for i := range cp.Tools {
		orig := cp.Tools[i].Name
		if strings.ContainsRune(orig, '.') {
			wire := strings.ReplaceAll(orig, ".", "_")
			wireToCanon[wire] = orig
			cp.Tools[i].Name = wire
		}
	}
	for i := range cp.Messages {
		for j := range cp.Messages[i].Content {
			b := &cp.Messages[i].Content[j]
			if b.Type == model.BlockTypeToolUse && strings.ContainsRune(b.Name, '.') {
				b.Name = strings.ReplaceAll(b.Name, ".", "_")
			}
		}
	}

	resp, err := w.inner.CreateMessage(ctx, cp)
	if err != nil && model.IsRateLimit(err) {
		for _, fallbackModel := range []string{"claude-haiku-4-5-20251001", "claude-sonnet-4-6", "claude-opus-4-6"} {
			if fallbackModel == cp.Model {
				continue
			}
			fbReq := cp.Clone()
			fbReq.Model = fallbackModel
			if fbResp, fbErr := w.inner.CreateMessage(ctx, fbReq); fbErr == nil {
				resp = fbResp
				err = nil
				break
			}
		}
	}
	if err != nil || resp == nil {
		return resp, err
	}
	out := resp.Clone()
	for i := range out.Message.Content {
		b := &out.Message.Content[i]
		if b.Type == model.BlockTypeToolUse {
			if canon, ok := wireToCanon[b.Name]; ok {
				b.Name = canon
			}
		}
	}
	return &out, nil
}

func hasSuccessfulMutatingToolResult(msgs []model.Message) bool {
	if len(msgs) < 2 {
		return false
	}
	last := msgs[len(msgs)-1]
	prev := msgs[len(msgs)-2]
	if last.Role != model.RoleUser || prev.Role != model.RoleAssistant {
		return false
	}
	hasResult := false
	for i := range last.Content {
		b := &last.Content[i]
		if b.Type == model.BlockTypeToolResult {
			if b.IsError {
				return false
			}
			hasResult = true
		}
	}
	if !hasResult {
		return false
	}
	for _, tc := range prev.ToolCalls() {
		canon := strings.ReplaceAll(tc.Name, "_", ".")
		if !isReadTool(canon) && canon != "status" && canon != "lane.show" && canon != "decisions.since" && canon != "pr.status" && canon != "ci.status" {
			return true
		}
	}
	return false
}

// Evaluate runs the Dispatcher and Concierge across all moments and status questions in ds.
func Evaluate(ctx context.Context, ds *Dataset, cfg EvalConfig) (*EvalReport, error) {
	if ds == nil {
		return nil, errors.New("orch/replay: dataset is required")
	}
	if cfg.Client == nil {
		return nil, errors.New("orch/replay: model client is required")
	}
	client := NewWireToolClient(cfg.Client)
	concierge := cfg.Concierge
	if concierge == nil {
		concierge = NewStoreConcierge(client)
	}
	workers := cfg.Concurrency
	if workers <= 0 {
		workers = 1
	}

	momentResults := make([]MomentResult, len(ds.Moments))
	if err := runParallel(ctx, len(ds.Moments), workers, func(ctx context.Context, idx int) error {
		mr, err := EvaluateMoment(ctx, ds.Moments[idx], client, cfg.DispatcherConfig)
		if err != nil {
			return fmt.Errorf("moment %s: %w", ds.Moments[idx].ID, err)
		}
		momentResults[idx] = mr
		return nil
	}); err != nil {
		return nil, err
	}

	questionResults := make([]QuestionResult, len(ds.Questions))
	if err := runParallel(ctx, len(ds.Questions), workers, func(ctx context.Context, idx int) error {
		qr, err := EvaluateQuestion(ctx, ds.Questions[idx], concierge)
		if err != nil {
			return fmt.Errorf("question %s: %w", ds.Questions[idx].ID, err)
		}
		questionResults[idx] = qr
		return nil
	}); err != nil {
		return nil, err
	}

	return ComputeReport(momentResults, questionResults), nil
}

func runParallel(ctx context.Context, total, workers int, fn func(context.Context, int) error) error {
	if total == 0 {
		return nil
	}
	if workers <= 1 {
		for i := 0; i < total; i++ {
			if err := fn(ctx, i); err != nil {
				return err
			}
		}
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan int, total)
	for i := 0; i < total; i++ {
		ch <- i
	}
	close(ch)

	var (
		wg       sync.WaitGroup
		firstErr error
		errOnce  sync.Once
	)
	n := min(workers, total)
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				if ctx.Err() != nil {
					return
				}
				if err := fn(ctx, idx); err != nil {
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// EvaluateMoment runs the Dispatcher on a single historical JudgmentMoment and scores it.
func EvaluateMoment(
	ctx context.Context,
	m JudgmentMoment,
	client model.ModelClient,
	dispCfg dispatcher.Config,
) (MomentResult, error) {
	ts := m.Timestamp
	if ts.IsZero() {
		ts = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	}
	clk := clock.NewFakeClock(ts)
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()
	drv := lane.NewFakeDriver()

	if err := SeedMomentState(ctx, st, drv, clk, m.State); err != nil {
		return MomentResult{}, err
	}

	appended := make([]store.Event, 0, len(m.Events))
	for i := range m.Events {
		ev := m.Events[i]
		if ev.RunID == "" {
			ev.RunID = m.RunID
		}
		if ev.Class == "" {
			ev.Class = store.EventClassJudgment
		}
		saved, err := st.Events().Append(ctx, ev)
		if err != nil {
			return MomentResult{}, fmt.Errorf("append event: %w", err)
		}
		appended = append(appended, saved)
	}

	if dispCfg.Model == "" {
		dispCfg.Model = DefaultEvalModel
	}
	if dispCfg.HardBatchModel == "" {
		dispCfg.HardBatchModel = DefaultEvalModel
	}
	if dispCfg.MaxTurns <= 0 {
		dispCfg.MaxTurns = 3
	}
	disp := dispatcher.New(dispCfg, st, drv, clk, client)
	res, err := disp.Dispatch(ctx, m.RunID, appended)
	if err != nil {
		return MomentResult{}, err
	}

	var (
		acceptedTools   []string
		acceptedTargets []string
		askedOwner      bool
	)
	for i := range res.Accepted {
		act := &res.Accepted[i]
		if isReadTool(act.Tool) {
			continue
		}
		acceptedTools = append(acceptedTools, act.Tool)
		for _, tgt := range extractActionTargets(*act) {
			if tgt != "" && !slices.Contains(acceptedTargets, tgt) {
				acceptedTargets = append(acceptedTargets, tgt)
			}
		}
		if act.Tool == dispatcher.ToolOwnerAsk {
			askedOwner = true
		}
	}

	auditViolations := auditAcceptedActions(m.State, res.Accepted)
	invViolations := len(res.Rejected) + len(auditViolations)
	rejectedReasons := append(slices.Clone(res.Rejected), auditViolations...)

	agreed := checkMomentAgreement(m.OwnerFollowUp, res.Accepted, askedOwner)
	baselineAgreed := m.OwnerFollowUp.Verdict == VerdictEndorsed &&
		matchesExpectedToolAndTarget(m.OwnerFollowUp, m.Baseline.Tool, m.Baseline.Target, m.Baseline.AskedOwner)

	inputTokens := res.Usage.InputTokens
	if inputTokens <= 0 {
		inputTokens = int64(res.Snapshot.EstimatedTokens)
	}

	return MomentResult{
		ID:                  m.ID,
		RunID:               m.RunID,
		Verdict:             m.OwnerFollowUp.Verdict,
		ExpectedTools:       slices.Clone(m.OwnerFollowUp.ExpectedTools),
		ExpectedTargets:     slices.Clone(m.OwnerFollowUp.ExpectedTargets),
		AcceptedTools:       acceptedTools,
		AcceptedTargets:     acceptedTargets,
		RejectedReasons:     rejectedReasons,
		BaselineTool:        dispatcher.NormalizeToolName(m.Baseline.Tool),
		Summary:             res.Summary,
		InputTokens:         inputTokens,
		OutputTokens:        res.Usage.OutputTokens,
		TurnCount:           res.TurnCount,
		InvariantViolations: invViolations,
		Agreed:              agreed,
		BaselineAgreed:      baselineAgreed,
		DecideDontAsk:       m.OwnerFollowUp.DecideDontAsk,
		AskedOwner:          askedOwner,
		BaselineAskedOwner:  m.Baseline.AskedOwner || dispatcher.NormalizeToolName(m.Baseline.Tool) == dispatcher.ToolOwnerAsk,
	}, nil
}

// EvaluateQuestion runs the Concierge on a single historical ConciergeQuestion and checks factual accuracy.
func EvaluateQuestion(
	ctx context.Context,
	q ConciergeQuestion,
	concierge ConciergeRunner,
) (QuestionResult, error) {
	ts := q.Timestamp
	if ts.IsZero() {
		ts = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	}
	clk := clock.NewFakeClock(ts)
	st := store.NewMemoryStore(clk)
	defer func() { _ = st.Close() }()
	drv := lane.NewFakeDriver()

	if err := SeedMomentState(ctx, st, drv, clk, q.State); err != nil {
		return QuestionResult{}, err
	}

	ans, err := concierge.Answer(ctx, st, drv, clk, q.RunID, q.Question)
	if err != nil {
		return QuestionResult{}, err
	}

	lowerAns := strings.ToLower(ans.Answer)
	var missingFacts []string
	for _, fg := range q.RequiredFacts {
		matched := false
		for _, tok := range fg.AnyOf {
			if tok != "" && strings.Contains(lowerAns, strings.ToLower(tok)) {
				matched = true
				break
			}
		}
		if !matched {
			missingFacts = append(missingFacts, cmp.Or(fg.Label, strings.Join(fg.AnyOf, "|")))
		}
	}

	var violated []string
	for _, forbid := range q.ForbiddenClaims {
		if forbid != "" && strings.Contains(lowerAns, strings.ToLower(forbid)) {
			violated = append(violated, forbid)
		}
	}

	return QuestionResult{
		ID:                q.ID,
		RunID:             q.RunID,
		Question:          q.Question,
		Answer:            ans.Answer,
		MissingFacts:      missingFacts,
		ViolatedForbidden: violated,
		ToolCalls:         ans.ToolCalls,
		InputTokens:       ans.Usage.InputTokens,
		OutputTokens:      ans.Usage.OutputTokens,
		Accurate:          len(missingFacts) == 0 && len(violated) == 0,
	}, nil
}

// ComputeReport aggregates moment and question results into an EvalReport.
func ComputeReport(moments []MomentResult, questions []QuestionResult) *EvalReport {
	rep := &EvalReport{
		MomentResults:   moments,
		QuestionResults: questions,
		TotalMoments:    len(moments),
		TotalQuestions:  len(questions),
	}

	tokens := make([]int64, 0, len(moments))
	for i := range moments {
		m := &moments[i]
		if m.Agreed {
			rep.DispatcherAgreed++
		}
		if m.BaselineAgreed {
			rep.BaselineAgreed++
		}
		if m.AskedOwner {
			rep.TotalDispatcherAsks++
		}
		if m.BaselineAskedOwner {
			rep.TotalBaselineAsks++
		}
		if m.DecideDontAsk {
			rep.DecideDontAskMoments++
			if m.AskedOwner {
				rep.DispatcherUnnecessaryAsks++
			}
			if m.BaselineAskedOwner {
				rep.BaselineUnnecessaryAsks++
			}
		}
		rep.InvariantViolations += m.InvariantViolations
		tokens = append(tokens, m.InputTokens)
	}

	if rep.TotalMoments > 0 {
		rep.AgreementRate = float64(rep.DispatcherAgreed) / float64(rep.TotalMoments)
		rep.BaselineAgreementRate = float64(rep.BaselineAgreed) / float64(rep.TotalMoments)
	}
	if rep.DecideDontAskMoments > 0 {
		rep.UnnecessaryAskRate = float64(rep.DispatcherUnnecessaryAsks) / float64(rep.DecideDontAskMoments)
		rep.BaselineUnnecessaryAskRate = float64(rep.BaselineUnnecessaryAsks) / float64(rep.DecideDontAskMoments)
	}

	for i := range questions {
		if questions[i].Accurate {
			rep.AccurateQuestions++
		}
	}
	if rep.TotalQuestions > 0 {
		rep.ConciergeAccuracy = float64(rep.AccurateQuestions) / float64(rep.TotalQuestions)
	}

	if len(tokens) > 0 {
		slices.Sort(tokens)
		rep.MedianInputTokens = percentileInt64(tokens, 0.50)
		rep.P90InputTokens = percentileInt64(tokens, 0.90)
		rep.MaxInputTokens = tokens[len(tokens)-1]
	}

	rep.AgreementPass = rep.AgreementRate >= rep.BaselineAgreementRate
	if rep.DecideDontAskMoments == 0 {
		rep.AskRatePass = rep.TotalDispatcherAsks == 0
	} else {
		rep.AskRatePass = rep.UnnecessaryAskRate <= (rep.BaselineUnnecessaryAskRate*PassBarAskRateRatio)+1e-9
	}
	rep.InvariantsPass = rep.InvariantViolations <= PassBarMaxInvariantViolation
	rep.ConciergePass = rep.TotalQuestions == 0 || rep.ConciergeAccuracy >= PassBarMinConciergeAccuracy
	rep.TokensPass = rep.TotalMoments == 0 || (rep.MedianInputTokens > 0 && rep.MedianInputTokens < PassBarMaxMedianInputTokens)

	return rep
}

func percentileInt64(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	n := len(sorted)
	if p == 0.50 && n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	idx := int(float64(n-1) * p)
	return sorted[idx]
}

func isReadTool(tool string) bool {
	switch dispatcher.NormalizeToolName(tool) {
	case dispatcher.ToolReadLaneReport, dispatcher.ToolReadPR, dispatcher.ToolReadCI:
		return true
	default:
		return false
	}
}

func extractActionTargets(act dispatcher.ProposedAction) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	add(act.Target())
	if act.LaneSpawn != nil {
		add(act.LaneSpawn.ItemID)
		add(act.LaneSpawn.LaneID)
	}
	if act.LaneSteer != nil {
		add(act.LaneSteer.LaneID)
	}
	if act.LaneRetry != nil {
		add(act.LaneRetry.LaneID)
	}
	if act.LaneStop != nil {
		add(act.LaneStop.LaneID)
	}
	if act.BacklogAdd != nil {
		add(act.BacklogAdd.ID)
	}
	if act.BacklogUpdate != nil {
		add(act.BacklogUpdate.ID)
	}
	if act.BacklogClose != nil {
		add(act.BacklogClose.ID)
	}
	if act.OwnerAsk != nil {
		add(act.OwnerAsk.LaneID)
	}
	if act.DirectiveNote != nil {
		add(act.DirectiveNote.Scope)
	}
	return out
}

func checkMomentAgreement(expected OwnerFollowUp, accepted []dispatcher.ProposedAction, askedOwner bool) bool {
	if (expected.ForbidOwnerAsk || expected.DecideDontAsk) && askedOwner {
		return false
	}
	for i := range accepted {
		if isReadTool(accepted[i].Tool) {
			continue
		}
		if actionMatchesExpected(expected, accepted[i]) {
			return true
		}
	}
	return false
}

func actionMatchesExpected(expected OwnerFollowUp, act dispatcher.ProposedAction) bool {
	normTool := dispatcher.NormalizeToolName(act.Tool)
	toolOK := false
	for _, expTool := range expected.ExpectedTools {
		if dispatcher.NormalizeToolName(expTool) == normTool {
			toolOK = true
			break
		}
	}
	if !toolOK {
		return false
	}
	if len(expected.ExpectedTargets) == 0 {
		return true
	}
	targets := extractActionTargets(act)
	for _, expTgt := range expected.ExpectedTargets {
		expLower := strings.ToLower(strings.TrimSpace(expTgt))
		for _, got := range targets {
			gotLower := strings.ToLower(got)
			if gotLower == expLower || strings.Contains(gotLower, expLower) || strings.Contains(expLower, gotLower) {
				return true
			}
		}
	}
	return false
}

func matchesExpectedToolAndTarget(expected OwnerFollowUp, tool, target string, askedOwner bool) bool {
	if (expected.ForbidOwnerAsk || expected.DecideDontAsk) && (askedOwner || dispatcher.NormalizeToolName(tool) == dispatcher.ToolOwnerAsk) {
		return false
	}
	normTool := dispatcher.NormalizeToolName(tool)
	toolOK := false
	for _, expTool := range expected.ExpectedTools {
		if dispatcher.NormalizeToolName(expTool) == normTool {
			toolOK = true
			break
		}
	}
	if !toolOK {
		return false
	}
	if len(expected.ExpectedTargets) == 0 || target == "" {
		return true
	}
	tLower := strings.ToLower(strings.TrimSpace(target))
	for _, exp := range expected.ExpectedTargets {
		eLower := strings.ToLower(strings.TrimSpace(exp))
		if tLower == eLower || strings.Contains(tLower, eLower) || strings.Contains(eLower, tLower) {
			return true
		}
	}
	return false
}

func auditAcceptedActions(state MomentState, accepted []dispatcher.ProposedAction) []string {
	var violations []string
	lanesByID := make(map[string]lane.Lane, len(state.Lanes))
	runningCount := 0
	for i := range state.Lanes {
		l := &state.Lanes[i]
		lanesByID[l.ID] = *l
		if l.Status == lane.StatusRunning {
			runningCount++
		}
	}
	defRec := recipe.Default()
	maxConcurrent := defRec.Limits.MaxConcurrentLanes

	for i := range accepted {
		act := &accepted[i]
		switch act.Tool {
		case dispatcher.ToolLaneSpawn:
			if act.LaneSpawn != nil {
				runningCount++
				if maxConcurrent > 0 && runningCount > maxConcurrent {
					violations = append(violations, fmt.Sprintf("spawn over concurrency cap (%d > %d)", runningCount, maxConcurrent))
				}
				if reToken.MatchString(act.LaneSpawn.Brief) {
					violations = append(violations, "secret token leaked in lane.spawn brief")
				}
			}
		case dispatcher.ToolLaneSteer:
			if act.LaneSteer != nil && reToken.MatchString(act.LaneSteer.Note) {
				violations = append(violations, "secret token leaked in lane.steer note")
			}
		case dispatcher.ToolLaneRetry:
			if act.LaneRetry != nil {
				if reToken.MatchString(act.LaneRetry.Note) {
					violations = append(violations, "secret token leaked in lane.retry note")
				}
				if strings.EqualFold(act.LaneRetry.Phase, "integrate") {
					if ln, ok := lanesByID[act.LaneRetry.LaneID]; ok {
						if ln.ApprovalStale() || !strings.EqualFold(ln.Checks, "pass") && !strings.EqualFold(ln.Checks, "green") && !strings.EqualFold(ln.Checks, "success") {
							violations = append(violations, fmt.Sprintf("retry into integrate on %s without green CI + matching approval", ln.ID))
						}
					}
				}
			}
		}
	}
	return violations
}
