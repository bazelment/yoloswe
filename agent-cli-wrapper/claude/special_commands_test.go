package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

func TestContextUsageSendsControlRequestAndDecodes(t *testing.T) {
	t.Parallel()
	s, buf := newStartedControlTestSession(t)

	resultCh := make(chan struct {
		usage *ContextUsage
		err   error
	}, 1)
	go func() {
		usage, err := s.ContextUsage(context.Background())
		resultCh <- struct {
			usage *ContextUsage
			err   error
		}{usage: usage, err: err}
	}()

	requestID := waitForPendingControlRequest(t, s, "")
	requireWrittenControlSubtype(t, buf, "get_context_usage")
	sendControlSuccess(t, s, requestID, map[string]any{
		"categories": []map[string]any{
			{"name": "Messages", "tokens": 123, "color": "blue"},
		},
		"totalTokens":  123,
		"rawMaxTokens": 200000,
		"apiUsage": map[string]any{
			"input_tokens":                10,
			"output_tokens":               2,
			"cache_creation_input_tokens": 3,
			"cache_read_input_tokens":     4,
		},
	})

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.Equal(t, 123, got.usage.TotalTokens)
		require.Equal(t, 200000, got.usage.EffectiveMaxTokens())
		require.Len(t, got.usage.Categories, 1)
		require.Equal(t, 10, got.usage.APIUsage.InputTokens)
		require.Contains(t, string(got.usage.Raw), `"categories"`)
	case <-time.After(2 * time.Second):
		t.Fatal("ContextUsage did not return")
	}
}

func TestSetEffortUpdatesEnvAndReadsBack(t *testing.T) {
	t.Parallel()
	s, buf := newStartedControlTestSession(t)

	resultCh := make(chan struct {
		settings *EffortSettings
		err      error
	}, 1)
	go func() {
		settings, err := s.SetEffort(context.Background(), EffortLow)
		resultCh <- struct {
			settings *EffortSettings
			err      error
		}{settings: settings, err: err}
	}()

	getID := waitForPendingControlRequest(t, s, "")
	envMsg := requireWrittenMessageType(t, buf, "update_environment_variables")
	require.Equal(t, "low", envMsg["variables"].(map[string]any)["CLAUDE_CODE_EFFORT_LEVEL"])
	requireWrittenControlSubtype(t, buf, "get_settings")
	sendControlSuccess(t, s, getID, map[string]any{
		"effective": map[string]any{},
		"sources":   []any{},
		"applied": map[string]any{
			"model":  "claude-sonnet-4-6",
			"effort": "low",
		},
	})

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.Equal(t, EffortLow, got.settings.Effort)
		require.False(t, got.settings.Auto)
		require.Equal(t, "claude-sonnet-4-6", got.settings.Model)
	case <-time.After(2 * time.Second):
		t.Fatal("SetEffort did not return")
	}
}

func TestClearEffortUpdatesEnvToAuto(t *testing.T) {
	t.Parallel()
	s, buf := newStartedControlTestSession(t)

	resultCh := make(chan error, 1)
	go func() {
		_, err := s.ClearEffort(context.Background())
		resultCh <- err
	}()

	getID := waitForPendingControlRequest(t, s, "")
	envMsg := requireWrittenMessageType(t, buf, "update_environment_variables")
	require.Equal(t, "auto", envMsg["variables"].(map[string]any)["CLAUDE_CODE_EFFORT_LEVEL"])
	sendControlSuccess(t, s, getID, map[string]any{
		"effective": map[string]any{},
		"sources":   []any{},
		"applied": map[string]any{
			"model":  "haiku",
			"effort": nil,
		},
	})

	select {
	case err := <-resultCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("ClearEffort did not return")
	}
}

func TestSetEffortRejectsInvalidLevel(t *testing.T) {
	t.Parallel()
	s, _ := newStartedControlTestSession(t)

	_, err := s.SetEffort(context.Background(), EffortLevel("fast"))
	require.ErrorIs(t, err, ErrInvalidEffort)
}

func TestUsageFetchesPlanUsageWithExplicitToken(t *testing.T) {
	t.Parallel()
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/oauth/usage", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		require.Equal(t, oauthBetaHeader, r.Header.Get("anthropic-beta"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":42.5,"resets_at":"2026-04-25T01:00:00Z"},"extra_usage":{"is_enabled":true,"monthly_limit":1000.0,"used_credits":250.5,"utilization":25}}`))
	}))
	defer server.Close()

	s := NewSession(WithOAuthToken("tok_test"), WithUsageBaseURL(server.URL))
	usage, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer tok_test", gotAuth)
	require.NotNil(t, usage.FiveHour)
	require.InDelta(t, 42.5, *usage.FiveHour.Utilization, 0.001)
	require.True(t, usage.ExtraUsage.IsEnabled)
	require.InDelta(t, 250.5, *usage.ExtraUsage.UsedCredits, 0.001)
	require.True(t, usage.HasData())
	require.Contains(t, usage.Report(), "Current session: 42% used")
}

func TestUsageReadsStoredOAuthCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	expiresAt := time.Now().Add(time.Hour).UnixMilli()
	credentials := `{"claudeAiOauth":{"accessToken":"stored_tok","expiresAt":` + jsonNumber(expiresAt) + `,"subscriptionType":"max","rateLimitTier":"default_claude_max_20x","scopes":["user:profile","user:inference"]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(credentials), 0o600))

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"seven_day":{"utilization":12,"resets_at":null}}`))
	}))
	defer server.Close()

	s := NewSession(WithUsageBaseURL(server.URL))
	usage, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer stored_tok", gotAuth)
	require.Equal(t, "max", usage.SubscriptionType)
	require.Equal(t, "default_claude_max_20x", usage.RateLimitTier)
	require.NotNil(t, usage.SevenDay)
}

func TestPlanUsageReportMatchesClaudeUsageSections(t *testing.T) {
	t.Parallel()
	usage := PlanUsage{
		SubscriptionType: "max",
		FiveHour:         &UsageRateLimit{Utilization: float64Ptr(42.9)},
		SevenDay:         &UsageRateLimit{Utilization: float64Ptr(12)},
		SevenDaySonnet:   &UsageRateLimit{Utilization: float64Ptr(8)},
		SevenDayOpus:     &UsageRateLimit{Utilization: float64Ptr(99)},
		ExtraUsage: &ExtraUsage{
			IsEnabled:    true,
			MonthlyLimit: float64Ptr(1000),
			UsedCredits:  float64Ptr(250),
			Utilization:  float64Ptr(25),
		},
	}

	lines := usage.ReportLines()
	require.Len(t, lines, 4)
	require.Equal(t, "Current session", lines[0].Title)
	require.Equal(t, "Current week (all models)", lines[1].Title)
	require.Equal(t, "Current week (Sonnet only)", lines[2].Title)
	require.Equal(t, "Extra usage", lines[3].Title)

	report := usage.Report()
	require.Contains(t, report, "Current session: 42% used")
	require.Contains(t, report, "Current week (all models): 12% used")
	require.Contains(t, report, "Current week (Sonnet only): 8% used")
	require.Contains(t, report, "Extra usage: 25% used - $2.50 / $10.00 spent")
	require.NotContains(t, report, "Opus")
}

func TestPlanUsageReportEmpty(t *testing.T) {
	t.Parallel()
	require.Equal(t, "/usage is only available for subscription plans.", PlanUsage{}.Report())
}

func TestUsageReturnsEmptyWithoutProfileScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	expiresAt := time.Now().Add(time.Hour).UnixMilli()
	credentials := `{"claudeAiOauth":{"accessToken":"stored_tok","expiresAt":` + jsonNumber(expiresAt) + `,"scopes":["user:inference"]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(credentials), 0o600))

	s := NewSession()
	usage, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.False(t, usage.HasData())
	require.Empty(t, usage.Raw)
}

func TestUsageReturnsEmptyWithoutStoredOAuthCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	s := NewSession()
	usage, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.False(t, usage.HasData())
	require.Empty(t, usage.Raw)
}

func TestUsageReturnsEmptyForInferenceOnlyEnvToken(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "inference_only")

	s := NewSession()
	usage, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.False(t, usage.HasData())
	require.Empty(t, usage.Raw)
}

func TestUsageUnavailableOnRejectedToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	s := NewSession(WithOAuthToken("bad"), WithUsageBaseURL(server.URL))
	_, err := s.Usage(context.Background())
	require.ErrorIs(t, err, ErrUsageUnavailable)
}

func TestUsageUnavailableOnExpiredStoredToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	expiredAt := time.Now().Add(-time.Hour).UnixMilli()
	credentials := `{"claudeAiOauth":{"accessToken":"expired_tok","expiresAt":` + jsonNumber(expiredAt) + `,"subscriptionType":"max","scopes":["user:profile","user:inference"]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(credentials), 0o600))

	s := NewSession()
	_, err := s.Usage(context.Background())
	require.ErrorIs(t, err, ErrUsageUnavailable)
}

func TestUsageReturnsEmptyWhenSessionEnvHasInferenceOnlyToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	s := NewSession(WithEnv(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "session_inference_only"}))
	usage, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.False(t, usage.HasData())
}

func TestUsageErrorOnHTTP500(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer server.Close()

	s := NewSession(WithOAuthToken("tok"), WithUsageBaseURL(server.URL))
	_, err := s.Usage(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUsageUnavailable)
	require.Contains(t, err.Error(), "500")
}

func TestUsageErrorOnMalformedJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not valid json`))
	}))
	defer server.Close()

	s := NewSession(WithOAuthToken("tok"), WithUsageBaseURL(server.URL))
	_, err := s.Usage(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode usage")
}

func TestUsageReadsCredentialsFromSessionEnvConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	// Host CLAUDE_CONFIG_DIR points somewhere empty; session env overrides it.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	expiresAt := time.Now().Add(time.Hour).UnixMilli()
	credentials := `{"claudeAiOauth":{"accessToken":"session_tok","expiresAt":` + jsonNumber(expiresAt) + `,"subscriptionType":"pro","scopes":["user:profile","user:inference"]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(credentials), 0o600))

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":50}}`))
	}))
	defer server.Close()

	s := NewSession(
		WithEnv(map[string]string{"CLAUDE_CONFIG_DIR": dir}),
		WithUsageBaseURL(server.URL),
	)
	usage, err := s.Usage(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer session_tok", gotAuth)
	require.NotNil(t, usage.FiveHour)
}

func TestGetEffortUsesEffectiveModelWhenNotApplied(t *testing.T) {
	t.Parallel()
	s, buf := newStartedControlTestSession(t)

	resultCh := make(chan struct {
		settings *EffortSettings
		err      error
	}, 1)
	go func() {
		settings, err := s.GetEffort(context.Background())
		resultCh <- struct {
			settings *EffortSettings
			err      error
		}{settings: settings, err: err}
	}()

	getID := waitForPendingControlRequest(t, s, "")
	requireWrittenControlSubtype(t, buf, "get_settings")
	// Model only in effective (CLI default), not in applied (no explicit override).
	sendControlSuccess(t, s, getID, map[string]any{
		"effective": map[string]any{"model": "claude-sonnet-4-6"},
		"sources":   []any{},
		"applied":   map[string]any{},
	})

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.Equal(t, "claude-sonnet-4-6", got.settings.Model)
		require.Equal(t, EffortAuto, got.settings.Effort)
		require.True(t, got.settings.Auto)
	case <-time.After(2 * time.Second):
		t.Fatal("GetEffort did not return")
	}
}

func newStartedControlTestSession(t *testing.T) (*Session, *bytes.Buffer) {
	t.Helper()
	s := newTestSession(t)
	s.started = true
	s.pendingControlResponses = make(map[string]chan protocol.ControlResponsePayload)
	buf := attachCapturingProcess(t, s)
	return s, buf
}

func waitForPendingControlRequest(t *testing.T, s *Session, exclude string) string {
	t.Helper()
	var requestID string
	require.Eventually(t, func() bool {
		s.pendingMu.Lock()
		defer s.pendingMu.Unlock()
		for id := range s.pendingControlResponses {
			if id != exclude {
				requestID = id
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	return requestID
}

func sendControlSuccess(t *testing.T, s *Session, requestID string, response map[string]any) {
	t.Helper()
	injectLine(t, s, map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   response,
		},
	})
}

func requireWrittenControlSubtype(t *testing.T, buf *bytes.Buffer, subtype string) map[string]any {
	t.Helper()
	var found map[string]any
	require.Eventually(t, func() bool {
		for _, msg := range writtenControlMessages(t, buf.String()) {
			req, ok := msg["request"].(map[string]any)
			if ok && req["subtype"] == subtype {
				found = msg
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	return found
}

func requireWrittenMessageType(t *testing.T, buf *bytes.Buffer, msgType string) map[string]any {
	t.Helper()
	var found map[string]any
	require.Eventually(t, func() bool {
		for _, msg := range writtenControlMessages(t, buf.String()) {
			if msg["type"] == msgType {
				found = msg
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	return found
}

func writtenControlMessages(t *testing.T, data string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(data), "\n")
	var messages []map[string]any
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &msg))
		messages = append(messages, msg)
	}
	return messages
}

func jsonNumber(n int64) string {
	data, err := json.Marshal(n)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func float64Ptr(v float64) *float64 {
	return &v
}
