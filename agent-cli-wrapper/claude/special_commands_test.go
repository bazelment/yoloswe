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

func TestMaxActiveUtilizationPrefersLimitsArray(t *testing.T) {
	t.Parallel()
	// limits[] present: a 99% weekly bucket while the 5-hour window is idle at
	// 5% must yield 99 — proving it is not five-hour-only.
	active := true
	usage := PlanUsage{
		FiveHour: &UsageRateLimit{Utilization: float64Ptr(5)},
		Limits: []PlanLimit{
			{Kind: "session", Percent: float64Ptr(5), IsActive: &active},
			{Kind: "weekly_all", Percent: float64Ptr(99), IsActive: &active},
		},
	}
	pct, ok := usage.MaxActiveUtilization()
	require.True(t, ok)
	require.Equal(t, 99.0, pct)
}

func TestMaxActiveUtilizationSkipsInactiveBuckets(t *testing.T) {
	t.Parallel()
	active, inactive := true, false
	usage := PlanUsage{
		Limits: []PlanLimit{
			{Kind: "weekly_scoped", Percent: float64Ptr(100), IsActive: &inactive},
			{Kind: "session", Percent: float64Ptr(40), IsActive: &active},
		},
	}
	pct, ok := usage.MaxActiveUtilization()
	require.True(t, ok)
	require.Equal(t, 40.0, pct)
}

func TestMaxActiveUtilizationUsesUtilizationWhenPercentAbsent(t *testing.T) {
	t.Parallel()
	usage := PlanUsage{
		Limits: []PlanLimit{{Kind: "session", Utilization: float64Ptr(77)}},
	}
	pct, ok := usage.MaxActiveUtilization()
	require.True(t, ok)
	require.Equal(t, 77.0, pct)
}

func TestMaxActiveUtilizationFallsBackToNamedFields(t *testing.T) {
	t.Parallel()
	// Legacy payload with no limits[]: use the named windows and return the max.
	usage := PlanUsage{
		FiveHour:     &UsageRateLimit{Utilization: float64Ptr(10)},
		SevenDayOpus: &UsageRateLimit{Utilization: float64Ptr(88)},
	}
	pct, ok := usage.MaxActiveUtilization()
	require.True(t, ok)
	require.Equal(t, 88.0, pct)
}

func TestMaxActiveUtilizationEmptyIsNotOK(t *testing.T) {
	t.Parallel()
	_, ok := PlanUsage{}.MaxActiveUtilization()
	require.False(t, ok)
}

// scopedLimit builds an active weekly_scoped bucket for a given model display
// name (id left empty, matching the real /api/oauth/usage payload).
func scopedLimit(pct float64, displayName string) PlanLimit {
	active := true
	return PlanLimit{
		Kind:     "weekly_scoped",
		Percent:  float64Ptr(pct),
		IsActive: &active,
		Scope:    &PlanLimitScope{Model: &ScopedModel{DisplayName: displayName}},
	}
}

func TestMaxActiveUtilizationForModel(t *testing.T) {
	t.Parallel()
	active, inactive := true, false

	// An idle (inactive) session window plus a Fable-scoped weekly cap at 100%:
	// the shape that must not gate a non-Fable model.
	incident := PlanUsage{
		Limits: []PlanLimit{
			{Kind: "session", Percent: float64Ptr(20), IsActive: &inactive},
			scopedLimit(100, "Fable"),
		},
	}

	tests := []struct {
		name     string
		modelID  string
		modelLbl string
		usage    PlanUsage
		wantPct  float64
		wantOK   bool
	}{
		{
			name:     "fable cap does not gate opus",
			usage:    incident,
			modelID:  "opus",
			modelLbl: "opus",
			wantOK:   false, // session bucket is inactive; Fable cap excluded
		},
		{
			name:     "fable cap gates fable (case-insensitive)",
			usage:    incident,
			modelID:  "fable",
			modelLbl: "fable",
			wantPct:  100,
			wantOK:   true,
		},
		{
			name: "unscoped weekly_all counts for any model",
			usage: PlanUsage{Limits: []PlanLimit{
				{Kind: "weekly_all", Percent: float64Ptr(90), IsActive: &active},
				scopedLimit(100, "Fable"),
			}},
			modelID:  "opus",
			modelLbl: "opus",
			wantPct:  90, // Fable's 100 excluded, weekly_all's 90 kept
			wantOK:   true,
		},
		{
			name: "scope matched by id when present",
			usage: PlanUsage{Limits: []PlanLimit{func() PlanLimit {
				l := scopedLimit(100, "Sonnet display")
				l.Scope.Model.ID = "sonnet"
				return l
			}()}},
			modelID:  "sonnet",
			modelLbl: "sonnet",
			wantPct:  100,
			wantOK:   true,
		},
		{
			name: "scope with no model identifier applies to all",
			usage: PlanUsage{Limits: []PlanLimit{func() PlanLimit {
				l := scopedLimit(100, "")
				l.Scope.Model.DisplayName = ""
				return l
			}()}},
			modelID:  "opus",
			modelLbl: "opus",
			wantPct:  100,
			wantOK:   true,
		},
		{
			name: "legacy named windows apply to every model",
			usage: PlanUsage{
				FiveHour:     &UsageRateLimit{Utilization: float64Ptr(10)},
				SevenDayOpus: &UsageRateLimit{Utilization: float64Ptr(70)},
			},
			modelID:  "sonnet",
			modelLbl: "sonnet",
			wantPct:  70,
			wantOK:   true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pct, ok := tc.usage.MaxActiveUtilizationForModel(tc.modelID, tc.modelLbl)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.Equal(t, tc.wantPct, pct)
			}
		})
	}
}

func TestPlanUsageReportSurfacesLimitsOnlyPayload(t *testing.T) {
	t.Parallel()
	// Forward-compat payload: no named FiveHour/SevenDay* fields, only limits[].
	// HasData and Report must still surface the buckets, not look empty.
	active := true
	reset := "2026-07-07T21:00:00Z"
	usage := PlanUsage{
		Limits: []PlanLimit{
			{Kind: "session", Percent: float64Ptr(42), ResetsAt: &reset, IsActive: &active},
			{Kind: "weekly_all", Percent: float64Ptr(88), IsActive: &active},
		},
	}
	require.True(t, usage.HasData(), "limits-only payload must count as data")

	report := usage.Report()
	require.NotEqual(t, "/usage is only available for subscription plans.", report)
	require.Contains(t, report, "Current session: 42% used")
	require.Contains(t, report, "Current week (all models): 88% used")
}

func TestPlanUsageReportSurfacesLimitsAlongsideExtraUsage(t *testing.T) {
	t.Parallel()
	// Mixed forward-compat payload: no named plan windows, but extra_usage IS
	// present. The extra-usage row must not suppress the plan-limit buckets —
	// both appear, with the limit rows first.
	active := true
	usage := PlanUsage{
		SubscriptionType: "max",
		Limits: []PlanLimit{
			{Kind: "session", Percent: float64Ptr(55), IsActive: &active},
		},
		ExtraUsage: &ExtraUsage{IsEnabled: true, MonthlyLimit: float64Ptr(1000), UsedCredits: float64Ptr(100)},
	}
	lines := usage.ReportLines()
	require.Len(t, lines, 2)
	require.Equal(t, "Current session", lines[0].Title, "plan-limit row must come first")
	require.Equal(t, "Extra usage", lines[1].Title)
	require.Contains(t, usage.Report(), "Current session: 55% used")
}

func TestPlanUsageReportPrefersNamedFieldsOverLimits(t *testing.T) {
	t.Parallel()
	// When named windows are present, they drive the report and the limits[]
	// fallback stays dormant — no duplicate rows.
	usage := PlanUsage{
		FiveHour: &UsageRateLimit{Utilization: float64Ptr(30)},
		Limits:   []PlanLimit{{Kind: "session", Percent: float64Ptr(99)}},
	}
	lines := usage.ReportLines()
	require.Len(t, lines, 1)
	require.Equal(t, "Current session", lines[0].Title)
	require.Contains(t, usage.Report(), "30% used")
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
