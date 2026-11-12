package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
)

// OpenAIClient implements ModelClient using the OpenAI Responses API.
//
//nolint:govet
type OpenAIClient struct {
	clock             clock.Clock
	httpClient        *http.Client
	pricing           map[string]ModelPricing
	totalUsage        Usage
	baseURL           string
	apiKey            string
	maxRetries        int
	retryBaseDelay    time.Duration
	retryMaxDelay     time.Duration
	requestTimeout    time.Duration
	streamIdleTimeout time.Duration
	mu                sync.Mutex
}

type OpenAIClientOption func(*OpenAIClient)

func WithOpenAIBaseURL(url string) OpenAIClientOption {
	return func(c *OpenAIClient) { c.baseURL = strings.TrimSpace(url) }
}
func WithOpenAIAPIKey(key string) OpenAIClientOption { return func(c *OpenAIClient) { c.apiKey = key } }
func WithOpenAIHTTPClient(h *http.Client) OpenAIClientOption {
	return func(c *OpenAIClient) {
		if h != nil {
			c.httpClient = h
		}
	}
}
func WithOpenAIMaxRetries(n int) OpenAIClientOption {
	return func(c *OpenAIClient) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}
func WithOpenAIRetryBackoff(base, maxDelay time.Duration) OpenAIClientOption {
	return func(c *OpenAIClient) { c.retryBaseDelay, c.retryMaxDelay = max(base, 0), max(maxDelay, base) }
}
func WithOpenAIClock(clk clock.Clock) OpenAIClientOption {
	return func(c *OpenAIClient) {
		if clk != nil {
			c.clock = clk
		}
	}
}
func WithOpenAIPricing(p map[string]ModelPricing) OpenAIClientOption {
	return func(c *OpenAIClient) { c.pricing = p }
}
func WithOpenAIRequestTimeout(d time.Duration) OpenAIClientOption {
	return func(c *OpenAIClient) { c.requestTimeout = d }
}
func WithOpenAIStreamIdleTimeout(d time.Duration) OpenAIClientOption {
	return func(c *OpenAIClient) { c.streamIdleTimeout = d }
}

func NewOpenAIClient(opts ...OpenAIClientOption) *OpenAIClient {
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("LLM_GATEWAY_URL"))
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
		if strings.HasSuffix(baseURL, "/anthropic") {
			baseURL = strings.TrimSuffix(baseURL, "/anthropic") + "/openai"
		}
	}
	c := &OpenAIClient{
		clock: clock.NewRealClock(), httpClient: &http.Client{Timeout: DefaultHTTPTimeout},
		baseURL: baseURL, apiKey: strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		maxRetries: DefaultMaxRetries, retryBaseDelay: DefaultRetryBaseDelay, retryMaxDelay: DefaultRetryMaxDelay,
		requestTimeout: 120 * time.Second, streamIdleTimeout: 45 * time.Second,
		pricing: defaultOpenAIPricing(),
	}
	if c.apiKey == "" {
		c.apiKey = strings.TrimSpace(os.Getenv("NEBULA_LLM_KEY"))
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.baseURL == "" {
		c.baseURL = "https://api.openai.com"
	}
	return c
}

func defaultOpenAIPricing() map[string]ModelPricing {
	return map[string]ModelPricing{"gpt-6.1-sol": {InputPerMTok: 0, OutputPerMTok: 0}, "gpt-6-sol": {InputPerMTok: 0, OutputPerMTok: 0}}
}
func (c *OpenAIClient) TotalUsage() Usage { c.mu.Lock(); defer c.mu.Unlock(); return c.totalUsage }
func (c *OpenAIClient) ResetUsage()       { c.mu.Lock(); defer c.mu.Unlock(); c.totalUsage = Usage{} }
func (c *OpenAIClient) RunToolLoop(ctx context.Context, req Request, reg *ToolRegistry, opts ...ToolLoopOption) (*ToolLoopResult, error) {
	return RunToolLoop(ctx, c, req, reg, opts...)
}

//nolint:govet
type responsesRequest struct {
	Store           *bool           `json:"store"`
	Stream          *bool           `json:"stream"`
	Input           []responsesItem `json:"input"`
	Tools           []responsesTool `json:"tools,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
}
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

//nolint:govet
type responsesItem struct {
	Type      string  `json:"type"`
	Role      string  `json:"role,omitempty"`
	Content   any     `json:"content,omitempty"`
	ID        string  `json:"id,omitempty"`
	CallID    string  `json:"call_id,omitempty"`
	Name      string  `json:"name,omitempty"`
	Arguments string  `json:"arguments,omitempty"`
	Output    *string `json:"output,omitempty"`
}

func (c *OpenAIClient) CreateMessage(ctx context.Context, req Request) (*Response, error) {
	p := responsesRequest{Model: req.Model, Instructions: req.System, MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature}
	store := false
	p.Store = &store
	stream := true
	p.Stream = &stream
	if p.MaxOutputTokens <= 0 {
		p.MaxOutputTokens = DefaultMaxTokens
	}
	for _, m := range req.Messages {
		p.Input = append(p.Input, openAIInput(m)...)
	}
	for _, t := range req.Tools {
		p.Tools = append(p.Tools, responsesTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("orch/model: marshal OpenAI request: %w", err)
	}
	endpoint := strings.TrimRight(c.baseURL, "/")
	if !strings.HasSuffix(endpoint, "/v1/responses") {
		if strings.HasSuffix(endpoint, "/v1") {
			endpoint += "/responses"
		} else {
			endpoint += "/v1/responses"
		}
	}
	for attempt := 1; attempt <= c.maxRetries+1; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if c.requestTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, c.requestTimeout)
		}
		hreq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		hresp, err := c.httpClient.Do(hreq)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt <= c.maxRetries {
				log.Printf("orch/model: OpenAI request stalled model=%s attempt=%d: %v", req.Model, attempt, err)
				if err := c.wait(ctx, attempt); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("orch/model: OpenAI request: %w", err)
		}
		var data []byte
		var readErr error
		if hresp.StatusCode >= 200 && hresp.StatusCode < 300 && strings.Contains(hresp.Header.Get("Content-Type"), "text/event-stream") {
			data, readErr = readResponsesStream(hresp.Body, c.streamIdleTimeout)
		} else {
			data, readErr = io.ReadAll(hresp.Body)
		}
		hresp.Body.Close()
		cancel()
		if readErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt <= c.maxRetries {
				log.Printf("orch/model: OpenAI stream stalled model=%s attempt=%d: %v", req.Model, attempt, readErr)
				if err := c.wait(ctx, attempt); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("orch/model: OpenAI stream failed after %d attempts: %w", attempt, readErr)
		}
		if hresp.StatusCode >= 200 && hresp.StatusCode < 300 {
			resp, err := decodeResponses(data, req.Model)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			resp.Usage.CostUSD = CalculateCost(resp.Model, resp.Usage, c.pricing)
			c.totalUsage = c.totalUsage.Add(resp.Usage)
			c.mu.Unlock()
			return resp, nil
		}
		apiErr := parseAPIError(hresp.StatusCode, hresp.Header, data, attempt)
		if apiErr.Retryable() && attempt <= c.maxRetries {
			if err := c.wait(ctx, attempt); err != nil {
				return nil, err
			}
			continue
		}
		return nil, apiErr
	}
	return nil, fmt.Errorf("orch/model: OpenAI retry loop exhausted")
}

func readResponsesStream(r io.ReadCloser, idle time.Duration) ([]byte, error) {
	//nolint:govet
	type result struct {
		data []byte
		err  error
	}
	results := make(chan result, 1)
	go func() {
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for s.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.Text()), "data:"))
			if line == "" || line == "[DONE]" {
				continue
			}
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal([]byte(line), &event) == nil && (event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete") && len(event.Response) > 0 {
				results <- result{data: event.Response}
				return
			}
		}
		if err := s.Err(); err != nil {
			results <- result{err: err}
			return
		}
		results <- result{err: fmt.Errorf("orch/model: OpenAI stream ended without response.completed")}
	}()
	if idle <= 0 {
		v := <-results
		return v.data, v.err
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	select {
	case v := <-results:
		return v.data, v.err
	case <-timer.C:
		_ = r.Close()
		return nil, fmt.Errorf("orch/model: OpenAI stream idle timeout after %s", idle)
	}
}

func openAIInput(m Message) []responsesItem {
	var out []responsesItem
	var text strings.Builder
	for i := range m.Content {
		b := m.Content[i]
		if b.Type == BlockTypeText {
			text.WriteString(b.Text)
		}
	}
	if text.Len() > 0 {
		out = append(out, responsesItem{Type: "message", Role: string(m.Role), Content: text.String()})
	}
	for i := range m.Content {
		b := m.Content[i]
		switch b.Type {
		case BlockTypeToolUse:
			id := b.Signature
			if id == "" {
				id = b.ID
			}
			out = append(out, responsesItem{Type: "function_call", ID: id, CallID: b.ID, Name: b.Name, Arguments: string(b.Input)})
		case BlockTypeToolResult:
			output := b.Content
			out = append(out, responsesItem{Type: "function_call_output", CallID: b.ToolUseID, Output: &output})
		}
	}
	return out
}

func decodeResponses(data []byte, requestedModel string) (*Response, error) {
	if bytes.Contains(data, []byte("data:")) {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "" || line == "[DONE]" {
				continue
			}
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal([]byte(line), &event) == nil && event.Type == "response.completed" && len(event.Response) > 0 {
				data = event.Response
				break
			}
		}
	}
	var raw struct {
		ID, Model, Status string
		Error             *struct {
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Text      string `json:"text"`
			Content   []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("orch/model: decode OpenAI response: %w", err)
	}
	if raw.Status == "failed" {
		if raw.Error != nil && raw.Error.Message != "" {
			return nil, fmt.Errorf("orch/model: OpenAI response failed: %s", raw.Error.Message)
		}
		return nil, fmt.Errorf("orch/model: OpenAI response failed")
	}
	if raw.Status == "incomplete" {
		reason := "incomplete"
		if raw.IncompleteDetails != nil && raw.IncompleteDetails.Reason != "" {
			reason = raw.IncompleteDetails.Reason
		}
		return nil, fmt.Errorf("orch/model: OpenAI response incomplete: %s", reason)
	}
	var blocks []ContentBlock
	stop := StopReasonEndTurn
	for _, item := range raw.Output {
		switch item.Type {
		case "message":
			if item.Text != "" {
				blocks = append(blocks, NewTextBlock(item.Text))
			}
			for _, c := range item.Content {
				blocks = append(blocks, NewTextBlock(c.Text))
			}
		case "function_call":
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			block := NewToolUseBlock(item.CallID, item.Name, json.RawMessage(args))
			block.Signature = item.ID
			blocks = append(blocks, block)
			stop = StopReasonToolUse
		}
	}
	if raw.Model == "" {
		raw.Model = requestedModel
	}
	return &Response{ID: raw.ID, Model: raw.Model, StopReason: stop, Message: NewAssistantMessage(blocks...), Usage: Usage{InputTokens: raw.Usage.InputTokens, OutputTokens: raw.Usage.OutputTokens}}, nil
}
func (c *OpenAIClient) wait(ctx context.Context, attempt int) error {
	d := c.retryBaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= c.retryMaxDelay {
			d = c.retryMaxDelay
			break
		}
	}
	if d <= 0 {
		return nil
	}
	timer := c.clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}
