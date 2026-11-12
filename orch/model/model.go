// Package model defines the Messages API client interface, message/tool types,
// token usage accounting, and a deterministic record/replay fake for tests.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Default limits for Messages API requests and tool loops.
const (
	DefaultMaxTokens = 4096
	DefaultMaxTurns  = 10
)

// ErrMaxTurnsExceeded is returned when a tool_use loop reaches MaxTurns while
// the model is still requesting tool calls.
var ErrMaxTurnsExceeded = errors.New("orch/model: tool loop exceeded max turns")

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
	Signature string          `json:"signature,omitempty"`
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

// MarshalJSON serializes ContentBlock using the wire shape required by the
// Anthropic Messages API for each block type.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case BlockTypeText:
		return json.Marshal(struct {
			Type BlockType `json:"type"`
			Text string    `json:"text"`
		}{
			Type: BlockTypeText,
			Text: b.Text,
		})
	case BlockTypeThinking:
		return json.Marshal(struct {
			Type      BlockType `json:"type"`
			Thinking  string    `json:"thinking"`
			Signature string    `json:"signature,omitempty"`
		}{
			Type:      BlockTypeThinking,
			Thinking:  b.Text,
			Signature: b.Signature,
		})
	case BlockTypeToolUse:
		input := b.Input
		if len(bytes.TrimSpace(input)) == 0 {
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  BlockType       `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{
			Type:  BlockTypeToolUse,
			ID:    b.ID,
			Name:  b.Name,
			Input: input,
		})
	case BlockTypeToolResult:
		return json.Marshal(struct {
			Type      BlockType `json:"type"`
			ToolUseID string    `json:"tool_use_id"`
			Content   string    `json:"content,omitempty"`
			IsError   bool      `json:"is_error,omitempty"`
		}{
			Type:      BlockTypeToolResult,
			ToolUseID: b.ToolUseID,
			Content:   b.Content,
			IsError:   b.IsError,
		})
	default:
		type plainBlock ContentBlock
		return json.Marshal(plainBlock(b))
	}
}

// UnmarshalJSON decodes ContentBlock from either Anthropic wire JSON or internal JSON.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type      BlockType       `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		Input     json.RawMessage `json:"input"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.Type = raw.Type
	b.Text = raw.Text
	if b.Type == BlockTypeThinking && b.Text == "" && raw.Thinking != "" {
		b.Text = raw.Thinking
	}
	b.Signature = raw.Signature
	b.ID = raw.ID
	b.Name = raw.Name
	b.ToolUseID = raw.ToolUseID
	b.Input = raw.Input
	b.IsError = raw.IsError

	if len(raw.Content) > 0 && string(raw.Content) != "null" {
		var s string
		if err := json.Unmarshal(raw.Content, &s); err == nil {
			b.Content = s
		} else {
			var subBlocks []ContentBlock
			if err := json.Unmarshal(raw.Content, &subBlocks); err == nil {
				var sb strings.Builder
				for i := range subBlocks {
					sb.WriteString(subBlocks[i].Text)
				}
				b.Content = sb.String()
			} else {
				b.Content = string(raw.Content)
			}
		}
	}
	return nil
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

// Block converts r into a tool_result ContentBlock.
func (r ToolResult) Block() ContentBlock {
	return NewToolResultBlock(r.ToolUseID, r.Content, r.IsError)
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

// UnmarshalJSON decodes a Message whose content is either a []ContentBlock array
// or a shorthand string.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    Role            `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	trimmed := bytes.TrimSpace(raw.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		m.Content = nil
		return nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		m.Content = []ContentBlock{NewTextBlock(text)}
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return err
	}
	m.Content = blocks
	return nil
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

// Thinking concatenates all thinking blocks in m.
func (m Message) Thinking() string {
	var b strings.Builder
	for i := range m.Content {
		if m.Content[i].Type == BlockTypeThinking {
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

// ModelPricing defines USD price per 1M tokens for each token class.
type ModelPricing struct {
	InputPerMTok         float64 `json:"input_per_mtok"`
	OutputPerMTok        float64 `json:"output_per_mtok"`
	CacheReadPerMTok     float64 `json:"cache_read_per_mtok"`
	CacheCreationPerMTok float64 `json:"cache_creation_per_mtok"`
}

// DefaultPricing returns built-in USD per 1M token rates for Anthropic models.
func DefaultPricing() map[string]ModelPricing {
	opus := ModelPricing{
		InputPerMTok:         15.0,
		OutputPerMTok:        75.0,
		CacheReadPerMTok:     1.5,
		CacheCreationPerMTok: 18.75,
	}
	sonnet := ModelPricing{
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheReadPerMTok:     0.3,
		CacheCreationPerMTok: 3.75,
	}
	haiku := ModelPricing{
		InputPerMTok:         0.8,
		OutputPerMTok:        4.0,
		CacheReadPerMTok:     0.08,
		CacheCreationPerMTok: 1.0,
	}
	return map[string]ModelPricing{
		"opus":                       opus,
		"claude-opus-4-7":            opus,
		"claude-opus-4-6":            opus,
		"claude-opus-4-5":            opus,
		"claude-opus-4-1":            opus,
		"claude-opus-4-20250514":     opus,
		"sonnet":                     sonnet,
		"claude-sonnet-4-6":          sonnet,
		"claude-sonnet-4-5":          sonnet,
		"claude-sonnet-4-5-20250929": sonnet,
		"claude-sonnet-4-20250514":   sonnet,
		"haiku":                      haiku,
		"claude-haiku-4-5":           haiku,
		"claude-haiku-4-5-20251001":  haiku,
		"claude-3-5-haiku-20241022":  haiku,
	}
}

// CalculateCost computes the USD cost for usage u on modelName using the
// provided pricing table (falling back to DefaultPricing if pricing is nil).
func CalculateCost(modelName string, u Usage, pricing map[string]ModelPricing) float64 {
	if pricing == nil {
		pricing = DefaultPricing()
	}
	rate, ok := lookupModelPricing(modelName, pricing)
	if !ok {
		return 0
	}
	cost := float64(max(u.InputTokens, 0)) / 1_000_000.0 * rate.InputPerMTok
	cost += float64(max(u.OutputTokens, 0)) / 1_000_000.0 * rate.OutputPerMTok
	cost += float64(max(u.CacheReadInputTokens, 0)) / 1_000_000.0 * rate.CacheReadPerMTok
	cost += float64(max(u.CacheCreationInputTokens, 0)) / 1_000_000.0 * rate.CacheCreationPerMTok
	return cost
}

func lookupModelPricing(modelName string, pricing map[string]ModelPricing) (ModelPricing, bool) {
	norm := strings.ToLower(strings.TrimSpace(modelName))
	if rate, ok := pricing[norm]; ok {
		return rate, true
	}
	switch {
	case strings.Contains(norm, "opus"):
		if rate, ok := pricing["opus"]; ok {
			return rate, true
		}
		if rate, ok := pricing["claude-opus-4-7"]; ok {
			return rate, true
		}
	case strings.Contains(norm, "sonnet"):
		if rate, ok := pricing["sonnet"]; ok {
			return rate, true
		}
		if rate, ok := pricing["claude-sonnet-4-6"]; ok {
			return rate, true
		}
	case strings.Contains(norm, "haiku"):
		if rate, ok := pricing["haiku"]; ok {
			return rate, true
		}
		if rate, ok := pricing["claude-haiku-4-5-20251001"]; ok {
			return rate, true
		}
	}
	return ModelPricing{}, false
}

// Request is a Messages API call payload.
type Request struct {
	Metadata      map[string]string `json:"metadata,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	ToolRegistry  *ToolRegistry     `json:"-"`
	Model         string            `json:"model"`
	System        string            `json:"system,omitempty"`
	Messages      []Message         `json:"messages"`
	Tools         []ToolDefinition  `json:"tools,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	MaxTokens     int               `json:"max_tokens"`
	MaxTurns      int               `json:"-"`
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
	Transcript []Message  `json:"transcript,omitempty"`
	Usage      Usage      `json:"usage"`
	Turns      int        `json:"turns,omitempty"`
}

func (r Response) Clone() Response {
	out := r
	out.Message = r.Message.Clone()
	if r.Transcript != nil {
		out.Transcript = make([]Message, len(r.Transcript))
		for i := range r.Transcript {
			out.Transcript[i] = r.Transcript[i].Clone()
		}
	}
	return out
}

// UnmarshalJSON supports both the internal Response shape (with a nested
// "message" field) and the raw Anthropic Messages API wire shape (with top-level
// "role" and "content" fields).
func (r *Response) UnmarshalJSON(data []byte) error {
	var raw struct {
		Message    *Message       `json:"message"`
		ID         string         `json:"id"`
		Model      string         `json:"model"`
		Role       Role           `json:"role"`
		StopReason StopReason     `json:"stop_reason"`
		Content    []ContentBlock `json:"content"`
		Transcript []Message      `json:"transcript"`
		Usage      Usage          `json:"usage"`
		Turns      int            `json:"turns"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.ID = raw.ID
	r.Model = raw.Model
	r.StopReason = raw.StopReason
	r.Usage = raw.Usage
	r.Turns = raw.Turns
	r.Transcript = raw.Transcript
	if raw.Message != nil && (raw.Message.Role != "" || len(raw.Message.Content) > 0) {
		r.Message = *raw.Message
		if r.Message.Role == "" {
			r.Message.Role = RoleAssistant
		}
	} else {
		role := raw.Role
		if role == "" {
			role = RoleAssistant
		}
		r.Message = Message{
			Role:    role,
			Content: raw.Content,
		}
	}
	return nil
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
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && string(trimmed) != "null" {
			if err := json.Unmarshal(trimmed, &params); err != nil {
				return "", fmt.Errorf("invalid arguments for tool %s: %w", name, err)
			}
		}
		return handler(ctx, params)
	})
	return reg
}

// AddToolJSON registers a type-safe tool handler whose input schema is derived
// from T and whose return value R is marshaled to JSON (or returned directly if string).
func AddToolJSON[T any, R any](
	reg *ToolRegistry,
	name, description string,
	handler func(context.Context, T) (R, error),
) *ToolRegistry {
	return AddTool[T](reg, name, description, func(ctx context.Context, params T) (string, error) {
		res, err := handler(ctx, params)
		if err != nil {
			return "", err
		}
		if s, ok := any(res).(string); ok {
			return s, nil
		}
		b, err := json.Marshal(res)
		if err != nil {
			return "", fmt.Errorf("marshal result for tool %s: %w", name, err)
		}
		return string(b), nil
	})
}

// Definitions returns the registered tool definitions in registration order.
func (r *ToolRegistry) Definitions() []ToolDefinition {
	if r == nil {
		return nil
	}
	out := make([]ToolDefinition, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.def.Clone()
	}
	return out
}

// Has reports whether a tool with name is registered.
func (r *ToolRegistry) Has(name string) bool {
	if r == nil {
		return false
	}
	for _, e := range r.entries {
		if e.def.Name == name {
			return true
		}
	}
	return false
}

// Names returns the registered tool names in registration order.
func (r *ToolRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, len(r.entries))
	for i, e := range r.entries {
		names[i] = e.def.Name
	}
	return names
}

// Execute invokes the handler matching call.Name and returns a ToolResult.
func (r *ToolRegistry) Execute(ctx context.Context, call ToolCall) ToolResult {
	if r != nil {
		for _, e := range r.entries {
			if e.def.Name == call.Name {
				res, err := e.handler(ctx, call.Input)
				if err != nil {
					return ToolResult{ToolUseID: call.ID, Content: err.Error(), IsError: true}
				}
				return ToolResult{ToolUseID: call.ID, Content: res, IsError: false}
			}
		}
	}
	return ToolResult{
		ToolUseID: call.ID,
		Content:   fmt.Sprintf("unknown tool: %s", call.Name),
		IsError:   true,
	}
}

// Dispatch invokes the handler matching call.Name and wraps its output in a
// tool_result ContentBlock.
func (r *ToolRegistry) Dispatch(ctx context.Context, call ToolCall) ContentBlock {
	return r.Execute(ctx, call).Block()
}

// SchemaFor generates a JSON Schema object for struct type T from json and
// jsonschema struct tags.
func SchemaFor[T any]() json.RawMessage {
	t := reflect.TypeFor[T]()
	schema := reflectSchema(t)
	b, err := json.Marshal(schema)
	if err != nil {
		var zero T
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
	case reflect.Map:
		if t.Key().Kind() == reflect.String {
			return map[string]any{
				"type":                 "object",
				"additionalProperties": reflectSchema(t.Elem()),
			}
		}
		return map[string]any{"type": "object"}
	case reflect.Struct:
		props := make(map[string]any)
		var required []string
		collectStructFields(t, props, &required)
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

func collectStructFields(t reflect.Type, props map[string]any, required *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		if f.Anonymous && jsonTag == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				collectStructFields(ft, props, required)
				continue
			}
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
		if desc := f.Tag.Get("jsonschema_description"); desc != "" {
			prop["description"] = desc
		}
		if jsTag := f.Tag.Get("jsonschema"); jsTag != "" {
			var enums []any
			for _, part := range strings.Split(jsTag, ",") {
				part = strings.TrimSpace(part)
				switch {
				case part == "required":
					isReq = true
				case strings.HasPrefix(part, "description="):
					prop["description"] = strings.TrimPrefix(part, "description=")
				case strings.HasPrefix(part, "enum="):
					enums = append(enums, strings.TrimPrefix(part, "enum="))
				case strings.HasPrefix(part, "default="):
					prop["default"] = strings.TrimPrefix(part, "default=")
				}
			}
			if len(enums) > 0 {
				prop["enum"] = enums
			}
		}
		props[name] = prop
		if isReq {
			*required = append(*required, name)
		}
	}
}

// ToolLoopResult captures the full execution of a bounded tool_use/tool_result loop.
type ToolLoopResult struct {
	Messages  []Message  `json:"messages"`
	Turns     []Response `json:"turns"`
	Response  Response   `json:"response"`
	Usage     Usage      `json:"usage"`
	TurnCount int        `json:"turn_count"`
}

type toolLoopConfig struct {
	onTurn     func(turn int, resp Response)
	onToolCall func(turn int, call ToolCall, result ContentBlock)
	maxTurns   int
}

// ToolLoopOption configures RunToolLoop.
type ToolLoopOption func(*toolLoopConfig)

// WithMaxTurns bounds the number of model calls in RunToolLoop.
func WithMaxTurns(maxTurns int) ToolLoopOption {
	return func(c *toolLoopConfig) {
		c.maxTurns = maxTurns
	}
}

// WithOnTurn registers a callback invoked after each model turn in RunToolLoop.
func WithOnTurn(fn func(turn int, resp Response)) ToolLoopOption {
	return func(c *toolLoopConfig) {
		c.onTurn = fn
	}
}

// WithOnToolCall registers a callback invoked after each tool execution in RunToolLoop.
func WithOnToolCall(fn func(turn int, call ToolCall, result ContentBlock)) ToolLoopOption {
	return func(c *toolLoopConfig) {
		c.onToolCall = fn
	}
}

// RunToolLoop executes a Messages API request and repeatedly dispatches tool_use
// blocks against reg until the model finishes its turn or maxTurns is reached.
func RunToolLoop(
	ctx context.Context,
	client ModelClient,
	req Request,
	reg *ToolRegistry,
	opts ...ToolLoopOption,
) (*ToolLoopResult, error) {
	cfg := toolLoopConfig{
		maxTurns: req.MaxTurns,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.maxTurns <= 0 {
		cfg.maxTurns = DefaultMaxTurns
	}
	if reg == nil {
		reg = req.ToolRegistry
	}

	curReq := req.Clone()
	curReq.ToolRegistry = nil
	curReq.MaxTurns = 0
	if len(curReq.Tools) == 0 && reg != nil {
		curReq.Tools = reg.Definitions()
	}

	messages := make([]Message, len(curReq.Messages))
	for i := range curReq.Messages {
		messages[i] = curReq.Messages[i].Clone()
	}

	var (
		turns      []Response
		totalUsage Usage
	)

	for turn := 1; turn <= cfg.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		curReq.Messages = make([]Message, len(messages))
		for i := range messages {
			curReq.Messages[i] = messages[i].Clone()
		}

		resp, err := client.CreateMessage(ctx, curReq)
		if err != nil {
			return &ToolLoopResult{
				Messages:  messages,
				Turns:     turns,
				Usage:     totalUsage,
				TurnCount: len(turns),
			}, err
		}

		turnResp := resp.Clone()
		turns = append(turns, turnResp)
		totalUsage = totalUsage.Add(turnResp.Usage)

		assistantMsg := turnResp.Message.Clone()
		if assistantMsg.Role == "" {
			assistantMsg.Role = RoleAssistant
		}
		messages = append(messages, assistantMsg)

		if cfg.onTurn != nil {
			cfg.onTurn(turn, turnResp)
		}

		calls := turnResp.ToolCalls()
		if turnResp.StopReason != StopReasonToolUse || len(calls) == 0 {
			finalResp := turnResp.Clone()
			finalResp.Usage = totalUsage
			finalResp.Turns = turn
			finalResp.Transcript = make([]Message, len(messages))
			for i := range messages {
				finalResp.Transcript[i] = messages[i].Clone()
			}
			return &ToolLoopResult{
				Response:  finalResp,
				Messages:  messages,
				Turns:     turns,
				Usage:     totalUsage,
				TurnCount: turn,
			}, nil
		}

		if turn >= cfg.maxTurns {
			finalResp := turnResp.Clone()
			finalResp.Usage = totalUsage
			finalResp.Turns = turn
			finalResp.Transcript = make([]Message, len(messages))
			for i := range messages {
				finalResp.Transcript[i] = messages[i].Clone()
			}
			return &ToolLoopResult{
				Response:  finalResp,
				Messages:  messages,
				Turns:     turns,
				Usage:     totalUsage,
				TurnCount: turn,
			}, fmt.Errorf("%w: stopped after %d turns", ErrMaxTurnsExceeded, turn)
		}

		if reg == nil {
			return nil, fmt.Errorf("orch/model: model returned tool_use but no ToolRegistry was provided")
		}

		results := make([]ContentBlock, 0, len(calls))
		for _, call := range calls {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			resBlock := reg.Dispatch(ctx, call)
			results = append(results, resBlock)
			if cfg.onToolCall != nil {
				cfg.onToolCall(turn, call, resBlock)
			}
		}
		messages = append(messages, NewToolResultMessage(results...))
	}

	return nil, fmt.Errorf("%w: stopped after %d turns", ErrMaxTurnsExceeded, cfg.maxTurns)
}

// RecordedExchange captures a single request and its corresponding response (or error)
// for deterministic record/replay testing.
type RecordedExchange struct {
	Error    string   `json:"error,omitempty"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

func (e RecordedExchange) Clone() RecordedExchange {
	return RecordedExchange{
		Request:  e.Request.Clone(),
		Response: e.Response.Clone(),
		Error:    e.Error,
	}
}

// Recording is the on-disk JSON representation of a sequence of model exchanges.
type Recording struct {
	Exchanges []RecordedExchange `json:"exchanges"`
}

type scriptedCall struct {
	err  error
	req  *Request
	resp Response
}

// FakeClient is a deterministic record/replay ModelClient for tests.
type FakeClient struct {
	upstream    ModelClient
	recordPath  string
	Requests    []Request
	exchanges   []RecordedExchange
	queue       []scriptedCall
	totalUsage  Usage
	mu          sync.Mutex
	strictMatch bool
}

// NewFakeClient creates a FakeClient queued with the given responses.
func NewFakeClient(responses ...Response) *FakeClient {
	queue := make([]scriptedCall, len(responses))
	for i := range responses {
		queue[i] = scriptedCall{resp: responses[i].Clone()}
	}
	return &FakeClient{queue: queue}
}

// NewRecordingFakeClient creates a FakeClient that forwards every call to upstream,
// records each Request+Response pair in memory, and writes the JSON recording to
// path (when path is non-empty).
func NewRecordingFakeClient(path string, upstream ModelClient) *FakeClient {
	return &FakeClient{
		upstream:   upstream,
		recordPath: path,
	}
}

// LoadFakeClient loads recorded request/response exchanges from path and returns
// a FakeClient ready to replay them deterministically.
func LoadFakeClient(path string) (*FakeClient, error) {
	f := &FakeClient{}
	if err := f.Load(path); err != nil {
		return nil, err
	}
	return f, nil
}

// NewRecordOrReplayClient returns a FakeClient in record mode (forwarding to upstream
// and writing path) when record is true, or in deterministic replay mode (reading
// from path with no upstream calls) when record is false.
func NewRecordOrReplayClient(path string, record bool, upstream ModelClient) (*FakeClient, error) {
	if record {
		if upstream == nil {
			return nil, errors.New("orch/model: upstream ModelClient is required when record=true")
		}
		return NewRecordingFakeClient(path, upstream), nil
	}
	return LoadFakeClient(path)
}

// SetStrictMatch configures whether replayed calls verify that the incoming
// Request matches the recorded Request (model, system, messages, and tools).
func (f *FakeClient) SetStrictMatch(strict bool) *FakeClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.strictMatch = strict
	return f
}

// Enqueue appends a scripted response (or error) to the fake client's queue.
func (f *FakeClient) Enqueue(resp Response, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, scriptedCall{resp: resp.Clone(), err: err})
}

// EnqueueExchange appends a recorded request/response exchange to the fake client's queue.
func (f *FakeClient) EnqueueExchange(ex RecordedExchange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reqCopy := ex.Request.Clone()
	var callErr error
	if ex.Error != "" {
		callErr = errors.New(ex.Error)
	}
	f.queue = append(f.queue, scriptedCall{
		req:  &reqCopy,
		resp: ex.Response.Clone(),
		err:  callErr,
	})
}

// Exchanges returns a copy of all recorded request/response exchanges.
func (f *FakeClient) Exchanges() []RecordedExchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedExchange, len(f.exchanges))
	for i := range f.exchanges {
		out[i] = f.exchanges[i].Clone()
	}
	return out
}

// TotalUsage returns the cumulative token usage and cost across all completed calls.
func (f *FakeClient) TotalUsage() Usage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.totalUsage
}

// Save writes the recorded exchanges to path as indented JSON.
func (f *FakeClient) Save(path string) error {
	f.mu.Lock()
	snapshot := make([]RecordedExchange, len(f.exchanges))
	for i := range f.exchanges {
		snapshot[i] = f.exchanges[i].Clone()
	}
	f.mu.Unlock()
	return SaveRecording(path, snapshot)
}

// Load reads recorded exchanges from path and queues them for deterministic replay.
func (f *FakeClient) Load(path string) error {
	exchanges, err := LoadRecording(path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range exchanges {
		ex := &exchanges[i]
		reqCopy := ex.Request.Clone()
		var callErr error
		if ex.Error != "" {
			callErr = errors.New(ex.Error)
		}
		f.queue = append(f.queue, scriptedCall{
			req:  &reqCopy,
			resp: ex.Response.Clone(),
			err:  callErr,
		})
	}
	return nil
}

// SaveRecording writes exchanges to path as formatted JSON, creating parent
// directories as needed.
func SaveRecording(path string, exchanges []RecordedExchange) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("orch/model: create recording dir: %w", err)
	}
	rec := Recording{Exchanges: exchanges}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("orch/model: marshal recording: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("orch/model: write recording %s: %w", path, err)
	}
	return nil
}

// LoadRecording reads recorded exchanges from a JSON file (supporting either a
// Recording object {"exchanges": [...]} or a top-level []RecordedExchange array).
func LoadRecording(path string) ([]RecordedExchange, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("orch/model: read recording %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var list []RecordedExchange
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, fmt.Errorf("orch/model: decode recording array %s: %w", path, err)
		}
		return list, nil
	}
	var rec Recording
	if err := json.Unmarshal(trimmed, &rec); err != nil {
		return nil, fmt.Errorf("orch/model: decode recording %s: %w", path, err)
	}
	return rec.Exchanges, nil
}

// CreateMessage records req and either forwards to upstream (in record mode) or
// returns the next queued response (in replay mode). If req.ToolRegistry is set,
// CreateMessage executes the bounded tool loop using RunToolLoop.
func (f *FakeClient) CreateMessage(ctx context.Context, req Request) (*Response, error) {
	if req.ToolRegistry != nil {
		res, err := RunToolLoop(ctx, f, req, req.ToolRegistry, WithMaxTurns(req.MaxTurns))
		if res != nil {
			cp := res.Response.Clone()
			return &cp, err
		}
		return nil, err
	}

	f.mu.Lock()
	upstream := f.upstream
	recordPath := f.recordPath
	f.mu.Unlock()

	if upstream != nil {
		resp, err := upstream.CreateMessage(ctx, req.Clone())
		f.mu.Lock()
		f.Requests = append(f.Requests, req.Clone())
		ex := RecordedExchange{Request: req.Clone()}
		if err != nil {
			ex.Error = err.Error()
		} else if resp != nil {
			ex.Response = resp.Clone()
			f.totalUsage = f.totalUsage.Add(resp.Usage)
		}
		f.exchanges = append(f.exchanges, ex)
		snapshot := make([]RecordedExchange, len(f.exchanges))
		for i := range f.exchanges {
			snapshot[i] = f.exchanges[i].Clone()
		}
		f.mu.Unlock()

		if recordPath != "" {
			if saveErr := SaveRecording(recordPath, snapshot); saveErr != nil && err == nil {
				return nil, saveErr
			}
		}
		if err != nil {
			return nil, err
		}
		cp := resp.Clone()
		return &cp, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	callNum := len(f.Requests) + 1
	f.Requests = append(f.Requests, req.Clone())
	if len(f.queue) == 0 {
		return nil, fmt.Errorf("orch/model: FakeClient has no more scripted responses (call #%d)", callNum)
	}
	next := f.queue[0]
	f.queue = f.queue[1:]

	if f.strictMatch && next.req != nil {
		if err := checkRequestMatch(callNum, *next.req, req); err != nil {
			return nil, err
		}
	}

	ex := RecordedExchange{
		Request:  req.Clone(),
		Response: next.resp.Clone(),
	}
	if next.err != nil {
		ex.Error = next.err.Error()
	}
	f.exchanges = append(f.exchanges, ex)

	if next.err != nil {
		return nil, next.err
	}
	cp := next.resp.Clone()
	if cp.Usage.CostUSD == 0 {
		modelName := cp.Model
		if modelName == "" {
			modelName = req.Model
		}
		cp.Usage.CostUSD = CalculateCost(modelName, cp.Usage, nil)
	}
	f.totalUsage = f.totalUsage.Add(cp.Usage)
	return &cp, nil
}

func checkRequestMatch(callNum int, want, got Request) error {
	if want.Model != "" && want.Model != got.Model {
		return fmt.Errorf("orch/model: replay call #%d model mismatch: got %q, want %q", callNum, got.Model, want.Model)
	}
	if want.System != got.System {
		return fmt.Errorf("orch/model: replay call #%d system mismatch: got %q, want %q", callNum, got.System, want.System)
	}
	wantMsgs, err := json.Marshal(want.Messages)
	if err != nil {
		return err
	}
	gotMsgs, err := json.Marshal(got.Messages)
	if err != nil {
		return err
	}
	if !bytes.Equal(wantMsgs, gotMsgs) {
		return fmt.Errorf("orch/model: replay call #%d messages mismatch:\ngot:  %s\nwant: %s", callNum, gotMsgs, wantMsgs)
	}
	return nil
}
