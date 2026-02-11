package remote

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/wt"
	"github.com/bazelment/yoloswe/wt/taskrouter"
)

// assertTimeEqual checks that two times represent the same instant.
// The converter roundtrip loses timezone info (UnixNano -> time.Unix returns Local),
// so we compare instants rather than using assert.Equal which also checks timezone.
func assertTimeEqual(t *testing.T, expected, actual time.Time, msgAndArgs ...interface{}) {
	t.Helper()
	assert.True(t, expected.Equal(actual),
		"expected time %v to equal %v (instant comparison)", expected, actual)
}

// assertTimePtrEqual checks that two time pointers represent the same instant.
func assertTimePtrEqual(t *testing.T, expected, actual *time.Time, msgAndArgs ...interface{}) {
	t.Helper()
	if expected == nil {
		assert.Nil(t, actual)
		return
	}
	require.NotNil(t, actual)
	assert.True(t, expected.Equal(*actual),
		"expected time %v to equal %v (instant comparison)", *expected, *actual)
}

// ============================================================================
// Time helpers
// ============================================================================

func TestTimeToUnixNs_ZeroTime(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int64(0), timeToUnixNs(time.Time{}))
}

func TestTimeToUnixNs_NonZero(t *testing.T) {
	t.Parallel()
	ts := time.Date(2025, 6, 15, 10, 30, 0, 123456789, time.UTC)
	ns := timeToUnixNs(ts)
	assert.Equal(t, ts.UnixNano(), ns)
}

func TestTimeFromUnixNs_Zero(t *testing.T) {
	t.Parallel()
	result := timeFromUnixNs(0)
	assert.True(t, result.IsZero())
}

func TestTimeFromUnixNs_NonZero(t *testing.T) {
	t.Parallel()
	ts := time.Date(2025, 6, 15, 10, 30, 0, 123456789, time.UTC)
	ns := ts.UnixNano()
	result := timeFromUnixNs(ns)
	assertTimeEqual(t, ts, result)
}

func TestTimeRoundtrip(t *testing.T) {
	t.Parallel()
	original := time.Date(2025, 1, 15, 8, 30, 45, 999999999, time.UTC)
	ns := timeToUnixNs(original)
	roundtripped := timeFromUnixNs(ns)
	assertTimeEqual(t, original, roundtripped)
}

func TestTimePtrToUnixNs_Nil(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int64(0), timePtrToUnixNs(nil))
}

func TestTimePtrToUnixNs_NonNil(t *testing.T) {
	t.Parallel()
	ts := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, ts.UnixNano(), timePtrToUnixNs(&ts))
}

func TestTimePtrFromUnixNs_Zero(t *testing.T) {
	t.Parallel()
	assert.Nil(t, timePtrFromUnixNs(0))
}

func TestTimePtrFromUnixNs_NonZero(t *testing.T) {
	t.Parallel()
	ts := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	result := timePtrFromUnixNs(ts.UnixNano())
	require.NotNil(t, result)
	assertTimeEqual(t, ts, *result)
}

func TestTimePtrRoundtrip(t *testing.T) {
	t.Parallel()
	original := time.Date(2025, 7, 20, 14, 30, 0, 500000000, time.UTC)
	ns := timePtrToUnixNs(&original)
	roundtripped := timePtrFromUnixNs(ns)
	require.NotNil(t, roundtripped)
	assertTimeEqual(t, original, *roundtripped)
}

func TestTimePtrRoundtrip_Nil(t *testing.T) {
	t.Parallel()
	ns := timePtrToUnixNs(nil)
	assert.Equal(t, int64(0), ns)
	assert.Nil(t, timePtrFromUnixNs(ns))
}

// ============================================================================
// SessionInfo roundtrip
// ============================================================================

func TestSessionInfoRoundtrip_Full(t *testing.T) {
	t.Parallel()
	started := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	completed := time.Date(2025, 6, 1, 10, 5, 0, 0, time.UTC)

	original := session.SessionInfo{
		ID:             "sess-123",
		Type:           session.SessionTypePlanner,
		Status:         session.StatusRunning,
		WorktreePath:   "/home/user/worktrees/repo/feature-x",
		WorktreeName:   "feature-x",
		Prompt:         "Fix the login bug",
		Title:          "Login Bug Fix",
		Model:          "claude-opus-4",
		PlanFilePath:   "/tmp/plan.md",
		TmuxWindowName: "session-1",
		RunnerType:     "tmux",
		CreatedAt:      time.Date(2025, 6, 1, 9, 59, 0, 0, time.UTC),
		StartedAt:      &started,
		CompletedAt:    &completed,
		ErrorMsg:       "something went wrong",
		Progress: session.SessionProgressSnapshot{
			CurrentPhase: "coding",
			CurrentTool:  "edit_file",
			StatusLine:   "Editing main.go",
			TurnCount:    5,
			TotalCostUSD: 0.42,
			InputTokens:  15000,
			OutputTokens: 3000,
			LastActivity: time.Date(2025, 6, 1, 10, 4, 30, 0, time.UTC),
		},
	}

	proto := SessionInfoToProto(original)
	require.NotNil(t, proto)

	roundtripped := SessionInfoFromProto(proto)

	assert.Equal(t, original.ID, roundtripped.ID)
	assert.Equal(t, original.Type, roundtripped.Type)
	assert.Equal(t, original.Status, roundtripped.Status)
	assert.Equal(t, original.WorktreePath, roundtripped.WorktreePath)
	assert.Equal(t, original.WorktreeName, roundtripped.WorktreeName)
	assert.Equal(t, original.Prompt, roundtripped.Prompt)
	assert.Equal(t, original.Title, roundtripped.Title)
	assert.Equal(t, original.Model, roundtripped.Model)
	assert.Equal(t, original.PlanFilePath, roundtripped.PlanFilePath)
	assert.Equal(t, original.TmuxWindowName, roundtripped.TmuxWindowName)
	assert.Equal(t, original.RunnerType, roundtripped.RunnerType)
	assertTimeEqual(t, original.CreatedAt, roundtripped.CreatedAt)
	assertTimePtrEqual(t, original.StartedAt, roundtripped.StartedAt)
	assertTimePtrEqual(t, original.CompletedAt, roundtripped.CompletedAt)
	assert.Equal(t, original.ErrorMsg, roundtripped.ErrorMsg)

	assert.Equal(t, original.Progress.CurrentPhase, roundtripped.Progress.CurrentPhase)
	assert.Equal(t, original.Progress.CurrentTool, roundtripped.Progress.CurrentTool)
	assert.Equal(t, original.Progress.StatusLine, roundtripped.Progress.StatusLine)
	assert.Equal(t, original.Progress.TurnCount, roundtripped.Progress.TurnCount)
	assert.InDelta(t, original.Progress.TotalCostUSD, roundtripped.Progress.TotalCostUSD, 1e-9)
	assert.Equal(t, original.Progress.InputTokens, roundtripped.Progress.InputTokens)
	assert.Equal(t, original.Progress.OutputTokens, roundtripped.Progress.OutputTokens)
	assertTimeEqual(t, original.Progress.LastActivity, roundtripped.Progress.LastActivity)
}

func TestSessionInfoFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := SessionInfoFromProto(nil)
	assert.Equal(t, session.SessionInfo{}, result)
}

func TestSessionInfoRoundtrip_NilOptionalTimes(t *testing.T) {
	t.Parallel()
	original := session.SessionInfo{
		ID:        "sess-456",
		Status:    session.StatusPending,
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	proto := SessionInfoToProto(original)
	roundtripped := SessionInfoFromProto(proto)

	assert.Nil(t, roundtripped.StartedAt)
	assert.Nil(t, roundtripped.CompletedAt)
	assertTimeEqual(t, original.CreatedAt, roundtripped.CreatedAt)
}

// ============================================================================
// SessionProgressSnapshot roundtrip
// ============================================================================

func TestSessionProgressSnapshotRoundtrip(t *testing.T) {
	t.Parallel()
	original := session.SessionProgressSnapshot{
		CurrentPhase: "planning",
		CurrentTool:  "read_file",
		StatusLine:   "Reading config...",
		TurnCount:    3,
		TotalCostUSD: 1.23,
		InputTokens:  50000,
		OutputTokens: 10000,
		LastActivity: time.Date(2025, 5, 10, 15, 0, 0, 0, time.UTC),
	}

	proto := SessionProgressSnapshotToProto(original)
	require.NotNil(t, proto)
	roundtripped := SessionProgressSnapshotFromProto(proto)

	assert.Equal(t, original.CurrentPhase, roundtripped.CurrentPhase)
	assert.Equal(t, original.CurrentTool, roundtripped.CurrentTool)
	assert.Equal(t, original.StatusLine, roundtripped.StatusLine)
	assert.Equal(t, original.TurnCount, roundtripped.TurnCount)
	assert.InDelta(t, original.TotalCostUSD, roundtripped.TotalCostUSD, 1e-9)
	assert.Equal(t, original.InputTokens, roundtripped.InputTokens)
	assert.Equal(t, original.OutputTokens, roundtripped.OutputTokens)
	assertTimeEqual(t, original.LastActivity, roundtripped.LastActivity)
}

func TestSessionProgressSnapshotFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := SessionProgressSnapshotFromProto(nil)
	assert.Equal(t, session.SessionProgressSnapshot{}, result)
}

func TestSessionProgressSnapshotRoundtrip_ZeroValues(t *testing.T) {
	t.Parallel()
	original := session.SessionProgressSnapshot{}
	proto := SessionProgressSnapshotToProto(original)
	roundtripped := SessionProgressSnapshotFromProto(proto)
	// Zero values: all fields are zero/empty, including time which stays zero.
	assert.Equal(t, original.CurrentPhase, roundtripped.CurrentPhase)
	assert.Equal(t, original.TurnCount, roundtripped.TurnCount)
	assert.True(t, roundtripped.LastActivity.IsZero())
}

// ============================================================================
// OutputLine roundtrip
// ============================================================================

func TestOutputLineRoundtrip_Full(t *testing.T) {
	t.Parallel()
	original := session.OutputLine{
		Timestamp:  time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
		Type:       session.OutputTypeToolStart,
		Content:    "Running edit_file tool",
		ToolName:   "edit_file",
		ToolID:     "tool-abc-123",
		ToolState:  session.ToolStateRunning,
		ToolInput:  map[string]interface{}{"file": "main.go", "line": float64(42)},
		ToolResult: "File edited successfully",
		StartTime:  time.Date(2025, 6, 1, 9, 59, 55, 0, time.UTC),
		TurnNumber: 2,
		CostUSD:    0.05,
		DurationMs: 1500,
		IsError:    false,
	}

	proto := OutputLineToProto(original)
	require.NotNil(t, proto)
	roundtripped := OutputLineFromProto(proto)

	assertTimeEqual(t, original.Timestamp, roundtripped.Timestamp)
	assert.Equal(t, original.Type, roundtripped.Type)
	assert.Equal(t, original.Content, roundtripped.Content)
	assert.Equal(t, original.ToolName, roundtripped.ToolName)
	assert.Equal(t, original.ToolID, roundtripped.ToolID)
	assert.Equal(t, original.ToolState, roundtripped.ToolState)
	assert.Equal(t, original.TurnNumber, roundtripped.TurnNumber)
	assert.InDelta(t, original.CostUSD, roundtripped.CostUSD, 1e-9)
	assert.Equal(t, original.DurationMs, roundtripped.DurationMs)
	assert.Equal(t, original.IsError, roundtripped.IsError)
	assertTimeEqual(t, original.StartTime, roundtripped.StartTime)

	// ToolInput roundtrips through JSON, so we check key fields
	require.NotNil(t, roundtripped.ToolInput)
	assert.Equal(t, "main.go", roundtripped.ToolInput["file"])
	assert.Equal(t, float64(42), roundtripped.ToolInput["line"])

	// ToolResult roundtrips through JSON
	assert.Equal(t, "File edited successfully", roundtripped.ToolResult)
}

func TestOutputLineFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := OutputLineFromProto(nil)
	assert.Equal(t, session.OutputLine{}, result)
}

func TestOutputLineRoundtrip_NilToolFields(t *testing.T) {
	t.Parallel()
	original := session.OutputLine{
		Timestamp: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
		Type:      session.OutputTypeText,
		Content:   "Just some text",
	}

	proto := OutputLineToProto(original)
	roundtripped := OutputLineFromProto(proto)

	assert.Nil(t, roundtripped.ToolInput)
	assert.Nil(t, roundtripped.ToolResult)
	assert.Equal(t, original.Content, roundtripped.Content)
	assert.Equal(t, original.Type, roundtripped.Type)
}

func TestOutputLineRoundtrip_IsErrorTrue(t *testing.T) {
	t.Parallel()
	original := session.OutputLine{
		Timestamp: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
		Type:      session.OutputTypeError,
		Content:   "Command failed",
		IsError:   true,
	}

	proto := OutputLineToProto(original)
	roundtripped := OutputLineFromProto(proto)
	assert.True(t, roundtripped.IsError)
	assert.Equal(t, session.OutputTypeError, roundtripped.Type)
}

func TestOutputLineRoundtrip_ComplexToolResult(t *testing.T) {
	t.Parallel()
	// ToolResult can be a complex JSON value (map/list)
	original := session.OutputLine{
		Timestamp:  time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
		Type:       session.OutputTypeToolResult,
		ToolResult: map[string]interface{}{"status": "ok", "lines": float64(10)},
	}

	proto := OutputLineToProto(original)
	roundtripped := OutputLineFromProto(proto)

	require.NotNil(t, roundtripped.ToolResult)
	resultMap, ok := roundtripped.ToolResult.(map[string]interface{})
	require.True(t, ok, "ToolResult should be a map after JSON roundtrip")
	assert.Equal(t, "ok", resultMap["status"])
	assert.Equal(t, float64(10), resultMap["lines"])
}

// ============================================================================
// Worktree roundtrip
// ============================================================================

func TestWorktreeRoundtrip(t *testing.T) {
	t.Parallel()
	original := wt.Worktree{
		Path:       "/home/user/worktrees/repo/feature-x",
		Branch:     "feature-x",
		Commit:     "abc12345",
		IsDetached: false,
	}

	proto := WorktreeToProto(original)
	require.NotNil(t, proto)
	roundtripped := WorktreeFromProto(proto)

	assert.Equal(t, original, roundtripped)
}

func TestWorktreeFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := WorktreeFromProto(nil)
	assert.Equal(t, wt.Worktree{}, result)
}

func TestWorktreeRoundtrip_Detached(t *testing.T) {
	t.Parallel()
	original := wt.Worktree{
		Path:       "/home/user/worktrees/repo/detached",
		Branch:     "(detached)",
		Commit:     "deadbeef",
		IsDetached: true,
	}

	proto := WorktreeToProto(original)
	roundtripped := WorktreeFromProto(proto)
	assert.Equal(t, original, roundtripped)
}

// ============================================================================
// WorktreeStatus roundtrip
// ============================================================================

func TestWorktreeStatusRoundtrip(t *testing.T) {
	t.Parallel()
	original := &wt.WorktreeStatus{
		Worktree: wt.Worktree{
			Path:   "/home/user/worktrees/repo/feature-x",
			Branch: "feature-x",
			Commit: "abc12345",
		},
		IsDirty:        true,
		Ahead:          3,
		Behind:         1,
		PRNumber:       42,
		PRURL:          "https://github.com/org/repo/pull/42",
		PRState:        "OPEN",
		PRReviewStatus: "APPROVED",
		PRIsDraft:      false,
		LastCommitMsg:  "fix: login form validation",
		LastCommitTime: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	proto := WorktreeStatusToProto(original)
	require.NotNil(t, proto)
	roundtripped := WorktreeStatusFromProto(proto)
	require.NotNil(t, roundtripped)

	assert.Equal(t, original.Worktree, roundtripped.Worktree)
	assert.Equal(t, original.IsDirty, roundtripped.IsDirty)
	assert.Equal(t, original.Ahead, roundtripped.Ahead)
	assert.Equal(t, original.Behind, roundtripped.Behind)
	assert.Equal(t, original.PRNumber, roundtripped.PRNumber)
	assert.Equal(t, original.PRURL, roundtripped.PRURL)
	assert.Equal(t, original.PRState, roundtripped.PRState)
	assert.Equal(t, original.PRReviewStatus, roundtripped.PRReviewStatus)
	assert.Equal(t, original.PRIsDraft, roundtripped.PRIsDraft)
	assert.Equal(t, original.LastCommitMsg, roundtripped.LastCommitMsg)
	assertTimeEqual(t, original.LastCommitTime, roundtripped.LastCommitTime)
}

func TestWorktreeStatusToProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, WorktreeStatusToProto(nil))
}

func TestWorktreeStatusFromProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, WorktreeStatusFromProto(nil))
}

// ============================================================================
// PRInfo roundtrip
// ============================================================================

func TestPRInfoRoundtrip(t *testing.T) {
	t.Parallel()
	original := wt.PRInfo{
		URL:            "https://github.com/org/repo/pull/99",
		HeadRefName:    "feature-y",
		BaseRefName:    "main",
		State:          "OPEN",
		ReviewDecision: "CHANGES_REQUESTED",
		Number:         99,
		IsDraft:        true,
	}

	proto := PRInfoToProto(original)
	require.NotNil(t, proto)
	roundtripped := PRInfoFromProto(proto)

	assert.Equal(t, original, roundtripped)
}

func TestPRInfoFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := PRInfoFromProto(nil)
	assert.Equal(t, wt.PRInfo{}, result)
}

// ============================================================================
// CommitInfo roundtrip
// ============================================================================

func TestCommitInfoRoundtrip(t *testing.T) {
	t.Parallel()
	original := wt.CommitInfo{
		Hash:    "abc123def456",
		Subject: "feat: add new feature",
		Author:  "Jane Doe",
		Date:    time.Date(2025, 5, 20, 16, 30, 0, 0, time.UTC),
	}

	proto := CommitInfoToProto(original)
	require.NotNil(t, proto)
	roundtripped := CommitInfoFromProto(proto)

	assert.Equal(t, original.Hash, roundtripped.Hash)
	assert.Equal(t, original.Subject, roundtripped.Subject)
	assert.Equal(t, original.Author, roundtripped.Author)
	assertTimeEqual(t, original.Date, roundtripped.Date)
}

func TestCommitInfoFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := CommitInfoFromProto(nil)
	assert.Equal(t, wt.CommitInfo{}, result)
}

// ============================================================================
// WorktreeContext roundtrip
// ============================================================================

func TestWorktreeContextRoundtrip(t *testing.T) {
	t.Parallel()
	original := &wt.WorktreeContext{
		Path:           "/home/user/worktrees/repo/feature-x",
		Branch:         "feature-x",
		Goal:           "Implement login form",
		Parent:         "main",
		IsDirty:        true,
		Ahead:          2,
		Behind:         0,
		ChangedFiles:   []string{"login.go", "auth.go"},
		UntrackedFiles: []string{"new_file.go"},
		RecentCommits: []wt.CommitInfo{
			{
				Hash:    "commit1",
				Subject: "initial",
				Author:  "Alice",
				Date:    time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC),
			},
			{
				Hash:    "commit2",
				Subject: "update",
				Author:  "Bob",
				Date:    time.Date(2025, 5, 2, 11, 0, 0, 0, time.UTC),
			},
		},
		DiffStat:    "2 files changed, 50 insertions(+), 10 deletions(-)",
		DiffContent: "--- a/login.go\n+++ b/login.go\n+new line",
		PRNumber:    42,
		PRURL:       "https://github.com/org/repo/pull/42",
		PRState:     "OPEN",
		GatheredAt:  time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
	}

	proto := WorktreeContextToProto(original)
	require.NotNil(t, proto)
	roundtripped := WorktreeContextFromProto(proto)
	require.NotNil(t, roundtripped)

	assert.Equal(t, original.Path, roundtripped.Path)
	assert.Equal(t, original.Branch, roundtripped.Branch)
	assert.Equal(t, original.Goal, roundtripped.Goal)
	assert.Equal(t, original.Parent, roundtripped.Parent)
	assert.Equal(t, original.IsDirty, roundtripped.IsDirty)
	assert.Equal(t, original.Ahead, roundtripped.Ahead)
	assert.Equal(t, original.Behind, roundtripped.Behind)
	assert.Equal(t, original.ChangedFiles, roundtripped.ChangedFiles)
	assert.Equal(t, original.UntrackedFiles, roundtripped.UntrackedFiles)
	assert.Equal(t, original.DiffStat, roundtripped.DiffStat)
	assert.Equal(t, original.DiffContent, roundtripped.DiffContent)
	assert.Equal(t, original.PRNumber, roundtripped.PRNumber)
	assert.Equal(t, original.PRURL, roundtripped.PRURL)
	assert.Equal(t, original.PRState, roundtripped.PRState)
	assertTimeEqual(t, original.GatheredAt, roundtripped.GatheredAt)

	require.Len(t, roundtripped.RecentCommits, 2)
	assert.Equal(t, original.RecentCommits[0].Hash, roundtripped.RecentCommits[0].Hash)
	assert.Equal(t, original.RecentCommits[0].Subject, roundtripped.RecentCommits[0].Subject)
	assert.Equal(t, original.RecentCommits[0].Author, roundtripped.RecentCommits[0].Author)
	assertTimeEqual(t, original.RecentCommits[0].Date, roundtripped.RecentCommits[0].Date)
	assert.Equal(t, original.RecentCommits[1].Hash, roundtripped.RecentCommits[1].Hash)
	assertTimeEqual(t, original.RecentCommits[1].Date, roundtripped.RecentCommits[1].Date)
}

func TestWorktreeContextToProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, WorktreeContextToProto(nil))
}

func TestWorktreeContextFromProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, WorktreeContextFromProto(nil))
}

func TestWorktreeContextRoundtrip_EmptySlices(t *testing.T) {
	t.Parallel()
	original := &wt.WorktreeContext{
		Path:   "/some/path",
		Branch: "main",
	}

	proto := WorktreeContextToProto(original)
	roundtripped := WorktreeContextFromProto(proto)
	require.NotNil(t, roundtripped)

	// nil slices become nil after roundtrip (proto empty repeated = nil)
	assert.Nil(t, roundtripped.ChangedFiles)
	assert.Nil(t, roundtripped.UntrackedFiles)
	assert.Empty(t, roundtripped.RecentCommits)
}

// ============================================================================
// ContextOptions roundtrip
// ============================================================================

func TestContextOptionsRoundtrip(t *testing.T) {
	t.Parallel()
	original := wt.ContextOptions{
		IncludeDiff:     true,
		IncludeDiffStat: true,
		IncludeFileList: true,
		IncludePRInfo:   true,
		IncludeCommits:  10,
		MaxDiffBytes:    100000,
	}

	proto := ContextOptionsToProto(original)
	require.NotNil(t, proto)
	roundtripped := ContextOptionsFromProto(proto)

	assert.Equal(t, original, roundtripped)
}

func TestContextOptionsFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := ContextOptionsFromProto(nil)
	assert.Equal(t, wt.ContextOptions{}, result)
}

func TestContextOptionsRoundtrip_AllFalse(t *testing.T) {
	t.Parallel()
	original := wt.ContextOptions{}
	proto := ContextOptionsToProto(original)
	roundtripped := ContextOptionsFromProto(proto)
	assert.Equal(t, original, roundtripped)
}

// ============================================================================
// MergeOptions roundtrip
// ============================================================================

func TestMergeOptionsRoundtrip(t *testing.T) {
	t.Parallel()
	original := wt.MergeOptions{
		MergeMethod: "squash",
		Keep:        true,
	}

	proto := MergeOptionsToProto(original)
	require.NotNil(t, proto)
	roundtripped := MergeOptionsFromProto(proto)

	assert.Equal(t, original, roundtripped)
}

func TestMergeOptionsFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := MergeOptionsFromProto(nil)
	assert.Equal(t, wt.MergeOptions{}, result)
}

// ============================================================================
// SessionMeta roundtrip
// ============================================================================

func TestSessionMetaRoundtrip(t *testing.T) {
	t.Parallel()
	completed := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	original := &session.SessionMeta{
		ID:           "meta-123",
		Type:         session.SessionTypeBuilder,
		Status:       session.StatusCompleted,
		RepoName:     "my-repo",
		WorktreeName: "feature-z",
		Prompt:       "Implement feature Z",
		Title:        "Feature Z Implementation",
		Model:        "claude-sonnet-4",
		CreatedAt:    time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
		CompletedAt:  &completed,
	}

	proto := SessionMetaToProto(original)
	require.NotNil(t, proto)
	roundtripped := SessionMetaFromProto(proto)
	require.NotNil(t, roundtripped)

	assert.Equal(t, original.ID, roundtripped.ID)
	assert.Equal(t, original.Type, roundtripped.Type)
	assert.Equal(t, original.Status, roundtripped.Status)
	assert.Equal(t, original.RepoName, roundtripped.RepoName)
	assert.Equal(t, original.WorktreeName, roundtripped.WorktreeName)
	assert.Equal(t, original.Prompt, roundtripped.Prompt)
	assert.Equal(t, original.Title, roundtripped.Title)
	assert.Equal(t, original.Model, roundtripped.Model)
	assertTimeEqual(t, original.CreatedAt, roundtripped.CreatedAt)
	assertTimePtrEqual(t, original.CompletedAt, roundtripped.CompletedAt)
}

func TestSessionMetaToProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, SessionMetaToProto(nil))
}

func TestSessionMetaFromProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, SessionMetaFromProto(nil))
}

func TestSessionMetaRoundtrip_NilCompletedAt(t *testing.T) {
	t.Parallel()
	original := &session.SessionMeta{
		ID:        "meta-456",
		Status:    session.StatusRunning,
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	proto := SessionMetaToProto(original)
	roundtripped := SessionMetaFromProto(proto)
	require.NotNil(t, roundtripped)
	assert.Nil(t, roundtripped.CompletedAt)
}

// ============================================================================
// StoredSession roundtrip
// ============================================================================

func TestStoredSessionRoundtrip_Full(t *testing.T) {
	t.Parallel()
	started := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	completed := time.Date(2025, 6, 1, 10, 5, 0, 0, time.UTC)

	original := &session.StoredSession{
		ID:           "stored-123",
		Type:         session.SessionTypeBuilder,
		Status:       session.StatusCompleted,
		RepoName:     "my-repo",
		WorktreePath: "/home/user/worktrees/repo/feature-x",
		WorktreeName: "feature-x",
		Prompt:       "Fix bugs",
		Title:        "Bug Fixes",
		Model:        "claude-opus-4",
		CreatedAt:    time.Date(2025, 6, 1, 9, 59, 0, 0, time.UTC),
		StartedAt:    &started,
		CompletedAt:  &completed,
		ErrorMsg:     "timeout exceeded",
		Progress: &session.StoredProgress{
			TurnCount:    10,
			TotalCostUSD: 2.50,
			InputTokens:  100000,
			OutputTokens: 20000,
		},
		Output: []session.OutputLine{
			{
				Timestamp: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
				Type:      session.OutputTypeText,
				Content:   "Starting analysis...",
			},
			{
				Timestamp: time.Date(2025, 6, 1, 10, 1, 0, 0, time.UTC),
				Type:      session.OutputTypeToolStart,
				ToolName:  "edit_file",
				ToolID:    "tool-1",
				ToolState: session.ToolStateComplete,
			},
		},
	}

	proto := StoredSessionToProto(original)
	require.NotNil(t, proto)
	roundtripped := StoredSessionFromProto(proto)
	require.NotNil(t, roundtripped)

	assert.Equal(t, original.ID, roundtripped.ID)
	assert.Equal(t, original.Type, roundtripped.Type)
	assert.Equal(t, original.Status, roundtripped.Status)
	assert.Equal(t, original.RepoName, roundtripped.RepoName)
	assert.Equal(t, original.WorktreePath, roundtripped.WorktreePath)
	assert.Equal(t, original.WorktreeName, roundtripped.WorktreeName)
	assert.Equal(t, original.Prompt, roundtripped.Prompt)
	assert.Equal(t, original.Title, roundtripped.Title)
	assert.Equal(t, original.Model, roundtripped.Model)
	assertTimeEqual(t, original.CreatedAt, roundtripped.CreatedAt)
	assertTimePtrEqual(t, original.StartedAt, roundtripped.StartedAt)
	assertTimePtrEqual(t, original.CompletedAt, roundtripped.CompletedAt)
	assert.Equal(t, original.ErrorMsg, roundtripped.ErrorMsg)

	require.NotNil(t, roundtripped.Progress)
	assert.Equal(t, original.Progress.TurnCount, roundtripped.Progress.TurnCount)
	assert.InDelta(t, original.Progress.TotalCostUSD, roundtripped.Progress.TotalCostUSD, 1e-9)
	assert.Equal(t, original.Progress.InputTokens, roundtripped.Progress.InputTokens)
	assert.Equal(t, original.Progress.OutputTokens, roundtripped.Progress.OutputTokens)

	require.Len(t, roundtripped.Output, 2)
	assert.Equal(t, original.Output[0].Content, roundtripped.Output[0].Content)
	assert.Equal(t, original.Output[0].Type, roundtripped.Output[0].Type)
	assert.Equal(t, original.Output[1].ToolName, roundtripped.Output[1].ToolName)
	assert.Equal(t, original.Output[1].ToolID, roundtripped.Output[1].ToolID)
}

func TestStoredSessionToProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, StoredSessionToProto(nil))
}

func TestStoredSessionFromProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, StoredSessionFromProto(nil))
}

func TestStoredSessionRoundtrip_NilProgress(t *testing.T) {
	t.Parallel()
	original := &session.StoredSession{
		ID:        "stored-456",
		Status:    session.StatusPending,
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	proto := StoredSessionToProto(original)
	roundtripped := StoredSessionFromProto(proto)
	require.NotNil(t, roundtripped)
	assert.Nil(t, roundtripped.Progress)
}

func TestStoredSessionRoundtrip_EmptyOutput(t *testing.T) {
	t.Parallel()
	original := &session.StoredSession{
		ID:        "stored-789",
		Status:    session.StatusRunning,
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Output:    []session.OutputLine{},
	}

	proto := StoredSessionToProto(original)
	roundtripped := StoredSessionFromProto(proto)
	require.NotNil(t, roundtripped)
	assert.Empty(t, roundtripped.Output)
}

// ============================================================================
// Events
// ============================================================================

func TestSessionEventToProto_StateChange(t *testing.T) {
	t.Parallel()
	event := session.SessionStateChangeEvent{
		SessionID: "sess-100",
		OldStatus: session.StatusPending,
		NewStatus: session.StatusRunning,
	}

	proto := SessionEventToProto(event)
	require.NotNil(t, proto)
	require.NotNil(t, proto.GetStateChange())

	sc := proto.GetStateChange()
	assert.Equal(t, "sess-100", sc.SessionId)
	assert.Equal(t, "pending", sc.OldStatus)
	assert.Equal(t, "running", sc.NewStatus)
}

func TestSessionEventToProto_Output(t *testing.T) {
	t.Parallel()
	event := session.SessionOutputEvent{
		SessionID: "sess-200",
		Line: session.OutputLine{
			Timestamp: time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC),
			Type:      session.OutputTypeText,
			Content:   "Hello world",
		},
	}

	proto := SessionEventToProto(event)
	require.NotNil(t, proto)
	require.NotNil(t, proto.GetOutput())

	out := proto.GetOutput()
	assert.Equal(t, "sess-200", out.SessionId)
	require.NotNil(t, out.Line)
	assert.Equal(t, "Hello world", out.Line.Content)
	assert.Equal(t, "text", out.Line.Type)
}

func TestSessionEventToProto_UnknownType(t *testing.T) {
	t.Parallel()
	// Unknown event type should return nil
	result := SessionEventToProto("unknown-event")
	assert.Nil(t, result)
}

func TestSessionEventToProto_InterfaceNil(t *testing.T) {
	t.Parallel()
	result := SessionEventToProto(nil)
	assert.Nil(t, result)
}

// ============================================================================
// RouteRequest roundtrip
// ============================================================================

func TestRouteRequestRoundtrip(t *testing.T) {
	t.Parallel()
	original := taskrouter.RouteRequest{
		Prompt:    "Fix the authentication bug",
		CurrentWT: "feature-auth",
		RepoName:  "my-repo",
		Worktrees: []taskrouter.WorktreeInfo{
			{
				Name:       "feature-auth",
				Path:       "/home/user/worktrees/repo/feature-auth",
				Goal:       "Implement OAuth2",
				Parent:     "main",
				PRState:    "OPEN",
				LastCommit: "Update auth handler",
				IsDirty:    true,
				IsAhead:    true,
				IsMerged:   false,
			},
			{
				Name:       "feature-ui",
				Path:       "/home/user/worktrees/repo/feature-ui",
				Goal:       "New dashboard",
				Parent:     "main",
				PRState:    "",
				LastCommit: "Initial commit",
				IsDirty:    false,
				IsAhead:    false,
				IsMerged:   false,
			},
		},
	}

	proto := RouteRequestToProto(original)
	require.NotNil(t, proto)
	roundtripped := RouteRequestFromProto(proto)

	assert.Equal(t, original.Prompt, roundtripped.Prompt)
	assert.Equal(t, original.CurrentWT, roundtripped.CurrentWT)
	assert.Equal(t, original.RepoName, roundtripped.RepoName)
	require.Len(t, roundtripped.Worktrees, 2)

	assert.Equal(t, original.Worktrees[0].Name, roundtripped.Worktrees[0].Name)
	assert.Equal(t, original.Worktrees[0].Path, roundtripped.Worktrees[0].Path)
	assert.Equal(t, original.Worktrees[0].Goal, roundtripped.Worktrees[0].Goal)
	assert.Equal(t, original.Worktrees[0].Parent, roundtripped.Worktrees[0].Parent)
	assert.Equal(t, original.Worktrees[0].PRState, roundtripped.Worktrees[0].PRState)
	assert.Equal(t, original.Worktrees[0].LastCommit, roundtripped.Worktrees[0].LastCommit)
	assert.Equal(t, original.Worktrees[0].IsDirty, roundtripped.Worktrees[0].IsDirty)
	assert.Equal(t, original.Worktrees[0].IsAhead, roundtripped.Worktrees[0].IsAhead)
	assert.Equal(t, original.Worktrees[0].IsMerged, roundtripped.Worktrees[0].IsMerged)

	assert.Equal(t, original.Worktrees[1].Name, roundtripped.Worktrees[1].Name)
	assert.False(t, roundtripped.Worktrees[1].IsDirty)
}

func TestRouteRequestFromProto_Nil(t *testing.T) {
	t.Parallel()
	result := RouteRequestFromProto(nil)
	assert.Equal(t, taskrouter.RouteRequest{}, result)
}

func TestRouteRequestRoundtrip_EmptyWorktrees(t *testing.T) {
	t.Parallel()
	original := taskrouter.RouteRequest{
		Prompt:   "New task",
		RepoName: "repo",
	}

	proto := RouteRequestToProto(original)
	roundtripped := RouteRequestFromProto(proto)

	assert.Equal(t, original.Prompt, roundtripped.Prompt)
	assert.Empty(t, roundtripped.Worktrees)
}

// ============================================================================
// RouteProposal roundtrip
// ============================================================================

func TestRouteProposalRoundtrip_CreateNew(t *testing.T) {
	t.Parallel()
	original := &taskrouter.RouteProposal{
		Action:    taskrouter.ActionCreateNew,
		Worktree:  "feature-new-login",
		Parent:    "main",
		Reasoning: "No existing worktree matches the login feature request",
	}

	proto := RouteProposalToProto(original)
	require.NotNil(t, proto)
	roundtripped := RouteProposalFromProto(proto)
	require.NotNil(t, roundtripped)

	assert.Equal(t, original.Action, roundtripped.Action)
	assert.Equal(t, original.Worktree, roundtripped.Worktree)
	assert.Equal(t, original.Parent, roundtripped.Parent)
	assert.Equal(t, original.Reasoning, roundtripped.Reasoning)
}

func TestRouteProposalRoundtrip_UseExisting(t *testing.T) {
	t.Parallel()
	original := &taskrouter.RouteProposal{
		Action:    taskrouter.ActionUseExisting,
		Worktree:  "feature-auth",
		Reasoning: "The auth feature worktree closely matches this task",
	}

	proto := RouteProposalToProto(original)
	roundtripped := RouteProposalFromProto(proto)
	require.NotNil(t, roundtripped)

	assert.Equal(t, taskrouter.ActionUseExisting, roundtripped.Action)
	assert.Equal(t, "feature-auth", roundtripped.Worktree)
	assert.Empty(t, roundtripped.Parent)
}

func TestRouteProposalToProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, RouteProposalToProto(nil))
}

func TestRouteProposalFromProto_Nil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, RouteProposalFromProto(nil))
}
