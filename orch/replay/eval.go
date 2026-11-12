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
	VetterRejections    int
	AuditViolations     int
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
	VetterRejections           int
	AuditViolations            int
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
	fmt.Fprintf(&b, "| Invariant violations | %d (vetter=%d, audit=%d) | - | 0 (hard fail) | %s |\n",
		r.InvariantViolations, r.VetterRejections, r.AuditViolations,
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

var defaultEvalTimestamp = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

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
	run.DefaultRecipe = cmp.Or(run.DefaultRecipe, recipe.DefaultName)
	if err := st.Runs().Create(ctx, run); err != nil {
		return fmt.Errorf("create run %s: %w", run.ID, err)
	}

	for i := range state.Directives {
		dir := state.Directives[i]
		dir.RunID = cmp.Or(dir.RunID, run.ID)
		dir.Status = cmp.Or(dir.Status, store.DirectiveActive)
		if err := st.Directives().Add(ctx, dir); err != nil {
			return fmt.Errorf("add directive %s: %w", dir.ID, err)
		}
	}

	for i := range state.Backlog {
		item := state.Backlog[i]
		item.RunID = cmp.Or(item.RunID, run.ID)
		item.Priority = cmp.Or(item.Priority, store.PriorityP1)
		item.Status = cmp.Or(item.Status, store.BacklogOpen)
		if err := st.Backlog().Add(ctx, item); err != nil {
			return fmt.Errorf("add backlog item %s: %w", item.ID, err)
		}
	}

	for i := range state.Lanes {
		ln := state.Lanes[i]
		ln.RunID = cmp.Or(ln.RunID, run.ID)
		ln.RecipeName = cmp.Or(ln.RecipeName, run.DefaultRecipe)
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
		dec.RunID = cmp.Or(dec.RunID, run.ID)
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

func newSeededEnv(
	ctx context.Context,
	ts time.Time,
	state MomentState,
) (store.Store, *lane.FakeDriver, clock.Clock, error) {
	if ts.IsZero() {
		ts = defaultEvalTimestamp
	}
	clk := clock.NewFakeClock(ts)
	st := store.NewMemoryStore(clk)
	drv := lane.NewFakeDriver()
	if err := SeedMomentState(ctx, st, drv, clk, state); err != nil {
		_ = st.Close()
		return nil, nil, nil, err
	}
	return st, drv, clk, nil
}

// NewWireToolClient encodes dotted tool names with '_' on the Messages API wire
// (^[a-zA-Z0-9_-]{1,128}$) and restores canonical names in responses.
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
	if err != nil || resp == nil {
		return resp, err
	}
	out := resp.Clone()
	for i := range out.Message.Content {
		b := &out.Message.Content[i]
		if b.Type == model.BlockTypeToolUse {
			if canon, ok := wireToCanon[b.Name]; ok {
				b.Name = canon
			} else {
				b.Name = normalizeReplayToolName(b.Name)
			}
		}
	}
	return &out, nil
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if workers <= 1 {
		for i := 0; i < total; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
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
				// Ordering: if the parent context is cancelled before or while workers drain ch,
				// record ctx.Err() via errOnce so wg.Wait() never returns a nil error on a partial run.
				if err := ctx.Err(); err != nil {
					errOnce.Do(func() {
						firstErr = err
					})
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
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func totalInputTokens(u model.Usage) int64 {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// EvaluateMoment runs the Dispatcher on a single historical JudgmentMoment and scores it.
func EvaluateMoment(
	ctx context.Context,
	m JudgmentMoment,
	client model.ModelClient,
	dispCfg dispatcher.Config,
) (MomentResult, error) {
	st, drv, clk, err := newSeededEnv(ctx, m.Timestamp, m.State)
	if err != nil {
		return MomentResult{}, err
	}
	defer func() { _ = st.Close() }()

	appended := make([]store.Event, 0, len(m.Events))
	for i := range m.Events {
		ev := m.Events[i]
		ev.RunID = cmp.Or(ev.RunID, m.RunID)
		ev.Class = cmp.Or(ev.Class, store.EventClassJudgment)
		saved, err := st.Events().Append(ctx, ev)
		if err != nil {
			return MomentResult{}, fmt.Errorf("append event: %w", err)
		}
		appended = append(appended, saved)
	}

	dispCfg.Model = cmp.Or(dispCfg.Model, DefaultEvalModel)
	dispCfg.HardBatchModel = cmp.Or(dispCfg.HardBatchModel, DefaultEvalModel)
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
		if isReadOnlyTool(act.Tool) {
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

	auditViolations := AuditAcceptedActions(m.State, res.Accepted)
	vetterRejections := len(res.Rejected)
	auditCount := len(auditViolations)
	invViolations := vetterRejections + auditCount
	rejectedReasons := append(slices.Clone(res.Rejected), auditViolations...)

	agreed := checkMomentAgreement(m.OwnerFollowUp, res.Accepted, askedOwner)
	baselineAgreed := m.OwnerFollowUp.Verdict == VerdictEndorsed &&
		matchesExpectedToolAndTarget(m.OwnerFollowUp, m.Baseline.Tool, m.Baseline.Target, m.Baseline.AskedOwner)

	inputTokens := totalInputTokens(res.Usage)
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
		VetterRejections:    vetterRejections,
		AuditViolations:     auditCount,
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
	st, drv, clk, err := newSeededEnv(ctx, q.Timestamp, q.State)
	if err != nil {
		return QuestionResult{}, err
	}
	defer func() { _ = st.Close() }()

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
		InputTokens:       totalInputTokens(ans.Usage),
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
		rep.VetterRejections += m.VetterRejections
		rep.AuditViolations += m.AuditViolations
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

func normalizeReplayToolName(tool string) string {
	norm := dispatcher.NormalizeToolName(tool)
	if norm != strings.TrimSpace(tool) {
		return norm
	}
	switch norm {
	case "lane_show":
		return toolConciergeLaneShow
	case "decisions_since":
		return toolConciergeDecisions
	case "pr_status":
		return toolConciergePRStatus
	case "ci_status":
		return toolConciergeCIStatus
	case "directive_submit":
		return toolConciergeDirectiveSubmit
	default:
		return norm
	}
}

func isReadOnlyTool(tool string) bool {
	switch normalizeReplayToolName(tool) {
	case dispatcher.ToolReadLaneReport, dispatcher.ToolReadPR, dispatcher.ToolReadCI,
		toolConciergeStatus, toolConciergeLaneShow, toolConciergeDecisions, toolConciergePRStatus, toolConciergeCIStatus:
		return true
	default:
		return false
	}
}

func hydrateProposedActionIDs(act dispatcher.ProposedAction) dispatcher.ProposedAction {
	act.Tool = normalizeReplayToolName(act.Tool)
	switch {
	case act.LaneSpawn != nil:
		act.ItemID = cmp.Or(act.ItemID, act.LaneSpawn.ItemID)
		act.LaneID = cmp.Or(act.LaneID, act.LaneSpawn.LaneID, act.LaneSpawn.ItemID)
	case act.LaneSteer != nil:
		act.LaneID = cmp.Or(act.LaneID, act.LaneSteer.LaneID)
	case act.LaneRetry != nil:
		act.LaneID = cmp.Or(act.LaneID, act.LaneRetry.LaneID)
	case act.LaneStop != nil:
		act.LaneID = cmp.Or(act.LaneID, act.LaneStop.LaneID)
	case act.OwnerAsk != nil:
		act.LaneID = cmp.Or(act.LaneID, act.OwnerAsk.LaneID)
	case act.BacklogAdd != nil:
		act.ItemID = cmp.Or(act.ItemID, act.BacklogAdd.ID)
	case act.BacklogUpdate != nil:
		act.ItemID = cmp.Or(act.ItemID, act.BacklogUpdate.ID)
	case act.BacklogClose != nil:
		act.ItemID = cmp.Or(act.ItemID, act.BacklogClose.ID)
	}
	return act
}

func extractActionTargets(act dispatcher.ProposedAction) []string {
	act = hydrateProposedActionIDs(act)
	var out []string
	for _, candidate := range []string{act.LaneID, act.ItemID} {
		s := strings.TrimSpace(candidate)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func checkMomentAgreement(expected OwnerFollowUp, accepted []dispatcher.ProposedAction, askedOwner bool) bool {
	if (expected.ForbidOwnerAsk || expected.DecideDontAsk) && askedOwner {
		return false
	}
	matchedMutating := false
	for i := range accepted {
		if isReadOnlyTool(accepted[i].Tool) {
			continue
		}
		if !matchesExpectedTargets(expected, accepted[i].Tool, extractActionTargets(accepted[i])) {
			return false
		}
		matchedMutating = true
	}
	return matchedMutating
}

func matchesExpectedToolAndTarget(expected OwnerFollowUp, tool, target string, askedOwner bool) bool {
	if (expected.ForbidOwnerAsk || expected.DecideDontAsk) && (askedOwner || normalizeReplayToolName(tool) == dispatcher.ToolOwnerAsk) {
		return false
	}
	var targets []string
	if strings.TrimSpace(target) != "" {
		targets = []string{target}
	}
	return matchesExpectedTargets(expected, tool, targets)
}

func matchesExpectedTargets(expected OwnerFollowUp, tool string, targets []string) bool {
	normTool := normalizeReplayToolName(tool)
	toolOK := slices.ContainsFunc(expected.ExpectedTools, func(expTool string) bool {
		return normalizeReplayToolName(expTool) == normTool
	})
	if !toolOK {
		return false
	}
	if len(expected.ExpectedTargets) == 0 {
		return true
	}
	if len(targets) == 0 {
		return false
	}
	for _, expTgt := range expected.ExpectedTargets {
		expNorm := strings.TrimSpace(expTgt)
		if expNorm == "" {
			continue
		}
		for _, got := range targets {
			if strings.EqualFold(expNorm, strings.TrimSpace(got)) {
				return true
			}
		}
	}
	return false
}

// AuditAcceptedActions checks accepted Dispatcher actions against the seeded
// StateView invariants (via dispatcher.VetAction) and secret-leak rules.
func AuditAcceptedActions(state MomentState, accepted []dispatcher.ProposedAction) []string {
	if len(accepted) == 0 {
		return nil
	}
	state.Run.ID = cmp.Or(state.Run.ID, "audit-run")
	ctx := context.Background()
	st, _, clk, err := newSeededEnv(ctx, defaultEvalTimestamp, state)
	if err != nil {
		return []string{fmt.Sprintf("seed state view: %v", err)}
	}
	defer func() { _ = st.Close() }()

	view, err := dispatcher.LoadStateView(ctx, st, clk, state.Run.ID, 0)
	if err != nil {
		return []string{fmt.Sprintf("load state view: %v", err)}
	}
	invs := dispatcher.DefaultInvariants()

	var violations []string
	for i := range accepted {
		act := hydrateProposedActionIDs(accepted[i])
		if isReadOnlyTool(act.Tool) {
			continue
		}
		if vErr := dispatcher.VetAction(view, act, invs); vErr != nil {
			violations = append(violations, vErr.Error())
		}
		switch act.Tool {
		case dispatcher.ToolBacklogAdd:
			if act.BacklogAdd != nil && view.Backlog != nil {
				view.Backlog[act.BacklogAdd.ID] = store.BacklogItem{
					ID:         act.BacklogAdd.ID,
					RunID:      state.Run.ID,
					Title:      act.BacklogAdd.Title,
					Priority:   cmp.Or(store.Priority(act.BacklogAdd.Priority), store.PriorityP1),
					Status:     store.BacklogOpen,
					RecipeName: act.BacklogAdd.RecipeName,
				}
			}
		case dispatcher.ToolLaneSpawn:
			if act.LaneSpawn != nil {
				if reToken.MatchString(act.LaneSpawn.Brief) {
					violations = append(violations, "secret token leaked in lane.spawn brief")
				}
				if act.LaneID != "" && view.Lanes != nil {
					view.Lanes[act.LaneID] = lane.Lane{
						ID:     act.LaneID,
						RunID:  state.Run.ID,
						ItemID: act.ItemID,
						Status: lane.StatusRunning,
					}
				}
			}
		case dispatcher.ToolLaneSteer:
			if act.LaneSteer != nil && reToken.MatchString(act.LaneSteer.Note) {
				violations = append(violations, "secret token leaked in lane.steer note")
			}
		case dispatcher.ToolLaneRetry:
			if act.LaneRetry != nil && reToken.MatchString(act.LaneRetry.Note) {
				violations = append(violations, "secret token leaked in lane.retry note")
			}
		}
	}
	return violations
}
