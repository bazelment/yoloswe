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

// TestLinear_CommentPolling verifies comment posting and retrieval via real Linear.
// Note: PollForFeedback filters out IsSelf=true comments, so we test FetchComments
// directly since the authenticated API user's comments always have IsSelf=true.
func TestLinear_CommentPolling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	client := getLinearClient(t)
	issueID := getTestIssueID(t)

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
			assert.True(t, c.IsSelf, "comment posted by our API key should have IsSelf=true")
			t.Logf("Comment: ID=%s, IsSelf=%v, Body=%q", c.ID, c.IsSelf, c.Body)

			// Verify ParseCommentAction works with the real comment body.
			action := jiradozer.ParseCommentAction(c.Body)
			assert.Equal(t, jiradozer.FeedbackComment, action, "non-keyword body should be FeedbackComment")
			break
		}
	}
	assert.True(t, found, "should find posted comment in FetchComments result")
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
			_, err := client.PostComment(ctx, issue.ID, v.body)
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
