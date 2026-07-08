package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

const (
	controlRequestDefaultTimeout = 10 * time.Second
	defaultUsageBaseURL          = "https://api.anthropic.com"
	usageEndpointPath            = "/api/oauth/usage"
	oauthBetaHeader              = "oauth-2025-04-20"
)

// EffortLevel is the named reasoning effort level used by Claude Code.
type EffortLevel string

const (
	// EffortAuto clears explicit effort and lets the CLI/model default apply.
	EffortAuto EffortLevel = "auto"
	EffortLow  EffortLevel = "low"
	EffortMed  EffortLevel = "medium"
	EffortHigh EffortLevel = "high"
	EffortMax  EffortLevel = "max"
)

var (
	// ErrInvalidEffort is returned when an unknown effort level is requested.
	ErrInvalidEffort = errors.New("invalid effort level")
	// ErrUsageUnavailable is returned when real plan usage cannot be fetched
	// because profile-scoped OAuth credentials are expired or rejected.
	ErrUsageUnavailable = errors.New("claude plan usage unavailable")
)

// UsageHTTPClient is the subset of http.Client used by Usage.
type UsageHTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Shared default client for Usage so connections / TLS sessions get reused
// across polls (status bars typically fetch /usage on a short interval).
var defaultUsageHTTPClient = sync.OnceValue(func() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
})

// ContextCategory is one category in the current context window.
type ContextCategory struct {
	Name       string `json:"name"`
	Color      string `json:"color"`
	Tokens     int    `json:"tokens"`
	IsDeferred bool   `json:"isDeferred,omitempty"`
}

// ContextAPIUsage mirrors the token usage object embedded in /context data.
type ContextAPIUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ContextUsage contains the structured SDK response behind /context.
type ContextUsage struct {
	APIUsage     *ContextAPIUsage  `json:"apiUsage"`
	Raw          json.RawMessage   `json:"-"`
	Categories   []ContextCategory `json:"categories"`
	TotalTokens  int               `json:"totalTokens"`
	MaxTokens    int               `json:"maxTokens"`
	RawMaxTokens int               `json:"rawMaxTokens"`
}

// EffectiveMaxTokens returns the populated max token field. Older CLI builds
// use rawMaxTokens while the SDK schema names the same value maxTokens.
func (u ContextUsage) EffectiveMaxTokens() int {
	if u.RawMaxTokens > 0 {
		return u.RawMaxTokens
	}
	return u.MaxTokens
}

// EffortSettings reports the effort that the CLI says will be applied.
type EffortSettings struct {
	Model  string
	Effort EffortLevel
	Raw    protocol.GetSettingsResponse
	Auto   bool
}

// UsageRateLimit is a single plan usage bucket from /api/oauth/usage.
type UsageRateLimit struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// PlanLimit is one entry of the generic limits[] array in /api/oauth/usage.
// It is forward-compatible: new window kinds (session, weekly_all,
// weekly_scoped, …) surface here without a schema change. Either percent or
// utilization may carry the 0–100 utilization figure depending on payload
// version; utilizationPct prefers whichever is present.
type PlanLimit struct {
	Percent     *float64        `json:"percent"`
	Utilization *float64        `json:"utilization"`
	ResetsAt    *string         `json:"resets_at"`
	IsActive    *bool           `json:"is_active"`
	Scope       *PlanLimitScope `json:"scope,omitempty"`
	Kind        string          `json:"kind"`
}

// PlanLimitScope narrows a plan-limit bucket to a specific model (a
// weekly_scoped window). A nil Model means the bucket is account-wide.
type PlanLimitScope struct {
	Model *ScopedModel `json:"model"`
}

// ScopedModel identifies the model a scoped plan-limit applies to. The API
// sometimes leaves ID empty and carries only DisplayName (e.g. "Fable").
type ScopedModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// appliesToModel reports whether this limit bucket counts toward the given
// model. A scoped bucket applies only when its model matches modelID or
// modelLabel (case-insensitive on either id or display name). An unscoped
// bucket — or a scoped one with no usable identifier — applies to every model.
func (l PlanLimit) appliesToModel(modelID, modelLabel string) bool {
	if l.Scope == nil || l.Scope.Model == nil {
		return true
	}
	m := l.Scope.Model
	if m.ID == "" && m.DisplayName == "" {
		return true
	}
	return scopeIDMatches(m.ID, modelID, modelLabel) ||
		scopeIDMatches(m.DisplayName, modelID, modelLabel)
}

// scopeIDMatches reports whether a scope identifier (id or display name) refers
// to the same model as modelID/modelLabel. It matches case-insensitively on the
// raw strings and, for Claude, on the model *family* — so a scope reported as
// the bare family ("Opus") matches a versioned config ("claude-opus-4-8") and
// vice versa. Empty scope identifiers never match.
func scopeIDMatches(scopeID, modelID, modelLabel string) bool {
	if scopeID == "" {
		return false
	}
	if strings.EqualFold(scopeID, modelID) || strings.EqualFold(scopeID, modelLabel) {
		return true
	}
	fam := claudeModelFamily(scopeID)
	return fam != "" &&
		(fam == claudeModelFamily(modelID) || fam == claudeModelFamily(modelLabel))
}

// claudeModelFamily maps a Claude model id, label, or plan-scope display name to
// its lowercase family ("opus", "sonnet", "haiku", "fable"). It handles bare
// aliases ("opus"), versioned ids ("claude-opus-4-8"), and API display names
// ("Opus") by substring match on the family token. Non-Claude or unrecognized
// ids return "" (no family), so they only ever match on the exact-string path.
func claudeModelFamily(id string) string {
	s := strings.ToLower(id)
	for _, fam := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(s, fam) {
			return fam
		}
	}
	return ""
}

// title returns a human-readable label for the bucket's window kind, used when
// the payload carries only the generic limits[] array (no named FiveHour/
// SevenDay* fields). Unknown kinds fall back to the raw kind string.
func (l PlanLimit) title() string {
	switch l.Kind {
	case "session", "five_hour":
		return "Current session"
	case "weekly_all", "seven_day":
		return "Current week (all models)"
	case "weekly_scoped", "seven_day_scoped":
		return "Current week (scoped)"
	case "":
		return "Plan limit"
	default:
		return l.Kind
	}
}

// active reports whether the limit is an active window. is_active is treated as
// true when the field is absent (older payloads omit it), so a bucket is only
// excluded when the server explicitly marks it inactive.
func (l PlanLimit) active() bool {
	return l.IsActive == nil || *l.IsActive
}

// utilizationPct returns the bucket's utilization (0–100) and whether a value
// was present, preferring percent over utilization.
func (l PlanLimit) utilizationPct() (float64, bool) {
	if l.Percent != nil {
		return *l.Percent, true
	}
	if l.Utilization != nil {
		return *l.Utilization, true
	}
	return 0, false
}

// ExtraUsage describes Claude.ai extra usage state.
type ExtraUsage struct {
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
	IsEnabled    bool     `json:"is_enabled"`
}

// PlanUsage is the real Claude.ai plan/rate-limit usage payload.
type PlanUsage struct {
	FiveHour         *UsageRateLimit `json:"five_hour,omitempty"`
	SevenDay         *UsageRateLimit `json:"seven_day,omitempty"`
	SevenDayOAuthApp *UsageRateLimit `json:"seven_day_oauth_apps,omitempty"`
	SevenDayOpus     *UsageRateLimit `json:"seven_day_opus,omitempty"`
	SevenDaySonnet   *UsageRateLimit `json:"seven_day_sonnet,omitempty"`
	ExtraUsage       *ExtraUsage     `json:"extra_usage,omitempty"`
	Limits           []PlanLimit     `json:"limits,omitempty"`
	SubscriptionType string          `json:"-"`
	RateLimitTier    string          `json:"-"`
	Raw              json.RawMessage `json:"-"`
}

// UsageReportLine is one display row from Claude Code's /usage view.
type UsageReportLine struct {
	Utilization *float64
	ResetsAt    *string
	Title       string
	Detail      string
}

// HasData reports whether the usage response includes any visible usage field.
// The generic limits[] array counts: a forward-compatible payload that carries
// only limits[] (no named FiveHour/SevenDay* fields) still has real data.
func (u PlanUsage) HasData() bool {
	return u.FiveHour != nil ||
		u.SevenDay != nil ||
		u.SevenDayOAuthApp != nil ||
		u.SevenDayOpus != nil ||
		u.SevenDaySonnet != nil ||
		u.ExtraUsage != nil ||
		len(u.Limits) > 0
}

// MaxActiveUtilization returns the highest utilization (0–100) across all
// active plan-limit windows. It prefers the generic limits[] array — covering
// the session, weekly-all, weekly-scoped, and any future window kind — so a
// weekly or per-model exhaustion is caught even when the 5-hour window is idle.
// When limits[] is absent (older payloads), it falls back to the named
// FiveHour/SevenDay* fields. ok is false when no usable bucket is present, and
// callers must fail open (not block) on !ok.
//
// This model-blind view does NOT filter by model, so a model-scoped window
// contributes to the max even for a different model. Callers gating a specific
// model on utilization must use MaxActiveUtilizationForModel; this variant is
// for account-wide "how full is the plan" reporting.
func (u PlanUsage) MaxActiveUtilization() (pct float64, ok bool) {
	return u.maxActiveUtilization(
		func(PlanLimit) bool { return true },
		func(string) bool { return true },
	)
}

// MaxActiveUtilizationForModel is the model-aware variant of
// MaxActiveUtilization: an active bucket counts only when it applies to the
// given model. The pre-flight skip must use this — otherwise a scoped cap on
// one model (e.g. Fable at 100%) trips the skip for a different model (e.g.
// Opus) that still has headroom. Model-awareness covers both schemas: the
// generic limits[] buckets (via appliesToModel) and the legacy named windows
// (SevenDayOpus / SevenDaySonnet are model-scoped, so they gate only their
// own model).
func (u PlanUsage) MaxActiveUtilizationForModel(modelID, modelLabel string) (pct float64, ok bool) {
	namedApplies := func(scopeModel string) bool {
		// An empty scopeModel is an account-wide window; it applies to every model.
		return scopeModel == "" || scopeIDMatches(scopeModel, modelID, modelLabel)
	}
	return u.maxActiveUtilization(
		func(l PlanLimit) bool { return l.appliesToModel(modelID, modelLabel) },
		namedApplies,
	)
}

// maxActiveUtilization returns the highest utilization across active buckets.
// applies gates the generic limits[] entries; namedApplies gates the legacy
// named windows by their implied model (empty means account-wide, always counted).
// The named windows are only consulted when limits[] carries nothing usable.
func (u PlanUsage) maxActiveUtilization(applies func(PlanLimit) bool, namedApplies func(scopeModel string) bool) (pct float64, ok bool) {
	for _, l := range u.Limits {
		if !l.active() || !applies(l) {
			continue
		}
		if v, has := l.utilizationPct(); has && (!ok || v > pct) {
			pct, ok = v, true
		}
	}
	if ok {
		return pct, true
	}
	// Legacy payloads without limits[]: consider the named windows. FiveHour,
	// SevenDay, and SevenDayOAuthApp are account-wide; SevenDayOpus and
	// SevenDaySonnet are model-scoped and must gate only their own model.
	named := []struct {
		limit *UsageRateLimit
		model string // empty means account-wide (applies to every model)
	}{
		{u.FiveHour, ""},
		{u.SevenDay, ""},
		{u.SevenDayOAuthApp, ""},
		{u.SevenDayOpus, "opus"},
		{u.SevenDaySonnet, "sonnet"},
	}
	for _, n := range named {
		if n.limit == nil || n.limit.Utilization == nil || !namedApplies(n.model) {
			continue
		}
		if v := *n.limit.Utilization; !ok || v > pct {
			pct, ok = v, true
		}
	}
	return pct, ok
}

// ReportLines returns the meaningful rows displayed by Claude Code's /usage.
func (u PlanUsage) ReportLines() []UsageReportLine {
	var lines []UsageReportLine
	appendLimit := func(title string, limit *UsageRateLimit) {
		if limit == nil {
			return
		}
		lines = append(lines, UsageReportLine{
			Utilization: limit.Utilization,
			ResetsAt:    limit.ResetsAt,
			Title:       title,
		})
	}

	appendLimit("Current session", u.FiveHour)
	appendLimit("Current week (all models)", u.SevenDay)
	if u.SubscriptionType == "" || u.SubscriptionType == "max" || u.SubscriptionType == "team" {
		appendLimit("Current week (Sonnet only)", u.SevenDaySonnet)
	}

	// Forward-compat fallback: if the payload carried only the generic limits[]
	// array (no named plan windows produced rows), surface those buckets so
	// Report() isn't blank when the API drops the legacy FiveHour/SevenDay*
	// fields. Emitted before Extra usage — an extra-usage row must not suppress
	// the plan-limit rows (they answer different questions), so the fallback is
	// gated on the plan-window count, not total len(lines).
	if len(lines) == 0 {
		for _, l := range u.Limits {
			if !l.active() {
				continue
			}
			var util *float64
			if v, has := l.utilizationPct(); has {
				util = &v
			}
			lines = append(lines, UsageReportLine{
				Utilization: util,
				ResetsAt:    l.ResetsAt,
				Title:       l.title(),
			})
		}
	}

	if u.ExtraUsage != nil && (u.SubscriptionType == "pro" || u.SubscriptionType == "max") {
		line := UsageReportLine{
			Utilization: u.ExtraUsage.Utilization,
			Title:       "Extra usage",
		}
		switch {
		case !u.ExtraUsage.IsEnabled:
			line.Detail = "Extra usage not enabled"
		case u.ExtraUsage.MonthlyLimit == nil:
			line.Detail = "Unlimited"
		case u.ExtraUsage.UsedCredits != nil:
			line.Detail = fmt.Sprintf("%s / %s spent",
				formatUsageCostCents(*u.ExtraUsage.UsedCredits),
				formatUsageCostCents(*u.ExtraUsage.MonthlyLimit))
		}
		lines = append(lines, line)
	}

	return lines
}

// Report formats usage in the same row-oriented shape as Claude Code's /usage.
func (u PlanUsage) Report() string {
	lines := u.ReportLines()
	if len(lines) == 0 {
		return "/usage is only available for subscription plans."
	}
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, line.String())
	}
	return strings.Join(parts, "\n")
}

// String formats one /usage report row.
func (l UsageReportLine) String() string {
	details := make([]string, 0, 3)
	if l.Utilization != nil {
		details = append(details, fmt.Sprintf("%d%% used", int(*l.Utilization)))
	}
	if l.Detail != "" {
		details = append(details, l.Detail)
	}
	if l.ResetsAt != nil && *l.ResetsAt != "" {
		details = append(details, "Resets "+formatUsageResetText(*l.ResetsAt))
	}
	if len(details) == 0 {
		return l.Title
	}
	return l.Title + ": " + strings.Join(details, " - ")
}

// ContextUsage returns the structured data behind Claude Code's /context.
func (s *Session) ContextUsage(ctx context.Context) (*ContextUsage, error) {
	if err := s.checkControlSessionReady(); err != nil {
		return nil, err
	}

	resp, err := s.sendControlRequestLocked(ctx, subtypeOnlyRequest(protocol.ControlRequestSubtypeGetContextUsage), controlRequestDefaultTimeout)
	if err != nil {
		return nil, err
	}

	raw, err := marshalControlPayload(resp.Response)
	if err != nil {
		return nil, fmt.Errorf("marshal context usage: %w", err)
	}
	var usage ContextUsage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, fmt.Errorf("decode context usage: %w", err)
	}
	usage.Raw = raw
	return &usage, nil
}

// GetEffort returns the effort level the CLI says will be applied.
func (s *Session) GetEffort(ctx context.Context) (*EffortSettings, error) {
	if err := s.checkControlSessionReady(); err != nil {
		return nil, err
	}

	resp, err := s.sendControlRequestLocked(ctx, subtypeOnlyRequest(protocol.ControlRequestSubtypeGetSettings), controlRequestDefaultTimeout)
	if err != nil {
		return nil, err
	}

	raw, err := marshalControlPayload(resp.Response)
	if err != nil {
		return nil, fmt.Errorf("marshal settings: %w", err)
	}
	var settings protocol.GetSettingsResponse
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}

	info := &EffortSettings{
		Raw:    settings,
		Effort: EffortAuto,
		Auto:   true,
	}
	// Model: prefer the explicitly applied value; fall back to the effective
	// (merged) value so info.Model is always populated when the CLI reports one.
	if model, ok := settings.Applied["model"].(string); ok && model != "" {
		info.Model = model
	} else if model, ok := settings.Effective["model"].(string); ok {
		info.Model = model
	}
	if effort, ok := settings.Applied["effort"].(string); ok && effort != "" {
		level := EffortLevel(effort)
		if level.validExplicitEffort() {
			info.Effort = level
			info.Auto = false
		}
	}
	return info, nil
}

// SetEffort sets the session-scoped effort level and returns the applied state.
func (s *Session) SetEffort(ctx context.Context, level EffortLevel) (*EffortSettings, error) {
	if level == EffortAuto {
		return s.ClearEffort(ctx)
	}
	if !level.validExplicitEffort() {
		return nil, fmt.Errorf("%w: %s", ErrInvalidEffort, level)
	}
	if err := s.updateEnvironmentVariables(map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": string(level)}); err != nil {
		return nil, err
	}
	return s.GetEffort(ctx)
}

// ClearEffort clears explicit effort and returns the applied state.
func (s *Session) ClearEffort(ctx context.Context) (*EffortSettings, error) {
	if err := s.updateEnvironmentVariables(map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": "auto"}); err != nil {
		return nil, err
	}
	return s.GetEffort(ctx)
}

// Usage fetches the real Claude.ai plan usage backing /usage. If the current
// auth source cannot expose subscription usage (no profile-scoped OAuth
// credentials available), it returns an empty PlanUsage like the interactive
// CLI command. ErrUsageUnavailable is returned only when credentials exist but
// are expired or rejected by the server.
//
// When the token comes from WithOAuthToken (not stored credentials),
// PlanUsage.SubscriptionType and RateLimitTier will be empty, which means
// subscription-conditional rows (e.g. Extra usage) are skipped in ReportLines.
func (s *Session) Usage(ctx context.Context) (*PlanUsage, error) {
	creds, err := s.usageOAuthToken()
	if err != nil {
		return nil, err
	}
	if !creds.OK {
		return &PlanUsage{}, nil
	}

	baseURL := s.config.UsageBaseURL
	if baseURL == "" {
		baseURL = defaultUsageBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+usageEndpointPath, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/unknown (sdk-go)")
	req.Header.Set("Authorization", "Bearer "+creds.Token)
	req.Header.Set("anthropic-beta", oauthBetaHeader)

	client := s.config.UsageHTTPClient
	if client == nil {
		client = defaultUsageHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: OAuth token was rejected with HTTP %d", ErrUsageUnavailable, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("fetch usage failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var usage PlanUsage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	usage.Raw = body
	usage.SubscriptionType = creds.SubscriptionType
	usage.RateLimitTier = creds.RateLimitTier
	return &usage, nil
}

func (s *Session) updateEnvironmentVariables(vars map[string]string) error {
	if err := s.checkControlSessionReady(); err != nil {
		return err
	}
	msg := protocol.UpdateEnvironmentVariablesMessage{
		Type:      protocol.MessageTypeUpdateEnvironmentVariables,
		Variables: vars,
	}
	if err := s.process.WriteMessage(msg); err != nil {
		return err
	}
	if s.recorder != nil {
		s.recorder.RecordSent(msg)
	}
	return nil
}

// checkControlSessionReady is a best-effort precondition check. It is not
// atomic with the subsequent send — Stop() may race in between, in which case
// the underlying WriteMessage will return an error. Matches the existing
// pattern used by sendInitialize.
func (s *Session) checkControlSessionReady() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.started || s.process == nil {
		return ErrNotStarted
	}
	if s.stopping {
		return ErrStopping
	}
	return nil
}

// subtypeOnlyRequest builds the wire body for control requests whose only
// field is the subtype (get_context_usage, get_settings, interrupt, ...).
func subtypeOnlyRequest(subtype protocol.ControlRequestSubtype) any {
	return struct {
		Subtype string `json:"subtype"`
	}{Subtype: string(subtype)}
}

// marshalControlPayload normalises a control response payload into JSON bytes.
// Control responses arrive as map[string]any after the line-level decode, so
// the only way to typed-decode them is a marshal+unmarshal round-trip.
func marshalControlPayload(payload any) ([]byte, error) {
	if payload == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(payload)
}

func (level EffortLevel) validExplicitEffort() bool {
	switch level {
	case EffortLow, EffortMed, EffortHigh, EffortMax:
		return true
	default:
		return false
	}
}

// ParseEffort parses a user-supplied string into an EffortLevel. It accepts
// EffortAuto in addition to the explicit levels — callers that need to forbid
// "auto" should compare the result against EffortAuto themselves.
func ParseEffort(s string) (EffortLevel, error) {
	level := EffortLevel(s)
	if level == EffortAuto || level.validExplicitEffort() {
		return level, nil
	}
	return "", fmt.Errorf("%w: %q (valid: low, medium, high, max, auto)", ErrInvalidEffort, s)
}

type storedCredentials struct {
	ClaudeAIOAuth *storedOAuth `json:"claudeAiOauth"`
}

type storedOAuth struct {
	ExpiresAt        *int64   `json:"expiresAt"`
	AccessToken      string   `json:"accessToken"`
	SubscriptionType string   `json:"subscriptionType"`
	RateLimitTier    string   `json:"rateLimitTier"`
	Scopes           []string `json:"scopes"`
}

// oauthCreds is the resolved OAuth credential used by Usage.
type oauthCreds struct {
	Token            string
	SubscriptionType string
	RateLimitTier    string
	OK               bool
}

func (s *Session) usageOAuthToken() (oauthCreds, error) {
	if s.config.OAuthToken != "" {
		return oauthCreds{Token: s.config.OAuthToken, OK: true}, nil
	}
	// CLAUDE_CODE_OAUTH_TOKEN is API-token style auth without profile scope —
	// the /usage endpoint requires user:profile, so treat it as "no usage".
	// Check session-level env overlay first, then host env.
	if token := s.config.Env["CLAUDE_CODE_OAUTH_TOKEN"]; token != "" {
		return oauthCreds{}, nil
	}
	if token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		return oauthCreds{}, nil
	}

	creds, err := readStoredCredentials(s.config.Env)
	if err != nil {
		return oauthCreds{}, err
	}
	if creds == nil || creds.ClaudeAIOAuth == nil || creds.ClaudeAIOAuth.AccessToken == "" {
		return oauthCreds{}, nil
	}
	oauth := creds.ClaudeAIOAuth
	if !slices.Contains(oauth.Scopes, "user:inference") || !slices.Contains(oauth.Scopes, "user:profile") {
		return oauthCreds{}, nil
	}
	if oauth.ExpiresAt != nil && time.Now().UnixMilli() >= *oauth.ExpiresAt {
		return oauthCreds{}, fmt.Errorf("%w: OAuth token is expired", ErrUsageUnavailable)
	}
	return oauthCreds{
		Token:            oauth.AccessToken,
		SubscriptionType: oauth.SubscriptionType,
		RateLimitTier:    oauth.RateLimitTier,
		OK:               true,
	}, nil
}

func readStoredCredentials(sessionEnv map[string]string) (*storedCredentials, error) {
	path := filepath.Join(claudeConfigHomeDir(sessionEnv), ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Claude credentials: %w", err)
	}
	var creds storedCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("decode Claude credentials: %w", err)
	}
	return &creds, nil
}

// claudeConfigHomeDir returns the Claude config directory, checking the
// session env overlay before falling back to the host env and ~/.claude.
func claudeConfigHomeDir(sessionEnv map[string]string) string {
	if dir := sessionEnv["CLAUDE_CONFIG_DIR"]; dir != "" {
		return dir
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return filepath.Join(home, ".claude")
}

func formatUsageCostCents(cents float64) string {
	return fmt.Sprintf("$%.2f", cents/100)
}

func formatUsageResetText(resetsAt string) string {
	t, err := time.Parse(time.RFC3339, resetsAt)
	if err != nil {
		return resetsAt
	}

	now := time.Now()
	format := "3:04pm"
	if t.Minute() == 0 {
		format = "3pm"
	}
	if t.Sub(now) > 24*time.Hour {
		format = "Jan 2 " + format
		if t.Year() != now.Year() {
			format = "Jan 2, 2006 " + strings.TrimPrefix(format, "Jan 2 ")
		}
	}
	formatted := t.Local().Format(format)
	formatted = strings.ReplaceAll(formatted, "AM", "am")
	formatted = strings.ReplaceAll(formatted, "PM", "pm")
	return formatted + " (" + t.Local().Format("MST") + ")"
}
