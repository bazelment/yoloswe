// Package model defines the Messages API client interface, message/tool types,
// token usage accounting, and a deterministic record/replay fake for tests.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Role identifies the speaker of a Message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// StopReason explains why the model ended its turn.
type StopReason string

const (
	StopReasonEndTurn      StopReason = "end_turn"
	StopReasonToolUse      StopReason = "tool_use"
	StopReasonMaxTokens    StopReason = "max_tokens"
	StopReasonStopSequence StopReason = "stop_sequence"
)

// BlockType identifies the kind of a ContentBlock.
type BlockType string

const (
	BlockTypeText       BlockType = "text"
	BlockTypeThinking   BlockType = "thinking"
	BlockTypeToolUse    BlockType = "tool_use"
	BlockTypeToolResult BlockType = "tool_result"
)

// ContentBlock is one part of a Message (text, thinking, tool_use, or tool_result).
type ContentBlock struct {
	Type      BlockType       `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (b ContentBlock) Clone() ContentBlock {
	out := b
	if b.Input != nil {
		out.Input = slices.Clone(b.Input)
	}
	return out
}

func NewTextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockTypeText, Text: text}
}

func NewThinkingBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockTypeThinking, Text: text}
}

func NewToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{
		Type:  BlockTypeToolUse,
		ID:    id,
		Name:  name,
		Input: slices.Clone(input),
	}
}

func NewToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{
		Type:      BlockTypeToolResult,
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}
}

// ToolCall represents a single tool invocation extracted from an assistant message.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult represents the output of executing a ToolCall.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message is one conversational turn in a Request or Response.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

func (m Message) Clone() Message {
	out := Message{Role: m.Role}
	if m.Content != nil {
		out.Content = make([]ContentBlock, len(m.Content))
		for i := range m.Content {
			out.Content[i] = m.Content[i].Clone()
		}
	}
	return out
}

func NewUserMessage(text string) Message {
	return Message{
		Role:    RoleUser,
		Content: []ContentBlock{NewTextBlock(text)},
	}
}

func NewAssistantMessage(blocks ...ContentBlock) Message {
	return Message{
		Role:    RoleAssistant,
		Content: slices.Clone(blocks),
	}
}

func NewToolResultMessage(results ...ContentBlock) Message {
	return Message{
		Role:    RoleUser,
		Content: slices.Clone(results),
	}
}

// Text concatenates all text blocks in m.
func (m Message) Text() string {
	var b strings.Builder
	for i := range m.Content {
		if m.Content[i].Type == BlockTypeText {
			b.WriteString(m.Content[i].Text)
		}
	}
	return b.String()
}

// ToolCalls extracts all tool_use blocks from m.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for i := range m.Content {
		b := &m.Content[i]
		if b.Type == BlockTypeToolUse {
			calls = append(calls, ToolCall{
				ID:    b.ID,
				Name:  b.Name,
				Input: slices.Clone(b.Input),
			})
		}
	}
	return calls
}

// ToolDefinition describes a tool exposed to the model.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (d ToolDefinition) Clone() ToolDefinition {
	return ToolDefinition{
		Name:        d.Name,
		Description: d.Description,
		InputSchema: slices.Clone(d.InputSchema),
	}
}

// Usage tracks token consumption and cost for a model call or turn.
type Usage struct {
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens,omitempty"`
	CostUSD                  float64 `json:"cost_usd,omitempty"`
}

// TotalTokens returns the sum of input (including cache) and output tokens.
func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:              u.InputTokens + other.InputTokens,
		OutputTokens:             u.OutputTokens + other.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens + other.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens + other.CacheReadInputTokens,
		CostUSD:                  u.CostUSD + other.CostUSD,
	}
}

// Request is a Messages API call payload.
type Request struct {
	Metadata      map[string]string `json:"metadata,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	Model         string            `json:"model"`
	System        string            `json:"system,omitempty"`
	Messages      []Message         `json:"messages"`
	Tools         []ToolDefinition  `json:"tools,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	MaxTokens     int               `json:"max_tokens"`
}

func (r Request) Clone() Request {
	out := r
	if r.Metadata != nil {
		out.Metadata = maps.Clone(r.Metadata)
	}
	if r.Temperature != nil {
		v := *r.Temperature
		out.Temperature = &v
	}
	if r.Messages != nil {
		out.Messages = make([]Message, len(r.Messages))
		for i := range r.Messages {
			out.Messages[i] = r.Messages[i].Clone()
		}
	}
	if r.Tools != nil {
		out.Tools = make([]ToolDefinition, len(r.Tools))
		for i := range r.Tools {
			out.Tools[i] = r.Tools[i].Clone()
		}
	}
	out.StopSequences = slices.Clone(r.StopSequences)
	return out
}

// Response is a Messages API completion response.
type Response struct {
	ID         string     `json:"id"`
	Model      string     `json:"model"`
	StopReason StopReason `json:"stop_reason"`
	Message    Message    `json:"message"`
	Usage      Usage      `json:"usage"`
}

func (r Response) Clone() Response {
	out := r
	out.Message = r.Message.Clone()
	return out
}

func (r Response) Text() string {
	return r.Message.Text()
}

func (r Response) ToolCalls() []ToolCall {
	return r.Message.ToolCalls()
}

// ModelClient invokes the Messages API.
type ModelClient interface {
	CreateMessage(ctx context.Context, req Request) (*Response, error)
}

type Client = ModelClient

// ToolRegistry holds typed tool definitions and handlers for an agent role.
type ToolRegistry struct {
	entries []toolEntry
}

type toolEntry struct {
	handler func(context.Context, json.RawMessage) (string, error)
	def     ToolDefinition
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{}
}

// Register adds a tool with an explicit ToolDefinition and raw JSON handler.
func (r *ToolRegistry) Register(def ToolDefinition, handler func(context.Context, json.RawMessage) (string, error)) *ToolRegistry {
	r.entries = append(r.entries, toolEntry{
		def:     def.Clone(),
		handler: handler,
	})
	return r
}

// AddTool registers a type-safe tool handler whose JSON input schema is derived
// from the struct type T.
func AddTool[T any](
	reg *ToolRegistry,
	name, description string,
	handler func(context.Context, T) (string, error),
) *ToolRegistry {
	schema := SchemaFor[T]()
	reg.Register(ToolDefinition{
		Name:        name,
		Description: description,
		InputSchema: schema,
	}, func(ctx context.Context, raw json.RawMessage) (string, error) {
		var params T
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &params); err != nil {
				return "", fmt.Errorf("invalid arguments for tool %s: %w", name, err)
			}
		}
		return handler(ctx, params)
	})
	return reg
}

// Definitions returns the registered tool definitions in registration order.
func (r *ToolRegistry) Definitions() []ToolDefinition {
	out := make([]ToolDefinition, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.def.Clone()
	}
	return out
}

// Dispatch invokes the handler matching call.Name and wraps its output in a
// tool_result ContentBlock.
func (r *ToolRegistry) Dispatch(ctx context.Context, call ToolCall) ContentBlock {
	for _, e := range r.entries {
		if e.def.Name == call.Name {
			res, err := e.handler(ctx, call.Input)
			if err != nil {
				return NewToolResultBlock(call.ID, err.Error(), true)
			}
			return NewToolResultBlock(call.ID, res, false)
		}
	}
	return NewToolResultBlock(call.ID, fmt.Sprintf("unknown tool: %s", call.Name), true)
}

// SchemaFor generates a JSON Schema object for struct type T from json and
// jsonschema struct tags.
func SchemaFor[T any]() json.RawMessage {
	var zero T
	schema := reflectSchema(reflect.TypeOf(zero))
	b, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("orch/model: failed to marshal schema for %T: %v", zero, err))
	}
	return json.RawMessage(b)
}

func reflectSchema(t reflect.Type) map[string]any {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return map[string]any{"type": "object"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{
			"type":  "array",
			"items": reflectSchema(t.Elem()),
		}
	case reflect.Struct:
		props := make(map[string]any)
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			jsonTag := f.Tag.Get("json")
			if jsonTag == "-" {
				continue
			}
			name := f.Name
			omitempty := false
			if jsonTag != "" {
				parts := strings.Split(jsonTag, ",")
				if parts[0] != "" {
					name = parts[0]
				}
				for _, opt := range parts[1:] {
					if opt == "omitempty" {
						omitempty = true
					}
				}
			}
			prop := reflectSchema(f.Type)
			isReq := !omitempty
			if jsTag := f.Tag.Get("jsonschema"); jsTag != "" {
				for _, part := range strings.Split(jsTag, ",") {
					part = strings.TrimSpace(part)
					switch {
					case part == "required":
						isReq = true
					case strings.HasPrefix(part, "description="):
						prop["description"] = strings.TrimPrefix(part, "description=")
					}
				}
			}
			props[name] = prop
			if isReq {
				required = append(required, name)
			}
		}
		res := map[string]any{
			"type":       "object",
			"properties": props,
		}
		if len(required) > 0 {
			res["required"] = required
		}
		return res
	default:
		return map[string]any{"type": "object"}
	}
}

type scriptedCall struct {
	err  error
	resp Response
}

// FakeClient is a deterministic record/replay ModelClient for tests.
type FakeClient struct {
	Requests []Request
	queue    []scriptedCall
	mu       sync.Mutex
}

// NewFakeClient creates a FakeClient queued with the given responses.
func NewFakeClient(responses ...Response) *FakeClient {
	queue := make([]scriptedCall, len(responses))
	for i := range responses {
		queue[i] = scriptedCall{resp: responses[i].Clone()}
	}
	return &FakeClient{queue: queue}
}

// Enqueue appends a scripted response (or error) to the fake client's queue.
func (f *FakeClient) Enqueue(resp Response, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, scriptedCall{resp: resp.Clone(), err: err})
}

// CreateMessage records req and returns the next queued response.
func (f *FakeClient) CreateMessage(_ context.Context, req Request) (*Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, req.Clone())
	if len(f.queue) == 0 {
		return nil, fmt.Errorf("orch/model: FakeClient has no more scripted responses (call #%d)", len(f.Requests))
	}
	next := f.queue[0]
	f.queue = f.queue[1:]
	if next.err != nil {
		return nil, next.err
	}
	cp := next.resp.Clone()
	return &cp, nil
}
