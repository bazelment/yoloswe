package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
)

// Default HTTP client configuration constants for the Anthropic Messages API.
const (
	DefaultBaseURL          = "https://api.anthropic.com"
	DefaultAnthropicVersion = "2023-06-01"
	DefaultMaxRetries       = 3
	DefaultRetryBaseDelay   = 200 * time.Millisecond
	DefaultRetryMaxDelay    = 5 * time.Second
	DefaultHTTPTimeout      = 120 * time.Second
)

// APIError represents a non-2xx HTTP response from the Anthropic Messages API.
type APIError struct {
	Type       string `json:"type"`
	Message    string `json:"message"`
	Body       string `json:"body,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	StatusCode int    `json:"status_code"`
	Attempts   int    `json:"attempts"`
}

func (e *APIError) Error() string {
	var parts []string
	parts = append(parts, fmt.Sprintf("orch/model: messages API status %d", e.StatusCode))
	if e.Type != "" {
		parts = append(parts, fmt.Sprintf("(%s)", e.Type))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	} else if e.Body != "" {
		parts = append(parts, e.Body)
	}
	if e.Attempts > 1 {
		parts = append(parts, fmt.Sprintf("[attempts=%d]", e.Attempts))
	}
	return strings.Join(parts, ": ")
}

// Retryable reports whether the HTTP status code is transient (429, 529, or 5xx).
func (e *APIError) Retryable() bool {
	return isRetryableStatusCode(e.StatusCode)
}

func isRetryableStatusCode(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == 529 ||
		(code >= 500 && code < 600)
}

// IsRateLimit reports whether err is a 429 rate-limit APIError.
func IsRateLimit(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests
}

// IsOverloaded reports whether err is a 529 overloaded APIError.
func IsOverloaded(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 529
}

// IsRetryable reports whether err is a retryable APIError.
func IsRetryable(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Retryable()
}

// HTTPClient implements ModelClient over the Anthropic Messages API using net/http.
type HTTPClient struct {
	clock            clock.Clock
	httpClient       *http.Client
	headers          map[string]string
	pricing          map[string]ModelPricing
	baseURL          string
	apiKey           string
	authToken        string
	anthropicVersion string
	totalUsage       Usage
	retryBaseDelay   time.Duration
	retryMaxDelay    time.Duration
	maxRetries       int
	mu               sync.Mutex
	baseURLSet       bool
	apiKeySet        bool
	authTokenSet     bool
}

// ClientOption configures an HTTPClient.
type ClientOption func(*HTTPClient)

// WithBaseURL sets the base URL for the Messages API (e.g. "https://api.anthropic.com"
// or an llm-gateway URL).
func WithBaseURL(baseURL string) ClientOption {
	return func(c *HTTPClient) {
		c.baseURL = strings.TrimSpace(baseURL)
		c.baseURLSet = true
	}
}

// WithAPIKey sets the x-api-key header credential.
func WithAPIKey(apiKey string) ClientOption {
	return func(c *HTTPClient) {
		c.apiKey = apiKey
		c.apiKeySet = true
	}
}

// WithAuthToken sets the Authorization: Bearer <token> header credential.
func WithAuthToken(token string) ClientOption {
	return func(c *HTTPClient) {
		c.authToken = token
		c.authTokenSet = true
	}
}

// WithHeader adds a custom HTTP header to every Messages API request.
func WithHeader(key, value string) ClientOption {
	return func(c *HTTPClient) {
		if c.headers == nil {
			c.headers = make(map[string]string)
		}
		c.headers[key] = value
	}
}

// WithHeaders adds multiple custom HTTP headers to every Messages API request.
func WithHeaders(headers map[string]string) ClientOption {
	return func(c *HTTPClient) {
		if c.headers == nil {
			c.headers = make(map[string]string, len(headers))
		}
		for k, v := range headers {
			c.headers[k] = v
		}
	}
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *HTTPClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithAnthropicVersion sets the anthropic-version header (default "2023-06-01").
func WithAnthropicVersion(version string) ClientOption {
	return func(c *HTTPClient) {
		c.anthropicVersion = strings.TrimSpace(version)
	}
}

// WithClock sets the Clock used for retry backoff timers.
func WithClock(clk clock.Clock) ClientOption {
	return func(c *HTTPClient) {
		if clk != nil {
			c.clock = clk
		}
	}
}

// WithMaxRetries sets the maximum number of retry attempts on transient 429/529/5xx errors.
func WithMaxRetries(maxRetries int) ClientOption {
	return func(c *HTTPClient) {
		c.maxRetries = max(maxRetries, 0)
	}
}

// WithRetryBackoff sets the base and maximum backoff duration between retry attempts.
func WithRetryBackoff(baseDelay, maxDelay time.Duration) ClientOption {
	return func(c *HTTPClient) {
		c.retryBaseDelay = max(baseDelay, 0)
		c.retryMaxDelay = max(maxDelay, c.retryBaseDelay)
	}
}

// WithPricing overrides the per-model token pricing table used for CostUSD accounting.
func WithPricing(pricing map[string]ModelPricing) ClientOption {
	return func(c *HTTPClient) {
		c.pricing = maps.Clone(pricing)
	}
}

// ParseCustomHeaders parses a newline-separated list of "Header: Value" pairs
// (the format used by ANTHROPIC_CUSTOM_HEADERS).
func ParseCustomHeaders(raw string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// NewHTTPClient creates an HTTPClient configured with opts, falling back to
// ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, and
// ANTHROPIC_CUSTOM_HEADERS environment variables when not explicitly set.
func NewHTTPClient(opts ...ClientOption) *HTTPClient {
	c := &HTTPClient{
		clock:            clock.NewRealClock(),
		httpClient:       &http.Client{Timeout: DefaultHTTPTimeout},
		headers:          ParseCustomHeaders(os.Getenv("ANTHROPIC_CUSTOM_HEADERS")),
		pricing:          DefaultPricing(),
		baseURL:          DefaultBaseURL,
		anthropicVersion: DefaultAnthropicVersion,
		maxRetries:       DefaultMaxRetries,
		retryBaseDelay:   DefaultRetryBaseDelay,
		retryMaxDelay:    DefaultRetryMaxDelay,
	}
	for _, opt := range opts {
		opt(c)
	}
	if !c.baseURLSet {
		if envURL := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")); envURL != "" {
			c.baseURL = envURL
		}
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if !c.apiKeySet {
		c.apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if !c.authTokenSet {
		c.authToken = os.Getenv("ANTHROPIC_AUTH_TOKEN")
	}
	return c
}

// TotalUsage returns the cumulative token usage and cost across all completed
// HTTP requests made by this client.
func (c *HTTPClient) TotalUsage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalUsage
}

// ResetUsage clears the client's cumulative usage counters.
func (c *HTTPClient) ResetUsage() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalUsage = Usage{}
}

// RunToolLoop executes a bounded tool_use/tool_result loop on this client.
func (c *HTTPClient) RunToolLoop(
	ctx context.Context,
	req Request,
	reg *ToolRegistry,
	opts ...ToolLoopOption,
) (*ToolLoopResult, error) {
	return RunToolLoop(ctx, c, req, reg, opts...)
}

type wireRequest struct {
	Metadata      map[string]string `json:"metadata,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	Model         string            `json:"model"`
	System        string            `json:"system,omitempty"`
	Messages      []Message         `json:"messages"`
	Tools         []ToolDefinition  `json:"tools,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	MaxTokens     int               `json:"max_tokens"`
}

// CreateMessage sends a Messages API request (or runs the bounded tool loop if
// req.ToolRegistry is set) and returns the parsed Response with Usage and CostUSD.
func (c *HTTPClient) CreateMessage(ctx context.Context, req Request) (*Response, error) {
	if req.ToolRegistry != nil {
		res, err := RunToolLoop(ctx, c, req, req.ToolRegistry, WithMaxTurns(req.MaxTurns))
		if res != nil {
			cp := res.Response.Clone()
			return &cp, err
		}
		return nil, err
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	payload := wireRequest{
		Model:         req.Model,
		System:        req.System,
		Messages:      req.Messages,
		Tools:         req.Tools,
		StopSequences: req.StopSequences,
		MaxTokens:     maxTokens,
		Temperature:   req.Temperature,
		Metadata:      req.Metadata,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("orch/model: marshal request: %w", err)
	}

	endpoint := messagesEndpointURL(c.baseURL)
	maxAttempts := c.maxRetries + 1

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("orch/model: build http request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.anthropicVersion != "" {
			httpReq.Header.Set("anthropic-version", c.anthropicVersion)
		}
		if c.apiKey != "" {
			httpReq.Header.Set("x-api-key", c.apiKey)
		}
		if c.authToken != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.authToken)
		}
		for k, v := range c.headers {
			httpReq.Header.Set(k, v)
		}

		httpResp, err := c.httpClient.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < maxAttempts {
				if waitErr := c.sleepRetry(ctx, attempt, nil); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, fmt.Errorf("orch/model: http request failed after %d attempts: %w", attempt, err)
		}

		respBytes, readErr := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		if readErr != nil {
			if attempt < maxAttempts {
				if waitErr := c.sleepRetry(ctx, attempt, httpResp.Header); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, fmt.Errorf("orch/model: read response body: %w", readErr)
		}

		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			var resp Response
			if err := json.Unmarshal(respBytes, &resp); err != nil {
				return nil, fmt.Errorf("orch/model: decode response: %w", err)
			}
			if resp.Model == "" {
				resp.Model = req.Model
			}
			if resp.Turns == 0 {
				resp.Turns = 1
			}
			if resp.Usage.CostUSD == 0 {
				resp.Usage.CostUSD = CalculateCost(resp.Model, resp.Usage, c.pricing)
			}
			c.mu.Lock()
			c.totalUsage = c.totalUsage.Add(resp.Usage)
			c.mu.Unlock()
			return &resp, nil
		}

		apiErr := parseAPIError(httpResp.StatusCode, httpResp.Header, respBytes, attempt)
		if apiErr.Retryable() && attempt < maxAttempts {
			if waitErr := c.sleepRetry(ctx, attempt, httpResp.Header); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		return nil, apiErr
	}

	return nil, errors.New("orch/model: unreachable retry loop exit")
}

func messagesEndpointURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		trimmed = DefaultBaseURL
	}
	if strings.HasSuffix(trimmed, "/v1/messages") {
		return trimmed
	}
	if strings.HasSuffix(trimmed, "/v1") {
		return trimmed + "/messages"
	}
	return trimmed + "/v1/messages"
}

func parseAPIError(statusCode int, hdr http.Header, body []byte, attempts int) *APIError {
	apiErr := &APIError{
		StatusCode: statusCode,
		Body:       strings.TrimSpace(string(body)),
		Attempts:   attempts,
	}
	if hdr != nil {
		apiErr.RequestID = hdr.Get("request-id")
		if apiErr.RequestID == "" {
			apiErr.RequestID = hdr.Get("x-request-id")
		}
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		switch {
		case envelope.Error.Message != "" || envelope.Error.Type != "":
			apiErr.Type = envelope.Error.Type
			apiErr.Message = envelope.Error.Message
		case envelope.Message != "":
			apiErr.Type = envelope.Type
			apiErr.Message = envelope.Message
		}
	}
	return apiErr
}

func (c *HTTPClient) sleepRetry(ctx context.Context, attempt int, hdr http.Header) error {
	delay := c.retryDelay(attempt, hdr)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := c.clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

func (c *HTTPClient) retryDelay(attempt int, hdr http.Header) time.Duration {
	if hdr != nil {
		if msStr := strings.TrimSpace(hdr.Get("retry-after-ms")); msStr != "" {
			if ms, err := strconv.ParseInt(msStr, 10, 64); err == nil && ms >= 0 {
				d := time.Duration(ms) * time.Millisecond
				if c.retryMaxDelay > 0 && d > c.retryMaxDelay {
					return c.retryMaxDelay
				}
				return d
			}
		}
		if secStr := strings.TrimSpace(hdr.Get("Retry-After")); secStr != "" {
			if secs, err := strconv.ParseFloat(secStr, 64); err == nil && secs >= 0 {
				d := time.Duration(secs * float64(time.Second))
				if c.retryMaxDelay > 0 && d > c.retryMaxDelay {
					return c.retryMaxDelay
				}
				return d
			}
		}
	}
	delay := c.retryBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if c.retryMaxDelay > 0 && delay >= c.retryMaxDelay {
			return c.retryMaxDelay
		}
	}
	if c.retryMaxDelay > 0 && delay > c.retryMaxDelay {
		return c.retryMaxDelay
	}
	return delay
}
