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
		agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: false},
		agent.ProviderAgy:    {Provider: agent.ProviderAgy, Installed: false},
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
		{"gemini-99-ultra", agent.ProviderAgy},
		{"cursor-fast", agent.ProviderCursor},
		{"composer-3", agent.ProviderCursor},
		{"agy-pro", agent.ProviderAgy},
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

// TestManager_PrefixModelRoutesToCorrectProvider verifies that a model ID
// resolved only by prefix rule selects the right provider in runSession — not
// the Claude default. The assertion strategy: mark the expected provider as
// not installed so the session fails with "provider X is not available"; if
// routing fell through to Claude, the error would name "claude" instead.
func TestManager_PrefixModelRoutesToCorrectProvider(t *testing.T) {
	t.Parallel()

	cases := []struct {
		modelID         string
		unavailProvider string
		availProvider   string // must be installed so the registry accepts the session
	}{
		{"gpt-future-9000", agent.ProviderCodex, agent.ProviderClaude},
		{"gemini-99-ultra", agent.ProviderAgy, agent.ProviderClaude},
		{"cursor-fast-99", agent.ProviderCursor, agent.ProviderClaude},
		{"composer-v9", agent.ProviderCursor, agent.ProviderClaude},
		{"agy-pro", agent.ProviderAgy, agent.ProviderClaude},
	}

	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			t.Parallel()

			statusMap := map[string]agent.ProviderStatus{
				agent.ProviderClaude: {Provider: agent.ProviderClaude, Installed: true},
				agent.ProviderCodex:  {Provider: agent.ProviderCodex, Installed: true},
				agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: true},
				agent.ProviderAgy:    {Provider: agent.ProviderAgy, Installed: true},
			}
			// Mark the target provider as not installed so runSession rejects it
			// with a message naming that provider — proving routing chose it.
			statusMap[tc.unavailProvider] = agent.ProviderStatus{Provider: tc.unavailProvider, Installed: false}
			avail := agent.NewProviderAvailabilityFromMap(statusMap)
			reg := agent.NewModelRegistry(avail, nil)

			mgr := NewManagerWithConfig(ManagerConfig{
				ModelRegistry: reg,
				SessionMode:   SessionModeTUI,
			})
			t.Cleanup(mgr.Close)

			sid, err := mgr.StartSession(SessionTypeBuilder, t.TempDir(), "test prompt", tc.modelID)
			require.NoError(t, err)

			require.Eventually(t, func() bool {
				info, ok := mgr.GetSessionInfo(sid)
				return ok && info.Status == StatusFailed && info.ErrorMsg != ""
			}, 5*time.Second, 10*time.Millisecond)

			info, ok := mgr.GetSessionInfo(sid)
			require.True(t, ok)
			require.NotEmpty(t, info.ErrorMsg)
			assert.Contains(t, info.ErrorMsg, tc.unavailProvider,
				"error should name the resolved provider, not the Claude fallback")
		})
	}
}

// TestManager_UnknownModelLandsInStatusFailed verifies that the full manager
// path fails clearly when a session is started with an unrecognized model ID
// that has no curated entry and no recognized prefix.
func TestManager_UnknownModelLandsInStatusFailed(t *testing.T) {
	t.Parallel()

	avail := agent.NewProviderAvailabilityFromMap(map[string]agent.ProviderStatus{
		agent.ProviderClaude: {Provider: agent.ProviderClaude, Installed: true},
		agent.ProviderCodex:  {Provider: agent.ProviderCodex, Installed: true},
		agent.ProviderCursor: {Provider: agent.ProviderCursor, Installed: true},
		agent.ProviderAgy:    {Provider: agent.ProviderAgy, Installed: true},
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
		info, ok := mgr.GetSessionInfo(sid)
		return ok && info.Status == StatusFailed && info.ErrorMsg != ""
	}, 5*time.Second, 10*time.Millisecond)

	info, ok := mgr.GetSessionInfo(sid)
	require.True(t, ok)
	require.NotEmpty(t, info.ErrorMsg)
	assert.Contains(t, info.ErrorMsg, "foo-bar")
	assert.Contains(t, info.ErrorMsg, "gpt-")

	lines := mgr.GetSessionOutput(sid)
	var hasError bool
	for _, l := range lines {
		if l.Type == OutputTypeError && l.Content != "" {
			hasError = true
		}
	}
	assert.True(t, hasError, "expected at least one OutputTypeError line")
}
