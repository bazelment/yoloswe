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
	sessionOpts = append(sessionOpts, agySessionOpts(cfg)...)

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

// agySessionOpts builds the agy session options an ExecuteConfig implies.
//
// Split out of Execute so the arguments that actually reach the CLI are
// unit-testable without a subprocess (see cursorSessionOpts/codexTurnOptions
// for the same idiom, and TestAgySessionOpts_* for the tests this enables).
func agySessionOpts(cfg ExecuteConfig) []agy.SessionOption {
	var opts []agy.SessionOption

	// applyOptions defaults Model to "sonnet" when the caller named nothing;
	// CLIModelArg strips it as a Claude ID, along with placeholders and any
	// other provider's models (see cursorSessionOpts for the same idiom).
	model := CLIModelArg(cfg.Model, ProviderAgy)
	if model != "" {
		opts = append(opts, agy.WithModel(model))
	}

	// agy's catalog encodes the effort level in the model id itself
	// (gemini-3.8-flash-low, gemini-3.1-pro-high, ...) *and* offers a separate
	// --effort flag. Passing both is a hard error from the CLI:
	//
	//	Error: invalid model selection (--model "gemini-3.1-pro-high"
	//	--effort "low"): --model gemini-3.1-pro-high conflicts with --effort=low
	//
	// A model id that already pins a level is therefore the more specific
	// request and wins; --effort is only emitted when the model leaves the
	// level open. Without this an agy-backed caller that sets both a curated
	// model and a neutral effort - which ProviderSupportsEffort(agy)=true now
	// invites - fails at session start.
	if effort := agyEffortLevel(cfg.Effort); effort != "" && !modelPinsEffort(model) {
		opts = append(opts, agy.WithEffort(effort))
	}

	if cfg.WorkDir != "" {
		opts = append(opts, agy.WithWorkDir(cfg.WorkDir))
	}
	if cfg.ResumeSessionID != "" {
		opts = append(opts, agy.WithConversation(cfg.ResumeSessionID))
	}
	switch strings.ToLower(strings.TrimSpace(cfg.PermissionMode)) {
	case "bypass":
		opts = append(opts, agy.WithDangerouslySkipPermissions())
	case "plan":
		opts = append(opts, agy.WithSandbox())
	}
	return opts
}

// modelPinsEffort reports whether an agy model id already encodes a reasoning
// effort level, making a separate --effort flag redundant and, when the two
// disagree, a CLI error. agy spells these as a trailing -low/-medium/-high.
func modelPinsEffort(model string) bool {
	for _, suffix := range []string{"-low", "-medium", "-high"} {
		if strings.HasSuffix(model, suffix) {
			return true
		}
	}
	return false
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
