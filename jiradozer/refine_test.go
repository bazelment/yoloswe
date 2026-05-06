package jiradozer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
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

func TestRunRefineUsesExplicitFeedbackBeforePRFetch(t *testing.T) {
	issue := testIssue()
	workDir := t.TempDir()
	cfg := testConfig()
	cfg.WorkDir = "/original"
	cfg.Source.BranchPrefix = "jiradozer"

	called := false
	err := RunRefine(context.Background(), RefineOptions{
		Issue:    issue,
		Tracker:  &mockWorkflowTracker{},
		Config:   cfg,
		Logger:   discardLogger(),
		Feedback: "make timeout configurable",
		WorkDir:  workDir,
		GH:       panicGHRunner{},
		RunWorkflow: func(wf *Workflow) error {
			called = true
			assert.Equal(t, StepValidating, wf.state.Current())
			assert.Equal(t, "make timeout configurable", wf.prFeedback)
			assert.Equal(t, workDir, wf.config.WorkDir)
			assert.True(t, wf.refineMode)
			return nil
		},
	})

	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "/original", cfg.WorkDir, "RunRefine must not mutate caller config")
}

func TestRunRefineMissingWorktreeError(t *testing.T) {
	cfg := testConfig()
	cfg.Source.BranchPrefix = "definitely-missing-jiradozer-branch"
	issue := testIssue()
	issue.Identifier = "NO-SUCH-BRANCH"

	err := RunRefine(context.Background(), RefineOptions{
		Issue:    issue,
		Tracker:  &mockWorkflowTracker{},
		Config:   cfg,
		Logger:   discardLogger(),
		Feedback: "manual feedback",
		GH:       panicGHRunner{},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree for branch")
	assert.Contains(t, err.Error(), "--work-dir")
}

type panicGHRunner struct{}

func (panicGHRunner) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	return nil, fmt.Errorf("unexpected gh call: %v", args)
}
