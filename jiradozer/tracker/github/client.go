// Package github implements the tracker.IssueTracker interface for GitHub Issues
// using the gh CLI via wt.GHRunner.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
)

const reviewLabel = "in-review"

// Client implements tracker.IssueTracker for GitHub Issues via the gh CLI.
type Client struct {
	gh        wt.GHRunner
	selfErr   error
	owner     string
	repo      string
	selfLogin string
	selfOnce  sync.Once
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

// parseIdentifier splits "owner/repo#123" into (owner, repo, number).
func parseIdentifier(identifier string) (owner, repo string, number int, err error) {
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
	if err != nil || n <= 0 {
		return "", "", 0, fmt.Errorf("invalid issue number in %q: %w", identifier, err)
	}
	return owner, repo, n, nil
}

func (c *Client) ensureSelfLogin(ctx context.Context) error {
	c.selfOnce.Do(func() {
		result, err := c.gh.Run(ctx, []string{"api", "/user"}, "")
		if err != nil {
			c.selfErr = fmt.Errorf("get authenticated user: %w", err)
			return
		}
		var user ghUser
		if err := json.Unmarshal([]byte(result.Stdout), &user); err != nil {
			c.selfErr = fmt.Errorf("parse /user response: %w", err)
			return
		}
		c.selfLogin = user.Login
	})
	return c.selfErr
}

func (c *Client) apiPath(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func (c *Client) FetchIssue(ctx context.Context, identifier string) (*tracker.Issue, error) {
	owner, repo, number, err := parseIdentifier(identifier)
	if err != nil {
		return nil, err
	}

	result, err := c.gh.Run(ctx, []string{
		"api", c.apiPath("repos/%s/%s/issues/%d", owner, repo, number),
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
	if filter.TeamKey != "" {
		var err error
		owner, repo, err = ParseOwnerRepo(filter.TeamKey)
		if err != nil {
			return nil, fmt.Errorf("invalid team key: %w", err)
		}
	}

	// Map jiradozer state names to GitHub issue states.
	ghState := "open"
	for _, s := range filter.States {
		lower := strings.ToLower(s)
		if lower == "done" || lower == "closed" {
			ghState = "closed"
			break
		}
	}

	path := fmt.Sprintf("repos/%s/%s/issues?state=%s", owner, repo, ghState)
	if len(filter.Labels) > 0 {
		path += "&labels=" + strings.Join(filter.Labels, ",")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	path += fmt.Sprintf("&per_page=%d", limit)

	result, err := c.gh.Run(ctx, []string{"api", path}, "")
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}

	var ghIssues []ghIssue
	if err := json.Unmarshal([]byte(result.Stdout), &ghIssues); err != nil {
		return nil, fmt.Errorf("parse issues response: %w", err)
	}

	var issues []*tracker.Issue
	for _, gi := range ghIssues {
		// GitHub returns pull requests in the issues endpoint; skip them.
		if gi.PullRequest != nil {
			continue
		}
		issues = append(issues, c.ghIssueToTracker(gi, owner, repo))
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
		{ID: "open", Name: "In Progress", Type: "started"},
		{ID: "label:" + reviewLabel, Name: "In Review", Type: "started"},
		{ID: "closed", Name: "Done", Type: "completed"},
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

	comment := tracker.Comment{
		ID:     strconv.FormatInt(gc.ID, 10),
		Body:   gc.Body,
		IsSelf: true,
	}
	if t, err := time.Parse(time.RFC3339, gc.CreatedAt); err == nil {
		comment.CreatedAt = t
	}
	return comment, nil
}

func (c *Client) UpdateIssueState(ctx context.Context, issueID string, stateID string) error {
	switch {
	case stateID == "open":
		// Reopen and remove review label.
		if _, err := c.gh.Run(ctx, []string{
			"api", "-X", "PATCH",
			fmt.Sprintf("repos/%s/%s/issues/%s", c.owner, c.repo, issueID),
			"-f", "state=open",
		}, ""); err != nil {
			return fmt.Errorf("update issue state to open: %w", err)
		}
		c.removeLabel(ctx, issueID, reviewLabel) //nolint:errcheck // best-effort
		return nil

	case stateID == "label:"+reviewLabel:
		// Add review label via JSON body.
		if _, err := c.gh.Run(ctx, []string{
			"api", "-X", "POST",
			fmt.Sprintf("repos/%s/%s/issues/%s/labels", c.owner, c.repo, issueID),
			"-f", fmt.Sprintf("labels[]=%s", reviewLabel),
		}, ""); err != nil {
			return fmt.Errorf("add review label: %w", err)
		}
		return nil

	case stateID == "closed":
		// Close and remove review label.
		if _, err := c.gh.Run(ctx, []string{
			"api", "-X", "PATCH",
			fmt.Sprintf("repos/%s/%s/issues/%s", c.owner, c.repo, issueID),
			"-f", "state=closed",
		}, ""); err != nil {
			return fmt.Errorf("update issue state to closed: %w", err)
		}
		c.removeLabel(ctx, issueID, reviewLabel) //nolint:errcheck // best-effort
		return nil

	default:
		return fmt.Errorf("unknown state ID: %q", stateID)
	}
}

func (c *Client) removeLabel(ctx context.Context, issueID, label string) error {
	_, err := c.gh.Run(ctx, []string{
		"api", "-X", "DELETE",
		fmt.Sprintf("repos/%s/%s/issues/%s/labels/%s", c.owner, c.repo, issueID, label),
	}, "")
	return err
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
