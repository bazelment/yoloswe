package linear

import (
	"fmt"
	"strconv"
	"strings"
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
      labels { nodes { name } }
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

// listIssuesQuery returns issues matching the given filter criteria.
// teamKey filters by team, states filters by state name, labels filters by label name.
func listIssuesQuery(teamKey string, states, labels []string, limit int) graphqlRequest {
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
      labels { nodes { name } }
      team { id }
    }
  }
}`
	issueFilter := map[string]any{}

	if teamKey != "" {
		issueFilter["team"] = map[string]any{
			"key": map[string]any{"eq": teamKey},
		}
	}
	if len(states) > 0 {
		issueFilter["state"] = map[string]any{
			"name": map[string]any{"in": states},
		}
	}
	if len(labels) > 0 {
		issueFilter["labels"] = map[string]any{
			"name": map[string]any{"in": labels},
		}
	}

	first := limit
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
