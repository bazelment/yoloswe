//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	ghtracker "github.com/bazelment/yoloswe/jiradozer/tracker/github"
	"github.com/bazelment/yoloswe/wt"
)

// getGitHubClient returns a real GitHub tracker client, skipping the test if
// JIRADOZER_TEST_GITHUB_REPO is not set.
func getGitHubClient(t *testing.T) *ghtracker.Client {
	t.Helper()
	repo := os.Getenv("JIRADOZER_TEST_GITHUB_REPO")
	if repo == "" {
		t.Skip("JIRADOZER_TEST_GITHUB_REPO not set — skipping GitHub integration test")
	}
	owner, repoName, err := ghtracker.ParseOwnerRepo(repo)
	require.NoError(t, err)
	return ghtracker.NewClient(&wt.DefaultGHRunner{}, owner, repoName)
}

// getGitHubTestIssue returns the issue identifier for integration testing.
func getGitHubTestIssue(t *testing.T) string {
	t.Helper()
	id := os.Getenv("JIRADOZER_TEST_GITHUB_ISSUE")
	if id == "" {
		t.Skip("JIRADOZER_TEST_GITHUB_ISSUE not set — skipping GitHub integration test")
	}
	return id
}

// TestGitHub_FetchIssue verifies that we can fetch a real issue from GitHub.
func TestGitHub_FetchIssue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getGitHubClient(t)
	issueID := getGitHubTestIssue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)
	require.NotNil(t, issue)

	assert.NotEmpty(t, issue.ID)
	assert.Equal(t, issueID, issue.Identifier)
	assert.NotEmpty(t, issue.Title)
	assert.NotNil(t, issue.URL)
	t.Logf("Issue: %s — %s (state: %s, team: %s)", issue.Identifier, issue.Title, issue.State, issue.TeamID)
}

// TestGitHub_PostAndFetchComment verifies round-trip comment creation and retrieval.
func TestGitHub_PostAndFetchComment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getGitHubClient(t)
	issueID := getGitHubTestIssue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fetch issue to get internal ID.
	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)

	// Post a test comment.
	commentBody := "**Integration Test** — " + time.Now().Format(time.RFC3339)
	posted, err := client.PostComment(ctx, issue.ID, commentBody)
	require.NoError(t, err)

	// Fetch comments and verify ours is there.
	since := posted.CreatedAt.Add(-10 * time.Second)
	comments, err := client.FetchComments(ctx, issue.ID, since)
	require.NoError(t, err)

	var found bool
	for _, c := range comments {
		if c.Body == commentBody {
			found = true
			assert.True(t, c.IsSelf, "comment posted by us should have IsSelf=true")
			t.Logf("Found comment: ID=%s, IsSelf=%v, CreatedAt=%s", c.ID, c.IsSelf, c.CreatedAt)
			break
		}
	}
	assert.True(t, found, "posted comment should be retrievable")
}

// TestGitHub_FetchWorkflowStates verifies state retrieval.
func TestGitHub_FetchWorkflowStates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getGitHubClient(t)
	repo := os.Getenv("JIRADOZER_TEST_GITHUB_REPO")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	states, err := client.FetchWorkflowStates(ctx, repo)
	require.NoError(t, err)
	require.Len(t, states, 3)

	assert.Equal(t, "In Progress", states[0].Name)
	assert.Equal(t, "In Review", states[1].Name)
	assert.Equal(t, "Done", states[2].Name)

	for _, s := range states {
		t.Logf("State: %s (%s) — type=%s", s.Name, s.ID, s.Type)
	}
}

// TestGitHub_ListIssues verifies listing open issues from the test repo.
func TestGitHub_ListIssues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getGitHubClient(t)
	repo := os.Getenv("JIRADOZER_TEST_GITHUB_REPO")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := client.ListIssues(ctx, tracker.IssueFilter{
		TeamKey: repo,
		States:  []string{"Todo"},
		Limit:   5,
	})
	require.NoError(t, err)
	t.Logf("Found %d open issues", len(issues))
	for _, iss := range issues {
		t.Logf("  %s — %s (state: %s)", iss.Identifier, iss.Title, iss.State)
	}
}

// TestGitHub_StateResolution verifies that jiradozer config state names
// resolve to GitHub state IDs.
func TestGitHub_StateResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getGitHubClient(t)
	repo := os.Getenv("JIRADOZER_TEST_GITHUB_REPO")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	states, err := client.FetchWorkflowStates(ctx, repo)
	require.NoError(t, err)

	// Build a name→ID map like the workflow does.
	cfg := jiradozer.StatesConfig{
		InProgress: "In Progress",
		InReview:   "In Review",
		Done:       "Done",
	}
	nameMap := map[string]string{
		cfg.InProgress: "in_progress",
		cfg.InReview:   "in_review",
		cfg.Done:       "done",
	}

	stateIDs := make(map[string]string)
	for _, s := range states {
		if logicalName, ok := nameMap[s.Name]; ok {
			stateIDs[logicalName] = s.ID
		}
	}

	t.Logf("Resolved states: %+v", stateIDs)

	// All three states should resolve for GitHub.
	assert.NotEmpty(t, stateIDs["in_progress"], "in_progress should resolve")
	assert.NotEmpty(t, stateIDs["in_review"], "in_review should resolve")
	assert.NotEmpty(t, stateIDs["done"], "done should resolve")
}

// TestGitHub_CommentPolling verifies comment posting and action parsing.
func TestGitHub_CommentPolling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getGitHubClient(t)
	issueID := getGitHubTestIssue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)

	// Post a uniquely identifiable comment.
	marker := time.Now().Format("20060102-150405.000")
	commentBody := "polling-test-" + marker
	since := time.Now().Add(-2 * time.Second)

	_, err = client.PostComment(ctx, issue.ID, commentBody)
	require.NoError(t, err)

	// Fetch comments and verify ours is there, with correct IsSelf flag.
	comments, err := client.FetchComments(ctx, issue.ID, since)
	require.NoError(t, err)

	var found bool
	for _, c := range comments {
		if c.Body == commentBody {
			found = true
			assert.True(t, c.IsSelf, "comment posted by our token should have IsSelf=true")
			t.Logf("Comment: ID=%s, IsSelf=%v, Body=%q", c.ID, c.IsSelf, c.Body)

			action := jiradozer.ParseCommentAction(c.Body)
			assert.Equal(t, jiradozer.FeedbackComment, action, "non-keyword body should be FeedbackComment")
			break
		}
	}
	assert.True(t, found, "should find posted comment in FetchComments result")
}
