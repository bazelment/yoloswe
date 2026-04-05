package linear

import (
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
func parseIdentifier(identifier string) (teamKey string, number float64) {
	parts := strings.SplitN(identifier, "-", 2)
	if len(parts) == 2 {
		teamKey = parts[0]
		n, _ := strconv.Atoi(parts[1])
		number = float64(n)
	}
	return teamKey, number
}

// fetchIssueByIdentifierQuery returns a single issue by its human-readable identifier
// (e.g. "INF-199"). Linear's IssueFilter doesn't have an "identifier" field, so we
// filter by team key + issue number instead.
func fetchIssueByIdentifierQuery(identifier string) graphqlRequest {
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
	teamKey, number := parseIdentifier(identifier)
	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"teamKey": teamKey, "number": number},
	}
}

// fetchCommentsQuery returns comments on an issue, ordered by creation time.
func fetchCommentsQuery(issueID string, afterCursor string) graphqlRequest {
	query := `query FetchComments($issueId: String!, $first: Int!, $after: String) {
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
	query := `query FetchWorkflowStates($teamId: String!) {
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
