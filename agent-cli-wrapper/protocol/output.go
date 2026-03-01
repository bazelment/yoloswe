package protocol

// NewUserTextMessage constructs a UserMessageToSend with a plain text string.
func NewUserTextMessage(text string) UserMessageToSend {
	return UserMessageToSend{
		Type: "user",
		Message: UserMessageToSendInner{
			Role:    "user",
			Content: text,
		},
	}
}

// NewPermissionAllow constructs a control_response that grants tool execution.
//
// input must be a non-nil map; pass the original CanUseToolRequest.Input when
// no modifications are needed (the wire format forbids a null updatedInput).
// perms may be nil if no permission rule updates are required.
func NewPermissionAllow(requestID string, input map[string]interface{}, perms []PermissionUpdate) ControlResponse {
	if input == nil {
		input = map[string]interface{}{}
	}
	return ControlResponse{
		Type: MessageTypeControlResponse,
		Response: ControlResponsePayload{
			Subtype:   "success",
			RequestID: requestID,
			Response: PermissionResultAllow{
				Behavior:           PermissionBehaviorAllow,
				UpdatedInput:       input,
				UpdatedPermissions: perms,
			},
		},
	}
}

// NewPermissionDeny constructs a control_response that blocks tool execution.
//
// message is the human-readable reason shown to the user.
// interrupt signals Claude to stop the current turn rather than continue.
func NewPermissionDeny(requestID string, message string, interrupt bool) ControlResponse {
	return ControlResponse{
		Type: MessageTypeControlResponse,
		Response: ControlResponsePayload{
			Subtype:   "success",
			RequestID: requestID,
			Response: PermissionResultDeny{
				Behavior:  PermissionBehaviorDeny,
				Message:   message,
				Interrupt: interrupt,
			},
		},
	}
}

// NewMCPResponse constructs a control_response wrapping an MCP result.
// result is typically a JSONRPCResponse (for success or error alike).
func NewMCPResponse(requestID string, result interface{}) ControlResponse {
	return ControlResponse{
		Type: MessageTypeControlResponse,
		Response: ControlResponsePayload{
			Subtype:   "success",
			RequestID: requestID,
			Response:  MCPResponsePayload{MCPResponse: result},
		},
	}
}

// NewInterrupt constructs a control_request that interrupts the current turn.
func NewInterrupt(requestID string) ControlRequestToSend {
	return ControlRequestToSend{
		Type:      "control_request",
		RequestID: requestID,
		Request:   InterruptRequestToSend{Subtype: string(ControlRequestSubtypeInterrupt)},
	}
}

// NewSetPermissionMode constructs a control_request that changes the CLI permission mode.
func NewSetPermissionMode(requestID, mode string) ControlRequestToSend {
	return ControlRequestToSend{
		Type:      "control_request",
		RequestID: requestID,
		Request:   SetPermissionModeRequestToSend{Subtype: string(ControlRequestSubtypeSetPermissionMode), Mode: mode},
	}
}

// NewSetModel constructs a control_request that switches the active model.
func NewSetModel(requestID, model string) ControlRequestToSend {
	return ControlRequestToSend{
		Type:      "control_request",
		RequestID: requestID,
		Request:   SetModelRequestToSend{Subtype: "set_model", Model: model},
	}
}
