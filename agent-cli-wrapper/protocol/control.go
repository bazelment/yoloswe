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
	ControlRequestSubtypeCanUseTool        ControlRequestSubtype = "can_use_tool"
	ControlRequestSubtypeSetPermissionMode ControlRequestSubtype = "set_permission_mode"
	ControlRequestSubtypeInterrupt         ControlRequestSubtype = "interrupt"
	ControlRequestSubtypeMCPMessage        ControlRequestSubtype = "mcp_message"
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
	default:
		slog.Warn("skipping unknown control request subtype", "subtype", base.Subtype)
		return nil, nil
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
