package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
)

// Default HTTP client configuration constants for the Anthropic Messages API.
const (
	DefaultBaseURL           = "https://api.anthropic.com"
	DefaultAnthropicVersion  = "2023-06-01"
	DefaultMaxRetries        = 3
	DefaultRetryBaseDelay    = 200 * time.Millisecond
	DefaultRetryMaxDelay     = 5 * time.Second
	DefaultRetryAfterCeiling = 2 * time.Minute
	DefaultRetryTotalCeiling = 5 * time.Minute
	DefaultHTTPTimeout       = 120 * time.Second
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
	parts = append(parts, fmt.Sprintf("orca/model: messages API status %d", e.StatusCode))
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
}

// ClientOption configures an HTTPClient.
type ClientOption func(*HTTPClient)

// WithBaseURL sets the Messages API base URL.
func WithBaseURL(baseURL string) ClientOption {
	return func(c *HTTPClient) {
		c.baseURL = strings.TrimSpace(baseURL)
	}
}

// WithAPIKey sets the x-api-key header credential.
func WithAPIKey(apiKey string) ClientOption {
	return func(c *HTTPClient) {
		c.apiKey = apiKey
	}
}

// WithAuthToken sets the Authorization: Bearer <token> header credential.
func WithAuthToken(token string) ClientOption {
	return func(c *HTTPClient) {
		c.authToken = token
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
		maps.Copy(c.headers, headers)
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

// ParseCustomHeaders parses newline-separated "Header: Value" pairs (ANTHROPIC_CUSTOM_HEADERS format).
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
		pricing:          defaultPricing,
		baseURL:          strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
		apiKey:           os.Getenv("ANTHROPIC_API_KEY"),
		authToken:        os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		anthropicVersion: DefaultAnthropicVersion,
		maxRetries:       DefaultMaxRetries,
		retryBaseDelay:   DefaultRetryBaseDelay,
		retryMaxDelay:    DefaultRetryMaxDelay,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
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
	OutputConfig  *reasoningConfig  `json:"output_config,omitempty"`
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
		return runToolLoopResponse(ctx, c, req)
	}
	codec, err := newToolNameCodec(req, 128)
	if err != nil {
		return nil, err
	}
	wireReq := req.Clone()
	for i := range wireReq.Tools {
		wireReq.Tools[i].Name = codec.wire(wireReq.Tools[i].Name)
	}
	for i := range wireReq.Messages {
		for j := range wireReq.Messages[i].Content {
			if wireReq.Messages[i].Content[j].Type == BlockTypeToolUse {
				wireReq.Messages[i].Content[j].Name = codec.wire(wireReq.Messages[i].Content[j].Name)
			}
		}
	}

	maxTokens := wireReq.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	payload := wireRequest{
		Model:         wireReq.Model,
		System:        wireReq.System,
		Messages:      wireReq.Messages,
		Tools:         wireReq.Tools,
		StopSequences: wireReq.StopSequences,
		MaxTokens:     maxTokens,
		Temperature:   wireReq.Temperature,
		Metadata:      maps.Clone(wireReq.Metadata),
	}
	delete(payload.Metadata, "role")
	delete(payload.Metadata, "provider")
	if wireReq.ReasoningEffort != "" {
		payload.OutputConfig = &reasoningConfig{Effort: wireReq.ReasoningEffort}
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("orca/model: marshal request: %w", err)
	}

	endpoint := messagesEndpointURL(c.baseURL)
	maxAttempts := c.maxRetries + 1
	retryStart := c.clock.Now()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("orca/model: build http request: %w", err)
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
				if waitErr := c.sleepRetry(ctx, attempt, nil, 0, retryStart); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, fmt.Errorf("orca/model: http request failed after %d attempts: %w", attempt, err)
		}

		respBytes, readErr := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		if readErr != nil {
			if attempt < maxAttempts {
				if waitErr := c.sleepRetry(ctx, attempt, httpResp.Header, httpResp.StatusCode, retryStart); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, fmt.Errorf("orca/model: read response body: %w", readErr)
		}

		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			var resp Response
			if err := json.Unmarshal(respBytes, &resp); err != nil {
				return nil, fmt.Errorf("orca/model: decode response: %w", err)
			}
			for i := range resp.Message.Content {
				if resp.Message.Content[i].Type == BlockTypeToolUse {
					resp.Message.Content[i].Name = codec.original(resp.Message.Content[i].Name)
				}
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
			if c.clock.Now().Sub(retryStart) >= DefaultRetryTotalCeiling {
				return nil, apiErr
			}
			if waitErr := c.sleepRetry(ctx, attempt, httpResp.Header, httpResp.StatusCode, retryStart); waitErr != nil {
				return nil, waitErr
			}
			if c.clock.Now().Sub(retryStart) >= DefaultRetryTotalCeiling {
				return nil, apiErr
			}
			continue
		}
		return nil, apiErr
	}

	return nil, errors.New("orca/model: unreachable retry loop exit")
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

func (c *HTTPClient) sleepRetry(ctx context.Context, attempt int, hdr http.Header, status int, retryStart time.Time) error {
	delay := c.retryDelay(attempt, hdr)
	remaining := DefaultRetryTotalCeiling - c.clock.Now().Sub(retryStart)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	if delay > remaining {
		delay = remaining
	}
	if delay > 0 {
		log.Printf("orca/model: anthropic retry wait status=%d wait=%s attempt=%d", status, delay, attempt)
	}
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
	delay, ok := parseRetryAfter(hdr)
	if !ok {
		delay = c.retryBaseDelay
		for i := 1; i < attempt; i++ {
			if c.retryMaxDelay > 0 && delay >= c.retryMaxDelay {
				return c.retryMaxDelay
			}
			delay *= 2
		}
	}
	if _, ok := parseRetryAfter(hdr); ok {
		if delay > DefaultRetryAfterCeiling {
			delay = DefaultRetryAfterCeiling
		}
		return delay
	}
	if c.retryMaxDelay > 0 && delay > c.retryMaxDelay {
		return c.retryMaxDelay
	}
	return delay
}

func parseRetryAfter(hdr http.Header) (time.Duration, bool) {
	if hdr == nil {
		return 0, false
	}
	if msStr := strings.TrimSpace(hdr.Get("retry-after-ms")); msStr != "" {
		if ms, err := strconv.ParseFloat(msStr, 64); err == nil && ms >= 0 && !math.IsInf(ms, 0) && !math.IsNaN(ms) {
			if ms > float64(DefaultRetryAfterCeiling/time.Millisecond) {
				ms = float64(DefaultRetryAfterCeiling / time.Millisecond)
			}
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	if secStr := strings.TrimSpace(hdr.Get("Retry-After")); secStr != "" {
		if secs, err := strconv.ParseFloat(secStr, 64); err == nil && secs >= 0 && !math.IsInf(secs, 0) && !math.IsNaN(secs) {
			if secs > DefaultRetryAfterCeiling.Seconds() {
				secs = DefaultRetryAfterCeiling.Seconds()
			}
			return time.Duration(secs * float64(time.Second)), true
		}
		if when, err := http.ParseTime(secStr); err == nil {
			d := time.Until(when)
			if d < 0 {
				d = 0
			}
			if d > DefaultRetryAfterCeiling {
				d = DefaultRetryAfterCeiling
			}
			return d, true
		}
	}
	return 0, false
}
