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

// IssueFilter specifies criteria for listing issues.
//
// Filters is a generic key-value map; each tracker interprets the keys it
// understands and silently ignores unknown ones. Multi-value fields use
// comma-separated strings (e.g. "Todo,InProgress").
//
// Common keys:
//
//	Universal:  team, state, label
//	GitHub:     milestone, assignee, search (raw GitHub search query)
//	Linear:     project, cycle ("current" for active cycle), assignee
type IssueFilter struct {
	Filters map[string]string // tracker-specific key-value pairs
	Limit   int               // max results; 0 = default (50)
}
