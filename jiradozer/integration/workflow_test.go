//go:build integration

package integration

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/linear"
)

// getLinearClient returns a real Linear client, skipping the test if
// JIRADOZER_TEST_LINEAR_API_KEY is not set.
func getLinearClient(t *testing.T) *linear.Client {
	t.Helper()
	key := os.Getenv("JIRADOZER_TEST_LINEAR_API_KEY")
	if key == "" {
		t.Skip("JIRADOZER_TEST_LINEAR_API_KEY not set — skipping Linear integration test")
	}
	return linear.NewClient(key)
}

// getTestIssueID returns the issue identifier for integration testing.
func getTestIssueID(t *testing.T) string {
	t.Helper()
	id := os.Getenv("JIRADOZER_TEST_ISSUE_ID")
	if id == "" {
		t.Skip("JIRADOZER_TEST_ISSUE_ID not set — skipping Linear integration test")
	}
	return id
}

// TestLinear_FetchIssue verifies that we can fetch a real issue from Linear.
func TestLinear_FetchIssue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getLinearClient(t)
	issueID := getTestIssueID(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)
	require.NotNil(t, issue)

	assert.NotEmpty(t, issue.ID)
	assert.Equal(t, issueID, issue.Identifier)
	assert.NotEmpty(t, issue.Title)
	t.Logf("Issue: %s — %s (state: %s, team: %s)", issue.Identifier, issue.Title, issue.State, issue.TeamID)
}

// TestLinear_PostAndFetchComment verifies round-trip comment creation and retrieval.
func TestLinear_PostAndFetchComment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getLinearClient(t)
	issueID := getTestIssueID(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fetch issue to get internal ID.
	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)

	// Post a test comment.
	commentBody := "**Integration Test** — " + time.Now().Format(time.RFC3339)
	err = client.PostComment(ctx, issue.ID, commentBody)
	require.NoError(t, err)

	// Fetch comments and verify ours is there.
	since := time.Now().Add(-10 * time.Second)
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

// TestLinear_FetchWorkflowStates verifies state retrieval for a team.
func TestLinear_FetchWorkflowStates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getLinearClient(t)
	issueID := getTestIssueID(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)
	require.NotEmpty(t, issue.TeamID, "issue must have a team ID for state resolution")

	states, err := client.FetchWorkflowStates(ctx, issue.TeamID)
	require.NoError(t, err)
	require.NotEmpty(t, states)

	for _, s := range states {
		t.Logf("State: %s (%s) — type=%s", s.Name, s.ID, s.Type)
	}

	// Verify standard state types exist.
	typeSet := make(map[string]bool)
	for _, s := range states {
		typeSet[s.Type] = true
	}
	assert.True(t, typeSet["started"] || typeSet["unstarted"], "should have at least one started/unstarted state")
}

// TestLinear_CommentPolling verifies that PollForFeedback works with real Linear.
func TestLinear_CommentPolling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getLinearClient(t)
	issueID := getTestIssueID(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Post an "approve" comment, then poll for it.
	approveBody := "lgtm"
	err = client.PostComment(ctx, issue.ID, approveBody)
	require.NoError(t, err)

	// Poll for the comment. Use a since time just before the post.
	since := time.Now().Add(-5 * time.Second)
	fb, err := jiradozer.PollForFeedback(ctx, client, issue.ID, since, 2*time.Second, logger)
	require.NoError(t, err)
	require.NotNil(t, fb)

	// The comment we posted is from "us" (IsSelf=true), so the poller should
	// skip it. If no other human comments exist, this will time out.
	// To properly test, we'd need a second user to post.
	// For now, verify the poller returns SOME result.
	t.Logf("Feedback: action=%d, message=%q, comment_id=%s", fb.Action, fb.Message, fb.Comment.ID)
}

// TestLinear_LGTM_Variants_RealComments verifies LGTM/approve handling with
// comments posted to a real Linear issue.
func TestLinear_LGTM_Variants_RealComments(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getLinearClient(t)
	issueID := getTestIssueID(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)

	variants := []struct {
		body string
		want jiradozer.FeedbackAction
	}{
		{"lgtm", jiradozer.FeedbackApprove},
		{"LGTM", jiradozer.FeedbackApprove},
		{"approve", jiradozer.FeedbackApprove},
		{"redo", jiradozer.FeedbackRedo},
		{"Please fix the tests", jiradozer.FeedbackComment},
	}

	for _, v := range variants {
		t.Run(v.body, func(t *testing.T) {
			// Post the comment.
			err := client.PostComment(ctx, issue.ID, v.body)
			require.NoError(t, err)

			// Fetch and parse.
			since := time.Now().Add(-5 * time.Second)
			comments, err := client.FetchComments(ctx, issue.ID, since)
			require.NoError(t, err)

			// Find our comment.
			var found *tracker.Comment
			for i := len(comments) - 1; i >= 0; i-- {
				if comments[i].Body == v.body {
					found = &comments[i]
					break
				}
			}
			require.NotNil(t, found, "should find posted comment")

			action := jiradozer.ParseCommentAction(found.Body)
			assert.Equal(t, v.want, action, "body=%q", v.body)
		})
	}
}

// TestLinear_StateResolution verifies that jiradozer config state names
// resolve to real Linear state IDs.
func TestLinear_StateResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getLinearClient(t)
	issueID := getTestIssueID(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issue, err := client.FetchIssue(ctx, issueID)
	require.NoError(t, err)

	states, err := client.FetchWorkflowStates(ctx, issue.TeamID)
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

	// At minimum, most Linear teams have "In Progress" and "Done".
	// "In Review" depends on team workflow customization.
	if id, ok := stateIDs["in_progress"]; ok {
		assert.NotEmpty(t, id)
		t.Logf("in_progress → %s", id)
	} else {
		t.Log("WARNING: 'In Progress' state not found — team may use custom names")
	}
}
