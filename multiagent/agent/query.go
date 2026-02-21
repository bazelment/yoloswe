package agent

import (
	"context"
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
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
	default:
		return nil, fmt.Errorf("unknown provider %q for model %q", m.Provider, m.ID)
	}
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
