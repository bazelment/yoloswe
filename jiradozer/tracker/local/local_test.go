package local

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func newTestTracker(t *testing.T) *Tracker {
	t.Helper()
	dir := t.TempDir()
	tr, err := NewTracker(dir)
	require.NoError(t, err)
	return tr
}

func TestCreateAndFetchIssue(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	issue, err := tr.CreateIssue("Add retry logic", "When HTTP requests fail with 5xx, retry up to 3 times")
	require.NoError(t, err)
	assert.Equal(t, "local-1", issue.ID)
	assert.Equal(t, "LOCAL-1", issue.Identifier)
	assert.Equal(t, "Add retry logic", issue.Title)
	assert.Equal(t, "Todo", issue.State)
	assert.Equal(t, "local", issue.TeamID)

	fetched, err := tr.FetchIssue(ctx, "LOCAL-1")
	require.NoError(t, err)
	assert.Equal(t, issue.ID, fetched.ID)
	assert.Equal(t, issue.Title, fetched.Title)
	assert.Equal(t, "local", fetched.TeamID)
	require.NotNil(t, fetched.Description)
	assert.Contains(t, *fetched.Description, "retry up to 3 times")
}

func TestFetchIssueNotFound(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	_, err := tr.FetchIssue(ctx, "LOCAL-999")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestListIssues(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	_, err := tr.CreateIssue("Task 1", "desc 1")
	require.NoError(t, err)
	_, err = tr.CreateIssue("Task 2", "desc 2")
	require.NoError(t, err)

	issues, err := tr.ListIssues(ctx, tracker.IssueFilter{})
	require.NoError(t, err)
	assert.Len(t, issues, 2)
}

func TestListIssuesFilterByState(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	issue1, err := tr.CreateIssue("Task 1", "desc 1")
	require.NoError(t, err)
	_, err = tr.CreateIssue("Task 2", "desc 2")
	require.NoError(t, err)

	// Move issue1 to "In Progress".
	err = tr.UpdateIssueState(ctx, issue1.ID, "local-in-progress")
	require.NoError(t, err)

	// Filter for Todo only.
	issues, err := tr.ListIssues(ctx, tracker.IssueFilter{States: []string{"Todo"}})
	require.NoError(t, err)
	assert.Len(t, issues, 1)
	assert.Equal(t, "LOCAL-2", issues[0].Identifier)

	// Filter for In Progress only.
	issues, err = tr.ListIssues(ctx, tracker.IssueFilter{States: []string{"In Progress"}})
	require.NoError(t, err)
	assert.Len(t, issues, 1)
	assert.Equal(t, "LOCAL-1", issues[0].Identifier)
}

func TestComments(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	issue, err := tr.CreateIssue("Task", "desc")
	require.NoError(t, err)

	before := time.Now().Add(-time.Millisecond)

	err = tr.PostComment(ctx, issue.ID, "## Plan Complete\n\nHere is the plan.")
	require.NoError(t, err)

	// Fetch comments since before the post.
	comments, err := tr.FetchComments(ctx, issue.ID, before)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "## Plan Complete\n\nHere is the plan.", comments[0].Body)
	assert.True(t, comments[0].IsSelf)
	assert.Equal(t, "jiradozer", comments[0].UserName)

	// Fetch comments since after the post.
	comments, err = tr.FetchComments(ctx, issue.ID, time.Now().Add(time.Second))
	require.NoError(t, err)
	assert.Empty(t, comments)
}

func TestUpdateState(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	issue, err := tr.CreateIssue("Task", "desc")
	require.NoError(t, err)

	err = tr.UpdateIssueState(ctx, issue.ID, "local-in-progress")
	require.NoError(t, err)

	fetched, err := tr.FetchIssue(ctx, "LOCAL-1")
	require.NoError(t, err)
	assert.Equal(t, "In Progress", fetched.State)

	err = tr.UpdateIssueState(ctx, issue.ID, "local-done")
	require.NoError(t, err)

	fetched, err = tr.FetchIssue(ctx, "LOCAL-1")
	require.NoError(t, err)
	assert.Equal(t, "Done", fetched.State)
}

func TestWorkflowStates(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	states, err := tr.FetchWorkflowStates(ctx, "any-team")
	require.NoError(t, err)
	require.Len(t, states, 3)

	assert.Equal(t, "In Progress", states[0].Name)
	assert.Equal(t, "started", states[0].Type)
	assert.Equal(t, "In Review", states[1].Name)
	assert.Equal(t, "Done", states[2].Name)
	assert.Equal(t, "completed", states[2].Type)
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()

	tr1, err := NewTracker(dir)
	require.NoError(t, err)

	_, err = tr1.CreateIssue("Persistent task", "should survive")
	require.NoError(t, err)

	// Create a new tracker pointing at the same directory.
	tr2, err := NewTracker(dir)
	require.NoError(t, err)

	fetched, err := tr2.FetchIssue(context.Background(), "LOCAL-1")
	require.NoError(t, err)
	assert.Equal(t, "Persistent task", fetched.Title)
}

func TestAutoIncrementID(t *testing.T) {
	tr := newTestTracker(t)

	i1, err := tr.CreateIssue("One", "d1")
	require.NoError(t, err)
	i2, err := tr.CreateIssue("Two", "d2")
	require.NoError(t, err)
	i3, err := tr.CreateIssue("Three", "d3")
	require.NoError(t, err)

	assert.Equal(t, "LOCAL-1", i1.Identifier)
	assert.Equal(t, "LOCAL-2", i2.Identifier)
	assert.Equal(t, "LOCAL-3", i3.Identifier)
}

func TestPostCommentNotFound(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	err := tr.PostComment(ctx, "nonexistent", "hello")
	assert.Error(t, err)
}

func TestPathTraversal(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	_, err := tr.FetchIssue(ctx, "../../../etc/passwd")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path escapes tracker directory")
}

func TestUpdateStateNotFound(t *testing.T) {
	tr := newTestTracker(t)
	ctx := context.Background()

	err := tr.UpdateIssueState(ctx, "nonexistent", "local-done")
	assert.Error(t, err)
}
