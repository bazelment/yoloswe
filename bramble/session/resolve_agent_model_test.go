package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

func TestResolveAgentModel_ExactMatchFromGlobalList(t *testing.T) {
	t.Parallel()

	m, err := resolveAgentModel("opus", "", nil)
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

	m, err := resolveAgentModel("opus", "", reg)
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
		{"fable-5", agent.ProviderClaude},
	}

	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			t.Parallel()
			m, err := resolveAgentModel(tc.modelID, "", nil)
			require.NoError(t, err)
			assert.Equal(t, tc.modelID, m.ID)
			assert.Equal(t, tc.provider, m.Provider)
			assert.Equal(t, tc.modelID, m.Label)
		})
	}
}

func TestResolveAgentModel_UnknownModelFails(t *testing.T) {
	t.Parallel()

	_, err := resolveAgentModel("foo-bar", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "foo-bar")
	assert.Contains(t, err.Error(), "gpt-")
}

func TestResolveAgentModel_EmptyModelFails(t *testing.T) {
	t.Parallel()

	_, err := resolveAgentModel("", "", nil)
	require.Error(t, err)
}

// Drives the SpawnOpts route, which is the one every real caller uses: bramble
// main.go fills SpawnOpts from the IPC request. An earlier version of this test
// went through per-manager defaults instead, so it would have stayed green if
// the SpawnOpts plumbing were deleted outright.
func TestManager_ExplicitBackendAllowsOpenRouterModelEndToEnd(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	// Inline rather than t.Setenv so the test stays parallel-safe; the manager
	// now refuses to launch an endpoint whose key resolves to nothing.
	endpoint.APIKey = "openrouter-test-key"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	sid, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		info, ok := mgr.GetSessionInfo(sid)
		return ok && info.Status == StatusIdle
	}, 5*time.Second, 10*time.Millisecond)

	info, ok := mgr.GetSessionInfo(sid)
	require.True(t, ok)
	assert.Equal(t, "stealth/ox-alpha", info.Model)
	assert.Equal(t, ProviderCodex, info.Backend)
	assert.NotContains(t, info.ErrorMsg, "unknown model")
}

func TestManager_CodexRejectsDeadChatWire(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.Wire = llmendpoint.WireAPIChat
	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTmux})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder,
		t.TempDir(),
		"test prompt",
		"stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer supported")
	assert.Contains(t, err.Error(), "responses")
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
		{"fable-5", agent.ProviderClaude, agent.ProviderCodex},
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

// Only the env var *name* is guaranteed to cross IPC from `bramble new-session`;
// this process is the one that reads it. Without this guard the wrappers omit
// the auth headers while still setting the base URL, and the session launches
// pointed at the endpoint with no credential — a remote 401 that names nothing.
func TestManager_UnresolvedEndpointKeyIsNamed(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKeyEnv = "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY"
	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTmux})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY",
		"the error must name the variable the operator typed")
}

// The guard must key off resolution, not off APIKeyEnv: an inline key (the
// shape `bramble new-session` now sends after resolving in the client) has no
// env var to read and must launch.
func TestManager_InlineEndpointKeySatisfiesTheGuard(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKeyEnv = "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY"
	endpoint.APIKey = "resolved-by-the-client"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.NoError(t, err)
}

// An explicit --backend means the model ID is that backend's own, so there is
// no sensible default to substitute. Defaulting anyway launched
// `codex -m sonnet` against the endpoint and surfaced a remote 400 instead of
// naming the missing flag — and left resolveAgentModel's empty-model guard
// unreachable from this path.
func TestManager_ExplicitBackendWithoutModelNamesTheMissingFlag(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	// Synchronously, so `bramble new-session` prints it on stderr rather than a
	// session ID followed by a background failure the operator has to go
	// looking for in the TUI.
	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model must not be empty")
	assert.NotContains(t, err.Error(), "sonnet", "the default must not have been substituted")
}

// ResumeSession does not go through startSessionWithID, so the credential
// guard installed there left the resume path uncovered — and a persisted
// endpoint is exactly the case that needs it: SessionToStored writes it via
// Redacted(), so the rehydrated endpoint has no APIKey and re-resolves through
// APIKeyEnv. A TUI whose environment lacks that variable would resume the
// window against the endpoint with the user's own credentials.
func TestManager_ResumeChecksEndpointCredential(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKeyEnv = "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY"
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    &silentEphemeralProvider{},
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	// Register the session directly: startSessionWithID would reject the
	// unresolvable key up front, which is the very check this path skips.
	sid := SessionID("builder-resumed")
	mgr.mu.Lock()
	mgr.sessions[sid] = &Session{
		ID:           sid,
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		WorktreePath: t.TempDir(),
		Model:        "stealth/ox-alpha",
		Backend:      ProviderCodex,
		LLMEndpoint:  endpoint,
		CLISessionID: "cli-abc123",
		Progress:     &SessionProgress{LastActivity: time.Now()},
	}
	mgr.mu.Unlock()

	require.NoError(t, mgr.ResumeSession(sid, "continue"))
	require.Eventually(t, func() bool {
		info, ok := mgr.GetSessionInfo(sid)
		return ok && info.Status == StatusFailed
	}, 5*time.Second, 10*time.Millisecond)

	info, ok := mgr.GetSessionInfo(sid)
	require.True(t, ok)
	assert.Contains(t, info.ErrorMsg, "BRAMBLE_TEST_ABSENT_OPENROUTER_KEY",
		"a resumed session must name the unresolved variable, not fail on a remote 401")
}

// endpointRecordingProvider captures the ExecuteConfig its turn was given, so a
// test can assert on what actually reached the provider rather than on what the
// manager was asked for.
type endpointRecordingProvider struct {
	endpoint llmendpoint.Endpoint
	model    string
	mu       sync.Mutex
	seen     bool
}

func (p *endpointRecordingProvider) Name() string                    { return "endpoint-recording" }
func (p *endpointRecordingProvider) Events() <-chan agent.AgentEvent { return nil }
func (p *endpointRecordingProvider) Close() error                    { return nil }

func (p *endpointRecordingProvider) Execute(_ context.Context, prompt string, _ *wt.WorktreeContext, opts ...agent.ExecuteOption) (*agent.AgentResult, error) {
	cfg := agent.ExecuteConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	p.mu.Lock()
	p.endpoint = cfg.LLMEndpoint
	p.model = cfg.Model
	p.seen = true
	p.mu.Unlock()
	return &agent.AgentResult{Text: "ok: " + prompt, Success: true}, nil
}

func (p *endpointRecordingProvider) observed() (llmendpoint.Endpoint, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endpoint, p.model, p.seen
}

// Asserting on session metadata proves only that the flags were parsed. This
// drives the whole TUI chain — session.LLMEndpoint to providerRunner to
// WithProviderLLMEndpoint to the provider's ExecuteConfig — so deleting any
// link fails here. The endpoint is attached once after the provider branch in
// runSession, which is what makes this cover every branch rather than the one
// this fake happens to take.
func TestManager_EndpointReachesTheProviderTurn(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	provider := &endpointRecordingProvider{}
	mgr := NewManagerWithConfig(ManagerConfig{
		Provider:    provider,
		SessionMode: SessionModeTUI,
	})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "stealth/ox-alpha",
		SpawnOpts{Backend: ProviderCodex, LLMEndpoint: endpoint},
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, _, seen := provider.observed()
		return seen
	}, 5*time.Second, 10*time.Millisecond, "provider turn never ran")

	gotEndpoint, gotModel, _ := provider.observed()
	assert.Equal(t, endpoint, gotEndpoint, "the session's endpoint must reach the provider turn intact")
	// The model must arrive WITH the endpoint, not merely alongside it in the
	// session record. claude.WithLLMEndpoint skips the ANTHROPIC_MODEL and
	// ANTHROPIC_DEFAULT_* side-call pins when Model is empty — the condition its
	// own doc comment says surfaces as a misleading "model may not exist" error
	// against a single-model endpoint — and codex would get no -m. Asserting
	// only the endpoint let this branch omit the model unnoticed.
	assert.Equal(t, "stealth/ox-alpha", gotModel,
		"the session's model must reach the provider turn alongside the endpoint")
}

// agent.CLIModelArg drops the model flag entirely when the id belongs to
// another provider, so without this rule `--backend codex --model opus`
// launched codex with no -m — running codex's default model while bramble
// recorded and displayed "opus". Rejecting the pair at the producer is what
// keeps that unreachable rather than papering over it at each consumer.
func TestManager_BackendAndModelMustAgree(t *testing.T) {
	t.Parallel()

	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTmux})
	t.Cleanup(mgr.Close)

	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "opus",
		SpawnOpts{Backend: ProviderCodex},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opus")
	assert.Contains(t, err.Error(), ProviderCodex)
	assert.Contains(t, err.Error(), ProviderClaude, "the error must name the backend the model does belong to")
}

// The rule must not reject the case the PR exists for: a third-party id the
// curated registry has never heard of is exactly what --backend carries.
func TestManager_UncuratedModelIsAllowedWithExplicitBackend(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateBackendModel(ProviderCodex, "stealth/ox-alpha"))
	require.NoError(t, validateBackendModel(ProviderClaude, "stealth/ox-alpha"))
	// A curated id whose provider matches is equally fine.
	require.NoError(t, validateBackendModel(ProviderClaude, "opus"))
	// And no backend means no pairing to check.
	require.NoError(t, validateBackendModel("", "opus"))
}

// Redacted() drops Headers, so a persisted header-bearing endpoint comes back
// without them and fails opaquely against a gateway that requires one. Refuse
// at creation, where the caller can still act on it.
func TestManager_HeaderBearingEndpointIsRefusedWhenPersisted(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	endpoint.Headers = map[string]string{"X-Tenant": "acme"}

	// The guard is about what survives the store, so it keys off the store.
	require.NoError(t, validatePersistableEndpoint(endpoint, false),
		"a manager with no store never rehydrates, so headers cannot vanish")
	err := validatePersistableEndpoint(endpoint, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "header")
	assert.Contains(t, err.Error(), "resume")

	// Redacted() really does drop them — the premise of the guard, asserted
	// rather than assumed, since a future change to Redacted would make this
	// rule obsolete rather than merely wrong.
	assert.Empty(t, endpoint.Redacted().Headers)
	assert.NotEmpty(t, endpoint.Headers, "Redacted must not mutate the original")
}

// Redacted() drops an inline APIKey, and unlike the env-var form there is
// nothing to re-resolve from — so an inline-only endpoint launches and then
// cannot be resumed. Same rule as the header case: what the store cannot
// reconstruct must be refused where the caller can still act on it.
func TestManager_InlineOnlyKeyIsRefusedWhenPersisted(t *testing.T) {
	t.Parallel()

	inlineOnly := llmendpoint.Endpoint{
		BaseURL: llmendpoint.OpenRouterBaseURL,
		APIKey:  "inline-only-key",
		Wire:    llmendpoint.WireAPIResponses,
	}
	require.NoError(t, inlineOnly.Validate(), "the endpoint is otherwise valid; only persistence rejects it")
	require.NoError(t, validatePersistableEndpoint(inlineOnly, false),
		"a manager with no store never rehydrates, so an inline key cannot go missing")

	err := validatePersistableEndpoint(inlineOnly, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APIKeyEnv")

	// The recoverable shape — which is what `bramble new-session` sends after
	// resolving the key client-side — must still pass.
	recoverable := inlineOnly
	recoverable.APIKeyEnv = llmendpoint.OpenRouterAPIKeyEnv
	require.NoError(t, validatePersistableEndpoint(recoverable, true))

	// The premise, asserted rather than assumed.
	assert.Empty(t, inlineOnly.Redacted().APIKey)
}

// The TUI wrapper configs are the only place a session's endpoint reaches the
// planner, builder and codetalk wrappers, and until they were split out of
// runSession's switch nothing failed when one of them lost its LLMEndpoint
// line: a session with a dropped endpoint runs against the default provider
// with the user's own credentials, which is indistinguishable from a session
// that never had one. Table-driven over all three so a fourth wrapper arm is
// one row, not a fourth forgotten literal.
func TestManager_TUIWrapperConfigsCarrySessionEndpoint(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	session := &Session{
		ID:           "wrapper-cfg",
		Model:        "stealth/ox-alpha",
		Backend:      ProviderClaude,
		LLMEndpoint:  endpoint,
		WorktreePath: t.TempDir(),
		CLISessionID: "cli-resume-id",
	}
	mgr := NewManagerWithConfig(ManagerConfig{RecordingDir: t.TempDir()})
	t.Cleanup(mgr.Close)

	// Each row reduces one wrapper's config to the four values that must
	// travel with the session, so the assertions do not depend on config
	// types that differ between the three wrappers.
	type carried struct {
		endpoint                               llmendpoint.Endpoint
		model, workDir, resumeID, recordingDir string
	}
	plannerCfg := mgr.plannerConfigFor(session, nil)
	builderCfg := mgr.builderConfigFor(session)
	codeTalkCfg := mgr.codeTalkConfigFor(session)
	for _, tc := range []struct {
		got  carried
		name string
	}{
		{carried{plannerCfg.LLMEndpoint, plannerCfg.Model, plannerCfg.WorkDir, plannerCfg.ResumeSessionID, plannerCfg.RecordingDir}, "planner"},
		{carried{builderCfg.LLMEndpoint, builderCfg.Model, builderCfg.WorkDir, builderCfg.ResumeSessionID, builderCfg.RecordingDir}, "builder"},
		{carried{codeTalkCfg.LLMEndpoint, codeTalkCfg.Model, codeTalkCfg.WorkDir, codeTalkCfg.ResumeSessionID, codeTalkCfg.RecordingDir}, "codetalk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, endpoint, tc.got.endpoint,
				"the session's endpoint must reach the %s wrapper config", tc.name)
			// The model must travel WITH the endpoint: claude.WithLLMEndpoint
			// skips the ANTHROPIC_MODEL and ANTHROPIC_DEFAULT_* side-call pins
			// when Model is empty, which surfaces as a misleading "model may
			// not exist" against a single-model endpoint.
			assert.Equal(t, "stealth/ox-alpha", tc.got.model,
				"the session's model must reach the %s wrapper config", tc.name)
			assert.Equal(t, session.WorktreePath, tc.got.workDir)
			assert.Equal(t, "cli-resume-id", tc.got.resumeID)
			assert.Equal(t, mgr.config.RecordingDir, tc.got.recordingDir)
		})
	}
}

// A zero endpoint is the same as no endpoint everywhere downstream — every
// validateEndpoint* guard short-circuits on IsZero — so a restore that drops
// it fails silently against the default provider. storedToSession is the one
// place both restore paths (ReconcileTmuxSessions re-adopting a tmux window,
// rehydrateSession loading a terminal session for resume) rebuild a Session,
// so pinning it pins both.
func TestStoredToSession_CarriesBackendAndEndpoint(t *testing.T) {
	t.Parallel()

	// Shaped like what LoadSession returns: no literal key (persistSession
	// stores Redacted(), asserted on the serialized JSON in store_test.go),
	// the env reference kept, and a header map. Redacted() drops Headers, so
	// today's writer never emits one — but storedToSession's input is a JSON
	// file on disk, not that writer's return value, and its job is to not hand
	// the caller a map it shares with the record.
	storedEndpoint := llmendpoint.OpenRouter()
	storedEndpoint.APIKeyEnv = "OPENROUTER_API_KEY"
	storedEndpoint.Headers = map[string]string{"HTTP-Referer": "https://example.test"}
	require.Empty(t, storedEndpoint.APIKey, "a persisted endpoint never holds a literal key")

	stored := &StoredSession{
		ID:           "restored",
		Type:         SessionTypeBuilder,
		Status:       StatusCompleted,
		RepoName:     "test-repo",
		WorktreePath: "/path/wt",
		WorktreeName: "feature",
		Prompt:       "do the thing",
		Model:        "stealth/ox-alpha",
		Backend:      ProviderCodex,
		CLISessionID: "cli-abc123",
		LLMEndpoint:  &storedEndpoint,
	}

	session := storedToSession(stored)
	require.NotNil(t, session)
	assert.Equal(t, ProviderCodex, session.Backend)
	assert.Equal(t, "stealth/ox-alpha", session.Model)
	assert.Equal(t, storedEndpoint, session.LLMEndpoint,
		"a restored session must carry the persisted endpoint, credential reference included")
	assert.Equal(t, "OPENROUTER_API_KEY", session.LLMEndpoint.APIKeyEnv,
		"without the env reference the restored session has no way to obtain a key")

	// Cloned, not merely copied: the stored record outlives the restore and is
	// handed to other callers, so a mutation through the session must not reach
	// it. Headers is the only field that distinguishes the two — Endpoint is a
	// value type, so a plain `*stored.LLMEndpoint` already isolates every
	// scalar. An earlier version of this assertion mutated APIKeyEnv and so
	// passed with or without the Clone, which is worse than no assertion.
	require.NotNil(t, session.LLMEndpoint.Headers)
	session.LLMEndpoint.Headers["HTTP-Referer"] = "https://mutated.test"
	assert.Equal(t, "https://example.test", stored.LLMEndpoint.Headers["HTTP-Referer"],
		"storedToSession must clone the endpoint rather than share its header map")
}

// A nil stored endpoint is the common case (every session without one), and
// must produce a zero endpoint rather than panicking on the Clone.
func TestStoredToSession_NoEndpoint(t *testing.T) {
	t.Parallel()

	session := storedToSession(&StoredSession{ID: "plain", Type: SessionTypeBuilder})
	require.NotNil(t, session)
	assert.True(t, session.LLMEndpoint.IsZero())
}

// An endpoint on a delegator session was the one arm of runSession's wrapper
// switch that carried nothing: delegatorRunner builds its claude.Session from
// DelegatorBaseSessionOpts, which has no endpoint seam. Refused rather than
// dropped, so adding support is a deliberate act instead of the repair of a
// silent fallback.
func TestManager_DelegatorRejectsEndpoint(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	t.Cleanup(mgr.Close)

	sid, err := mgr.StartSessionWithOpts(
		SessionTypeDelegator, t.TempDir(), "test prompt", "opus",
		SpawnOpts{Backend: ProviderClaude, LLMEndpoint: endpoint},
	)
	require.NoError(t, err, "the endpoint is refused in runSession, not at spawn")
	require.Eventually(t, func() bool {
		info, ok := mgr.GetSessionInfo(sid)
		return ok && info.Status == StatusFailed
	}, 5*time.Second, 10*time.Millisecond, "delegator session never failed")

	info, ok := mgr.GetSessionInfo(sid)
	require.True(t, ok)
	assert.Contains(t, info.ErrorMsg, "per-session LLM endpoint",
		"the refusal must name the endpoint, not fail later against the default provider")
}

// startSessionWithID validated the endpoint against opts.Backend, which may be
// empty — so an endpoint on a model belonging to an unsupported provider
// passed every synchronous check, printed a session ID, and only failed in the
// background. That is the split the duplicated checks exist to close.
func TestStartSession_EndpointRejectedForInferredProvider(t *testing.T) {
	t.Parallel()

	endpoint := llmendpoint.OpenRouter()
	endpoint.APIKey = "openrouter-test-key"
	mgr := NewManagerWithConfig(ManagerConfig{SessionMode: SessionModeTUI})
	t.Cleanup(mgr.Close)

	// No Backend: the provider comes from the model id, which routes to agy.
	_, err := mgr.StartSessionWithOpts(
		SessionTypeBuilder, t.TempDir(), "test prompt", "gemini-3.8-flash-low",
		SpawnOpts{LLMEndpoint: endpoint},
	)
	require.Error(t, err, "an endpoint on a gemini-routed model must be refused at the call, not in the background")
	assert.Contains(t, err.Error(), ProviderAgy,
		"the error must name the provider the model resolved to")
}
