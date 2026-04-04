package tracker

import "time"

// Issue is the normalized issue record used by the workflow engine.
type Issue struct {
	Description *string
	BranchName  *string
	URL         *string
	ID          string
	Identifier  string // e.g. "ENG-123"
	Title       string
	State       string
	TeamID      string
	Labels      []string
}

// Comment is a single comment on an issue.
type Comment struct {
	CreatedAt time.Time
	ID        string
	Body      string
	UserName  string
	IsSelf    bool // true if posted by the authenticated API user (our bot)
}

// WorkflowState is a possible state in the issue tracker's workflow.
type WorkflowState struct {
	ID   string
	Name string
	Type string // e.g. "started", "unstarted", "completed", "canceled"
}
