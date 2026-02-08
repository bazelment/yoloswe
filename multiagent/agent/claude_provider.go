package agent

import (
	"context"
	"sync"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/wt"
)

// ClaudeProvider wraps the Claude SDK behind the Provider interface.
type ClaudeProvider struct {
	events chan AgentEvent
	mu     sync.Mutex
}

// NewClaudeProvider creates a new Claude provider.
func NewClaudeProvider() *ClaudeProvider {
	return &ClaudeProvider{
		events: make(chan AgentEvent, 100),
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

	// Map to Claude session options
	sessionOpts := []claude.SessionOption{
		claude.WithModel(cfg.Model),
		claude.WithDisablePlugins(),
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

	// Create ephemeral session
	session := claude.NewSession(sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	// Bridge Claude events to AgentEvent channel and EventHandler
	if cfg.EventHandler != nil {
		go bridgeClaudeEvents(session.Events(), cfg.EventHandler, p.events)
	}

	// Execute single turn
	result, err := session.Ask(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	return ClaudeResultToAgentResult(result), nil
}

func (p *ClaudeProvider) Events() <-chan AgentEvent { return p.events }

func (p *ClaudeProvider) Close() error {
	close(p.events)
	return nil
}

// ClaudeLongRunningProvider wraps a persistent Claude session.
type ClaudeLongRunningProvider struct {
	*ClaudeProvider
	session      *claude.Session
	sessionOpts  []claude.SessionOption
	eventHandler EventHandler
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
		go bridgeClaudeEvents(p.session.Events(), p.eventHandler, p.events)
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

// bridgeClaudeEvents converts Claude events to AgentEvents and forwards to handler.
func bridgeClaudeEvents(events <-chan claude.Event, handler EventHandler, ch chan<- AgentEvent) {
	if events == nil {
		return
	}
	for event := range events {
		switch e := event.(type) {
		case claude.TextEvent:
			handler.OnText(e.Text)
			select {
			case ch <- TextAgentEvent{Text: e.Text}:
			default:
			}
		case claude.ThinkingEvent:
			handler.OnThinking(e.Thinking)
			select {
			case ch <- ThinkingAgentEvent{Thinking: e.Thinking}:
			default:
			}
		case claude.ToolStartEvent:
			handler.OnToolStart(e.Name, e.ID, nil)
			select {
			case ch <- ToolStartAgentEvent{Name: e.Name, ID: e.ID}:
			default:
			}
		case claude.ToolCompleteEvent:
			handler.OnToolComplete(e.Name, e.ID, e.Input, nil, false)
			select {
			case ch <- ToolCompleteAgentEvent{Name: e.Name, ID: e.ID, Input: e.Input}:
			default:
			}
		case claude.TurnCompleteEvent:
			handler.OnTurnComplete(e.TurnNumber, e.Success, e.DurationMs, e.Usage.CostUSD)
			select {
			case ch <- TurnCompleteAgentEvent{TurnNumber: e.TurnNumber, Success: e.Success, DurationMs: e.DurationMs, CostUSD: e.Usage.CostUSD}:
			default:
			}
		case claude.ErrorEvent:
			handler.OnError(e.Error, e.Context)
			select {
			case ch <- ErrorAgentEvent{Err: e.Error, Context: e.Context}:
			default:
			}
		}
	}
}
