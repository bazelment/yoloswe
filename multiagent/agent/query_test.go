package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// Build guard: fail the build if the usage-related session option signatures
// drift (per CLAUDE.md convention for wrapper option dependencies).
var (
	_ claude.SessionOption = claude.WithUsageBaseURL("")
	_ claude.SessionOption = claude.WithOAuthToken("")
)

func TestNewProviderForModel_Claude(t *testing.T) {
	m := AgentModel{ID: "sonnet", Provider: ProviderClaude}
	p, err := NewProviderForModel(m)
	require.NoError(t, err)
	defer p.Close()
	assert.Equal(t, "claude", p.Name())
}

func TestNewProviderForModel_Gemini(t *testing.T) {
	m := AgentModel{ID: "gemini-2.5-flash", Provider: ProviderGemini}
	p, err := NewProviderForModel(m)
	require.NoError(t, err)
	defer p.Close()
	assert.Equal(t, "gemini", p.Name())
}

func TestNewProviderForModel_Codex(t *testing.T) {
	m := AgentModel{ID: "gpt-5.5", Provider: ProviderCodex}
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

func TestClaudeSessionUtilization_NonClaudeIsNotOK(t *testing.T) {
	t.Parallel()
	m := AgentModel{ID: "composer-2.5", Provider: ProviderCursor}
	_, ok := ClaudeSessionUtilization(t.Context(), m)
	assert.False(t, ok, "non-Claude provider must never report utilization")
}

func TestClaudeSessionUtilization_ReadsMaxActiveFromEndpoint(t *testing.T) {
	t.Parallel()
	// A 99% weekly bucket while the 5-hour window sits at 5%: the pre-flight
	// must surface 99, catching a weekly exhaustion the session window misses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 5},
			"limits": [
				{"kind": "session", "percent": 5, "is_active": true},
				{"kind": "weekly_all", "percent": 99, "is_active": true}
			]
		}`))
	}))
	defer srv.Close()

	m := AgentModel{ID: "sonnet", Provider: ProviderClaude}
	pct, ok := ClaudeSessionUtilization(t.Context(), m,
		claude.WithOAuthToken("test-token"),
		claude.WithUsageBaseURL(srv.URL))
	require.True(t, ok)
	assert.Equal(t, 99.0, pct)
}

func TestClaudeSessionUtilization_FailsOpenOnServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := AgentModel{ID: "sonnet", Provider: ProviderClaude}
	_, ok := ClaudeSessionUtilization(t.Context(), m,
		claude.WithOAuthToken("test-token"),
		claude.WithUsageBaseURL(srv.URL))
	assert.False(t, ok, "server error must fail open (ok=false), never block")
}
