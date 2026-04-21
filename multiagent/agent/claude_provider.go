package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/wt"
)

const retryPrompt = "retry"

// UnresolvedToolErrorMarkerPrefix is a stable prefix that identifies the
// unresolved-tool-error marker in agent text output. Callers can use it to
// detect whether the marker is already present before re-appending.
const UnresolvedToolErrorMarkerPrefix = "[unresolved tool error ("

const unresolvedToolErrorMarkerTemplate = UnresolvedToolErrorMarkerPrefix + "%s) after %d/%d retries — tool: %s\n excerpt: %s]"

// Abort-reason constants for UnresolvedToolError.Reason.
const (
	RetryStopExhausted      = "exhausted"
	RetryStopNoProgress     = "no_progress"
	RetryStopBudgetExceeded = "budget_exceeded"
	RetryStopCtxCancelled   = "ctx_cancelled"
)

// FormatUnresolvedToolErrorMarker returns the marker string used to surface
// an unresolved tool error in agent text output. Callers that replace
// AgentResult.Text downstream (e.g. plan-file readers) should re-append this
// marker so the failure is not silently dropped.
func FormatUnresolvedToolErrorMarker(e UnresolvedToolError) string {
	reason := e.Reason
	if reason == "" {
		reason = RetryStopExhausted
	}
	return fmt.Sprintf(unresolvedToolErrorMarkerTemplate, reason, e.Attempts, e.Max, e.Tool, e.Excerpt)
}

// AppendUnresolvedToolErrorMarker appends the marker to text, using a blank
// separator when text is non-empty. Exported for callers that re-append the
// marker after downstream text substitution.
func AppendUnresolvedToolErrorMarker(text string, e UnresolvedToolError) string {
	marker := FormatUnresolvedToolErrorMarker(e)
	if text == "" {
		return marker
	}
	return text + "\n\n" + marker
}

const minRetryWallClockBudget = 10 * time.Minute

func computeRetryTimeBudget(cfg ExecuteConfig) time.Duration {
	turnBudget := time.Duration(cfg.MaxTurns) * time.Minute
	if turnBudget > minRetryWallClockBudget {
		return turnBudget
	}
	return minRetryWallClockBudget
}

func buildRetryPrompt() string {
	return retryPrompt
}

// emitRetry fires the hook before each follow-up Ask so logs see the
// decision even if the retry subsequently hangs.
func emitRetry(h EventHandler, attempt, max int, toolName, excerpt string) {
	if h == nil {
		return
	}
	if rh, ok := h.(RetryHandler); ok {
		rh.OnRetry(attempt, max, toolName, excerpt)
	}
}

func emitRetryAbort(h EventHandler, reason, toolName, excerpt string) {
	if h == nil {
		return
	}
	if rh, ok := h.(RetryHandler); ok {
		rh.OnRetryAbort(reason, toolName, excerpt)
	}
}

// retrySession is the minimal slice that the retry loop depends on. Exists
// so tests can drive runRetryLoop with a fake that scripts a sequence of
// TurnResults without spawning a real CLI. Production callers supply a
// streamTurnSession that issues each retry via the raw streaming loop.
type retrySession interface {
	Ask(ctx context.Context, content string) (*claude.TurnResult, error)
}

// streamTurnSession adapts a streaming-turn closure to retrySession. Used by
// Execute so the retry loop can issue "retry" turns via the same streaming
// path the initial turn used, without runRetryLoop needing to know about
// logicalTurnState.
type streamTurnSession struct {
	fn func(ctx context.Context, prompt string) (*claude.TurnResult, error)
}

func (s streamTurnSession) Ask(ctx context.Context, content string) (*claude.TurnResult, error) {
	return s.fn(ctx, content)
}

// ClaudeProvider wraps the Claude SDK behind the Provider interface.
type ClaudeProvider struct {
	events      chan AgentEvent
	sessionOpts []claude.SessionOption
}

// NewClaudeProvider creates a new Claude provider.
// Optional session options are prepended before per-call options built from ExecuteConfig.
func NewClaudeProvider(sessionOpts ...claude.SessionOption) *ClaudeProvider {
	return &ClaudeProvider{
		events:      make(chan AgentEvent, 100),
		sessionOpts: sessionOpts,
	}
}

func (p *ClaudeProvider) Name() string { return "claude" }

// runRetryLoop drives the provider-layer retry-on-tool-error loop. Takes
// the result of the initial Ask and may issue follow-up Asks up to
// cfg.MaxToolErrorRetries times. Exits early when no tool_use_error is
// present or agent self-recovered (G1/G4), the retry budget is exhausted,
// or the ctx is cancelled. Returns the final TurnResult, the number of
// retry Asks issued, and the stop reason recorded on the last decision.
// The caller appends the unresolved marker and fires OnRetryAbort on the
// returned stop reason when applicable.
//
// Live background work is no longer gated here: the raw event stream in
// Execute never completes a logical turn while bg tasks are outstanding,
// so runRetryLoop only sees results after bg settles.
func runRetryLoop(ctx context.Context, session retrySession, initial *claude.TurnResult, cfg ExecuteConfig) (*claude.TurnResult, int, string, error) {
	result := initial
	start := time.Now()
	budget := computeRetryTimeBudget(cfg)
	stopReason := RetryStopExhausted
	var (
		prevExcerpt string
		havePrev    bool
		attempts    int
	)
	for attempts < cfg.MaxToolErrorRetries {
		toolName, excerpt, ok := claude.FinalTurnToolError(result.ContentBlocks)
		if !ok {
			break
		}
		if ctx.Err() != nil {
			stopReason = RetryStopCtxCancelled
			break
		}
		if time.Since(start) >= budget {
			stopReason = RetryStopBudgetExceeded
			break
		}
		if havePrev && excerpt == prevExcerpt {
			stopReason = RetryStopNoProgress
			break
		}
		prevExcerpt = excerpt
		havePrev = true
		attempts++

		emitRetry(cfg.EventHandler, attempts, cfg.MaxToolErrorRetries, toolName, excerpt)

		next, askErr := session.Ask(ctx, buildRetryPrompt())
		if askErr != nil {
			return nil, attempts, stopReason, askErr
		}
		result = next
	}
	return result, attempts, stopReason, nil
}

func (p *ClaudeProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error) {
	cfg := applyOptions(opts)

	// Build full prompt with worktree context
	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	// Map to Claude session options: provider-level opts first, then per-call overrides
	sessionOpts := append([]claude.SessionOption{}, p.sessionOpts...)
	sessionOpts = append(sessionOpts,
		claude.WithModel(cfg.Model),
	)
	if cfg.KeepUserSettings {
		sessionOpts = append(sessionOpts, claude.WithKeepUserSettings())
	}
	if cfg.WorkDir != "" {
		sessionOpts = append(sessionOpts, claude.WithWorkDir(cfg.WorkDir))
	}
	if cfg.SystemPrompt != "" {
		sessionOpts = append(sessionOpts, claude.WithSystemPrompt(cfg.SystemPrompt))
	}
	switch cfg.PermissionMode {
	case "bypass":
		sessionOpts = append(sessionOpts, claude.WithPermissionMode(claude.PermissionModeBypass))
	case "plan":
		sessionOpts = append(sessionOpts, claude.WithPermissionMode(claude.PermissionModePlan))
	}
	if cfg.MaxTurns > 0 {
		sessionOpts = append(sessionOpts, claude.WithMaxTurns(cfg.MaxTurns))
	}
	if cfg.MaxBudgetUSD > 0 {
		sessionOpts = append(sessionOpts, claude.WithMaxBudgetUSD(cfg.MaxBudgetUSD))
	}
	if cfg.ResumeSessionID != "" {
		sessionOpts = append(sessionOpts, claude.WithResume(cfg.ResumeSessionID))
	}

	// Create ephemeral session
	session := claude.NewSession(sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	// streamTurn drives one logical turn against the session using the raw
	// event stream. Replaces the old session.Ask path: consume events until
	// logicalTurnState.LogicalTurnDone() flips, feeding EventHandler + the
	// AgentEvent channel along the way.
	streamTurn := func(ctx context.Context, prompt string) (*claude.TurnResult, error) {
		if err := session.Query(ctx, prompt); err != nil {
			return nil, err
		}
		state := newLogicalTurnState()
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case ev, ok := <-session.Events():
				if !ok {
					return state.ToTurnResult(), state.Err()
				}
				state.Apply(ev)
				dispatchClaudeEvent(ev, cfg.EventHandler, p.events)
				if state.LogicalTurnDone() {
					return state.ToTurnResult(), state.Err()
				}
			}
		}
	}

	result, err := streamTurn(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	result, attempts, stopReason, err := runRetryLoop(ctx, streamTurnSession{fn: streamTurn}, result, cfg)
	if err != nil {
		return nil, err
	}

	agentResult := ClaudeResultToAgentResult(result)
	if info := session.Info(); info != nil {
		agentResult.SessionID = info.SessionID
	}
	if cfg.MaxToolErrorRetries > 0 {
		if toolName, excerpt, ok := claude.FinalTurnToolError(result.ContentBlocks); ok {
			unresolved := &UnresolvedToolError{
				Tool:     toolName,
				Excerpt:  excerpt,
				Reason:   stopReason,
				Attempts: attempts,
				Max:      cfg.MaxToolErrorRetries,
			}
			agentResult.UnresolvedToolError = unresolved
			agentResult.Text = AppendUnresolvedToolErrorMarker(agentResult.Text, *unresolved)
			// Fire the abort callback once per loop execution that stopped
			// with a tool error still present. Covers all four stop reasons:
			// exhausted, no_progress, budget_exceeded, ctx_cancelled.
			emitRetryAbort(cfg.EventHandler, stopReason, toolName, excerpt)
		}
	}
	return agentResult, nil
}

func (p *ClaudeProvider) Events() <-chan AgentEvent { return p.events }

func (p *ClaudeProvider) Close() error {
	close(p.events)
	return nil
}

// ClaudeLongRunningProvider wraps a persistent Claude session.
type ClaudeLongRunningProvider struct {
	eventHandler EventHandler
	*ClaudeProvider
	session     *claude.Session
	sessionOpts []claude.SessionOption
}

// NewClaudeLongRunningProvider creates a Claude provider with a persistent session.
func NewClaudeLongRunningProvider(sessionOpts ...claude.SessionOption) *ClaudeLongRunningProvider {
	return &ClaudeLongRunningProvider{
		ClaudeProvider: NewClaudeProvider(),
		sessionOpts:    sessionOpts,
	}
}

func (p *ClaudeLongRunningProvider) Start(ctx context.Context) error {
	p.session = claude.NewSession(p.sessionOpts...)
	if err := p.session.Start(ctx); err != nil {
		return err
	}
	if p.eventHandler != nil {
		go bridgeEvents(p.session.Events(), p.eventHandler, p.events, nil, "", nil)
	}
	return nil
}

// SendMessage issues one Ask on the persistent session. Unlike Execute, it
// does not run the retry loop — tool-error retries are not applied here.
func (p *ClaudeLongRunningProvider) SendMessage(ctx context.Context, message string) (*AgentResult, error) {
	result, err := p.session.Ask(ctx, message)
	if err != nil {
		return nil, err
	}
	return ClaudeResultToAgentResult(result), nil
}

func (p *ClaudeLongRunningProvider) Stop() error {
	if p.session != nil {
		return p.session.Stop()
	}
	return nil
}

// dispatchClaudeEvent mirrors bridgeEvents' kind-dispatch for a single
// Claude Event, invoking the EventHandler callbacks and emitting AgentEvent
// values. Inlined here so Execute can drive the event loop synchronously on
// the caller's goroutine — no bridge goroutine, no channel tee.
//
// Keep in sync with bridgeEvents (multiagent/agent/bridge.go). The two
// dispatch paths coexist because Execute needs to feed logicalTurnState
// alongside EventHandler on the same goroutine, while long-running providers
// still use the generic bridge.
func dispatchClaudeEvent(ev claude.Event, handler EventHandler, out chan<- AgentEvent) {
	sev, ok := any(ev).(agentstream.Event)
	if !ok {
		return
	}
	kind := sev.StreamEventKind()
	if kind == agentstream.KindUnknown {
		return
	}
	switch kind {
	case agentstream.KindReady:
		if handler != nil {
			if sh, ok := handler.(SessionInitHandler); ok {
				if re, ok := sev.(agentstream.Ready); ok {
					sh.OnSessionInit(re.StreamSessionID())
				}
			}
		}
	case agentstream.KindText:
		te := sev.(agentstream.Text)
		delta := te.StreamDelta()
		if handler != nil {
			handler.OnText(delta)
		}
		if out != nil {
			select {
			case out <- TextAgentEvent{Text: delta}:
			default:
			}
		}
	case agentstream.KindThinking:
		te := sev.(agentstream.Text)
		delta := te.StreamDelta()
		if handler != nil {
			handler.OnThinking(delta)
		}
		if out != nil {
			select {
			case out <- ThinkingAgentEvent{Thinking: delta}:
			default:
			}
		}
	case agentstream.KindToolStart:
		ts := sev.(agentstream.ToolStart)
		name := ts.StreamToolName()
		callID := ts.StreamToolCallID()
		input := ts.StreamToolInput()
		if handler != nil {
			handler.OnToolStart(name, callID, input)
		}
		if out != nil {
			select {
			case out <- ToolStartAgentEvent{Name: name, ID: callID, Input: input}:
			default:
			}
		}
	case agentstream.KindToolEnd:
		te := sev.(agentstream.ToolEnd)
		name := te.StreamToolName()
		callID := te.StreamToolCallID()
		input := te.StreamToolInput()
		result := te.StreamToolResult()
		isError := te.StreamToolIsError()
		if handler != nil {
			handler.OnToolComplete(name, callID, input, result, isError)
		}
		if out != nil {
			select {
			case out <- ToolCompleteAgentEvent{
				Name:    name,
				ID:      callID,
				Input:   input,
				Result:  result,
				IsError: isError,
			}:
			default:
			}
		}
	case agentstream.KindTurnComplete:
		tc := sev.(agentstream.TurnComplete)
		turnNum := tc.StreamTurnNum()
		success := tc.StreamIsSuccess()
		duration := tc.StreamDuration()
		cost := tc.StreamCost()
		if handler != nil {
			handler.OnTurnComplete(turnNum, success, duration, cost)
		}
		if out != nil {
			select {
			case out <- TurnCompleteAgentEvent{
				TurnNumber: turnNum,
				Success:    success,
				DurationMs: duration,
				CostUSD:    cost,
			}:
			default:
			}
		}
	case agentstream.KindError:
		ee := sev.(agentstream.Error)
		err := ee.StreamErr()
		errCtx := ee.StreamErrorContext()
		if handler != nil {
			handler.OnError(err, errCtx)
		}
		if out != nil {
			select {
			case out <- ErrorAgentEvent{Err: err, Context: errCtx}:
			default:
			}
		}
	}
}

// ClaudeResultToAgentResult converts a claude.TurnResult to AgentResult.
func ClaudeResultToAgentResult(r *claude.TurnResult) *AgentResult {
	if r == nil {
		return nil
	}
	return &AgentResult{
		Text:       r.Text,
		Thinking:   r.Thinking,
		Success:    r.Success,
		Error:      r.Error,
		DurationMs: r.DurationMs,
		Usage: AgentUsage{
			InputTokens:     r.Usage.InputTokens,
			OutputTokens:    r.Usage.OutputTokens,
			CacheReadTokens: r.Usage.CacheReadTokens,
			CostUSD:         r.Usage.CostUSD,
		},
	}
}
