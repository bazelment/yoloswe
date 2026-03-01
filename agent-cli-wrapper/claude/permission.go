package claude

import (
	"context"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// PermissionRequest contains the data for a permission request.
type PermissionRequest struct {
	Input       map[string]interface{}
	BlockedPath *string
	RequestID   string
	ToolName    string
}

// PermissionResponse contains the response to a permission request.
//
// Wire format notes (per Python SDK behavior):
//   - UpdatedInput: If nil, the permissionManager will use the original input from
//     the request as fallback. The wire format requires updatedInput to be an object,
//     NEVER null. Handlers that want to use the original input unchanged can leave
//     this nil - the manager handles the fallback.
//   - UpdatedPermissions: Can be nil (will be omitted from wire format). Only set
//     this when you want to add permission rules.
type PermissionResponse struct {
	UpdatedInput       map[string]interface{}
	Behavior           PermissionBehavior
	Message            string
	UpdatedPermissions []protocol.PermissionUpdate
	Interrupt          bool
}

// PermissionBehavior is the behavior for a permission response.
type PermissionBehavior string

const (
	// PermissionAllow allows tool execution.
	PermissionAllow PermissionBehavior = "allow"
	// PermissionDeny denies tool execution.
	PermissionDeny PermissionBehavior = "deny"
)

// PermissionHandler handles permission requests.
type PermissionHandler interface {
	HandlePermission(ctx context.Context, req *PermissionRequest) (*PermissionResponse, error)
}

// PermissionHandlerFunc is a function that implements PermissionHandler.
type PermissionHandlerFunc func(ctx context.Context, req *PermissionRequest) (*PermissionResponse, error)

// HandlePermission implements PermissionHandler.
func (f PermissionHandlerFunc) HandlePermission(ctx context.Context, req *PermissionRequest) (*PermissionResponse, error) {
	return f(ctx, req)
}

// permissionManager handles permission request/response flow.
type permissionManager struct {
	handler PermissionHandler
}

// newPermissionManager creates a new permission manager.
func newPermissionManager(handler PermissionHandler) *permissionManager {
	return &permissionManager{
		handler: handler,
	}
}

// ExtractPermissionRequest extracts a permission request from a control request.
// Returns nil if the control request is not a permission request.
func (pm *permissionManager) ExtractPermissionRequest(msg protocol.ControlRequest) *PermissionRequest {
	toolReq := protocol.ParseToolUseRequest(msg)
	if toolReq == nil {
		return nil
	}

	return &PermissionRequest{
		RequestID:   toolReq.RequestID,
		ToolName:    toolReq.ToolName,
		Input:       toolReq.Input,
		BlockedPath: toolReq.BlockedPath,
	}
}

// HandleRequest handles a permission request and returns a control response.
func (pm *permissionManager) HandleRequest(ctx context.Context, msg protocol.ControlRequest) (*protocol.ControlResponse, error) {
	req := pm.ExtractPermissionRequest(msg)
	if req == nil {
		// Not a permission request
		return nil, nil
	}

	if pm.handler == nil {
		// No handler configured, auto-deny
		return pm.buildDenyResponse(msg.RequestID, "No permission handler configured", false), nil
	}

	resp, err := pm.handler.HandlePermission(ctx, req)
	if err != nil {
		return pm.buildDenyResponse(msg.RequestID, "Permission handler error", false), err
	}

	return pm.buildResponse(msg.RequestID, resp, req.Input), nil
}

// buildResponse builds a control response from a permission response.
// originalInput is used as fallback when resp.UpdatedInput is nil (per Python SDK behavior).
func (pm *permissionManager) buildResponse(requestID string, resp *PermissionResponse, originalInput map[string]interface{}) *protocol.ControlResponse {
	if resp.Behavior == PermissionAllow {
		// Per Python SDK: updatedInput must be an object, never null
		// If user doesn't provide updated input, use the original input
		updatedInput := resp.UpdatedInput
		if updatedInput == nil {
			updatedInput = originalInput
		}
		// Ensure we never send nil - use empty map as last resort
		if updatedInput == nil {
			updatedInput = make(map[string]interface{})
		}

		result := protocol.NewPermissionAllow(requestID, updatedInput, resp.UpdatedPermissions)
		return &result
	}

	return pm.buildDenyResponse(requestID, resp.Message, resp.Interrupt)
}

// buildDenyResponse builds a deny control response.
func (pm *permissionManager) buildDenyResponse(requestID, message string, interrupt bool) *protocol.ControlResponse {
	result := protocol.NewPermissionDeny(requestID, message, interrupt)
	return &result
}

// DefaultPermissionHandler returns a handler that denies all permissions.
func DefaultPermissionHandler() PermissionHandler {
	return PermissionHandlerFunc(func(ctx context.Context, req *PermissionRequest) (*PermissionResponse, error) {
		return &PermissionResponse{
			Behavior: PermissionDeny,
			Message:  "No permission handler configured",
		}, nil
	})
}

// AllowAllPermissionHandler returns a handler that allows all permissions.
func AllowAllPermissionHandler() PermissionHandler {
	return PermissionHandlerFunc(func(ctx context.Context, req *PermissionRequest) (*PermissionResponse, error) {
		return &PermissionResponse{
			Behavior: PermissionAllow,
		}, nil
	})
}
