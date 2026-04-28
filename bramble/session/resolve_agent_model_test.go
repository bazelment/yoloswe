package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestResolveAgentModel_ExactMatchFromGlobalList(t *testing.T) {
	t.Parallel()

	m, err := resolveAgentModel("opus", nil)
	require.NoError(t, err)
	assert.Equal(t, "opus", m.ID)
	assert.Equal(t, agent.ProviderClaude, m.Provider)
}

func TestResolveAgentModel_ExactMatchFromRegistry(t *testing.T) {
	t.Parallel()

	avail := agent.NewProviderAvailabilityFromMap(map[string]agent.ProviderStatus{
		agent.ProviderClaude: {Provider: agent.ProviderClaude, Installed: true},
		agent.ProviderCodex:  {Provider: agent.ProviderCodex, Installed: false},
		agent.ProviderGemini: {Provider: agent.ProviderGemini, Installed: false},
		agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: false},
	})
	reg := agent.NewModelRegistry(avail, nil)

	m, err := resolveAgentModel("opus", reg)
	require.NoError(t, err)
	assert.Equal(t, "opus", m.ID)
	assert.Equal(t, agent.ProviderClaude, m.Provider)
}

func TestResolveAgentModel_PrefixFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		modelID  string
		provider string
	}{
		{"gpt-future-9000", agent.ProviderCodex},
		{"gemini-99-ultra", agent.ProviderGemini},
		{"cursor-fast", agent.ProviderCursor},
		{"composer-3", agent.ProviderCursor},
		{"claude-opus-5", agent.ProviderClaude},
	}

	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			t.Parallel()
			m, err := resolveAgentModel(tc.modelID, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.modelID, m.ID)
			assert.Equal(t, tc.provider, m.Provider)
			assert.Equal(t, tc.modelID, m.Label)
		})
	}
}

func TestResolveAgentModel_UnknownModelFails(t *testing.T) {
	t.Parallel()

	_, err := resolveAgentModel("foo-bar", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "foo-bar")
	assert.Contains(t, err.Error(), "gpt-")
}

func TestResolveAgentModel_EmptyModelFails(t *testing.T) {
	t.Parallel()

	_, err := resolveAgentModel("", nil)
	require.Error(t, err)
}

// TestManager_UnknownModelLandsInStatusFailed verifies that the full manager
// path fails clearly when a session is started with an unrecognized model ID
// that has no curated entry and no recognized prefix.
func TestManager_UnknownModelLandsInStatusFailed(t *testing.T) {
	t.Parallel()

	avail := agent.NewProviderAvailabilityFromMap(map[string]agent.ProviderStatus{
		agent.ProviderClaude: {Provider: agent.ProviderClaude, Installed: true},
		agent.ProviderCodex:  {Provider: agent.ProviderCodex, Installed: true},
		agent.ProviderGemini: {Provider: agent.ProviderGemini, Installed: true},
		agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: true},
	})
	reg := agent.NewModelRegistry(avail, nil)

	mgr := NewManagerWithConfig(ManagerConfig{
		ModelRegistry: reg,
		SessionMode:   SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	sid, err := mgr.StartSession(SessionTypeBuilder, t.TempDir(), "test prompt", "foo-bar")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		s, ok := mgr.GetSession(sid)
		return ok && s.Status == StatusFailed
	}, 5*time.Second, 10*time.Millisecond)

	s, ok := mgr.GetSession(sid)
	require.True(t, ok)
	require.NotNil(t, s.Error)
	assert.Contains(t, s.Error.Error(), "foo-bar")
	assert.Contains(t, s.Error.Error(), "gpt-")

	lines := mgr.GetSessionOutput(sid)
	var hasError bool
	for _, l := range lines {
		if l.Type == OutputTypeError && l.Content != "" {
			hasError = true
		}
	}
	assert.True(t, hasError, "expected at least one OutputTypeError line")
}
