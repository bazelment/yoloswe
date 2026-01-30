package codex

import "context"

// ApprovalPolicy controls tool execution approval.
type ApprovalPolicy string

const (
	// ApprovalPolicyUntrusted requires approval for everything.
	ApprovalPolicyUntrusted ApprovalPolicy = "untrusted"

	// ApprovalPolicyOnFailure approves unless command fails.
	ApprovalPolicyOnFailure ApprovalPolicy = "on-failure"

	// ApprovalPolicyOnRequest approves on explicit request.
	ApprovalPolicyOnRequest ApprovalPolicy = "on-request"

	// ApprovalPolicyNever auto-approves everything (use with caution).
	ApprovalPolicyNever ApprovalPolicy = "never"

	// Deprecated aliases for backwards compatibility
	ApprovalPolicySuggest  ApprovalPolicy = "untrusted"
	ApprovalPolicyAutoEdit ApprovalPolicy = "on-failure"
	ApprovalPolicyFullAuto ApprovalPolicy = "never"
)

// ApprovalRequest contains data for an approval request.
type ApprovalRequest struct {
	ThreadID string
	TurnID   string
	ToolName string
	Input    map[string]interface{}
}

// ApprovalResponse contains the response to an approval request.
type ApprovalResponse struct {
	Approved     bool
	Message      string
	UpdatedInput map[string]interface{}
}

// ApprovalHandler handles tool execution approval requests.
type ApprovalHandler interface {
	HandleApproval(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error)
}

// ApprovalHandlerFunc is a function adapter for ApprovalHandler.
type ApprovalHandlerFunc func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error)

// HandleApproval implements ApprovalHandler.
func (f ApprovalHandlerFunc) HandleApproval(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
	return f(ctx, req)
}

// AutoApproveHandler returns a handler that auto-approves all tools.
func AutoApproveHandler() ApprovalHandler {
	return ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		return &ApprovalResponse{Approved: true}, nil
	})
}

// DenyAllHandler returns a handler that denies all tools.
func DenyAllHandler() ApprovalHandler {
	return ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		return &ApprovalResponse{
			Approved: false,
			Message:  "tool execution denied by policy",
		}, nil
	})
}
