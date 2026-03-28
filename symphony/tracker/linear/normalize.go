package linear

import (
	"strings"
	"time"

	"github.com/bazelment/yoloswe/symphony/model"
)

// graphqlResponse is the top-level GraphQL response envelope.
type graphqlResponse struct {
	Data   *graphqlData   `json:"data"`
	Errors []graphqlError `json:"errors,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlData struct {
	Issues graphqlIssuesConnection `json:"issues"`
}

type graphqlIssuesConnection struct {
	PageInfo graphqlPageInfo `json:"pageInfo"`
	Nodes    []graphqlIssue  `json:"nodes"`
}

type graphqlPageInfo struct {
	EndCursor   *string `json:"endCursor"`
	HasNextPage bool    `json:"hasNextPage"`
}

type graphqlIssue struct {
	ID               string                    `json:"id"`
	Identifier       string                    `json:"identifier"`
	Title            string                    `json:"title"`
	Description      *string                   `json:"description"`
	Priority         *float64                  `json:"priority"`
	URL              *string                   `json:"url"`
	BranchName       *string                   `json:"branchName"`
	CreatedAt        *string                   `json:"createdAt"`
	UpdatedAt        *string                   `json:"updatedAt"`
	State            graphqlState              `json:"state"`
	Labels           graphqlLabelConnection    `json:"labels"`
	Relations        graphqlRelationConnection `json:"relations"`
	InverseRelations graphqlRelationConnection `json:"inverseRelations"`
}

type graphqlState struct {
	Name string `json:"name"`
}

type graphqlLabelConnection struct {
	Nodes []graphqlLabel `json:"nodes"`
}

type graphqlLabel struct {
	Name string `json:"name"`
}

type graphqlRelationConnection struct {
	Nodes []graphqlRelation `json:"nodes"`
}

type graphqlRelation struct {
	RelatedIssue *graphqlRelRef `json:"relatedIssue,omitempty"`
	Issue        *graphqlRelRef `json:"issue,omitempty"`
	Type         string         `json:"type"`
}

type graphqlRelRef struct {
	ID         string       `json:"id"`
	Identifier string       `json:"identifier"`
	State      graphqlState `json:"state"`
}

// normalizeIssues converts GraphQL issue nodes to the domain model.
func normalizeIssues(nodes []graphqlIssue) []model.Issue {
	issues := make([]model.Issue, 0, len(nodes))
	for i := range nodes {
		issues = append(issues, normalizeIssue(nodes[i]))
	}
	return issues
}

func normalizeIssue(n graphqlIssue) model.Issue {
	issue := model.Issue{
		ID:          n.ID,
		Identifier:  n.Identifier,
		Title:       n.Title,
		Description: n.Description,
		State:       n.State.Name,
		BranchName:  n.BranchName,
		URL:         n.URL,
		Labels:      normalizeLabels(n.Labels.Nodes),
		BlockedBy:   normalizeBlockedBy(n.InverseRelations.Nodes),
		Priority:    normalizePriority(n.Priority),
		CreatedAt:   parseTimestamp(n.CreatedAt),
		UpdatedAt:   parseTimestamp(n.UpdatedAt),
	}
	return issue
}

// normalizeLabels lowercases all label names (Spec Section 11.3).
func normalizeLabels(nodes []graphqlLabel) []string {
	if len(nodes) == 0 {
		return nil
	}
	labels := make([]string, len(nodes))
	for i, l := range nodes {
		labels[i] = strings.ToLower(l.Name)
	}
	return labels
}

// normalizeBlockedBy derives blockers from inverse relations where type is "blocks"
// (Spec Section 11.3).
func normalizeBlockedBy(nodes []graphqlRelation) []model.BlockerRef {
	var blockers []model.BlockerRef
	for _, r := range nodes {
		if r.Type != "blocks" {
			continue
		}
		ref := r.Issue
		if ref == nil {
			continue
		}
		id := ref.ID
		ident := ref.Identifier
		state := ref.State.Name
		blockers = append(blockers, model.BlockerRef{
			ID:         &id,
			Identifier: &ident,
			State:      &state,
		})
	}
	return blockers
}

// normalizePriority converts to integer; non-integer values become nil
// (Spec Section 11.3).
func normalizePriority(p *float64) *int {
	if p == nil {
		return nil
	}
	v := *p
	intVal := int(v)
	if float64(intVal) != v {
		return nil
	}
	return &intVal
}

// parseTimestamp parses an ISO-8601 timestamp string (Spec Section 11.3).
func parseTimestamp(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		// Try RFC3339Nano for sub-second precision.
		t, err = time.Parse(time.RFC3339Nano, *s)
		if err != nil {
			return nil
		}
	}
	return &t
}
