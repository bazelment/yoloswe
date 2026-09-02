package agent

import (
	"context"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
	"github.com/bazelment/yoloswe/wt"
)

// AgyProvider wraps Antigravity's agy CLI behind the Provider interface.
type AgyProvider struct {
	events      chan AgentEvent
	sessionOpts []agy.SessionOption
}

// NewAgyProvider creates a new Antigravity provider.
func NewAgyProvider(sessionOpts ...agy.SessionOption) *AgyProvider {
	return &AgyProvider{
		events:      make(chan AgentEvent, 100),
		sessionOpts: sessionOpts,
	}
}

func (p *AgyProvider) Name() string { return ProviderAgy }

func (p *AgyProvider) Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error) {
	cfg := applyOptions(opts)
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	sessionOpts := append([]agy.SessionOption{}, p.sessionOpts...)
	// applyOptions defaults Model to "sonnet" when the caller named nothing;
	// CLIModelArg strips it as a Claude ID, along with placeholders and any
	// other provider's models (see cursorSessionOpts for the same idiom).
	if model := CLIModelArg(cfg.Model, ProviderAgy); model != "" {
		sessionOpts = append(sessionOpts, agy.WithModel(model))
	}
	if effort := agyEffortLevel(cfg.Effort); effort != "" {
		sessionOpts = append(sessionOpts, agy.WithEffort(effort))
	}
	if cfg.WorkDir != "" {
		sessionOpts = append(sessionOpts, agy.WithWorkDir(cfg.WorkDir))
	}
	if cfg.ResumeSessionID != "" {
		sessionOpts = append(sessionOpts, agy.WithConversation(cfg.ResumeSessionID))
	}
	switch strings.ToLower(strings.TrimSpace(cfg.PermissionMode)) {
	case "bypass":
		sessionOpts = append(sessionOpts, agy.WithDangerouslySkipPermissions())
	case "plan":
		sessionOpts = append(sessionOpts, agy.WithSandbox())
	}

	session := agy.NewSession(fullPrompt, sessionOpts...)
	if err := session.Start(ctx); err != nil {
		return nil, err
	}
	defer session.Stop()

	var resultText strings.Builder
	for evt := range session.Events() {
		switch e := evt.(type) {
		case agy.TextEvent:
			resultText.WriteString(e.Text)
			if cfg.EventHandler != nil {
				cfg.EventHandler.OnText(e.Text)
			}
			p.events <- TextAgentEvent{Text: e.Text}
		case agy.TurnCompleteEvent:
			if cfg.EventHandler != nil {
				cfg.EventHandler.OnTurnComplete(1, e.Success, e.DurationMs, 0)
			}
			p.events <- TurnCompleteAgentEvent{TurnNumber: 1, Success: e.Success, DurationMs: e.DurationMs}
			return &AgentResult{
				Text:       resultText.String(),
				Success:    e.Success,
				Error:      e.Error,
				DurationMs: e.DurationMs,
				SessionID:  e.ConversationID,
				Usage: AgentUsage{
					InputTokens:     e.Usage.InputTokens,
					OutputTokens:    e.Usage.OutputTokens,
					CacheReadTokens: e.Usage.CacheReadTokens,
				},
			}, nil
		case agy.ErrorEvent:
			if cfg.EventHandler != nil {
				cfg.EventHandler.OnError(e.Error, e.Context)
			}
			p.events <- ErrorAgentEvent{Err: e.Error, Context: e.Context}
			return nil, e.Error
		}
	}

	return nil, agy.ErrNotStarted
}

func (p *AgyProvider) Events() <-chan AgentEvent { return p.events }

func (p *AgyProvider) Close() error {
	close(p.events)
	return nil
}

// agyEffortLevel maps the neutral agent.EffortLevel to agy's --effort values.
func agyEffortLevel(level EffortLevel) string {
	switch level {
	case EffortAuto, "":
		return ""
	case EffortLow:
		return "low"
	case EffortMedium:
		return "medium"
	case EffortHigh, EffortMax:
		return "high"
	}
	return ""
}
