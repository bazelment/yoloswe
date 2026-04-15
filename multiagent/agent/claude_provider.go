package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/wt"
)

// retryPromptTemplate is the user message sent when a turn ends with an
// unresolved tool error. It restates the failing tool and a short excerpt
// (the model's own history already has the full result, but restating
// makes the "act on this" signal unambiguous) and specifically names the
// parallel-cancellation footgun so the model doesn't try to "fix" the
// cancelled sibling rather than the real cause.
const retryPromptTemplate = `Your previous turn ended with a tool error that was not addressed:

  tool: %s
  result: %s

This is the source of the failure — if a sibling tool in a parallel batch was cancelled, the real error is in the *other* sibling. Fix the underlying problem and continue the task. Do not stop until the task is complete or you have a concrete blocker to report.`

// unresolvedToolErrorMarkerTemplate is appended to AgentResult.Text when
// the retry loop exits with a still-errored final turn. Downstream round
// logic sees this textually and can surface it in the round's comment.
const unresolvedToolErrorMarkerTemplate = "\n\n[unresolved tool error after %d/%d retries — tool: %s\n excerpt: %s]"

// minRetryWallClockBudget is the floor for computeRetryTimeBudget. The
// budget is max(this, cfg.MaxTurns * 1 minute) to keep short-turn configs
// from getting an unreasonably tight ceiling while still bounding runaway.
const minRetryWallClockBudget = 10 * time.Minute

// computeRetryTimeBudget returns the wall-clock cap for the retry loop,
// measured from the start of the *original* Ask. Prevents runaway where
// each retry takes 5+ minutes of agent work.
func computeRetryTimeBudget(cfg ExecuteConfig) time.Duration {
	turnBudget := time.Duration(cfg.MaxTurns) * time.Minute
	if turnBudget > minRetryWallClockBudget {
		return turnBudget
	}
	return minRetryWallClockBudget
}

// buildRetryPrompt renders the user message sent to the session on retry.
func buildRetryPrompt(toolName, excerpt string) string {
	return fmt.Sprintf(retryPromptTemplate, toolName, excerpt)
}

// appendUnresolvedToolErrorMarker annotates agent output with a marker
// describing the unresolved tool error. Consumers (e.g. jiradozer round
// comments) see this textually and can flag it for triage.
func appendUnresolvedToolErrorMarker(text string, attempts, max int, toolName, excerpt string) string {
	return text + fmt.Sprintf(unresolvedToolErrorMarkerTemplate, attempts, max, toolName, excerpt)
}

// emitRetry fires the optional RetryHandler hook on an EventHandler, if the
// handler implements it. Called before each follow-up Ask so logs and the
// renderer see the decision even if the retry subsequently hangs.
func emitRetry(h EventHandler, attempt, max int, toolName, excerpt string) {
	if h == nil {
		return
	}
	if rh, ok := h.(RetryHandler); ok {
		rh.OnRetry(attempt, max, toolName, excerpt)
	}
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

	// Execute the initial turn. When MaxToolErrorRetries > 0, the retry
	// loop below keeps the same live session alive and injects follow-up
	// user messages for turns that end with an unresolved tool error.
	start := time.Now()
	budget := computeRetryTimeBudget(cfg)

	result, err := session.Ask(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	var (
		prevExcerpt  string
		lastToolName string
		lastExcerpt  string
		lastOK       bool
		attempts     int
	)
	for attempt := 1; attempt <= cfg.MaxToolErrorRetries; attempt++ {
		toolName, excerpt, ok := claude.FinalTurnToolError(result.ContentBlocks)
		lastToolName, lastExcerpt, lastOK = toolName, excerpt, ok
		if !ok {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) >= budget {
			break
		}
		if prevExcerpt != "" && excerpt == prevExcerpt {
			// No-progress guard: the model is stuck on the same error, so
			// further retries will waste turns. Exit and let the marker
			// surface the failure.
			break
		}
		prevExcerpt = excerpt
		attempts = attempt

		emitRetry(cfg.EventHandler, attempt, cfg.MaxToolErrorRetries, toolName, excerpt)

		next, askErr := session.Ask(ctx, buildRetryPrompt(toolName, excerpt))
		if askErr != nil {
			return nil, askErr
		}
		result = next
		// Recompute after the retry; if the new turn is clean we'll break
		// on the next iteration's FinalTurnToolError check.
		lastToolName, lastExcerpt, lastOK = claude.FinalTurnToolError(result.ContentBlocks)
	}

	agentResult := ClaudeResultToAgentResult(result)
	if info := session.Info(); info != nil {
		agentResult.SessionID = info.SessionID
	}
	// If the retry loop exited with a still-errored final turn, annotate
	// the output so downstream round logic can surface the failure.
	if lastOK && cfg.MaxToolErrorRetries > 0 {
		agentResult.Text = appendUnresolvedToolErrorMarker(
			agentResult.Text, attempts, cfg.MaxToolErrorRetries, lastToolName, lastExcerpt)
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
