package agent

import (
	"encoding/json"
	"log/slog"
)

// HandleApproval processes an approval request from the Codex app-server.
// High-trust policy: auto-approve command and file-change requests.
// Spec Section 10.5.
func HandleApproval(proto *Protocol, msg *Message, logger *slog.Logger) {
	if msg.ID == nil {
		return
	}

	var params struct {
		ToolName string `json:"toolName"`
	}
	if msg.Params != nil {
		json.Unmarshal(msg.Params, &params)
	}

	logger.Debug("auto-approving tool", "tool", params.ToolName)
	proto.Respond(msg.ID, map[string]any{"approved": true})
}

// HandleToolCall processes an unsupported dynamic tool call.
// Returns a failure response and continues the session (doesn't stall).
// Spec Section 10.5.
func HandleToolCall(proto *Protocol, msg *Message, logger *slog.Logger) {
	if msg.ID == nil {
		return
	}

	var params struct {
		ToolName string `json:"name"`
	}
	if msg.Params != nil {
		json.Unmarshal(msg.Params, &params)
	}

	logger.Warn("unsupported tool call, rejecting", "tool", params.ToolName)
	proto.Respond(msg.ID, map[string]any{
		"success": false,
		"error":   "unsupported_tool_call",
	})
}
