// Package protocol — system message subtype union.
//
// SystemMessage is the JSON envelope emitted by the Claude Code CLI for all
// "type":"system" frames. The Subtype field selects a different payload
// schema: e.g. "init" carries tools/model/mcp_servers, while "compact_boundary"
// carries compact_metadata, and "task_notification" carries task usage stats.
//
// This file defines the SystemSubtype enum, one typed payload struct per
// subtype (fields mirror the upstream TypeScript schemas in
// the upstream Claude Code CLI's SDK entrypoint schemas), and a DecodePayload
// helper that dispatches on Subtype to return the right typed value.
package protocol

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// SystemSubtype enumerates all known "system" message subtypes.
type SystemSubtype string

const (
	SystemSubtypeInit                SystemSubtype = "init"
	SystemSubtypeStatus              SystemSubtype = "status"
	SystemSubtypeCompactBoundary     SystemSubtype = "compact_boundary"
	SystemSubtypePostTurnSummary     SystemSubtype = "post_turn_summary"
	SystemSubtypeAPIRetry            SystemSubtype = "api_retry"
	SystemSubtypeLocalCommandOutput  SystemSubtype = "local_command_output"
	SystemSubtypeHookStarted         SystemSubtype = "hook_started"
	SystemSubtypeHookProgress        SystemSubtype = "hook_progress"
	SystemSubtypeHookResponse        SystemSubtype = "hook_response"
	SystemSubtypeTaskNotification    SystemSubtype = "task_notification"
	SystemSubtypeTaskStarted         SystemSubtype = "task_started"
	SystemSubtypeTaskProgress        SystemSubtype = "task_progress"
	SystemSubtypeSessionStateChanged SystemSubtype = "session_state_changed"
	SystemSubtypeFilesPersisted      SystemSubtype = "files_persisted"
	SystemSubtypeElicitationComplete SystemSubtype = "elicitation_complete"
)

// SystemInitPayload is emitted once at session start. Contains the resolved
// tool/mcp/plugin/skill catalog, the working directory, claude_code_version,
// and the effective model and permission mode.
type SystemInitPayload struct {
	FastModeState     interface{} `json:"fast_mode_state,omitempty"`
	PermissionMode    string      `json:"permissionMode"`
	APIKeySource      string      `json:"apiKeySource"`
	SessionID         string      `json:"session_id"`
	UUID              string      `json:"uuid"`
	OutputStyle       string      `json:"output_style"`
	ClaudeCodeVersion string      `json:"claude_code_version"`
	CWD               string      `json:"cwd"`
	Model             string      `json:"model"`
	Betas             []string    `json:"betas,omitempty"`
	Tools             []string    `json:"tools"`
	Plugins           []Plugin    `json:"plugins"`
	Agents            []string    `json:"agents,omitempty"`
	Skills            []string    `json:"skills"`
	SlashCommands     []string    `json:"slash_commands"`
	MCPServers        []MCPServer `json:"mcp_servers"`
}

// SystemStatusPayload signals a transient session status change, e.g. when
// compaction is running ("compacting"). status is null when the session
// returns to idle.
type SystemStatusPayload struct {
	Status         *string `json:"status"`
	PermissionMode string  `json:"permissionMode,omitempty"`
	UUID           string  `json:"uuid"`
	SessionID      string  `json:"session_id"`
}

// CompactPreservedSegment relinks the preserved message range after a partial
// compaction. Unset when compaction summarizes everything.
type CompactPreservedSegment struct {
	HeadUUID   string `json:"head_uuid"`
	AnchorUUID string `json:"anchor_uuid"`
	TailUUID   string `json:"tail_uuid"`
}

// CompactMetadata describes a compaction event.
type CompactMetadata struct {
	PreservedSegment *CompactPreservedSegment `json:"preserved_segment,omitempty"`
	Trigger          string                   `json:"trigger"`
	PreTokens        int                      `json:"pre_tokens"`
}

// CompactBoundaryPayload is emitted when the CLI compacts conversation history
// (either on demand via /compact or automatically when approaching the context
// limit).
type CompactBoundaryPayload struct {
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id"`
	CompactMetadata CompactMetadata `json:"compact_metadata"`
}

// PostTurnSummaryPayload is an internal background summary emitted after each
// assistant turn. summarizes_uuid points to the assistant message summarized.
type PostTurnSummaryPayload struct {
	SummarizesUUID string   `json:"summarizes_uuid"`
	StatusCategory string   `json:"status_category"`
	StatusDetail   string   `json:"status_detail"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	RecentAction   string   `json:"recent_action"`
	NeedsAction    string   `json:"needs_action"`
	UUID           string   `json:"uuid"`
	SessionID      string   `json:"session_id"`
	ArtifactURLs   []string `json:"artifact_urls"`
	IsNoteworthy   bool     `json:"is_noteworthy"`
}

// APIRetryPayload is emitted when an API request fails with a retryable error
// and will be retried after a delay. ErrorStatus is null for connection errors
// (e.g. timeouts) that had no HTTP response.
type APIRetryPayload struct {
	ErrorStatus  *int   `json:"error_status"`
	Error        string `json:"error"`
	UUID         string `json:"uuid"`
	SessionID    string `json:"session_id"`
	Attempt      int    `json:"attempt"`
	MaxRetries   int    `json:"max_retries"`
	RetryDelayMs int    `json:"retry_delay_ms"`
}

// LocalCommandOutputPayload carries output from a local slash command
// (e.g. /voice, /cost). Displayed as assistant-style text in the transcript.
type LocalCommandOutputPayload struct {
	Content   string `json:"content"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// HookStartedPayload marks the start of a hook execution.
type HookStartedPayload struct {
	HookID    string `json:"hook_id"`
	HookName  string `json:"hook_name"`
	HookEvent string `json:"hook_event"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// HookProgressPayload streams partial stdout/stderr from a running hook.
type HookProgressPayload struct {
	HookID    string `json:"hook_id"`
	HookName  string `json:"hook_name"`
	HookEvent string `json:"hook_event"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Output    string `json:"output"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// HookResponsePayload marks the completion of a hook with its final output
// and outcome.
type HookResponsePayload struct {
	ExitCode  *int   `json:"exit_code,omitempty"`
	HookID    string `json:"hook_id"`
	HookName  string `json:"hook_name"`
	HookEvent string `json:"hook_event"`
	Output    string `json:"output"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Outcome   string `json:"outcome"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// TaskUsage is the shared usage stats carried on task lifecycle payloads.
type TaskUsage struct {
	TotalTokens int   `json:"total_tokens"`
	ToolUses    int   `json:"tool_uses"`
	DurationMs  int64 `json:"duration_ms"`
}

// TaskNotificationPayload signals completion (or failure/stop) of a background
// task spawned by the CLI (subagent, workflow, etc.).
type TaskNotificationPayload struct {
	ToolUseID  *string   `json:"tool_use_id,omitempty"`
	TaskID     string    `json:"task_id"`
	Status     string    `json:"status"`
	OutputFile string    `json:"output_file"`
	Summary    string    `json:"summary"`
	UUID       string    `json:"uuid"`
	SessionID  string    `json:"session_id"`
	Usage      TaskUsage `json:"usage,omitempty"`
}

// TaskStartedPayload marks the start of a background task.
type TaskStartedPayload struct {
	ToolUseID    *string `json:"tool_use_id,omitempty"`
	WorkflowName *string `json:"workflow_name,omitempty"`
	TaskID       string  `json:"task_id"`
	Description  string  `json:"description"`
	TaskType     string  `json:"task_type,omitempty"`
	Prompt       string  `json:"prompt,omitempty"`
	UUID         string  `json:"uuid"`
	SessionID    string  `json:"session_id"`
}

// TaskProgressPayload streams incremental progress from a running background
// task.
type TaskProgressPayload struct {
	ToolUseID    *string   `json:"tool_use_id,omitempty"`
	TaskID       string    `json:"task_id"`
	Description  string    `json:"description"`
	LastToolName string    `json:"last_tool_name,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	UUID         string    `json:"uuid"`
	SessionID    string    `json:"session_id"`
	Usage        TaskUsage `json:"usage"`
}

// SessionStateChangedPayload mirrors the CLI's own session state machine.
// "idle" is the authoritative turn-over signal (fires after heldBackResult
// flushes and the background-agent loop exits).
type SessionStateChangedPayload struct {
	State     string `json:"state"`
	UUID      string `json:"uuid"`
	SessionID string `json:"session_id"`
}

// PersistedFile describes a single successfully persisted file.
type PersistedFile struct {
	Filename string `json:"filename"`
	FileID   string `json:"file_id"`
}

// PersistedFileFailure describes a single failed persistence attempt.
type PersistedFileFailure struct {
	Filename string `json:"filename"`
	Error    string `json:"error"`
}

// FilesPersistedPayload reports the outcome of a batch file-save operation.
type FilesPersistedPayload struct {
	ProcessedAt string                 `json:"processed_at"`
	UUID        string                 `json:"uuid"`
	SessionID   string                 `json:"session_id"`
	Files       []PersistedFile        `json:"files"`
	Failed      []PersistedFileFailure `json:"failed"`
}

// ElicitationCompletePayload is emitted when an MCP server confirms that a
// URL-mode elicitation has completed.
type ElicitationCompletePayload struct {
	MCPServerName string `json:"mcp_server_name"`
	ElicitationID string `json:"elicitation_id"`
	UUID          string `json:"uuid"`
	SessionID     string `json:"session_id"`
}

// rawOrRemarshal returns m.raw if populated, otherwise re-marshals m. This
// supports both parsed-from-wire SystemMessages (raw set by UnmarshalJSON) and
// programmatically-constructed ones (used in tests).
func (m SystemMessage) rawOrRemarshal() ([]byte, error) {
	if len(m.raw) > 0 {
		return m.raw, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("remarshal SystemMessage: %w", err)
	}
	return b, nil
}

// DecodePayload returns the typed payload struct for m.Subtype. Unknown
// subtypes log at debug level and return (nil, nil) so callers can fall back
// to the flat SystemMessage fields.
func (m SystemMessage) DecodePayload() (any, error) {
	data, err := m.rawOrRemarshal()
	if err != nil {
		return nil, err
	}
	decode := func(dst any) (any, error) {
		if err := json.Unmarshal(data, dst); err != nil {
			return nil, fmt.Errorf("decode system payload %q: %w", m.Subtype, err)
		}
		return dst, nil
	}
	switch SystemSubtype(m.Subtype) {
	case SystemSubtypeInit:
		return decode(&SystemInitPayload{})
	case SystemSubtypeStatus:
		return decode(&SystemStatusPayload{})
	case SystemSubtypeCompactBoundary:
		return decode(&CompactBoundaryPayload{})
	case SystemSubtypePostTurnSummary:
		return decode(&PostTurnSummaryPayload{})
	case SystemSubtypeAPIRetry:
		return decode(&APIRetryPayload{})
	case SystemSubtypeLocalCommandOutput:
		return decode(&LocalCommandOutputPayload{})
	case SystemSubtypeHookStarted:
		return decode(&HookStartedPayload{})
	case SystemSubtypeHookProgress:
		return decode(&HookProgressPayload{})
	case SystemSubtypeHookResponse:
		return decode(&HookResponsePayload{})
	case SystemSubtypeTaskNotification:
		return decode(&TaskNotificationPayload{})
	case SystemSubtypeTaskStarted:
		return decode(&TaskStartedPayload{})
	case SystemSubtypeTaskProgress:
		return decode(&TaskProgressPayload{})
	case SystemSubtypeSessionStateChanged:
		return decode(&SessionStateChangedPayload{})
	case SystemSubtypeFilesPersisted:
		return decode(&FilesPersistedPayload{})
	case SystemSubtypeElicitationComplete:
		return decode(&ElicitationCompletePayload{})
	default:
		slog.Debug("unknown system subtype", "subtype", m.Subtype)
		return nil, nil
	}
}

// AsInit returns the init payload if m.Subtype is "init".
func (m SystemMessage) AsInit() (*SystemInitPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*SystemInitPayload)
	return v, ok
}

// AsStatus returns the status payload if m.Subtype is "status".
func (m SystemMessage) AsStatus() (*SystemStatusPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*SystemStatusPayload)
	return v, ok
}

// AsCompactBoundary returns the compact_boundary payload.
func (m SystemMessage) AsCompactBoundary() (*CompactBoundaryPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*CompactBoundaryPayload)
	return v, ok
}

// AsPostTurnSummary returns the post_turn_summary payload.
func (m SystemMessage) AsPostTurnSummary() (*PostTurnSummaryPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*PostTurnSummaryPayload)
	return v, ok
}

// AsAPIRetry returns the api_retry payload.
func (m SystemMessage) AsAPIRetry() (*APIRetryPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*APIRetryPayload)
	return v, ok
}

// AsLocalCommandOutput returns the local_command_output payload.
func (m SystemMessage) AsLocalCommandOutput() (*LocalCommandOutputPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*LocalCommandOutputPayload)
	return v, ok
}

// AsHookStarted returns the hook_started payload.
func (m SystemMessage) AsHookStarted() (*HookStartedPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*HookStartedPayload)
	return v, ok
}

// AsHookProgress returns the hook_progress payload.
func (m SystemMessage) AsHookProgress() (*HookProgressPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*HookProgressPayload)
	return v, ok
}

// AsHookResponse returns the hook_response payload.
func (m SystemMessage) AsHookResponse() (*HookResponsePayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*HookResponsePayload)
	return v, ok
}

// AsTaskNotification returns the task_notification payload.
func (m SystemMessage) AsTaskNotification() (*TaskNotificationPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*TaskNotificationPayload)
	return v, ok
}

// AsTaskStarted returns the task_started payload.
func (m SystemMessage) AsTaskStarted() (*TaskStartedPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*TaskStartedPayload)
	return v, ok
}

// AsTaskProgress returns the task_progress payload.
func (m SystemMessage) AsTaskProgress() (*TaskProgressPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*TaskProgressPayload)
	return v, ok
}

// AsSessionStateChanged returns the session_state_changed payload.
func (m SystemMessage) AsSessionStateChanged() (*SessionStateChangedPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*SessionStateChangedPayload)
	return v, ok
}

// AsFilesPersisted returns the files_persisted payload.
func (m SystemMessage) AsFilesPersisted() (*FilesPersistedPayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*FilesPersistedPayload)
	return v, ok
}

// AsElicitationComplete returns the elicitation_complete payload.
func (m SystemMessage) AsElicitationComplete() (*ElicitationCompletePayload, bool) {
	p, _ := m.DecodePayload()
	v, ok := p.(*ElicitationCompletePayload)
	return v, ok
}
