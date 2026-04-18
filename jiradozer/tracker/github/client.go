// Package github implements the tracker.IssueTracker interface for GitHub Issues
// using the gh CLI via wt.GHRunner.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
)

const (
	reviewLabel = "in-review"

	stateOpen   = "open"
	stateClosed = "closed"
	stateReview = "label:" + reviewLabel
)

// Client implements tracker.IssueTracker for GitHub Issues via the gh CLI.
type Client struct {
	gh        wt.GHRunner
	owner     string
	repo      string
	selfLogin string
	selfMu    sync.Mutex
}

// NewClient creates a new GitHub Issues tracker client.
func NewClient(gh wt.GHRunner, owner, repo string) *Client {
	return &Client{
		gh:    gh,
		owner: owner,
		repo:  repo,
	}
}

// ParseOwnerRepo splits "owner/repo" into its components.
func ParseOwnerRepo(s string) (owner, repo string, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid owner/repo %q: expected format owner/repo", s)
	}
	return parts[0], parts[1], nil
}

// parseIssueURL parses a GitHub issue URL into (owner, repo, number).
// Accepts URLs like https://github.com/owner/repo/issues/123 (any host).
func parseIssueURL(rawURL string) (owner, repo string, number int, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid issue URL %q: %w", rawURL, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return "", "", 0, fmt.Errorf("invalid issue URL %q: expected https://github.com/owner/repo/issues/number", rawURL)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", "", 0, fmt.Errorf("invalid issue number in URL %q", rawURL)
	}
	return parts[0], parts[1], n, nil
}

// ParseIdentifier splits "owner/repo#123" or a GitHub issue URL into (owner, repo, number).
func ParseIdentifier(identifier string) (owner, repo string, number int, err error) {
	if strings.HasPrefix(identifier, "https://") || strings.HasPrefix(identifier, "http://") {
		return parseIssueURL(identifier)
	}

	hashIdx := strings.LastIndex(identifier, "#")
	if hashIdx < 0 {
		return "", "", 0, fmt.Errorf("invalid identifier %q: expected format owner/repo#number", identifier)
	}
	ownerRepo := identifier[:hashIdx]
	numStr := identifier[hashIdx+1:]

	owner, repo, err = ParseOwnerRepo(ownerRepo)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid identifier %q: %w", identifier, err)
	}

	n, err := strconv.Atoi(numStr)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid issue number in %q: %w", identifier, err)
	}
	if n <= 0 {
		return "", "", 0, fmt.Errorf("invalid issue number in %q: must be positive", identifier)
	}
	return owner, repo, n, nil
}

func (c *Client) ensureSelfLogin(ctx context.Context) error {
	c.selfMu.Lock()
	defer c.selfMu.Unlock()
	if c.selfLogin != "" {
		return nil
	}
	result, err := c.gh.Run(ctx, []string{"api", "/user"}, "")
	if err != nil {
		return fmt.Errorf("get authenticated user: %w", err)
	}
	var user ghUser
	if err := json.Unmarshal([]byte(result.Stdout), &user); err != nil {
		return fmt.Errorf("parse /user response: %w", err)
	}
	c.selfLogin = user.Login
	return nil
}

func (c *Client) FetchIssue(ctx context.Context, identifier string) (*tracker.Issue, error) {
	owner, repo, number, err := ParseIdentifier(identifier)
	if err != nil {
		return nil, err
	}

	result, err := c.gh.Run(ctx, []string{
		"api", fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, number),
	}, "")
	if err != nil {
		return nil, fmt.Errorf("fetch issue %s: %w", identifier, err)
	}

	var gi ghIssue
	if err := json.Unmarshal([]byte(result.Stdout), &gi); err != nil {
		return nil, fmt.Errorf("parse issue response: %w", err)
	}

	return c.ghIssueToTracker(gi, owner, repo), nil
}

func (c *Client) ListIssues(ctx context.Context, filter tracker.IssueFilter) ([]*tracker.Issue, error) {
	owner := c.owner
	repo := c.repo
	if teamKey := filter.Filters[tracker.FilterTeam]; teamKey != "" {
		var err error
		owner, repo, err = ParseOwnerRepo(teamKey)
		if err != nil {
			return nil, fmt.Errorf("invalid team key: %w", err)
		}
	}

	// GitHub only supports a single state parameter; default to "open" unless
	// a "done"/"closed" state is explicitly requested.
	ghState := "open"
	for _, s := range tracker.SplitCSV(filter.Filters[tracker.FilterState]) {
		if lower := strings.ToLower(s); lower == "done" || lower == "closed" {
			ghState = "closed"
			break
		}
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	// GitHub's labels query parameter uses AND semantics, but the tracker
	// interface specifies OR. Issue one request per label and merge results.
	labelSets := tracker.SplitCSV(filter.Filters[tracker.FilterLabel])
	if len(labelSets) == 0 {
		labelSets = []string{""} // single request with no label filter
	}

	milestone := filter.Filters[tracker.FilterMilestone]
	assignee := filter.Filters[tracker.FilterAssignee]

	seen := make(map[int]bool)
	var issues []*tracker.Issue
	for _, label := range labelSets {
		path := fmt.Sprintf("repos/%s/%s/issues?state=%s&per_page=%d", owner, repo, ghState, limit)
		if label != "" {
			path += "&labels=" + url.QueryEscape(label)
		}
		if milestone != "" {
			path += "&milestone=" + url.QueryEscape(milestone)
		}
		if assignee != "" {
			path += "&assignee=" + url.QueryEscape(assignee)
		}

		result, err := c.gh.Run(ctx, []string{"api", path}, "")
		if err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}

		var ghIssues []ghIssue
		if err := json.Unmarshal([]byte(result.Stdout), &ghIssues); err != nil {
			return nil, fmt.Errorf("parse issues response: %w", err)
		}

		for _, gi := range ghIssues {
			if gi.PullRequest != nil {
				continue
			}
			if seen[gi.Number] {
				continue
			}
			seen[gi.Number] = true
			issues = append(issues, c.ghIssueToTracker(gi, owner, repo))
			if len(issues) >= limit {
				return issues, nil
			}
		}
	}
	return issues, nil
}

func (c *Client) FetchComments(ctx context.Context, issueID string, since time.Time) ([]tracker.Comment, error) {
	path := fmt.Sprintf("repos/%s/%s/issues/%s/comments", c.owner, c.repo, issueID)
	if !since.IsZero() {
		path += "?since=" + since.UTC().Format(time.RFC3339)
	}

	if err := c.ensureSelfLogin(ctx); err != nil {
		return nil, err
	}

	result, err := c.gh.Run(ctx, []string{"api", path}, "")
	if err != nil {
		return nil, fmt.Errorf("fetch comments: %w", err)
	}

	var ghComments []ghComment
	if err := json.Unmarshal([]byte(result.Stdout), &ghComments); err != nil {
		return nil, fmt.Errorf("parse comments response: %w", err)
	}

	var comments []tracker.Comment
	for _, gc := range ghComments {
		createdAt, err := time.Parse(time.RFC3339, gc.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse comment timestamp %q: %w", gc.CreatedAt, err)
		}
		comments = append(comments, tracker.Comment{
			ID:        strconv.FormatInt(gc.ID, 10),
			Body:      gc.Body,
			CreatedAt: createdAt,
			UserName:  gc.User.Login,
			IsSelf:    gc.User.Login == c.selfLogin,
		})
	}
	return comments, nil
}

func (c *Client) FetchWorkflowStates(_ context.Context, _ string) ([]tracker.WorkflowState, error) {
	return []tracker.WorkflowState{
		{ID: stateOpen, Name: "In Progress", Type: "started"},
		{ID: stateReview, Name: "In Review", Type: "started"},
		{ID: stateClosed, Name: "Done", Type: "completed"},
	}, nil
}

func (c *Client) PostComment(ctx context.Context, issueID string, body string) (tracker.Comment, error) {
	result, err := c.gh.Run(ctx, []string{
		"api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/issues/%s/comments", c.owner, c.repo, issueID),
		"-f", "body=" + body,
	}, "")
	if err != nil {
		return tracker.Comment{}, fmt.Errorf("post comment: %w", err)
	}

	var gc ghComment
	if err := json.Unmarshal([]byte(result.Stdout), &gc); err != nil {
		return tracker.Comment{}, fmt.Errorf("parse comment response: %w", err)
	}

	createdAt, err := time.Parse(time.RFC3339, gc.CreatedAt)
	if err != nil {
		return tracker.Comment{}, fmt.Errorf("parse comment timestamp %q: %w", gc.CreatedAt, err)
	}
	return tracker.Comment{
		ID:        strconv.FormatInt(gc.ID, 10),
		Body:      gc.Body,
		IsSelf:    true,
		CreatedAt: createdAt,
	}, nil
}

func (c *Client) UpdateIssueState(ctx context.Context, issueID string, stateID string) error {
	switch stateID {
	case stateOpen, stateClosed:
		ghState := "open"
		if stateID == stateClosed {
			ghState = "closed"
		}
		if _, err := c.gh.Run(ctx, []string{
			"api", "-X", "PATCH",
			fmt.Sprintf("repos/%s/%s/issues/%s", c.owner, c.repo, issueID),
			"-f", "state=" + ghState,
		}, ""); err != nil {
			return fmt.Errorf("update issue state to %s: %w", ghState, err)
		}
		c.RemoveLabel(ctx, issueID, reviewLabel) //nolint:errcheck // best-effort
		return nil

	case stateReview:
		return c.AddLabel(ctx, issueID, reviewLabel)

	default:
		return fmt.Errorf("unknown state ID: %q", stateID)
	}
}

// AddLabel adds a label to a GitHub issue. The operation is idempotent —
// GitHub returns 200 OK even if the label is already present.
func (c *Client) AddLabel(ctx context.Context, issueID string, label string) error {
	if _, err := c.gh.Run(ctx, []string{
		"api", "-X", "POST",
		fmt.Sprintf("repos/%s/%s/issues/%s/labels", c.owner, c.repo, issueID),
		"-f", fmt.Sprintf("labels[]=%s", label),
	}, ""); err != nil {
		return fmt.Errorf("add label %q to issue %s: %w", label, issueID, err)
	}
	return nil
}

// RemoveLabel removes a label from a GitHub issue. The operation is
// idempotent: a 404 response (label not present on issue) is treated as
// success.
func (c *Client) RemoveLabel(ctx context.Context, issueID, label string) error {
	result, err := c.gh.Run(ctx, []string{
		"api", "-X", "DELETE",
		fmt.Sprintf("repos/%s/%s/issues/%s/labels/%s", c.owner, c.repo, issueID, url.PathEscape(label)),
	}, "")
	if err != nil {
		if result != nil && strings.Contains(result.Stderr, "HTTP 404") {
			return nil
		}
		return fmt.Errorf("remove label %q from issue %s: %w", label, issueID, err)
	}
	return nil
}

func (c *Client) ghIssueToTracker(gi ghIssue, owner, repo string) *tracker.Issue {
	issue := &tracker.Issue{
		ID:         strconv.Itoa(gi.Number),
		Identifier: fmt.Sprintf("%s/%s#%d", owner, repo, gi.Number),
		Title:      gi.Title,
		State:      gi.State,
		TeamID:     fmt.Sprintf("%s/%s", owner, repo),
	}
	if gi.Body != nil {
		issue.Description = gi.Body
	}
	if gi.HTMLURL != "" {
		url := gi.HTMLURL
		issue.URL = &url
	}
	for _, l := range gi.Labels {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue
}

// GitHub API response types.

type ghIssue struct {
	Body        *string        `json:"body"`
	PullRequest *ghPullRequest `json:"pull_request"`
	HTMLURL     string         `json:"html_url"`
	Title       string         `json:"title"`
	State       string         `json:"state"`
	User        ghUser         `json:"user"`
	Labels      []ghLabel      `json:"labels"`
	Number      int            `json:"number"`
}

type ghPullRequest struct {
	URL string `json:"url"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghUser struct {
	Login string `json:"login"`
}

type ghComment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      ghUser `json:"user"`
	ID        int64  `json:"id"`
}
