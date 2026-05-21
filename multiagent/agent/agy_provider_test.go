package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

func TestAgyProvider_Name(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	assert.Equal(t, ProviderAgy, p.Name())
}

func TestAgyProvider_EventsChannel(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	ch := p.Events()
	require.NotNil(t, ch)
	assert.Equal(t, (<-chan AgentEvent)(p.events), ch)
}

func TestAgyProvider_RejectsExplicitEffort(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(EffortHigh))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEffortUnsupported)
	assert.Contains(t, err.Error(), ProviderAgy)
}

func TestAgyProvider_RejectsLLMEndpoint(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	_, err := p.Execute(context.Background(), "ignored", nil, WithProviderLLMEndpoint(llmendpoint.Endpoint{
		BaseURL:   "https://example.com/v1",
		APIKeyEnv: "EXAMPLE_API_KEY",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LLMEndpoint is not supported")
	assert.Contains(t, err.Error(), ProviderAgy)
}
