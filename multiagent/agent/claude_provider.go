package agent

import (
	"context"
	"fmt"
	"time"

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
	// RetryStopBgWorkLive fires when the turn ended with uncancelled
	// background work the agent deliberately parked on. Re-Asking the
	// session would interrupt the park and, on ephemeral sessions, the
	// defer Stop() would orphan the bg work. No retry is attempted.
	RetryStopBgWorkLive = "bg_work_live"
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

// retrySession is the minimal slice of *claude.Session that the retry
// loop depends on. Exists so tests can drive runRetryLoop with a fake
// that scripts a sequence of TurnResults without spawning a real CLI.
type retrySession interface {
	Ask(ctx context.Context, content string) (*claude.TurnResult, error)
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
// cfg.MaxToolErrorRetries times. Exits early when the turn is parked on
// bg work (G2), no tool_use_error is present (G1), the retry budget is
// exhausted, or the ctx is cancelled. Returns the final TurnResult, the
// number of retry Asks issued, and the stop reason recorded on the last
// decision. The caller appends the unresolved marker and fires
// OnRetryAbort on the returned stop reason when applicable.
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
		// G2: never retry when the turn ended with live background work.
		// Re-Ask would interrupt the park and defer session.Stop() would
		// orphan the bg tasks. Check before the content walk so the signal
		// dominates even on turns that also carry a tool_use_error.
		if result.HasLiveBackgroundWork {
			stopReason = RetryStopBgWorkLive
			break
		}
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

	// Bridge Claude events to AgentEvent channel and EventHandler.
	// session.Stop() closes the events channel, which causes bridgeEvents
	// to exit naturally after draining all buffered events. We wait on
	// bridgeDone to ensure all OnToolComplete callbacks (including
	// ExitPlanMode) have fired before Execute() returns.
	var bridgeDone chan struct{}
	if cfg.EventHandler != nil {
		bridgeDone = make(chan struct{})
		go func() {
			bridgeEvents(session.Events(), cfg.EventHandler, p.events, nil, "", nil)
			close(bridgeDone)
		}()
	}
	defer func() {
		session.Stop()
		if bridgeDone != nil {
			<-bridgeDone
		}
	}()

	result, err := session.Ask(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	result, attempts, stopReason, err := runRetryLoop(ctx, session, result, cfg)
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
			// with a tool error still present. Covers all five stop reasons:
			// exhausted, no_progress, budget_exceeded, ctx_cancelled, bg_work_live.
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
