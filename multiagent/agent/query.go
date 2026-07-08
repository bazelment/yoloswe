package agent

import (
	"context"
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// QueryResult is the provider-agnostic result of a one-shot query.
type QueryResult struct {
	Text  string
	Usage AgentUsage
}

// NewProviderForModel creates the appropriate Provider for the given AgentModel.
func NewProviderForModel(m AgentModel) (Provider, error) {
	switch m.Provider {
	case ProviderClaude:
		return NewClaudeProvider(), nil
	case ProviderGemini:
		return NewGeminiProvider(acp.WithBinaryArgs("--experimental-acp", "--model", m.ID)), nil
	case ProviderCodex:
		return NewCodexProvider(), nil
	case ProviderCursor:
		return NewCursorProvider(), nil
	case ProviderAgy:
		return NewAgyProvider(), nil
	default:
		return nil, fmt.Errorf("unknown provider %q for model %q", m.Provider, m.ID)
	}
}

// ClaudeSessionUtilization returns the maximum active plan-limit utilization
// (0–100) for the Claude account backing model. It owns claude.Session
// construction so callers (e.g. jiradozer) don't import the claude package
// directly. ok is false for non-Claude providers and whenever usage can't be
// read — the caller MUST fail open on !ok (never block a run on a best-effort
// pre-flight). No CLI subprocess is started: Usage reads stored OAuth
// credentials from disk and performs a single HTTP GET, so an unstarted session
// is sufficient.
func ClaudeSessionUtilization(ctx context.Context, model AgentModel, opts ...claude.SessionOption) (pct float64, ok bool) {
	if model.Provider != ProviderClaude {
		return 0, false
	}
	session := claude.NewSession(opts...)
	usage, err := session.Usage(ctx)
	if err != nil || usage == nil {
		return 0, false
	}
	// Model-aware: a weekly-scoped cap on a different model (e.g. Fable at 100%)
	// must not gate this model. Account-wide windows still count.
	return usage.MaxActiveUtilizationForModel(model.ID, model.Label)
}

// Query sends a one-shot prompt using the provider determined by modelID
// and returns the result. The modelID must match a registered model in AllModels.
func Query(ctx context.Context, modelID, prompt string) (*QueryResult, error) {
	m, ok := ModelByID(modelID)
	if !ok {
		return nil, fmt.Errorf("unknown model %q", modelID)
	}

	provider, err := NewProviderForModel(m)
	if err != nil {
		return nil, err
	}
	defer provider.Close()

	result, err := provider.Execute(ctx, prompt, nil, WithProviderModel(m.ID))
	if err != nil {
		return nil, err
	}

	if !result.Success {
		errMsg := "query failed"
		if result.Error != nil {
			errMsg = result.Error.Error()
		} else if result.Text != "" {
			errMsg = result.Text
		}
		return nil, fmt.Errorf("%s provider: %s", m.Provider, errMsg)
	}

	return &QueryResult{
		Text:  result.Text,
		Usage: result.Usage,
	}, nil
}
