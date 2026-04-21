package codex

import (
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
)

// EventType discriminates between event kinds.
type EventType int

const (
	// EventTypeClientReady fires when the client is initialized.
	EventTypeClientReady EventType = iota

	// EventTypeThreadStarted fires when a thread is created.
	EventTypeThreadStarted

	// EventTypeThreadReady fires when MCP startup completes for a thread.
	EventTypeThreadReady

	// EventTypeTurnStarted fires when a turn begins.
	EventTypeTurnStarted

	// EventTypeTurnCompleted fires when a turn finishes.
	EventTypeTurnCompleted

	// EventTypeTextDelta fires for streaming text chunks.
	EventTypeTextDelta

	// EventTypeItemStarted fires when an item (message, tool) starts.
	EventTypeItemStarted

	// EventTypeItemCompleted fires when an item completes.
	EventTypeItemCompleted

	// EventTypeTokenUsage fires with token usage information.
	EventTypeTokenUsage

	// EventTypeError fires on errors.
	EventTypeError

	// EventTypeStateChange fires on state transitions.
	EventTypeStateChange

	// EventTypeCommandStart fires when a shell command begins.
	EventTypeCommandStart

	// EventTypeCommandOutput fires for streaming command output.
	EventTypeCommandOutput

	// EventTypeCommandEnd fires when a shell command completes.
	EventTypeCommandEnd

	// EventTypeReasoningDelta fires for streaming reasoning/thinking text.
	EventTypeReasoningDelta
)

// Event is the interface for all events.
type Event interface {
	Type() EventType
}

// ClientReadyEvent fires when the client is initialized.
type ClientReadyEvent struct {
	UserAgent string
}

// Type returns the event type.
func (e ClientReadyEvent) Type() EventType { return EventTypeClientReady }

// ThreadStartedEvent fires when a thread is created.
type ThreadStartedEvent struct {
	ThreadID      string
	Model         string
	ModelProvider string
	WorkDir       string
}

// Type returns the event type.
func (e ThreadStartedEvent) Type() EventType { return EventTypeThreadStarted }

// ThreadReadyEvent fires when MCP startup completes for a thread.
type ThreadReadyEvent struct {
	ThreadID string
}

// Type returns the event type.
func (e ThreadReadyEvent) Type() EventType { return EventTypeThreadReady }

// TurnStartedEvent fires when a turn begins.
type TurnStartedEvent struct {
	ThreadID string
	TurnID   string
}

// Type returns the event type.
func (e TurnStartedEvent) Type() EventType { return EventTypeTurnStarted }

// TurnCompletedEvent fires when a turn finishes.
type TurnCompletedEvent struct {
	Error      error
	ThreadID   string
	TurnID     string
	FullText   string
	Usage      TurnUsage
	DurationMs int64
	Success    bool
}

// Type returns the event type.
func (e TurnCompletedEvent) Type() EventType { return EventTypeTurnCompleted }

func (e TurnCompletedEvent) StreamEventKind() agentstream.EventKind {
	return agentstream.KindTurnComplete
}
func (e TurnCompletedEvent) StreamTurnNum() int    { return TurnNumberFromID(e.TurnID) }
func (e TurnCompletedEvent) StreamIsSuccess() bool { return e.Success }
func (e TurnCompletedEvent) StreamDuration() int64 { return e.DurationMs }
func (e TurnCompletedEvent) StreamCost() float64   { return 0 }
func (e TurnCompletedEvent) ScopeID() string       { return e.ThreadID }

// TextDeltaEvent contains streaming text chunks.
type TextDeltaEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	Delta    string // New text chunk
	FullText string // Accumulated text so far
}

// Type returns the event type.
func (e TextDeltaEvent) Type() EventType { return EventTypeTextDelta }

func (e TextDeltaEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindText }
func (e TextDeltaEvent) StreamDelta() string                    { return e.Delta }
func (e TextDeltaEvent) ScopeID() string                        { return e.ThreadID }

// ItemStartedEvent fires when an item (message, tool) starts.
type ItemStartedEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	ItemType string
}

// Type returns the event type.
func (e ItemStartedEvent) Type() EventType { return EventTypeItemStarted }

// ItemCompletedEvent fires when an item completes.
type ItemCompletedEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	ItemType string
	Text     string // For message items
}

// Type returns the event type.
func (e ItemCompletedEvent) Type() EventType { return EventTypeItemCompleted }

// TokenUsageEvent contains token usage information.
type TokenUsageEvent struct {
	TotalUsage *TokenUsage
	LastUsage  *TokenUsage
	RateLimits *RateLimits
	ThreadID   string
}

// Type returns the event type.
func (e TokenUsageEvent) Type() EventType { return EventTypeTokenUsage }

// TurnUsage contains token usage for a turn.
type TurnUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
}

// ErrorEvent contains errors.
type ErrorEvent struct {
	Timestamp time.Time
	Error     error
	ThreadID  string
	TurnID    string
	Context   string
}

// Type returns the event type.
func (e ErrorEvent) Type() EventType { return EventTypeError }

func (e ErrorEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindError }
func (e ErrorEvent) StreamErr() error                       { return e.Error }
func (e ErrorEvent) StreamErrorContext() string             { return e.Context }
func (e ErrorEvent) ScopeID() string                        { return e.ThreadID }

// StateChangeEvent fires on state transitions.
type StateChangeEvent struct {
	ThreadID string // Empty for client-level state changes
	From     string
	To       string
}

// Type returns the event type.
func (e StateChangeEvent) Type() EventType { return EventTypeStateChange }

// CommandStartEvent fires when a shell command begins.
type CommandStartEvent struct {
	ThreadID  string
	TurnID    string
	CallID    string
	CWD       string
	ParsedCmd string
	Command   []string
}

// Type returns the event type.
func (e CommandStartEvent) Type() EventType { return EventTypeCommandStart }

func (e CommandStartEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindToolStart }
func (e CommandStartEvent) StreamToolName() string                 { return "Bash" }
func (e CommandStartEvent) StreamToolCallID() string               { return e.CallID }
func (e CommandStartEvent) StreamToolInput() map[string]interface{} {
	input := map[string]interface{}{}
	if cmd := commandText(e.ParsedCmd, e.Command); cmd != "" {
		input["command"] = cmd
	}
	if e.CWD != "" {
		input["cwd"] = e.CWD
	}
	return input
}
func (e CommandStartEvent) ScopeID() string { return e.ThreadID }

// CommandOutputEvent fires for streaming command output.
type CommandOutputEvent struct {
	ThreadID string
	TurnID   string
	CallID   string
	Stream   string // "stdout" or "stderr"
	Chunk    string
}

// Type returns the event type.
func (e CommandOutputEvent) Type() EventType { return EventTypeCommandOutput }

// CommandEndEvent fires when a shell command completes.
type CommandEndEvent struct {
	ThreadID   string
	TurnID     string
	CallID     string
	Stdout     string
	Stderr     string
	ExitCode   int
	DurationMs int64
}

// Type returns the event type.
func (e CommandEndEvent) Type() EventType { return EventTypeCommandEnd }

func (e CommandEndEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindToolEnd }
func (e CommandEndEvent) StreamToolName() string                  { return "Bash" }
func (e CommandEndEvent) StreamToolCallID() string                { return e.CallID }
func (e CommandEndEvent) StreamToolInput() map[string]interface{} { return nil }
func (e CommandEndEvent) StreamToolResult() interface{} {
	return map[string]interface{}{
		"stdout":      e.Stdout,
		"stderr":      e.Stderr,
		"exit_code":   e.ExitCode,
		"duration_ms": e.DurationMs,
	}
}
func (e CommandEndEvent) StreamToolIsError() bool { return e.ExitCode != 0 }
func (e CommandEndEvent) ScopeID() string         { return e.ThreadID }

// ReasoningDeltaEvent fires for streaming reasoning/thinking text.
type ReasoningDeltaEvent struct {
	ThreadID string
	TurnID   string
	ItemID   string
	Delta    string
}

// Type returns the event type.
func (e ReasoningDeltaEvent) Type() EventType { return EventTypeReasoningDelta }

func (e ReasoningDeltaEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindThinking }
func (e ReasoningDeltaEvent) StreamDelta() string                    { return e.Delta }
func (e ReasoningDeltaEvent) ScopeID() string                        { return e.ThreadID }

// commandText returns the best available human-readable command text.
func commandText(parsed string, command []string) string {
	cmd := strings.TrimSpace(parsed)
	if cmd == "" {
		cmd = strings.TrimSpace(strings.Join(command, " "))
	}
	return cmd
}
