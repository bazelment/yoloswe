package tracker

import (
	"strings"
	"time"
)

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

// Well-known filter keys for IssueFilter.Filters.
const (
	FilterTeam      = "team"      // team or repo identifier (e.g. "ENG", "owner/repo")
	FilterState     = "state"     // comma-separated state names (e.g. "Todo,InProgress")
	FilterLabel     = "label"     // comma-separated labels (OR semantics)
	FilterProject   = "project"   // Linear project name
	FilterCycle     = "cycle"     // Linear cycle ("current" for active cycle, or name)
	FilterMilestone = "milestone" // GitHub milestone
	FilterAssignee  = "assignee"  // GitHub/Linear assignee
)

// IssueFilter specifies criteria for listing issues.
//
// Filters is a generic key-value map keyed by the Filter* constants above.
// Each tracker interprets the keys it understands and silently ignores unknown
// ones. Multi-value fields use comma-separated strings.
type IssueFilter struct {
	Filters map[string]string // tracker-specific key-value pairs
	Limit   int               // max results; 0 = default (50)
}

// SplitCSV splits a comma-separated filter value into trimmed parts.
// Returns nil if the value is empty.
func SplitCSV(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}
