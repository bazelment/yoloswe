package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
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
	APIUsage     *ContextAPIUsage           `json:"apiUsage"`
	Raw          map[string]json.RawMessage `json:"-"`
	Categories   []ContextCategory          `json:"categories"`
	TotalTokens  int                        `json:"totalTokens"`
	MaxTokens    int                        `json:"maxTokens"`
	RawMaxTokens int                        `json:"rawMaxTokens"`
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

// ExtraUsage describes Claude.ai extra usage state.
type ExtraUsage struct {
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
	IsEnabled    bool     `json:"is_enabled"`
}

// PlanUsage is the real Claude.ai plan/rate-limit usage payload.
type PlanUsage struct {
	Raw              map[string]json.RawMessage `json:"-"`
	FiveHour         *UsageRateLimit            `json:"five_hour,omitempty"`
	SevenDay         *UsageRateLimit            `json:"seven_day,omitempty"`
	SevenDayOAuthApp *UsageRateLimit            `json:"seven_day_oauth_apps,omitempty"`
	SevenDayOpus     *UsageRateLimit            `json:"seven_day_opus,omitempty"`
	SevenDaySonnet   *UsageRateLimit            `json:"seven_day_sonnet,omitempty"`
	ExtraUsage       *ExtraUsage                `json:"extra_usage,omitempty"`
	SubscriptionType string                     `json:"-"`
	RateLimitTier    string                     `json:"-"`
}

// UsageReportLine is one display row from Claude Code's /usage view.
type UsageReportLine struct {
	Utilization *float64
	ResetsAt    *string
	Title       string
	Detail      string
}

// HasData reports whether the usage response includes any visible usage field.
func (u PlanUsage) HasData() bool {
	return u.FiveHour != nil ||
		u.SevenDay != nil ||
		u.SevenDayOAuthApp != nil ||
		u.SevenDayOpus != nil ||
		u.SevenDaySonnet != nil ||
		u.ExtraUsage != nil
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
	if err := s.ensureControlSessionReady(); err != nil {
		return nil, err
	}

	resp, err := s.sendControlRequestLocked(ctx, protocol.GetContextUsageRequestToSend{
		Subtype: string(protocol.ControlRequestSubtypeGetContextUsage),
	}, controlRequestDefaultTimeout)
	if err != nil {
		return nil, err
	}

	var usage ContextUsage
	if err := decodeControlPayload(resp.Response, &usage); err != nil {
		return nil, fmt.Errorf("decode context usage: %w", err)
	}
	if err := decodeControlPayload(resp.Response, &usage.Raw); err != nil {
		return nil, fmt.Errorf("decode raw context usage: %w", err)
	}
	return &usage, nil
}

// GetEffort returns the effort level the CLI says will be applied.
func (s *Session) GetEffort(ctx context.Context) (*EffortSettings, error) {
	if err := s.ensureControlSessionReady(); err != nil {
		return nil, err
	}

	resp, err := s.sendControlRequestLocked(ctx, protocol.GetSettingsRequestToSend{
		Subtype: string(protocol.ControlRequestSubtypeGetSettings),
	}, controlRequestDefaultTimeout)
	if err != nil {
		return nil, err
	}

	var settings protocol.GetSettingsResponse
	if err := decodeControlPayload(resp.Response, &settings); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}

	info := &EffortSettings{
		Raw:    settings,
		Effort: EffortAuto,
		Auto:   true,
	}
	if model, ok := settings.Applied["model"].(string); ok {
		info.Model = model
	}
	if effort, ok := settings.Applied["effort"].(string); ok && effort != "" {
		info.Effort = EffortLevel(effort)
		info.Auto = false
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
// auth source cannot expose subscription usage, it returns an empty PlanUsage
// like the interactive CLI command.
func (s *Session) Usage(ctx context.Context) (*PlanUsage, error) {
	token, ok, subscriptionType, rateLimitTier, err := s.usageOAuthToken()
	if err != nil {
		return nil, err
	}
	if !ok {
		return &PlanUsage{Raw: map[string]json.RawMessage{}}, nil
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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBetaHeader)

	client := s.config.UsageHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
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
	if err := json.Unmarshal(body, &usage.Raw); err != nil {
		return nil, fmt.Errorf("decode raw usage: %w", err)
	}
	usage.SubscriptionType = subscriptionType
	usage.RateLimitTier = rateLimitTier
	return &usage, nil
}

func (s *Session) updateEnvironmentVariables(vars map[string]string) error {
	if err := s.ensureControlSessionReady(); err != nil {
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

func (s *Session) ensureControlSessionReady() error {
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

func decodeControlPayload(payload interface{}, out interface{}) error {
	if payload == nil {
		return json.Unmarshal([]byte("{}"), out)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (level EffortLevel) validExplicitEffort() bool {
	switch level {
	case EffortLow, EffortMed, EffortHigh, EffortMax:
		return true
	default:
		return false
	}
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

func (s *Session) usageOAuthToken() (string, bool, string, string, error) {
	if s.config.OAuthToken != "" {
		return s.config.OAuthToken, true, "", "", nil
	}
	if token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		return "", false, "", "", nil
	}

	creds, err := readStoredCredentials()
	if err != nil {
		return "", false, "", "", err
	}
	if creds == nil || creds.ClaudeAIOAuth == nil || creds.ClaudeAIOAuth.AccessToken == "" {
		return "", false, "", "", nil
	}
	oauth := creds.ClaudeAIOAuth
	if !hasScope(oauth.Scopes, "user:inference") || !hasScope(oauth.Scopes, "user:profile") {
		return "", false, "", "", nil
	}
	if oauth.ExpiresAt != nil && time.Now().UnixMilli() >= *oauth.ExpiresAt {
		return "", false, "", "", fmt.Errorf("%w: OAuth token is expired", ErrUsageUnavailable)
	}
	return oauth.AccessToken, true, oauth.SubscriptionType, oauth.RateLimitTier, nil
}

func readStoredCredentials() (*storedCredentials, error) {
	path := filepath.Join(claudeConfigHomeDir(), ".credentials.json")
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

func claudeConfigHomeDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		current, currentErr := user.Current()
		if currentErr != nil || current.HomeDir == "" {
			return "."
		}
		home = current.HomeDir
	}
	return filepath.Join(home, ".claude")
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
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
