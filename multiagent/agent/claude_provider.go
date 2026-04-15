package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/wt"
)

const retryPromptTemplate = `Your previous turn ended with a tool error that was not addressed:

  tool: %s
  result: %s

This is the source of the failure — if a sibling tool in a parallel batch was cancelled, the real error is in the *other* sibling. Fix the underlying problem and continue the task. Do not stop until the task is complete or you have a concrete blocker to report.`

const unresolvedToolErrorMarkerTemplate = "\n\n[unresolved tool error after %d/%d retries — tool: %s\n excerpt: %s]"

const minRetryWallClockBudget = 10 * time.Minute

func computeRetryTimeBudget(cfg ExecuteConfig) time.Duration {
	turnBudget := time.Duration(cfg.MaxTurns) * time.Minute
	if turnBudget > minRetryWallClockBudget {
		return turnBudget
	}
	return minRetryWallClockBudget
}

func buildRetryPrompt(toolName, excerpt string) string {
	return fmt.Sprintf(retryPromptTemplate, toolName, excerpt)
}

func appendUnresolvedToolErrorMarker(text string, attempts, max int, toolName, excerpt string) string {
	return text + fmt.Sprintf(unresolvedToolErrorMarkerTemplate, attempts, max, toolName, excerpt)
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

	start := time.Now()
	budget := computeRetryTimeBudget(cfg)

	result, err := session.Ask(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	var (
		prevExcerpt string
		attempts    int
	)
	for attempts < cfg.MaxToolErrorRetries {
		toolName, excerpt, ok := claude.FinalTurnToolError(result.ContentBlocks)
		if !ok || ctx.Err() != nil || time.Since(start) >= budget {
			break
		}
		if attempts > 0 && excerpt == prevExcerpt {
			break
		}
		prevExcerpt = excerpt
		attempts++

		emitRetry(cfg.EventHandler, attempts, cfg.MaxToolErrorRetries, toolName, excerpt)

		next, askErr := session.Ask(ctx, buildRetryPrompt(toolName, excerpt))
		if askErr != nil {
			return nil, askErr
		}
		result = next
	}

	agentResult := ClaudeResultToAgentResult(result)
	if info := session.Info(); info != nil {
		agentResult.SessionID = info.SessionID
	}
	if cfg.MaxToolErrorRetries > 0 {
		if toolName, excerpt, ok := claude.FinalTurnToolError(result.ContentBlocks); ok {
			agentResult.Text = appendUnresolvedToolErrorMarker(
				agentResult.Text, attempts, cfg.MaxToolErrorRetries, toolName, excerpt)
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
