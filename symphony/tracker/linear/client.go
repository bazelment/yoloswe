package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/symphony/model"
)

const (
	// defaultEndpoint is the default Linear GraphQL API endpoint.
	defaultEndpoint = "https://api.linear.app/graphql"

	// networkTimeout is the HTTP request timeout per Spec Section 11.2.
	networkTimeout = 30 * time.Second

	// pageSize is the pagination page size per Spec Section 11.2.
	pageSize = 50
)

// Client is the Linear GraphQL tracker adapter.
type Client struct {
	httpClient *http.Client
	endpoint   string
	apiKey     string
}

// NewClient creates a new Linear tracker client.
// If endpoint is empty, the default Linear GraphQL endpoint is used.
func NewClient(endpoint, apiKey string) *Client {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Client{
		endpoint: endpoint,
		apiKey:   apiKey,
		httpClient: &http.Client{
			Timeout: networkTimeout,
		},
	}
}

// FetchCandidateIssues returns issues matching active states for the given project.
// Paginates through all results using cursor-based pagination.
func (c *Client) FetchCandidateIssues(ctx context.Context, activeStates []string, projectSlug string) ([]model.Issue, error) {
	if projectSlug == "" {
		return nil, &Error{
			Category: ErrMissingTrackerProjSlug,
			Message:  "project slug is required for candidate issue fetch",
		}
	}

	var allIssues []model.Issue
	var cursor string

	for {
		req := candidateIssuesQuery(projectSlug, activeStates, cursor)
		resp, err := c.execute(ctx, req)
		if err != nil {
			return nil, err
		}

		allIssues = append(allIssues, normalizeIssues(resp.Data.Issues.Nodes)...)

		if !resp.Data.Issues.PageInfo.HasNextPage {
			break
		}
		if resp.Data.Issues.PageInfo.EndCursor == nil {
			return nil, &Error{
				Category: ErrLinearMissingEndCursor,
				Message:  "pagination has next page but endCursor is nil",
			}
		}
		cursor = *resp.Data.Issues.PageInfo.EndCursor
	}

	return allIssues, nil
}

// FetchIssueStatesByIDs returns the current state of issues with the given IDs.
// Used for active-run reconciliation.
func (c *Client) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	req := issueStatesByIDsQuery(ids)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, err
	}
	return normalizeIssues(resp.Data.Issues.Nodes), nil
}

// FetchIssuesByStates returns issues in the given state names for the given project.
// Used for startup terminal cleanup. Paginates through all results.
func (c *Client) FetchIssuesByStates(ctx context.Context, states []string, projectSlug string) ([]model.Issue, error) {
	if len(states) == 0 {
		return nil, nil
	}

	var allIssues []model.Issue
	var cursor string

	for {
		req := issuesByStatesQuery(states, projectSlug, cursor)
		resp, err := c.execute(ctx, req)
		if err != nil {
			return nil, err
		}

		allIssues = append(allIssues, normalizeIssues(resp.Data.Issues.Nodes)...)

		if !resp.Data.Issues.PageInfo.HasNextPage {
			break
		}
		if resp.Data.Issues.PageInfo.EndCursor == nil {
			return nil, &Error{
				Category: ErrLinearMissingEndCursor,
				Message:  "pagination has next page but endCursor is nil",
			}
		}
		cursor = *resp.Data.Issues.PageInfo.EndCursor
	}

	return allIssues, nil
}

// execute sends a GraphQL request to the Linear API and returns the parsed response.
func (c *Client) execute(ctx context.Context, gqlReq graphqlRequest) (*graphqlResponse, error) {
	body, err := json.Marshal(gqlReq)
	if err != nil {
		return nil, &Error{
			Category: ErrLinearAPIRequest,
			Message:  "failed to marshal GraphQL request",
			Cause:    err,
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{
			Category: ErrLinearAPIRequest,
			Message:  "failed to create HTTP request",
			Cause:    err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Linear API expects "Bearer <key>". Add prefix if not already present.
	authValue := c.apiKey
	if !strings.HasPrefix(authValue, "Bearer ") {
		authValue = "Bearer " + authValue
	}
	httpReq.Header.Set("Authorization", authValue)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, &Error{
			Category: ErrLinearAPIRequest,
			Message:  "HTTP request failed",
			Cause:    err,
		}
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &Error{
			Category: ErrLinearAPIRequest,
			Message:  "failed to read response body",
			Cause:    err,
		}
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, &Error{
			Category: ErrLinearAPIStatus,
			Message:  fmt.Sprintf("Linear API returned HTTP %d: %s", httpResp.StatusCode, truncate(string(respBody), 200)),
		}
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, &Error{
			Category: ErrLinearUnknownPayload,
			Message:  "failed to unmarshal GraphQL response",
			Cause:    err,
		}
	}

	if len(gqlResp.Errors) > 0 {
		msgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			msgs[i] = e.Message
		}
		return nil, &Error{
			Category: ErrLinearGraphQLErrors,
			Message:  strings.Join(msgs, "; "),
		}
	}

	if gqlResp.Data == nil {
		return nil, &Error{
			Category: ErrLinearUnknownPayload,
			Message:  "GraphQL response has no data field",
		}
	}

	return &gqlResp, nil
}

// truncate limits a string to maxLen characters for error messages.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
