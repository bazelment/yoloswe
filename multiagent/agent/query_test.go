package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewProviderForModel_Claude(t *testing.T) {
	m := AgentModel{ID: "sonnet", Provider: ProviderClaude}
	p, err := NewProviderForModel(m)
	require.NoError(t, err)
	defer p.Close()
	assert.Equal(t, "claude", p.Name())
}

func TestNewProviderForModel_GeminiFamilyUsesAgy(t *testing.T) {
	m := AgentModel{ID: "gemini-3.1-flash-lite-preview", Provider: ProviderAgy}
	p, err := NewProviderForModel(m)
	require.NoError(t, err)
	defer p.Close()
	assert.Equal(t, "agy", p.Name())
}

func TestNewProviderForModel_GeminiProviderRetired(t *testing.T) {
	m := AgentModel{ID: "gemini-2.5-flash", Provider: ProviderGemini}
	_, err := NewProviderForModel(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gemini-cli is retired")
}

func TestNewProviderForModel_Codex(t *testing.T) {
	m := AgentModel{ID: "gpt-5.3-codex", Provider: ProviderCodex}
	p, err := NewProviderForModel(m)
	require.NoError(t, err)
	defer p.Close()
	assert.Equal(t, "codex", p.Name())
}

func TestNewProviderForModel_Unknown(t *testing.T) {
	m := AgentModel{ID: "foo", Provider: "unknown"}
	_, err := NewProviderForModel(m)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown provider")
}

func TestQuery_UnknownModel(t *testing.T) {
	_, err := Query(t.Context(), "nonexistent-model", "hello")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown model")
}
