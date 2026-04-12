package jiradozer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestParseCommentAction(t *testing.T) {
	tests := []struct {
		body string
		want FeedbackAction
	}{
		// Approve variants.
		{"approve", FeedbackApprove},
		{"Approve", FeedbackApprove},
		{"APPROVE", FeedbackApprove},
		{"  approve  ", FeedbackApprove},
		{"approved", FeedbackApprove},
		{"lgtm", FeedbackApprove},
		{"LGTM", FeedbackApprove},
		{"Lgtm", FeedbackApprove},
		{"ship it", FeedbackApprove},
		{"Ship It", FeedbackApprove},
		{"SHIP IT", FeedbackApprove},

		// Approve with trailing punctuation (stripped before matching).
		{"lgtm!", FeedbackApprove},
		{"lgtm.", FeedbackApprove},
		{"lgtm?", FeedbackApprove},
		{"LGTM!", FeedbackApprove},
		{"approved!", FeedbackApprove},
		{"approved.", FeedbackApprove},
		{"approve!", FeedbackApprove},
		{"ship it!", FeedbackApprove},
		{"ship it!!", FeedbackApprove},

		// Approve with trailing text on first line (not just punctuation — still rejected).
		{"approve this", FeedbackComment}, // "approve this" is not "approve"
		{"ship it now", FeedbackComment},  // "ship it now" is not "ship it"
		{"lgtm yeah", FeedbackComment},    // "lgtm yeah" is not "lgtm"

		// Redo variants.
		{"redo", FeedbackRedo},
		{"Redo", FeedbackRedo},
		{"REDO", FeedbackRedo},
		{"redo with changes", FeedbackRedo},
		{"redo: fix the tests", FeedbackRedo},
		{"retry", FeedbackRedo},
		{"Retry", FeedbackRedo},
		{"RETRY", FeedbackRedo},
		{"retry please", FeedbackRedo},
		{"Retry with feedback", FeedbackRedo},

		// Multiline: first line determines action.
		{"approve\n\nLooks great, nice work!", FeedbackApprove},
		{"lgtm\nsome extra notes here", FeedbackApprove},
		{"approved\r\nWindows line ending", FeedbackApprove},
		{"redo\n\nPlease address the test failures", FeedbackRedo},
		{"retry\n\nFix the build error on line 42", FeedbackRedo},

		// General feedback (no keyword match).
		{"Please fix the tests", FeedbackComment},
		{"I think the plan is good but could use more detail", FeedbackComment},
		{"Can you also add error handling?", FeedbackComment},

		// Edge cases.
		{"", FeedbackComment},
		{"   ", FeedbackComment},
		{"\n\n\n", FeedbackComment},
		{"```\napprove\n```", FeedbackComment}, // code block — first line is backticks

		// Leading/trailing whitespace on first line.
		{"  lgtm  ", FeedbackApprove},
		{"\tapprove\t", FeedbackApprove},
		{" redo ", FeedbackRedo},
	}

	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			got := ParseCommentAction(tt.body)
			assert.Equal(t, tt.want, got, "body=%q", tt.body)
		})
	}
}

// mockTracker implements tracker.IssueTracker for polling tests.
type mockPollerTracker struct {
	comments []tracker.Comment
	calls    int
}

func (m *mockPollerTracker) FetchIssue(_ context.Context, _ string) (*tracker.Issue, error) {
	return nil, nil
}

func (m *mockPollerTracker) ListIssues(_ context.Context, _ tracker.IssueFilter) ([]*tracker.Issue, error) {
	return nil, nil
}

func (m *mockPollerTracker) FetchComments(_ context.Context, _ string, _ time.Time) ([]tracker.Comment, error) {
	m.calls++
	return m.comments, nil
}

func (m *mockPollerTracker) FetchWorkflowStates(_ context.Context, _ string) ([]tracker.WorkflowState, error) {
	return nil, nil
}

func (m *mockPollerTracker) PostComment(_ context.Context, _ string, _ string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}

func (m *mockPollerTracker) UpdateIssueState(_ context.Context, _ string, _ string) error {
	return nil
}
func (m *mockPollerTracker) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}

func TestPollForFeedback_FiltersBotComments(t *testing.T) {
	now := time.Now()
	mt := &mockPollerTracker{
		comments: []tracker.Comment{
			{ID: "bot1", Body: "## Plan Complete\n\nHere is the plan...", IsSelf: true, CreatedAt: now},
			{ID: "bot2", Body: "**plan_review** — Waiting for review.", IsSelf: true, CreatedAt: now.Add(time.Second)},
			{ID: "human1", Body: "lgtm", IsSelf: false, CreatedAt: now.Add(2 * time.Second)},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := discardLogger()
	fb, err := PollForFeedback(ctx, mt, "issue-1", time.Time{}, 100*time.Millisecond, logger, []string{"bot1", "bot2"})
	require.NoError(t, err)
	assert.Equal(t, FeedbackApprove, fb.Action)
	assert.Equal(t, "lgtm", fb.Message)
	assert.Equal(t, "human1", fb.Comment.ID)
}

func TestPollForFeedback_UsesLastHumanComment(t *testing.T) {
	now := time.Now()
	mt := &mockPollerTracker{
		comments: []tracker.Comment{
			{ID: "h1", Body: "redo", IsSelf: false, CreatedAt: now},
			{ID: "h2", Body: "approve", IsSelf: false, CreatedAt: now.Add(time.Second)},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := discardLogger()
	fb, err := PollForFeedback(ctx, mt, "issue-1", time.Time{}, 100*time.Millisecond, logger, nil)
	require.NoError(t, err)
	// Should pick the last (most recent) non-excluded comment.
	assert.Equal(t, FeedbackApprove, fb.Action)
	assert.Equal(t, "h2", fb.Comment.ID)
}

func TestPollForFeedback_ContextCanceled(t *testing.T) {
	mt := &mockPollerTracker{
		comments: nil, // No comments, will keep polling.
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	logger := discardLogger()
	_, err := PollForFeedback(ctx, mt, "issue-1", time.Time{}, 50*time.Millisecond, logger, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestPollForFeedback_OnlyBotComments_KeepsPolling(t *testing.T) {
	mt := &mockPollerTracker{
		comments: []tracker.Comment{
			{ID: "bot1", Body: "step complete", IsSelf: true, CreatedAt: time.Now()},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	logger := discardLogger()
	_, err := PollForFeedback(ctx, mt, "issue-1", time.Time{}, 50*time.Millisecond, logger, []string{"bot1"})
	// Should time out because only excluded bot comments are present.
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	// Should have polled multiple times.
	assert.Greater(t, mt.calls, 1)
}

// TestPollForFeedback_SameUserExcludeByID verifies that when all comments
// have IsSelf=true (same API key for bot and human), excluding by bot comment
// IDs still allows human comments through.
func TestPollForFeedback_SameUserExcludeByID(t *testing.T) {
	now := time.Now()
	mt := &mockPollerTracker{
		comments: []tracker.Comment{
			{ID: "bot-result", Body: "## Plan Complete\n\nPlan output...", IsSelf: true, CreatedAt: now},
			{ID: "bot-waiting", Body: "**plan_review** — Waiting for review.", IsSelf: true, CreatedAt: now.Add(time.Second)},
			{ID: "human-approve", Body: "approve", IsSelf: true, CreatedAt: now.Add(2 * time.Second)},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := discardLogger()
	fb, err := PollForFeedback(ctx, mt, "issue-1", time.Time{}, 100*time.Millisecond, logger, []string{"bot-result", "bot-waiting"})
	require.NoError(t, err)
	assert.Equal(t, FeedbackApprove, fb.Action)
	assert.Equal(t, "approve", fb.Message)
	assert.Equal(t, "human-approve", fb.Comment.ID)
}
