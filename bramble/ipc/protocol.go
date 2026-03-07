// Package ipc provides a JSON-over-Unix-domain-socket IPC mechanism
// for communication between the bramble TUI server and CLI clients.
package ipc

// RequestType identifies the kind of IPC request.
type RequestType string

const (
	RequestPing         RequestType = "ping"
	RequestNewSession   RequestType = "new-session"
	RequestListSessions RequestType = "list-sessions"
	RequestNotify       RequestType = "notify"
	RequestCapturePane  RequestType = "capture-pane"
)

// Request is the envelope sent by the client to the server.
type Request struct {
	Params any         `json:"params,omitempty"`
	Type   RequestType `json:"type"`
	ID     string      `json:"id"`
}

// Response is the envelope sent by the server back to the client.
type Response struct {
	Result any    `json:"result,omitempty"`
	ID     string `json:"id"`
	Error  string `json:"error,omitempty"`
	OK     bool   `json:"ok"`
}

// NewSessionParams are the parameters for a new-session request.
type NewSessionParams struct {
	SessionType    string `json:"session_type"`            // "planner" or "builder"
	WorktreePath   string `json:"worktree_path,omitempty"` // existing worktree path (mutually exclusive with Branch)
	Branch         string `json:"branch,omitempty"`        // create new worktree with this branch name
	BaseBranch     string `json:"base_branch,omitempty"`   // base branch for new worktree (default: main)
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`           // model ID (default: provider default)
	Goal           string `json:"goal,omitempty"`            // worktree goal (used when creating)
	CreateWorktree bool   `json:"create_worktree,omitempty"` // if true, create a new worktree for Branch
}

// NewSessionResult is the result of a successful new-session request.
type NewSessionResult struct {
	SessionID    string `json:"session_id"`
	WorktreePath string `json:"worktree_path"`
}

// ListSessionsResult is the result of a list-sessions request.
type ListSessionsResult struct {
	Sessions []SessionSummary `json:"sessions"`
}

// SessionSummary is a brief snapshot of a session for list-sessions.
type SessionSummary struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	WorktreeName string `json:"worktree_name"`
	Prompt       string `json:"prompt"`
	Model        string `json:"model"`
}

// NotifyParams are the parameters for a notify request.
type NotifyParams struct {
	SessionID string `json:"session_id"`
}

// CapturePaneParams are the parameters for a capture-pane request.
type CapturePaneParams struct {
	SessionID string `json:"session_id"`
	Lines     int    `json:"lines,omitempty"` // number of lines to capture (default: 10)
}

// CapturePaneResult is the result of a successful capture-pane request.
type CapturePaneResult struct {
	Lines []string `json:"lines"`
}

// SockEnvVar is the environment variable name used to discover the socket path.
const SockEnvVar = "BRAMBLE_SOCK"
