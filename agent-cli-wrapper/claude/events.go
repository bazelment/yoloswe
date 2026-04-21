package claude

import (
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agentstream"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// EventType discriminates between event kinds.
type EventType int

const (
	// EventTypeReady fires when the session is initialized.
	EventTypeReady EventType = iota
	// EventTypeText fires for streaming text chunks.
	EventTypeText
	// EventTypeThinking fires for thinking chunks.
	EventTypeThinking
	// EventTypeToolStart fires when a tool begins execution.
	EventTypeToolStart
	// EventTypeToolProgress fires as tool input streams in.
	EventTypeToolProgress
	// EventTypeToolComplete fires when tool input is fully parsed.
	EventTypeToolComplete
	// EventTypeCLIToolResult fires when CLI sends back auto-executed tool results.
	EventTypeCLIToolResult
	// EventTypeTurnComplete fires when a turn finishes.
	EventTypeTurnComplete
	// EventTypeError fires on session errors.
	EventTypeError
	// EventTypeStateChange fires on session state transitions.
	EventTypeStateChange
	// EventTypeCompactBoundary fires when the CLI compacts conversation history.
	EventTypeCompactBoundary
	// EventTypeAPIRetry fires when a retryable API error is being retried.
	EventTypeAPIRetry
	// EventTypeTaskStarted fires when a background task starts.
	EventTypeTaskStarted
	// EventTypeTaskProgress fires on background task progress updates.
	EventTypeTaskProgress
	// EventTypeTaskNotification fires when a background task completes/fails/is killed.
	EventTypeTaskNotification
	// EventTypeTaskUpdated fires when a background task's state patch is emitted.
	EventTypeTaskUpdated
	// EventTypeHookLifecycle fires for hook_started/progress/response.
	EventTypeHookLifecycle
	// EventTypeRateLimit fires when the server's rate-limit state changes.
	EventTypeRateLimit
	// EventTypeCLISessionStateChanged fires when CLI's internal session state transitions.
	EventTypeCLISessionStateChanged
	// EventTypePostTurnSummary fires with a background post-turn summary.
	EventTypePostTurnSummary
	// EventTypeFilesPersisted fires when a file-save batch completes.
	EventTypeFilesPersisted
	// EventTypeAuthStatus fires with OAuth flow status updates.
	EventTypeAuthStatus
	// EventTypeToolExecutionProgress fires with elapsed time for a running tool.
	EventTypeToolExecutionProgress
	// EventTypeLocalCommandOutput fires with text from a local slash command.
	EventTypeLocalCommandOutput
	// EventTypeAssistantMessage fires per assistant message, with the full
	// per-message content blocks as the CLI emitted them. Parallel to the
	// Python claude-agent-sdk AssistantMessage. Always emitted — independent
	// of any wrapper-level turn coalescing.
	EventTypeAssistantMessage
	// EventTypeResultMessage fires once per CLI "turn" (every ResultMessage
	// the CLI emits — the Python SDK surfaces each one as ResultMessage).
	// Unlike TurnCompleteEvent, this is NOT coalesced across suppressed /
	// auto-continued turns: pure-bg and mixed-bg turns each produce a
	// ResultMessageEvent at their own ResultMessage boundary. Consumers that
	// need "logical turn done" semantics must observe the event stream
	// (bg tool lifecycle + ResultMessage) themselves.
	EventTypeResultMessage
	// EventTypeUserMessage fires per user message (CLI-injected tool_result
	// frames and user-initiated text). Parallel to the Python SDK's
	// UserMessage. Not coalesced.
	EventTypeUserMessage
	// EventTypeSystemMessage fires per system message the CLI emits that is
	// not already surfaced as a dedicated typed event. Parallel to the Python
	// SDK's SystemMessage catch-all. Subtype identifies the kind.
	EventTypeSystemMessage
)

// HookPhase identifies which hook lifecycle stage a HookLifecycleEvent represents.
type HookPhase string

const (
	// HookPhaseStarted marks the start of a hook execution.
	HookPhaseStarted HookPhase = "started"
	// HookPhaseProgress streams incremental stdout/stderr from a running hook.
	HookPhaseProgress HookPhase = "progress"
	// HookPhaseResponse marks hook completion with final output and outcome.
	HookPhaseResponse HookPhase = "response"
)

// Event is the interface for all events.
type Event interface {
	Type() EventType
}

// ReadyEvent fires when the session is initialized.
type ReadyEvent struct {
	Info SessionInfo
}

// Type returns the event type.
func (e ReadyEvent) Type() EventType { return EventTypeReady }

func (e ReadyEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindReady }
func (e ReadyEvent) StreamSessionID() string                { return e.Info.SessionID }

// TextEvent contains streaming text chunks.
type TextEvent struct {
	Text       string
	FullText   string
	TurnNumber int
}

// Type returns the event type.
func (e TextEvent) Type() EventType { return EventTypeText }

func (e TextEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindText }
func (e TextEvent) StreamDelta() string                    { return e.Text }

// ThinkingEvent contains thinking chunks.
type ThinkingEvent struct {
	Thinking     string
	FullThinking string
	TurnNumber   int
}

// Type returns the event type.
func (e ThinkingEvent) Type() EventType { return EventTypeThinking }

func (e ThinkingEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindThinking }
func (e ThinkingEvent) StreamDelta() string                    { return e.Thinking }

// ToolStartEvent fires when a tool begins execution.
type ToolStartEvent struct {
	Timestamp  time.Time
	ID         string
	Name       string
	TurnNumber int
}

// Type returns the event type.
func (e ToolStartEvent) Type() EventType { return EventTypeToolStart }

func (e ToolStartEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindToolStart }
func (e ToolStartEvent) StreamToolName() string                  { return e.Name }
func (e ToolStartEvent) StreamToolCallID() string                { return e.ID }
func (e ToolStartEvent) StreamToolInput() map[string]interface{} { return nil }

// ToolProgressEvent contains partial tool input.
type ToolProgressEvent struct {
	ID           string
	Name         string
	PartialInput string
	InputChunk   string
	TurnNumber   int
}

// Type returns the event type.
func (e ToolProgressEvent) Type() EventType { return EventTypeToolProgress }

// ToolCompleteEvent fires when tool input is fully parsed.
type ToolCompleteEvent struct {
	Timestamp  time.Time
	Input      map[string]interface{}
	ID         string
	Name       string
	TurnNumber int
}

// Type returns the event type.
func (e ToolCompleteEvent) Type() EventType { return EventTypeToolComplete }

func (e ToolCompleteEvent) StreamEventKind() agentstream.EventKind  { return agentstream.KindToolEnd }
func (e ToolCompleteEvent) StreamToolName() string                  { return e.Name }
func (e ToolCompleteEvent) StreamToolCallID() string                { return e.ID }
func (e ToolCompleteEvent) StreamToolInput() map[string]interface{} { return e.Input }
func (e ToolCompleteEvent) StreamToolResult() interface{}           { return nil }
func (e ToolCompleteEvent) StreamToolIsError() bool                 { return false }

// CLIToolResultEvent fires when CLI sends back auto-executed tool results.
type CLIToolResultEvent struct {
	Content    interface{}
	ToolUseID  string
	ToolName   string
	TurnNumber int
	IsError    bool
}

// Type returns the event type.
func (e CLIToolResultEvent) Type() EventType { return EventTypeCLIToolResult }

// TurnCompleteEvent fires when a turn finishes.
type TurnCompleteEvent struct {
	Error                 error
	Usage                 TurnUsage
	TurnNumber            int
	DurationMs            int64
	Success               bool
	HasLiveBackgroundWork bool
}

// Type returns the event type.
func (e TurnCompleteEvent) Type() EventType { return EventTypeTurnComplete }

func (e TurnCompleteEvent) StreamEventKind() agentstream.EventKind {
	return agentstream.KindTurnComplete
}
func (e TurnCompleteEvent) StreamTurnNum() int    { return e.TurnNumber }
func (e TurnCompleteEvent) StreamIsSuccess() bool { return e.Success }
func (e TurnCompleteEvent) StreamDuration() int64 { return e.DurationMs }
func (e TurnCompleteEvent) StreamCost() float64   { return e.Usage.CostUSD }

// ErrorEvent contains session errors.
type ErrorEvent struct {
	Error      error
	Context    string
	TurnNumber int
}

// Type returns the event type.
func (e ErrorEvent) Type() EventType { return EventTypeError }

func (e ErrorEvent) StreamEventKind() agentstream.EventKind { return agentstream.KindError }
func (e ErrorEvent) StreamErr() error                       { return e.Error }
func (e ErrorEvent) StreamErrorContext() string             { return e.Context }

// StateChangeEvent fires on session state transitions.
type StateChangeEvent struct {
	From SessionState
	To   SessionState
}

// Type returns the event type.
func (e StateChangeEvent) Type() EventType { return EventTypeStateChange }

// CompactBoundaryEvent fires when the CLI compacts conversation history
// (either on demand via /compact or automatically when approaching the
// context limit). Trigger is "manual" or "auto".
type CompactBoundaryEvent struct {
	PreservedSegment *protocol.CompactPreservedSegment
	Trigger          string
	PreTokens        int
	TurnNumber       int
}

// Type returns the event type.
func (e CompactBoundaryEvent) Type() EventType { return EventTypeCompactBoundary }

// APIRetryEvent fires when the CLI hits a retryable API error and is about
// to retry after RetryDelayMs. ErrorStatus is nil for connection errors
// that had no HTTP response.
type APIRetryEvent struct {
	ErrorStatus  *int
	ErrorType    string
	Attempt      int
	MaxRetries   int
	RetryDelayMs int
	TurnNumber   int
}

// Type returns the event type.
func (e APIRetryEvent) Type() EventType { return EventTypeAPIRetry }

// TaskStartedEvent fires when a background task (sub-agent or workflow) starts.
type TaskStartedEvent struct {
	ToolUseID    *string
	WorkflowName *string
	TaskID       string
	Description  string
	TaskType     string
	Prompt       string
	TurnNumber   int
}

// Type returns the event type.
func (e TaskStartedEvent) Type() EventType { return EventTypeTaskStarted }

// TaskProgressEvent fires with incremental progress from a running background task.
type TaskProgressEvent struct {
	ToolUseID    *string
	TaskID       string
	Description  string
	LastToolName string
	Summary      string
	Usage        protocol.TaskUsage
	TurnNumber   int
}

// Type returns the event type.
func (e TaskProgressEvent) Type() EventType { return EventTypeTaskProgress }

// TaskNotificationEvent fires when a background task completes. Status is
// "completed", "failed", or "killed".
type TaskNotificationEvent struct {
	ToolUseID  *string
	TaskID     string
	Status     string
	OutputFile string
	Summary    string
	Usage      protocol.TaskUsage
	TurnNumber int
}

// Type returns the event type.
func (e TaskNotificationEvent) Type() EventType { return EventTypeTaskNotification }

// TaskUpdatedEvent fires on a background-task state patch. Status and
// description are pointers because the upstream patch only carries fields
// that actually changed — an unchanged description is nil, not "". Terminal
// Status values are "completed", "failed", or "killed"; non-terminal values
// are "pending" and "running".
type TaskUpdatedEvent struct {
	Status         *string
	Description    *string
	EndTime        *int64
	TotalPausedMs  *int64
	Error          *string
	IsBackgrounded *bool
	TaskID         string
	TurnNumber     int
}

// Type returns the event type.
func (e TaskUpdatedEvent) Type() EventType { return EventTypeTaskUpdated }

// HookLifecycleEvent is a unified event for hook_started/hook_progress/
// hook_response system subtypes. Phase selects which stage this event
// represents; fields are populated where available per stage.
type HookLifecycleEvent struct {
	ExitCode      *int
	Phase         HookPhase
	HookID        string
	HookName      string
	HookEventName string
	Stdout        string
	Stderr        string
	Output        string
	Outcome       string
	TurnNumber    int
}

// Type returns the event type.
func (e HookLifecycleEvent) Type() EventType { return EventTypeHookLifecycle }

// RateLimitEvent fires whenever the server's rate-limit state changes for
// the current subscription (e.g. crossing into allowed_warning or rejected).
type RateLimitEvent struct {
	ResetsAt              *float64
	Utilization           *float64
	OverageResetsAt       *float64
	OverageDisabledReason *string
	SurpassedThreshold    *float64
	Status                string
	RateLimitType         string
	IsUsingOverage        bool
	IsOverageActive       bool
	TurnNumber            int
}

// Type returns the event type.
func (e RateLimitEvent) Type() EventType { return EventTypeRateLimit }

// CLISessionStateChangedEvent is the CLI's own session state transition
// (distinct from the SDK-internal StateChangeEvent). State is "idle",
// "running", or "requires_action".
type CLISessionStateChangedEvent struct {
	State      string
	TurnNumber int
}

// Type returns the event type.
func (e CLISessionStateChangedEvent) Type() EventType { return EventTypeCLISessionStateChanged }

// PostTurnSummaryEvent exposes the CLI's post-turn background summary.
type PostTurnSummaryEvent struct {
	SummarizesUUID string
	StatusCategory string
	StatusDetail   string
	Title          string
	Description    string
	RecentAction   string
	// NeedsAction is the raw upstream string (e.g. "true", "false", or a
	// future sentinel) — the CLI defines it as a string, so we surface it
	// verbatim rather than collapsing future states into a bool.
	NeedsAction  string
	ArtifactURLs []string
	TurnNumber   int
	IsNoteworthy bool
}

// Type returns the event type.
func (e PostTurnSummaryEvent) Type() EventType { return EventTypePostTurnSummary }

// FilesPersistedEvent reports a batch file-save outcome.
type FilesPersistedEvent struct {
	ProcessedAt string
	Files       []protocol.PersistedFile
	Failed      []protocol.PersistedFileFailure
	TurnNumber  int
}

// Type returns the event type.
func (e FilesPersistedEvent) Type() EventType { return EventTypeFilesPersisted }

// AuthStatusEvent reports progress of an interactive OAuth login flow.
type AuthStatusEvent struct {
	Error            *string
	Output           []string
	IsAuthenticating bool
	TurnNumber       int
}

// Type returns the event type.
func (e AuthStatusEvent) Type() EventType { return EventTypeAuthStatus }

// ToolExecutionProgressEvent fires periodically while a tool is executing,
// so consumers can display a live "running for Ns" indicator. Distinct from
// ToolProgressEvent which carries streaming tool-input deltas.
type ToolExecutionProgressEvent struct {
	ParentToolUseID    *string
	TaskID             *string
	ToolUseID          string
	ToolName           string
	ElapsedTimeSeconds float64
	TurnNumber         int
}

// Type returns the event type.
func (e ToolExecutionProgressEvent) Type() EventType { return EventTypeToolExecutionProgress }

// LocalCommandOutputEvent carries text output from a local slash command
// (e.g. /voice, /cost). Displayed as assistant-style text in the transcript.
type LocalCommandOutputEvent struct {
	Content    string
	TurnNumber int
}

// Type returns the event type.
func (e LocalCommandOutputEvent) Type() EventType { return EventTypeLocalCommandOutput }

// AssistantMessageEvent fires per assistant message with the full per-message
// content blocks as the CLI emitted them. Parallel to the Python SDK's
// AssistantMessage. Emitted raw — independent of any wrapper-level
// suppression/coalescing of turn completion.
type AssistantMessageEvent struct {
	ParentToolUse *string
	Model         string
	Blocks        []ContentBlock
	TurnNumber    int
}

// Type returns the event type.
func (e AssistantMessageEvent) Type() EventType { return EventTypeAssistantMessage }

// UserMessageEvent fires per user message. Parallel to the Python SDK's
// UserMessage. Emitted raw alongside existing typed CLI events.
type UserMessageEvent struct {
	ParentToolUse *string
	Blocks        []ContentBlock
	TurnNumber    int
}

// Type returns the event type.
func (e UserMessageEvent) Type() EventType { return EventTypeUserMessage }

// ResultMessageEvent fires once per CLI ResultMessage (every turn the CLI
// emits). Parallel to the Python SDK's ResultMessage. Unlike
// TurnCompleteEvent, this event is NEVER coalesced: pure-bg and mixed-bg
// turns each produce a ResultMessageEvent at their own ResultMessage
// boundary. Consumers that need "logical turn done" semantics must observe
// the raw event stream (bg tool lifecycle + ResultMessage) directly.
type ResultMessageEvent struct {
	Error         error
	Subtype       string
	StopReason    string
	Usage         TurnUsage
	DurationMs    int64
	DurationAPIMs int64
	TotalCostUSD  float64
	TurnNumber    int
	NumTurns      int
	IsError       bool
}

// Type returns the event type.
func (e ResultMessageEvent) Type() EventType { return EventTypeResultMessage }

// SystemMessageEvent fires per system message that isn't already surfaced as
// a dedicated typed event. Parallel to the Python SDK's SystemMessage.
type SystemMessageEvent struct {
	Data       map[string]interface{}
	Subtype    string
	TurnNumber int
}

// Type returns the event type.
func (e SystemMessageEvent) Type() EventType { return EventTypeSystemMessage }
