// Package ipc provides a JSON-over-Unix-domain-socket IPC mechanism
// for communication between the bramble TUI server and CLI clients.
package ipc

import "github.com/bazelment/yoloswe/bramble/session"

// RequestType identifies the kind of IPC request.
type RequestType string

const (
	RequestPing         RequestType = "ping"
	RequestNewSession   RequestType = "new-session"
	RequestListSessions RequestType = "list-sessions"
	RequestNotify       RequestType = "notify"
	RequestCapturePane  RequestType = "capture-pane"
	RequestRestart      RequestType = "restart"
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

// RestartParams are the parameters for a restart request.
//
// Restart is intentionally IPC-only and never exposed through the control
// protocol: the control plane is reachable from the network via the hub, and
// bouncing a user's TUI is not something a remote caller should be able to do.
type RestartParams struct {
	// Force skips the confirmation the TUI would otherwise show when live
	// in-process sessions would be lost.
	Force bool `json:"force,omitempty"`
}

// NewSessionParams are the parameters for a new-session request.
type NewSessionParams struct {
	SessionType  string `json:"session_type"`            // "planner", "builder", or "codetalk"
	WorktreePath string `json:"worktree_path,omitempty"` // existing worktree path (mutually exclusive with Branch)
	Branch       string `json:"branch,omitempty"`        // create new worktree with this branch name
	BaseBranch   string `json:"base_branch,omitempty"`   // base branch for new worktree (default: main)
	Prompt       string `json:"prompt"`
	Model        string `json:"model,omitempty"`     // model ID (default: provider default)
	Goal         string `json:"goal,omitempty"`      // worktree goal (used when creating)
	RepoName     string `json:"repo_name,omitempty"` // target repo; auto-detected from cwd if empty
	// ParentSessionID makes the new session a subagent of that session: when it
	// finishes, bramble delivers a completion report back there. When set with
	// no Branch and no WorktreePath, the child inherits the parent's worktree.
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// ParentInherited says ParentSessionID came from $BRAMBLE_SESSION_ID rather
	// than an explicit --parent. The two must be told apart on the server: an
	// explicitly named parent that does not resolve is a mistake worth failing
	// on, while an inherited one is only a default — and a default that cannot
	// be honored must not cost the caller a spawn that would have worked without
	// it. The registry sees only sessions adopted into an open manager, so any
	// agent whose own repo is not open in this bramble hits that case.
	ParentInherited bool `json:"parent_inherited,omitempty"`
	// RepoInferred says RepoName was auto-detected from the caller's cwd rather
	// than typed as --repo. Same rule as ParentInherited, and for the same
	// reason: a value the client guessed must not be weighed as a claim the user
	// made. A resolved parent knows its own repo exactly, and a cwd that happens
	// to sit in another worktree does not.
	RepoInferred   bool `json:"repo_inferred,omitempty"`
	CreateWorktree bool `json:"create_worktree,omitempty"` // if true, create a new worktree for Branch
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
	// ParentSessionID is the session that spawned this one, so a caller can
	// pick its own subagents out of the list. Empty for a top-level session.
	ParentSessionID string `json:"parent_session_id,omitempty"`
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
// The literal lives in package session, which injects it into tmux windows;
// aliasing it here keeps this consumer and that producer from drifting apart
// under a rename. Mirrors control.SockEnvVar.
const SockEnvVar = session.IPCSockEnvVar
