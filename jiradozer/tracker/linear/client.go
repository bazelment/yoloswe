// Package linear implements the tracker.IssueTracker interface for Linear.
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

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

const (
	defaultEndpoint = "https://api.linear.app/graphql"
	networkTimeout  = 30 * time.Second
)

// Client implements tracker.IssueTracker for Linear via GraphQL.
type Client struct {
	httpClient *http.Client
	endpoint   string
	apiKey     string
}

// NewClient creates a new Linear tracker client.
func NewClient(apiKey string) *Client {
	return &Client{
		endpoint: defaultEndpoint,
		apiKey:   apiKey,
		httpClient: &http.Client{
			Timeout: networkTimeout,
		},
	}
}

// NewClientWithEndpoint creates a client with a custom endpoint (for testing).
func NewClientWithEndpoint(endpoint, apiKey string) *Client {
	c := NewClient(apiKey)
	c.endpoint = endpoint
	return c
}

func (c *Client) FetchIssue(ctx context.Context, identifier string) (*tracker.Issue, error) {
	req := fetchIssueByIdentifierQuery(identifier)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fetch issue %s: %w", identifier, err)
	}

	if resp.Data == nil || len(resp.Data.Issues.Nodes) == 0 {
		return nil, fmt.Errorf("issue not found: %s", identifier)
	}

	node := resp.Data.Issues.Nodes[0]
	issue := &tracker.Issue{
		ID:          node.ID,
		Identifier:  node.Identifier,
		Title:       node.Title,
		Description: node.Description,
		State:       node.State.Name,
		BranchName:  node.BranchName,
		URL:         node.URL,
	}
	if node.Team != nil {
		issue.TeamID = node.Team.ID
	}
	for _, l := range node.Labels.Nodes {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue, nil
}

func (c *Client) FetchComments(ctx context.Context, issueID string, since time.Time) ([]tracker.Comment, error) {
	var allComments []tracker.Comment
	var cursor string

	for {
		req := fetchCommentsQuery(issueID, cursor)
		resp, err := c.execute(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("fetch comments: %w", err)
		}

		if resp.Data == nil {
			break
		}

		for _, n := range resp.Data.Comments.Nodes {
			createdAt, _ := time.Parse(time.RFC3339, n.CreatedAt)
			if !since.IsZero() && createdAt.Before(since) {
				continue
			}
			comment := tracker.Comment{
				ID:        n.ID,
				Body:      n.Body,
				CreatedAt: createdAt,
			}
			if n.User != nil {
				comment.UserName = n.User.Name
				comment.IsBot = n.User.IsMe
			}
			allComments = append(allComments, comment)
		}

		if !resp.Data.Comments.PageInfo.HasNextPage {
			break
		}
		if resp.Data.Comments.PageInfo.EndCursor == nil {
			break
		}
		cursor = *resp.Data.Comments.PageInfo.EndCursor
	}

	return allComments, nil
}

func (c *Client) FetchWorkflowStates(ctx context.Context, teamID string) ([]tracker.WorkflowState, error) {
	req := fetchWorkflowStatesQuery(teamID)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fetch workflow states: %w", err)
	}

	if resp.Data == nil {
		return nil, nil
	}

	var states []tracker.WorkflowState
	for _, n := range resp.Data.WorkflowStates.Nodes {
		states = append(states, tracker.WorkflowState{
			ID:   n.ID,
			Name: n.Name,
			Type: n.Type,
		})
	}
	return states, nil
}

func (c *Client) PostComment(ctx context.Context, issueID string, body string) error {
	req := createCommentMutation(issueID, body)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return fmt.Errorf("post comment: %w", err)
	}
	if resp.Data != nil && !resp.Data.CommentCreate.Success {
		return fmt.Errorf("post comment: mutation returned success=false")
	}
	return nil
}

func (c *Client) UpdateIssueState(ctx context.Context, issueID string, stateID string) error {
	req := updateIssueStateMutation(issueID, stateID)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return fmt.Errorf("update issue state: %w", err)
	}
	if resp.Data != nil && !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("update issue state: mutation returned success=false")
	}
	return nil
}

// execute sends a GraphQL request and returns the parsed response.
func (c *Client) execute(ctx context.Context, gqlReq graphqlRequest) (*graphqlResponse, error) {
	body, err := json.Marshal(gqlReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	authValue := c.apiKey
	if !strings.HasPrefix(authValue, "Bearer ") {
		authValue = "Bearer " + authValue
	}
	httpReq.Header.Set("Authorization", authValue)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear API returned HTTP %d: %s", httpResp.StatusCode, truncate(string(respBody), 200))
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		msgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("GraphQL errors: %s", strings.Join(msgs, "; "))
	}

	return &gqlResp, nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// graphqlResponse is the top-level GraphQL response envelope.
type graphqlResponse struct {
	Data   *graphqlData   `json:"data"`
	Errors []graphqlError `json:"errors,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlData struct {
	Issues         graphqlIssuesConnection         `json:"issues"`
	Comments       graphqlCommentsConnection       `json:"comments"`
	WorkflowStates graphqlWorkflowStatesConnection `json:"workflowStates"`
	CommentCreate  graphqlMutationResult           `json:"commentCreate"`
	IssueUpdate    graphqlMutationResult           `json:"issueUpdate"`
}

type graphqlIssuesConnection struct {
	Nodes []graphqlIssue `json:"nodes"`
}

type graphqlIssue struct {
	Description *string         `json:"description"`
	URL         *string         `json:"url"`
	BranchName  *string         `json:"branchName"`
	Team        *graphqlTeamRef `json:"team"`
	State       graphqlStateRef `json:"state"`
	ID          string          `json:"id"`
	Identifier  string          `json:"identifier"`
	Title       string          `json:"title"`
	Labels      graphqlLabels   `json:"labels"`
}

type graphqlStateRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type graphqlLabels struct {
	Nodes []graphqlLabel `json:"nodes"`
}

type graphqlLabel struct {
	Name string `json:"name"`
}

type graphqlTeamRef struct {
	ID string `json:"id"`
}

type graphqlCommentsConnection struct {
	PageInfo graphqlPageInfo  `json:"pageInfo"`
	Nodes    []graphqlComment `json:"nodes"`
}

type graphqlPageInfo struct {
	EndCursor   *string `json:"endCursor"`
	HasNextPage bool    `json:"hasNextPage"`
}

type graphqlComment struct {
	User      *graphqlUser `json:"user"`
	ID        string       `json:"id"`
	Body      string       `json:"body"`
	CreatedAt string       `json:"createdAt"`
}

type graphqlUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IsMe bool   `json:"isMe"`
}

type graphqlWorkflowStatesConnection struct {
	Nodes []graphqlWorkflowState `json:"nodes"`
}

type graphqlWorkflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type graphqlMutationResult struct {
	Success bool `json:"success"`
}
