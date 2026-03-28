package linear

// graphqlRequest is the JSON body sent to the Linear GraphQL endpoint.
type graphqlRequest struct {
	Variables map[string]any `json:"variables,omitempty"`
	Query     string         `json:"query"`
}

// candidateIssuesQuery builds the paginated query for fetching candidate issues
// filtered by project slugId and active state names.
func candidateIssuesQuery(projectSlug string, activeStates []string, afterCursor string) graphqlRequest {
	query := `query CandidateIssues($projectSlug: String!, $states: [String!]!, $first: Int!, $after: String) {
  issues(
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $states } }
    }
    first: $first
    after: $after
  ) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      id
      identifier
      title
      description
      priority
      url
      branchName
      createdAt
      updatedAt
      state {
        name
      }
      labels {
        nodes {
          name
        }
      }
      relations {
        nodes {
          type
          relatedIssue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
      inverseRelations {
        nodes {
          type
          issue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
    }
  }
}`

	vars := map[string]any{
		"projectSlug": projectSlug,
		"states":      activeStates,
		"first":       pageSize,
	}
	if afterCursor != "" {
		vars["after"] = afterCursor
	}
	return graphqlRequest{Query: query, Variables: vars}
}

// issueStatesByIDsQuery builds the query for refreshing issue states by IDs.
// Uses [ID!] variable type per Spec Section 11.2.
func issueStatesByIDsQuery(ids []string) graphqlRequest {
	query := `query IssueStatesByIDs($ids: [ID!]!) {
  issues(filter: { id: { in: $ids } }) {
    nodes {
      id
      identifier
      title
      description
      priority
      url
      branchName
      createdAt
      updatedAt
      state {
        name
      }
      labels {
        nodes {
          name
        }
      }
      relations {
        nodes {
          type
          relatedIssue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
      inverseRelations {
        nodes {
          type
          issue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
    }
  }
}`

	return graphqlRequest{
		Query:     query,
		Variables: map[string]any{"ids": ids},
	}
}

// issuesByStatesQuery builds the paginated query for fetching issues by state names,
// scoped to a project. Used for startup terminal cleanup (Spec Section 11.1).
func issuesByStatesQuery(states []string, projectSlug string, afterCursor string) graphqlRequest {
	query := `query IssuesByStates($states: [String!]!, $projectSlug: String!, $first: Int!, $after: String) {
  issues(
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $states } }
    }
    first: $first
    after: $after
  ) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      id
      identifier
      title
      description
      priority
      url
      branchName
      createdAt
      updatedAt
      state {
        name
      }
      labels {
        nodes {
          name
        }
      }
      relations {
        nodes {
          type
          relatedIssue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
      inverseRelations {
        nodes {
          type
          issue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
    }
  }
}`

	vars := map[string]any{
		"states":      states,
		"projectSlug": projectSlug,
		"first":       pageSize,
	}
	if afterCursor != "" {
		vars["after"] = afterCursor
	}
	return graphqlRequest{Query: query, Variables: vars}
}
