package agent

import (
	"context"
	"fmt"
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
	if cfg.Effort != "" && cfg.Effort != EffortAuto {
		return nil, EffortUnsupportedError(p.Name(), cfg.Effort)
	}
	if !cfg.LLMEndpoint.IsZero() {
		return nil, fmt.Errorf("agy: LLMEndpoint is not supported; use claude, codex, or cursor for third-party endpoint routing")
	}

	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	sessionOpts := append([]agy.SessionOption{}, p.sessionOpts...)
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
