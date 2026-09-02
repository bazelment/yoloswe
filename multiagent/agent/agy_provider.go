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

	fullPrompt := prompt
	if wtCtx != nil {
		fullPrompt = wtCtx.FormatForPrompt() + "\n\n" + prompt
	}

	sessionOpts, err := p.sessionOptsFor(cfg)
	if err != nil {
		return nil, err
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

// sessionOptsFor assembles the full option list Execute hands to agy: the
// provider's constructor options first, then the ExecuteConfig's, then the
// reconciliation pass.
//
// The order is load-bearing. reconcileAgyEffort must run LAST so it sees the
// MERGED config: the constructor's options can pin a model too, and a guard
// that inspected only cfg would miss it - the conformance test builds the
// provider with agy.WithModel(...) and an empty cfg.Model exactly that way.
func (p *AgyProvider) sessionOptsFor(cfg ExecuteConfig) ([]agy.SessionOption, error) {
	opts := append([]agy.SessionOption{}, p.sessionOpts...)
	opts = append(opts, agySessionOpts(cfg)...)

	// Reconcile against the assembled config, then re-apply the result: the
	// conflict is a property of the final command line, not of any one option.
	var merged agy.SessionConfig
	for _, opt := range opts {
		opt(&merged)
	}
	if err := reconcileAgyEffort(&merged); err != nil {
		return nil, err
	}
	return append(opts, func(c *agy.SessionConfig) {
		c.Model = merged.Model
		c.Effort = merged.Effort
	}), nil
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

	// Effort is reconciled against the merged config by reconcileAgyEffort,
	// which Execute appends last; record the request verbatim here.
	if effort := agyEffortLevel(cfg.Effort); effort != "" {
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

// agyEffortSuffixes are the reasoning levels agy encodes in a model id, longest
// first so splitModelEffort matches "-medium" before any shorter overlap.
var agyEffortSuffixes = []string{"-medium", "-high", "-low"}

// splitModelEffort separates an agy model id from the reasoning level it pins.
// agy's catalog spells the level as a trailing -low/-medium/-high
// (gemini-3.8-flash-low, gemini-3.1-pro-high, ...); base reports the id without
// it and pinned reports the level, empty when the id leaves the level open.
func splitModelEffort(model string) (base, pinned string) {
	for _, suffix := range agyEffortSuffixes {
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), strings.TrimPrefix(suffix, "-")
		}
	}
	return model, ""
}

// reconcileAgyEffort settles the two ways an agy invocation can carry a
// reasoning level, and must be applied LAST so it sees the merged config.
//
// agy's catalog encodes the level in the model id *and* offers a separate
// --effort flag. Passing both is a hard error from the CLI:
//
//	Error: invalid model selection (--model "gemini-3.1-pro-high"
//	--effort "low"): --model gemini-3.1-pro-high conflicts with --effort=low
//
// Dropping the requested level instead would be worse than the crash: callers
// consult ProviderSupportsEffort(ProviderAgy), which is now true, and act on a
// contract of honor-or-surface-an-error, never silent downgrade (see the
// --thinking-level handling in jiradozer/agent.go). So the request is honored
// by RETARGETING the model to the variant that encodes it - the level the
// caller asked for is the one that runs - and --effort is then dropped as
// redundant, leaving exactly one representation on the command line.
//
// Retargeting is only safe onto a variant that exists: agy's catalog is not a
// full cross product (gemini-3.1-pro ships -low and -high but no -medium, and
// `--model gemini-3.1-pro-medium` is rejected as "not recognized"). AllModels
// is this repo's record of which combinations are real. When it has no such
// variant the request CANNOT be honored, so this returns ErrEffortUnsupported
// rather than running at the model's own level - that would be exactly the
// silent downgrade the contract above forbids, and jiradozer/agent.go already
// handles this error.
//
// Uncurated ids are left alone. CLIModelArg passes them through precisely
// because "there the CLI is the authority, not this list" (model_registry.go),
// so AllModels cannot say whether a variant exists and this layer must not
// invent an answer in either direction: agy itself will accept or reject.
func reconcileAgyEffort(c *agy.SessionConfig) error {
	if c.Effort == "" || c.Model == "" {
		return nil
	}
	base, pinned := splitModelEffort(c.Model)
	if pinned == "" {
		// Nothing pinned by the model: --effort alone carries the level.
		return nil
	}
	if pinned == c.Effort {
		c.Effort = "" // Same level twice; keep one representation.
		return nil
	}
	if !isCuratedAgyModel(c.Model) {
		// Not ours to adjudicate - hand both to agy unchanged.
		return nil
	}
	retarget := base + "-" + c.Effort
	if !isCuratedAgyModel(retarget) {
		return fmt.Errorf("%w: agy has no %q variant of %q (requested effort %s on a model pinned to %s)",
			ErrEffortUnsupported, c.Effort, base, c.Effort, pinned)
	}
	c.Model = retarget
	c.Effort = ""
	return nil
}

// isCuratedAgyModel reports whether an id is a known agy model, i.e. one this
// repo has recorded in AllModels as runnable by the agy CLI.
func isCuratedAgyModel(id string) bool {
	m, ok := ModelByID(id)
	return ok && m.Provider == ProviderAgy && !m.Placeholder
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
