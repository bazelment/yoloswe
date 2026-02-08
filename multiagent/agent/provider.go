package agent

import (
	"context"

	"github.com/bazelment/yoloswe/wt"
)

// AgentResult is the provider-agnostic result of an agent execution.
// It replaces direct dependency on claude.TurnResult in the agent interfaces.
type AgentResult struct {
	Error         error
	Text          string
	Thinking      string
	ContentBlocks []AgentContentBlock
	Usage         AgentUsage
	DurationMs    int64
	Success       bool
}

// AgentContentBlock is a provider-agnostic content block.
type AgentContentBlock struct {
	ToolResult interface{}
	ToolInput  map[string]interface{}
	Type       string
	Text       string
	ToolName   string
	IsError    bool
}

// AgentUsage tracks token usage across providers.
type AgentUsage struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	CostUSD         float64
}

// AgentEventType identifies the type of streaming event.
type AgentEventType int

const (
	AgentEventText         AgentEventType = iota
	AgentEventThinking                    // Chain-of-thought / reasoning
	AgentEventToolStart                   // Tool invocation started
	AgentEventToolComplete                // Tool invocation completed
	AgentEventTurnComplete                // Turn finished
	AgentEventError                       // Error occurred
)

// AgentEvent is the provider-agnostic event interface for streaming.
type AgentEvent interface {
	AgentEventType() AgentEventType
}

// TextAgentEvent is emitted when the agent produces text.
type TextAgentEvent struct {
	Text string
}

func (e TextAgentEvent) AgentEventType() AgentEventType { return AgentEventText }

// ThinkingAgentEvent is emitted for chain-of-thought content.
type ThinkingAgentEvent struct {
	Thinking string
}

func (e ThinkingAgentEvent) AgentEventType() AgentEventType { return AgentEventThinking }

// ToolStartAgentEvent is emitted when a tool invocation begins.
type ToolStartAgentEvent struct {
	Input map[string]interface{}
	Name  string
	ID    string
}

func (e ToolStartAgentEvent) AgentEventType() AgentEventType { return AgentEventToolStart }

// ToolCompleteAgentEvent is emitted when a tool invocation finishes.
type ToolCompleteAgentEvent struct {
	Result  interface{}
	Input   map[string]interface{}
	Name    string
	ID      string
	IsError bool
}

func (e ToolCompleteAgentEvent) AgentEventType() AgentEventType { return AgentEventToolComplete }

// TurnCompleteAgentEvent is emitted when a turn finishes.
type TurnCompleteAgentEvent struct {
	TurnNumber int
	Success    bool
	DurationMs int64
	CostUSD    float64
}

func (e TurnCompleteAgentEvent) AgentEventType() AgentEventType { return AgentEventTurnComplete }

// ErrorAgentEvent is emitted when an error occurs.
type ErrorAgentEvent struct {
	Err     error
	Context string
}

func (e ErrorAgentEvent) AgentEventType() AgentEventType { return AgentEventError }

// EventHandler is the provider-agnostic callback interface for agent events.
// This mirrors the existing sessionEventHandler pattern in bramble/session.
type EventHandler interface {
	OnText(text string)
	OnThinking(thinking string)
	OnToolStart(name, id string, input map[string]interface{})
	OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool)
	OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64)
	OnError(err error, context string)
}

// Provider is the pluggable interface for agent backends.
// Adding a new backend (Gemini, Codex, etc.) means implementing this interface.
type Provider interface {
	// Name returns the provider name (e.g., "claude", "codex", "gemini").
	Name() string

	// Execute runs a prompt with optional worktree context and returns the result.
	Execute(ctx context.Context, prompt string, wtCtx *wt.WorktreeContext, opts ...ExecuteOption) (*AgentResult, error)

	// Events returns a channel for streaming events during execution.
	// May return nil if the provider does not support streaming.
	Events() <-chan AgentEvent

	// Close releases any resources held by the provider.
	Close() error
}

// LongRunningProvider extends Provider for persistent session backends.
type LongRunningProvider interface {
	Provider

	// Start initializes the provider session.
	Start(ctx context.Context) error

	// SendMessage sends a follow-up message in the existing session.
	SendMessage(ctx context.Context, message string) (*AgentResult, error)

	// Stop gracefully shuts down the provider session.
	Stop() error
}

// ExecuteOption configures a single execution.
type ExecuteOption func(*ExecuteConfig)

// ExecuteConfig holds execution configuration.
type ExecuteConfig struct {
	EventHandler   EventHandler
	Model          string
	WorkDir        string
	SystemPrompt   string
	PermissionMode string
	MaxTurns       int
	MaxBudgetUSD   float64
}

// WithProviderModel sets the model for a provider execution.
func WithProviderModel(model string) ExecuteOption {
	return func(c *ExecuteConfig) { c.Model = model }
}

// WithProviderWorkDir sets the working directory for a provider execution.
func WithProviderWorkDir(dir string) ExecuteOption {
	return func(c *ExecuteConfig) { c.WorkDir = dir }
}

// WithProviderSystemPrompt sets the system prompt for a provider execution.
func WithProviderSystemPrompt(prompt string) ExecuteOption {
	return func(c *ExecuteConfig) { c.SystemPrompt = prompt }
}

// WithProviderPermissionMode sets the permission mode for a provider execution.
func WithProviderPermissionMode(mode string) ExecuteOption {
	return func(c *ExecuteConfig) { c.PermissionMode = mode }
}

// WithProviderEventHandler sets the event handler for a provider execution.
func WithProviderEventHandler(h EventHandler) ExecuteOption {
	return func(c *ExecuteConfig) { c.EventHandler = h }
}

// applyOptions applies ExecuteOptions to a config, returning defaults for unset fields.
func applyOptions(opts []ExecuteOption) ExecuteConfig {
	cfg := ExecuteConfig{
		Model:          "sonnet",
		PermissionMode: "bypass",
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
