package linear

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// graphqlRequest is the JSON body sent to the Linear GraphQL endpoint.
type graphqlRequest struct {
	Variables map[string]any `json:"variables,omitempty"`
	Query     string         `json:"query"`
}

// parseIdentifier splits a human-readable identifier like "INF-199" into
// team key ("INF") and issue number (199).
func parseIdentifier(identifier string) (teamKey string, number float64, err error) {
	parts := strings.SplitN(identifier, "-", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", 0, fmt.Errorf("invalid issue identifier %q: expected format TEAM-NUMBER (e.g. INF-199)", identifier)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid issue number in %q: %w", identifier, err)
	}
	return parts[0], float64(n), nil
}

// fetchIssueByIdentifierQuery returns a single issue by its human-readable identifier
// (e.g. "INF-199"). Linear's IssueFilter doesn't have an "identifier" field, so we
// filter by team key + issue number instead.
func fetchIssueByIdentifierQuery(identifier string) (graphqlRequest, error) {
	query := `query FetchIssue($teamKey: String!, $number: Float!) {
  issues(filter: { team: { key: { eq: $teamKey } }, number: { eq: $number } }, first: 1) {
    nodes {
      id
      identifier
      title
      description
      url
      branchName
      state { id name type }
      labels { nodes { id name } }
      team { id }
    }
  }
}`
	teamKey, number, err := parseIdentifier(identifier)
	if err != nil {
		return graphqlRequest{}, err
	}
	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"teamKey": teamKey, "number": number},
	}, nil
}

// fetchIssueByIDQuery fetches a single issue by its Linear UUID (not the
// human-readable identifier). Used internally when we have the UUID but not
// the human identifier.
func fetchIssueByIDQuery(issueID string) graphqlRequest {
	query := `query FetchIssueByID($id: ID!) {
  issues(filter: { id: { eq: $id } }, first: 1) {
    nodes {
      id
      identifier
      title
      state { id name type }
      labels { nodes { id name } }
      team { id }
    }
  }
}`
	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"id": issueID},
	}
}

// listIssuesQuery returns issues matching the given filter criteria.
// Filter keys are read from filter.Filters; see tracker.IssueFilter for
// documented keys.
func listIssuesQuery(filter tracker.IssueFilter) graphqlRequest {
	query := `query ListIssues($filter: IssueFilter!, $first: Int!) {
  issues(filter: $filter, first: $first, orderBy: createdAt) {
    nodes {
      id
      identifier
      title
      description
      url
      branchName
      state { id name type }
      labels { nodes { id name } }
      team { id }
    }
  }
}`
	issueFilter := map[string]any{}

	if team := filter.Filters[tracker.FilterTeam]; team != "" {
		issueFilter["team"] = map[string]any{
			"key": map[string]any{"eq": team},
		}
	}
	if states := tracker.SplitCSV(filter.Filters[tracker.FilterState]); len(states) > 0 {
		issueFilter["state"] = map[string]any{
			"name": map[string]any{"in": states},
		}
	}
	if labels := tracker.SplitCSV(filter.Filters[tracker.FilterLabel]); len(labels) > 0 {
		issueFilter["labels"] = map[string]any{
			"some": map[string]any{
				"name": map[string]any{"in": labels},
			},
		}
	}
	if project := filter.Filters[tracker.FilterProject]; project != "" {
		issueFilter["project"] = map[string]any{
			"name": map[string]any{"containsIgnoreCase": project},
		}
	}
	if cycle := filter.Filters[tracker.FilterCycle]; cycle != "" {
		if cycle == "current" {
			issueFilter["cycle"] = map[string]any{
				"isActive": map[string]any{"eq": true},
			}
		} else {
			issueFilter["cycle"] = map[string]any{
				"name": map[string]any{"containsIgnoreCase": cycle},
			}
		}
	}
	if assignee := filter.Filters[tracker.FilterAssignee]; assignee != "" {
		issueFilter["assignee"] = map[string]any{
			"displayName": map[string]any{"containsIgnoreCase": assignee},
		}
	}

	first := filter.Limit
	if first <= 0 {
		first = 50
	}

	return graphqlRequest{
		Query: query,
		Variables: map[string]any{
			"filter": issueFilter,
			"first":  first,
		},
	}
}

// fetchCommentsQuery returns comments on an issue, ordered by creation time.
func fetchCommentsQuery(issueID string, afterCursor string) graphqlRequest {
	query := `query FetchComments($issueId: ID!, $first: Int!, $after: String) {
  comments(filter: { issue: { id: { eq: $issueId } } }, first: $first, after: $after, orderBy: createdAt) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      body
      createdAt
      user { id name isMe }
    }
  }
}`
	vars := map[string]any{
		"issueId": issueID,
		"first":   50,
	}
	if afterCursor != "" {
		vars["after"] = afterCursor
	}
	return graphqlRequest{Query: query, Variables: vars}
}

// fetchWorkflowStatesQuery returns all workflow states for a team.
func fetchWorkflowStatesQuery(teamID string) graphqlRequest {
	query := `query FetchWorkflowStates($teamId: ID!) {
  workflowStates(filter: { team: { id: { eq: $teamId } } }, first: 50) {
    nodes { id name type }
  }
}`
	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"teamId": teamID},
	}
}

// createCommentMutation posts a comment on an issue.
func createCommentMutation(issueID, body string) graphqlRequest {
	query := `mutation CreateComment($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id body createdAt }
  }
}`
	return graphqlRequest{
		Query: query,
		Variables: map[string]any{
			"issueId": issueID,
			"body":    body,
		},
	}
}

// updateIssueStateMutation transitions an issue to a new workflow state.
func updateIssueStateMutation(issueID, stateID string) graphqlRequest {
	query := `mutation UpdateIssueState($issueId: String!, $stateId: String!) {
  issueUpdate(id: $issueId, input: { stateId: $stateId }) {
    success
    issue { id state { id name } }
  }
}`
	return graphqlRequest{
		Query: query,
		Variables: map[string]any{
			"issueId": issueID,
			"stateId": stateID,
		},
	}
}

// searchLabelQuery looks up a label ID by name within the given team's
// organization. Linear labels are team-scoped.
func searchLabelQuery(teamID, name string) graphqlRequest {
	query := `query FindLabel($teamId: ID!, $name: String!) {
  issueLabels(filter: { team: { id: { eq: $teamId } }, name: { eq: $name } }, first: 1) {
    nodes { id name }
  }
}`
	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"teamId": teamID, "name": name},
	}
}

// createLabelMutation creates a new label with the given name in the team's workspace.
func createLabelMutation(teamID, name string) graphqlRequest {
	query := `mutation CreateLabel($teamId: String!, $name: String!) {
  issueLabelCreate(input: { teamId: $teamId, name: $name }) {
    success
    issueLabel { id name }
  }
}`
	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"teamId": teamID, "name": name},
	}
}

// addLabelToIssueMutation appends a label (by ID) to an issue without
// replacing existing labels. Linear's issueRelationCreate / issueLabelCreate
// don't support per-issue attachment; instead we read the current label IDs
// and pass the full merged set to issueUpdate. The caller is responsible for
// building the full list.
func addLabelToIssueMutation(issueID string, allLabelIDs []string) graphqlRequest {
	query := `mutation AddLabel($issueId: String!, $labelIds: [String!]!) {
  issueUpdate(id: $issueId, input: { labelIds: $labelIds }) {
    success
    issue { id labels { nodes { id name } } }
  }
}`
	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"issueId": issueID, "labelIds": allLabelIDs},
	}
}
