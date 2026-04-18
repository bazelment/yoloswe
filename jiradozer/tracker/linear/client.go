// Package linear implements the tracker.IssueTracker interface for Linear.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	req, err := fetchIssueByIdentifierQuery(identifier)
	if err != nil {
		return nil, err
	}
	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fetch issue %s: %w", identifier, err)
	}

	if resp.Data == nil || len(resp.Data.Issues.Nodes) == 0 {
		return nil, fmt.Errorf("issue not found: %s", identifier)
	}

	return nodeToIssue(resp.Data.Issues.Nodes[0]), nil
}

func (c *Client) ListIssues(ctx context.Context, filter tracker.IssueFilter) ([]*tracker.Issue, error) {
	req := listIssuesQuery(filter)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}

	if resp.Data == nil {
		return nil, nil
	}

	var issues []*tracker.Issue
	for i := range resp.Data.Issues.Nodes {
		issue := nodeToIssue(resp.Data.Issues.Nodes[i])
		issues = append(issues, issue)
	}
	return issues, nil
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
			createdAt, err := time.Parse(time.RFC3339, n.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("parse comment timestamp %q: %w", n.CreatedAt, err)
			}
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
				comment.IsSelf = n.User.IsMe
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

func (c *Client) PostComment(ctx context.Context, issueID string, body string) (tracker.Comment, error) {
	req := createCommentMutation(issueID, body)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return tracker.Comment{}, fmt.Errorf("post comment: %w", err)
	}
	if resp.Data == nil {
		return tracker.Comment{}, fmt.Errorf("post comment: response data is nil")
	}
	if !resp.Data.CommentCreate.Success {
		return tracker.Comment{}, fmt.Errorf("post comment: mutation returned success=false")
	}
	comment := tracker.Comment{IsSelf: true}
	if cr := resp.Data.CommentCreate.Comment; cr != nil {
		comment.ID = cr.ID
		comment.Body = cr.Body
		if t, err := time.Parse(time.RFC3339, cr.CreatedAt); err == nil {
			comment.CreatedAt = t
		} else {
			slog.Warn("failed to parse comment CreatedAt from Linear API", "raw", cr.CreatedAt, "error", err)
		}
	}
	return comment, nil
}

// AddLabel attaches a label (by name) to an issue. If the label does not exist
// in the team's workspace it is created first. The operation is idempotent.
// teamID is the Linear team ID used to scope the label lookup and creation.
// If the issue object's LabelIDs are known they must be passed via the
// tracker.Issue.LabelIDs field; otherwise we fetch the issue first.
func (c *Client) AddLabel(ctx context.Context, issueID string, label string) error {
	// Step 1: fetch the issue to get its team ID and current label IDs.
	// We need the team ID to scope the label search/creation.
	issueResp, err := c.fetchIssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("add label: fetch issue: %w", err)
	}

	// Step 2: find or create the label within the team.
	labelID, err := c.ensureLabel(ctx, issueResp.TeamID, label)
	if err != nil {
		return fmt.Errorf("add label: ensure label %q: %w", label, err)
	}

	// Step 3: merge the new label with the issue's existing label IDs (dedup).
	existingIDs := issueResp.LabelIDs
	merged := make([]string, 0, len(existingIDs)+1)
	found := false
	for _, id := range existingIDs {
		merged = append(merged, id)
		if id == labelID {
			found = true
		}
	}
	if found {
		return nil // already has the label — idempotent
	}
	merged = append(merged, labelID)

	// Step 4: update the issue with the full merged label set.
	req := addLabelToIssueMutation(issueID, merged)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return fmt.Errorf("add label to issue: %w", err)
	}
	if resp.Data == nil || !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("add label to issue: mutation returned success=false")
	}
	return nil
}

// RemoveLabel detaches a label (by name) from an issue. If the label is not
// present on the issue (or does not exist in the workspace), returns nil.
// The operation is idempotent.
func (c *Client) RemoveLabel(ctx context.Context, issueID string, label string) error {
	issueResp, err := c.fetchIssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("remove label: fetch issue: %w", err)
	}

	// Find the label ID on the issue by name. If not present, idempotent no-op.
	var targetID string
	for i, name := range issueResp.Labels {
		if name == label {
			if i < len(issueResp.LabelIDs) {
				targetID = issueResp.LabelIDs[i]
			}
			break
		}
	}
	if targetID == "" {
		return nil
	}

	remaining := make([]string, 0, len(issueResp.LabelIDs))
	for _, id := range issueResp.LabelIDs {
		if id != targetID {
			remaining = append(remaining, id)
		}
	}

	req := addLabelToIssueMutation(issueID, remaining)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return fmt.Errorf("remove label from issue: %w", err)
	}
	if resp.Data == nil || !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("remove label from issue: mutation returned success=false")
	}
	return nil
}

// fetchIssueByID returns a minimal issue record (team ID + label IDs) needed
// for AddLabel. Uses the issueID (Linear UUID), not the human identifier.
func (c *Client) fetchIssueByID(ctx context.Context, issueID string) (*tracker.Issue, error) {
	req := fetchIssueByIDQuery(issueID)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Data == nil || len(resp.Data.Issues.Nodes) == 0 {
		return nil, fmt.Errorf("issue not found: %s", issueID)
	}
	return nodeToIssue(resp.Data.Issues.Nodes[0]), nil
}

// ensureLabel finds a label by name within the given team, creating it if it
// does not exist. Returns the label's Linear UUID.
func (c *Client) ensureLabel(ctx context.Context, teamID, name string) (string, error) {
	searchResp, err := c.execute(ctx, searchLabelQuery(teamID, name))
	if err != nil {
		return "", fmt.Errorf("search label: %w", err)
	}
	if searchResp.Data != nil && len(searchResp.Data.IssueLabels.Nodes) > 0 {
		return searchResp.Data.IssueLabels.Nodes[0].ID, nil
	}

	// Label not found — create it.
	createResp, err := c.execute(ctx, createLabelMutation(teamID, name))
	if err != nil {
		return "", fmt.Errorf("create label: %w", err)
	}
	if createResp.Data == nil || !createResp.Data.IssueLabelCreate.Success || createResp.Data.IssueLabelCreate.IssueLabel == nil {
		return "", fmt.Errorf("create label: mutation returned success=false or missing label")
	}
	return createResp.Data.IssueLabelCreate.IssueLabel.ID, nil
}

func (c *Client) UpdateIssueState(ctx context.Context, issueID string, stateID string) error {
	req := updateIssueStateMutation(issueID, stateID)
	resp, err := c.execute(ctx, req)
	if err != nil {
		return fmt.Errorf("update issue state: %w", err)
	}
	if resp.Data == nil {
		return fmt.Errorf("update issue state: response data is nil")
	}
	if !resp.Data.IssueUpdate.Success {
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
	httpReq.Header.Set("Authorization", c.apiKey)

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

// nodeToIssue converts a GraphQL issue node to a tracker.Issue.
func nodeToIssue(node graphqlIssue) *tracker.Issue {
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
		issue.LabelIDs = append(issue.LabelIDs, l.ID)
	}
	return issue
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
	CommentCreate    graphqlMutationResult           `json:"commentCreate"`
	IssueUpdate      graphqlMutationResult           `json:"issueUpdate"`
	IssueLabelCreate graphqlLabelMutationResult      `json:"issueLabelCreate"`
	Issues           graphqlIssuesConnection         `json:"issues"`
	IssueLabels      graphqlLabelsConnection         `json:"issueLabels"`
	Comments         graphqlCommentsConnection       `json:"comments"`
	WorkflowStates   graphqlWorkflowStatesConnection `json:"workflowStates"`
}

type graphqlLabelsConnection struct {
	Nodes []graphqlLabel `json:"nodes"`
}

type graphqlLabelMutationResult struct {
	IssueLabel *graphqlLabel `json:"issueLabel,omitempty"`
	Success    bool          `json:"success"`
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
	ID   string `json:"id"`
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
	Comment *graphqlCommentResult `json:"comment,omitempty"`
	Success bool                  `json:"success"`
}

type graphqlCommentResult struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}
