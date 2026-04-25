package protocol

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// ControlRequest wraps control messages from CLI.
type ControlRequest struct {
	Type      MessageType     `json:"type"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// MsgType returns the message type.
func (m ControlRequest) MsgType() MessageType { return MessageTypeControlRequest }

// ParsedRequest parses the inner request from a ControlRequest.
func (m ControlRequest) ParsedRequest() (ControlRequestData, error) {
	return ParseControlRequest(m.Request)
}

// ControlRequestSubtype is the subtype of a control request.
type ControlRequestSubtype string

const (
	ControlRequestSubtypeCanUseTool           ControlRequestSubtype = "can_use_tool"
	ControlRequestSubtypeSetPermissionMode    ControlRequestSubtype = "set_permission_mode"
	ControlRequestSubtypeInterrupt            ControlRequestSubtype = "interrupt"
	ControlRequestSubtypeMCPMessage           ControlRequestSubtype = "mcp_message"
	ControlRequestSubtypeSetModel             ControlRequestSubtype = "set_model"
	ControlRequestSubtypeInitialize           ControlRequestSubtype = "initialize"
	ControlRequestSubtypeSetMaxThinkingTokens ControlRequestSubtype = "set_max_thinking_tokens"
	ControlRequestSubtypeMCPStatus            ControlRequestSubtype = "mcp_status"
	ControlRequestSubtypeMCPSetServers        ControlRequestSubtype = "mcp_set_servers"
	ControlRequestSubtypeMCPReconnect         ControlRequestSubtype = "mcp_reconnect"
	ControlRequestSubtypeMCPToggle            ControlRequestSubtype = "mcp_toggle"
	ControlRequestSubtypeGetContextUsage      ControlRequestSubtype = "get_context_usage"
	ControlRequestSubtypeReloadPlugins        ControlRequestSubtype = "reload_plugins"
	ControlRequestSubtypeRewindFiles          ControlRequestSubtype = "rewind_files"
	ControlRequestSubtypeCancelAsyncMessage   ControlRequestSubtype = "cancel_async_message"
	ControlRequestSubtypeSeedReadState        ControlRequestSubtype = "seed_read_state"
	ControlRequestSubtypeHookCallback         ControlRequestSubtype = "hook_callback"
	ControlRequestSubtypeGetSettings          ControlRequestSubtype = "get_settings"
	ControlRequestSubtypeApplyFlagSettings    ControlRequestSubtype = "apply_flag_settings"
	ControlRequestSubtypeStopTask             ControlRequestSubtype = "stop_task"
	ControlRequestSubtypeElicitation          ControlRequestSubtype = "elicitation"
)

// ControlResponseSubtype constants for response payloads.
const (
	ControlResponseSubtypeSuccess = "success"
)

// ControlRequestData is the interface for control request discrimination.
type ControlRequestData interface {
	Subtype() ControlRequestSubtype
}

// CanUseToolRequest asks permission for tool use.
type CanUseToolRequest struct {
	Input                 map[string]interface{} `json:"input"`
	BlockedPath           *string                `json:"blocked_path,omitempty"`
	SubtypeField          ControlRequestSubtype  `json:"subtype"`
	ToolName              string                 `json:"tool_name"`
	PermissionSuggestions []interface{}          `json:"permission_suggestions,omitempty"`
}

// Subtype returns the control request subtype.
func (r CanUseToolRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// SetPermissionModeRequest changes the permission mode.
type SetPermissionModeRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	Mode         string                `json:"mode"`
}

// Subtype returns the control request subtype.
func (r SetPermissionModeRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// InterruptRequest signals an interrupt.
type InterruptRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r InterruptRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// SetModelRequest is an inbound set_model control request.
type SetModelRequest struct {
	Model        *string               `json:"model,omitempty"`
	SubtypeField ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r SetModelRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// SetMaxThinkingTokensRequest sets the max thinking tokens.
type SetMaxThinkingTokensRequest struct {
	MaxThinkingTokens *int                  `json:"max_thinking_tokens"`
	SubtypeField      ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r SetMaxThinkingTokensRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// MCPStatusRequest asks for MCP server status.
type MCPStatusRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r MCPStatusRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// MCPSetServersRequest replaces the set of dynamically managed MCP servers.
// Server config shape depends on transport; kept as RawMessage since
// MCPServerConfig is not currently defined in protocol/mcp.go.
type MCPSetServersRequest struct {
	Servers      map[string]json.RawMessage `json:"servers"`
	SubtypeField ControlRequestSubtype      `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r MCPSetServersRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// MCPReconnectRequest reconnects a specific MCP server.
// Note: upstream field is camelCase `serverName` (not snake_case).
type MCPReconnectRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	ServerName   string                `json:"serverName"`
}

// Subtype returns the control request subtype.
func (r MCPReconnectRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// MCPToggleRequest enables or disables an MCP server.
// Note: upstream field is camelCase `serverName`.
type MCPToggleRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	ServerName   string                `json:"serverName"`
	Enabled      bool                  `json:"enabled"`
}

// Subtype returns the control request subtype.
func (r MCPToggleRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// GetContextUsageRequest asks for context window usage.
type GetContextUsageRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r GetContextUsageRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// ReloadPluginsRequest reloads plugins from disk.
type ReloadPluginsRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r ReloadPluginsRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// RewindFilesRequest rewinds file changes made since a specific user message.
type RewindFilesRequest struct {
	DryRun        *bool                 `json:"dry_run,omitempty"`
	SubtypeField  ControlRequestSubtype `json:"subtype"`
	UserMessageID string                `json:"user_message_id"`
}

// Subtype returns the control request subtype.
func (r RewindFilesRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// CancelAsyncMessageRequest drops a pending async user message from the queue.
type CancelAsyncMessageRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	MessageUUID  string                `json:"message_uuid"`
}

// Subtype returns the control request subtype.
func (r CancelAsyncMessageRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// SeedReadStateRequest seeds the readFileState cache.
type SeedReadStateRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	Path         string                `json:"path"`
	Mtime        float64               `json:"mtime"`
}

// Subtype returns the control request subtype.
func (r SeedReadStateRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// HookCallbackRequest delivers a hook callback and its input.
type HookCallbackRequest struct {
	Input        map[string]any        `json:"input"`
	ToolUseID    *string               `json:"tool_use_id,omitempty"`
	SubtypeField ControlRequestSubtype `json:"subtype"`
	CallbackID   string                `json:"callback_id"`
}

// Subtype returns the control request subtype.
func (r HookCallbackRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// GetSettingsRequest returns effective merged settings.
type GetSettingsRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r GetSettingsRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// ApplyFlagSettingsRequest merges settings into the flag settings layer.
type ApplyFlagSettingsRequest struct {
	Settings     map[string]any        `json:"settings"`
	SubtypeField ControlRequestSubtype `json:"subtype"`
}

// Subtype returns the control request subtype.
func (r ApplyFlagSettingsRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// StopTaskRequest stops a running task.
type StopTaskRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	TaskID       string                `json:"task_id"`
}

// Subtype returns the control request subtype.
func (r StopTaskRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// ElicitationRequest asks the SDK consumer to handle an MCP elicitation.
type ElicitationRequest struct {
	Mode            *string               `json:"mode,omitempty"`
	URL             *string               `json:"url,omitempty"`
	RequestedSchema map[string]any        `json:"requested_schema,omitempty"`
	SubtypeField    ControlRequestSubtype `json:"subtype"`
	MCPServerName   string                `json:"mcp_server_name"`
	Message         string                `json:"message"`
	ElicitationID   string                `json:"elicitation_id,omitempty"`
}

// Subtype returns the control request subtype.
func (r ElicitationRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// InitializeRequest initializes the SDK session.
// Fields with complex recursive shapes (Hooks, Agents, JSONSchema) are kept
// as loose maps/slices for now.
type InitializeRequest struct {
	Hooks                  map[string]any        `json:"hooks,omitempty"`
	JSONSchema             map[string]any        `json:"jsonSchema,omitempty"`
	Agents                 map[string]any        `json:"agents,omitempty"`
	SystemPrompt           *string               `json:"systemPrompt,omitempty"`
	AppendSystemPrompt     *string               `json:"appendSystemPrompt,omitempty"`
	PromptSuggestions      *bool                 `json:"promptSuggestions,omitempty"`
	AgentProgressSummaries *bool                 `json:"agentProgressSummaries,omitempty"`
	SubtypeField           ControlRequestSubtype `json:"subtype"`
	SDKMCPServers          []any                 `json:"sdkMcpServers,omitempty"`
}

// Subtype returns the control request subtype.
func (r InitializeRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// UnknownControlRequest preserves the raw payload for subtypes we don't
// yet recognize, so callers can still route them.
type UnknownControlRequest struct {
	SubtypeField ControlRequestSubtype `json:"subtype"`
	Raw          json.RawMessage       `json:"-"`
}

// Subtype returns the control request subtype.
func (r UnknownControlRequest) Subtype() ControlRequestSubtype { return r.SubtypeField }

// ---------------------------------------------------------------------------
// Control response payloads (inner Response field of ControlResponsePayload).
// ---------------------------------------------------------------------------

// InitializeResponse is the response to an initialize request.
type InitializeResponse struct {
	OutputStyle           *string  `json:"output_style,omitempty"`
	FastModeState         *string  `json:"fast_mode_state,omitempty"`
	PID                   *int     `json:"pid,omitempty"`
	Account               any      `json:"account,omitempty"`
	Commands              []any    `json:"commands"`
	Agents                []any    `json:"agents"`
	AvailableOutputStyles []string `json:"available_output_styles"`
	Models                []any    `json:"models"`
}

// MCPServerStatus describes the status of a single MCP server.
type MCPServerStatus struct {
	ServerInfo   any     `json:"serverInfo,omitempty"`
	Config       any     `json:"config,omitempty"`
	Capabilities any     `json:"capabilities,omitempty"`
	Error        *string `json:"error,omitempty"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	Tools        []any   `json:"tools,omitempty"`
}

// MCPStatusResponse is the response to an mcp_status request.
type MCPStatusResponse struct {
	McpServers []MCPServerStatus `json:"mcpServers"`
}

// GetContextUsageResponse is the response to a get_context_usage request.
// TODO: shape is complex (categories, gridRows, memoryFiles, mcpTools,
// messageBreakdown, apiUsage, etc). Kept as raw map until we need to consume.
type GetContextUsageResponse map[string]json.RawMessage

// RewindFilesResponse is the response to a rewind_files request.
type RewindFilesResponse struct {
	Error        *string  `json:"error,omitempty"`
	FilesChanged []string `json:"filesChanged,omitempty"`
	CanRewind    bool     `json:"canRewind"`
	Insertions   int      `json:"insertions,omitempty"`
	Deletions    int      `json:"deletions,omitempty"`
}

// GetSettingsResponse is the response to a get_settings request.
type GetSettingsResponse struct {
	Effective map[string]any `json:"effective"`
	Applied   map[string]any `json:"applied,omitempty"`
	Sources   []any          `json:"sources"`
}

// ReloadPluginsResponse is the response to a reload_plugins request.
type ReloadPluginsResponse struct {
	Commands   []any `json:"commands"`
	Agents     []any `json:"agents"`
	Plugins    []any `json:"plugins"`
	McpServers []any `json:"mcpServers"`
	ErrorCount int   `json:"error_count"`
}

// MCPSetServersResponse is the response to an mcp_set_servers request.
type MCPSetServersResponse struct {
	Errors  map[string]string `json:"errors"`
	Added   []string          `json:"added"`
	Removed []string          `json:"removed"`
}

// ElicitationResponse is sent back by the SDK consumer for elicitation.
type ElicitationResponse struct {
	Content map[string]any `json:"content,omitempty"`
	Action  string         `json:"action"`
}

// CancelAsyncMessageResponse is the response to a cancel_async_message request.
type CancelAsyncMessageResponse struct {
	Cancelled bool `json:"cancelled"`
}

// ParseControlRequest parses the inner request from a ControlRequest.
func ParseControlRequest(data json.RawMessage) (ControlRequestData, error) {
	var base struct {
		Subtype ControlRequestSubtype `json:"subtype"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, err
	}

	switch base.Subtype {
	case ControlRequestSubtypeCanUseTool:
		var r CanUseToolRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeSetPermissionMode:
		var r SetPermissionModeRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeInterrupt:
		var r InterruptRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeMCPMessage:
		var r MCPMessageRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeSetModel:
		var r SetModelRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeInitialize:
		var r InitializeRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeSetMaxThinkingTokens:
		var r SetMaxThinkingTokensRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeMCPStatus:
		var r MCPStatusRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeMCPSetServers:
		var r MCPSetServersRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeMCPReconnect:
		var r MCPReconnectRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeMCPToggle:
		var r MCPToggleRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeGetContextUsage:
		var r GetContextUsageRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeReloadPlugins:
		var r ReloadPluginsRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeRewindFiles:
		var r RewindFilesRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeCancelAsyncMessage:
		var r CancelAsyncMessageRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeSeedReadState:
		var r SeedReadStateRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeHookCallback:
		var r HookCallbackRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeGetSettings:
		var r GetSettingsRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeApplyFlagSettings:
		var r ApplyFlagSettingsRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeStopTask:
		var r StopTaskRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	case ControlRequestSubtypeElicitation:
		var r ElicitationRequest
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return r, nil
	default:
		slog.Warn("skipping unknown control request subtype", "subtype", base.Subtype)
		return UnknownControlRequest{SubtypeField: base.Subtype, Raw: data}, nil
	}
}

// ControlResponse wraps responses sent to CLI.
type ControlResponse struct {
	Type     MessageType            `json:"type"`
	Response ControlResponsePayload `json:"response"`
}

// MsgType returns the message type.
func (m ControlResponse) MsgType() MessageType { return MessageTypeControlResponse }

// Marshal serializes the control response to a JSON line ready to write to the CLI.
func (m ControlResponse) Marshal() ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal ControlResponse: %w", err)
	}
	return b, nil
}

// ControlResponsePayload is the inner response payload.
type ControlResponsePayload struct {
	Subtype   string      `json:"subtype"`
	RequestID string      `json:"request_id"`
	Response  interface{} `json:"response,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// PermissionBehavior is the behavior for a permission response.
type PermissionBehavior string

const (
	PermissionBehaviorAllow PermissionBehavior = "allow"
	PermissionBehaviorDeny  PermissionBehavior = "deny"
)

// PermissionResultAllow allows tool execution.
// Wire format notes (per Python SDK behavior):
// - updatedInput MUST be an object (record), never null - use original input as fallback
// - updatedPermissions can be omitted if nil
type PermissionResultAllow struct {
	Behavior           PermissionBehavior     `json:"behavior"`
	UpdatedInput       map[string]interface{} `json:"updatedInput"`
	UpdatedPermissions []PermissionUpdate     `json:"updatedPermissions,omitempty"`
}

// PermissionResultDeny denies tool execution.
type PermissionResultDeny struct {
	Behavior  PermissionBehavior `json:"behavior"`
	Message   string             `json:"message,omitempty"`
	Interrupt bool               `json:"interrupt,omitempty"`
}

// PermissionUpdate describes a permission rule update.
type PermissionUpdate struct {
	Type        string           `json:"type"`
	Behavior    string           `json:"behavior,omitempty"`
	Mode        string           `json:"mode,omitempty"`
	Destination string           `json:"destination,omitempty"`
	Rules       []PermissionRule `json:"rules,omitempty"`
	Directories []string         `json:"directories,omitempty"`
}

// PermissionRule describes a single permission rule.
type PermissionRule struct {
	ToolName    string `json:"tool_name"`
	RuleContent string `json:"rule_content,omitempty"`
}

// ControlRequestToSend is a control request we send to the CLI.
type ControlRequestToSend struct {
	Request   interface{} `json:"request"`
	Type      string      `json:"type"`
	RequestID string      `json:"request_id"`
}

// Marshal serializes the control request to a JSON line ready to write to the CLI.
func (m ControlRequestToSend) Marshal() ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal ControlRequestToSend: %w", err)
	}
	return b, nil
}

// SetPermissionModeRequestToSend is the request body for setting permission mode.
type SetPermissionModeRequestToSend struct {
	Subtype string `json:"subtype"`
	Mode    string `json:"mode"`
}

// InterruptRequestToSend is the request body for interrupting.
type InterruptRequestToSend struct {
	Subtype string `json:"subtype"`
}

// SetModelRequestToSend is the request body for setting the model.
type SetModelRequestToSend struct {
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
}

// GetContextUsageRequestToSend is the request body for context usage.
type GetContextUsageRequestToSend struct {
	Subtype string `json:"subtype"`
}

// GetSettingsRequestToSend is the request body for current settings.
type GetSettingsRequestToSend struct {
	Subtype string `json:"subtype"`
}

// ApplyFlagSettingsRequestToSend is the request body for session flag settings.
type ApplyFlagSettingsRequestToSend struct {
	Settings map[string]any `json:"settings"`
	Subtype  string         `json:"subtype"`
}

// ToolUseRequest contains parsed information about a tool use from a control request.
type ToolUseRequest struct {
	Input       map[string]interface{}
	BlockedPath *string
	RequestID   string
	ToolName    string
}

// ParseToolUseRequest extracts tool use information from a control request.
// Returns nil if the request is not a can_use_tool request.
func ParseToolUseRequest(msg ControlRequest) *ToolUseRequest {
	reqData, err := ParseControlRequest(msg.Request)
	if err != nil {
		return nil
	}

	canUseTool, ok := reqData.(CanUseToolRequest)
	if !ok {
		return nil
	}

	return &ToolUseRequest{
		RequestID:   msg.RequestID,
		ToolName:    canUseTool.ToolName,
		Input:       canUseTool.Input,
		BlockedPath: canUseTool.BlockedPath,
	}
}
