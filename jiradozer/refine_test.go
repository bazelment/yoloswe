package jiradozer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestParseGitWorktreePorcelainFindsBranch(t *testing.T) {
	output := `worktree /repo/main
HEAD abc123
branch refs/heads/main

worktree /repo/jiradozer/ENG-123
HEAD def456
branch refs/heads/jiradozer/ENG-123

`

	got := parseGitWorktreePorcelain(output)
	assert.Equal(t, "/repo/jiradozer/ENG-123", got["jiradozer/ENG-123"])
}

func TestRefineFeedbackFromIssueCommentUsesLatestRefineComment(t *testing.T) {
	mt := &mockWorkflowTracker{
		comments: []tracker.Comment{
			{ID: "c1", Body: "refine: old feedback", CreatedAt: time.Now().Add(-time.Hour)},
			{ID: "c2", Body: "ordinary comment", CreatedAt: time.Now().Add(-time.Minute)},
			{ID: "c3", Body: "refine: address the timeout review", CreatedAt: time.Now()},
		},
	}

	got, ok, err := RefineFeedbackFromIssueComment(context.Background(), mt, "issue-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "address the timeout review", got)
}
