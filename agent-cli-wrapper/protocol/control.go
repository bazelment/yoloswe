package protocol

import "encoding/json"

// ControlRequest wraps control messages from CLI.
type ControlRequest struct {
	Type      MessageType     `json:"type"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// MsgType returns the message type.
func (m ControlRequest) MsgType() MessageType { return MessageTypeControlRequest }

// ControlRequestSubtype is the subtype of a control request.
type ControlRequestSubtype string

const (
	ControlRequestSubtypeCanUseTool        ControlRequestSubtype = "can_use_tool"
	ControlRequestSubtypeSetPermissionMode ControlRequestSubtype = "set_permission_mode"
	ControlRequestSubtypeInterrupt         ControlRequestSubtype = "interrupt"
)

// ControlRequestData is the interface for control request discrimination.
type ControlRequestData interface {
	Subtype() ControlRequestSubtype
}

// CanUseToolRequest asks permission for tool use.
type CanUseToolRequest struct {
	SubtypeField          ControlRequestSubtype  `json:"subtype"`
	ToolName              string                 `json:"tool_name"`
	Input                 map[string]interface{} `json:"input"`
	PermissionSuggestions []interface{}          `json:"permission_suggestions,omitempty"`
	BlockedPath           *string                `json:"blocked_path,omitempty"`
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
	default:
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
	Rules       []PermissionRule `json:"rules,omitempty"`
	Behavior    string           `json:"behavior,omitempty"`
	Mode        string           `json:"mode,omitempty"`
	Directories []string         `json:"directories,omitempty"`
	Destination string           `json:"destination,omitempty"`
}

// PermissionRule describes a single permission rule.
type PermissionRule struct {
	ToolName    string `json:"tool_name"`
	RuleContent string `json:"rule_content,omitempty"`
}

// ControlRequestToSend is a control request we send to the CLI.
type ControlRequestToSend struct {
	Type      string      `json:"type"`
	RequestID string      `json:"request_id"`
	Request   interface{} `json:"request"`
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
	RequestID   string                 // The control request ID
	ToolName    string                 // Name of the tool being used
	Input       map[string]interface{} // Tool input parameters
	BlockedPath *string                // Path that triggered permission (if any)
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
