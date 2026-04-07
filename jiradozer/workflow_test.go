package jiradozer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// --- Mock Tracker ---

type trackerCall struct {
	method string
	args   []string
}

type mockWorkflowTracker struct {
	commentSets        [][]tracker.Comment // sequence of comment responses for polling
	workflowStates     []tracker.WorkflowState
	comments           []tracker.Comment  // returned by FetchComments
	postCommentReply   *tracker.Comment   // if set, PostComment always returns this
	postCommentReplies []*tracker.Comment // if set, PostComment cycles through these in order
	calls              []trackerCall
	mu                 sync.Mutex
	commentIdx         int // tracks which comment set to return (for polling)
	postCommentIdx     int // tracks which postCommentReplies entry to use
}

func (m *mockWorkflowTracker) FetchIssue(_ context.Context, id string) (*tracker.Issue, error) {
	m.recordCall("FetchIssue", id)
	return nil, nil
}

func (m *mockWorkflowTracker) ListIssues(_ context.Context, _ tracker.IssueFilter) ([]*tracker.Issue, error) {
	return nil, nil
}

func (m *mockWorkflowTracker) FetchComments(_ context.Context, issueID string, _ time.Time) ([]tracker.Comment, error) {
	m.recordCall("FetchComments", issueID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.commentSets != nil && m.commentIdx < len(m.commentSets) {
		comments := m.commentSets[m.commentIdx]
		m.commentIdx++
		return comments, nil
	}
	return m.comments, nil
}

func (m *mockWorkflowTracker) FetchWorkflowStates(_ context.Context, teamID string) ([]tracker.WorkflowState, error) {
	m.recordCall("FetchWorkflowStates", teamID)
	return m.workflowStates, nil
}

func (m *mockWorkflowTracker) PostComment(_ context.Context, issueID string, body string) (tracker.Comment, error) {
	m.mu.Lock()
	m.calls = append(m.calls, trackerCall{method: "PostComment", args: []string{issueID, body}})
	reply := m.postCommentReply
	var seqReply *tracker.Comment
	if reply == nil && len(m.postCommentReplies) > 0 {
		idx := m.postCommentIdx
		if idx < len(m.postCommentReplies) {
			m.postCommentIdx++
		} else {
			idx = len(m.postCommentReplies) - 1
		}
		seqReply = m.postCommentReplies[idx]
	}
	m.mu.Unlock()

	if reply != nil {
		return *reply, nil
	}
	if seqReply != nil {
		return *seqReply, nil
	}
	return tracker.Comment{CreatedAt: time.Now()}, nil
}

func (m *mockWorkflowTracker) UpdateIssueState(_ context.Context, issueID string, stateID string) error {
	m.recordCall("UpdateIssueState", issueID, stateID)
	return nil
}

func (m *mockWorkflowTracker) recordCall(method string, args ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, trackerCall{method: method, args: args})
}

func (m *mockWorkflowTracker) getCalls(method string) []trackerCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []trackerCall
	for _, c := range m.calls {
		if c.method == method {
			result = append(result, c)
		}
	}
	return result
}

// --- Test Helpers ---

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func testIssue() *tracker.Issue {
	desc := "Fix the widget rendering bug"
	url := "https://linear.app/team/ENG-123"
	return &tracker.Issue{
		ID:          "issue-id-1",
		Identifier:  "ENG-123",
		Title:       "Widget bug",
		Description: &desc,
		URL:         &url,
		TeamID:      "team-1",
		Labels:      []string{"bug"},
	}
}

func testConfig() *Config {
	return &Config{
		Tracker:      TrackerConfig{Kind: "linear", APIKey: "test-key"},
		Agent:        AgentConfig{Model: "sonnet"},
		WorkDir:      ".",
		BaseBranch:   "main",
		PollInterval: 50 * time.Millisecond,
		MaxBudgetUSD: 50.0,
		Plan:         StepConfig{PermissionMode: "plan", MaxTurns: 10},
		Build:        StepConfig{PermissionMode: "bypass", MaxTurns: 30},
		CreatePR:     StepConfig{PermissionMode: "bypass", MaxTurns: 5},
		Validate:     StepConfig{PermissionMode: "bypass", MaxTurns: 10},
		Ship:         StepConfig{PermissionMode: "bypass", MaxTurns: 10},
		States: StatesConfig{
			InProgress: "In Progress",
			InReview:   "In Review",
			Done:       "Done",
		},
	}
}

func testWorkflowStates() []tracker.WorkflowState {
	return []tracker.WorkflowState{
		{ID: "state-ip", Name: "In Progress", Type: "started"},
		{ID: "state-ir", Name: "In Review", Type: "started"},
		{ID: "state-done", Name: "Done", Type: "completed"},
	}
}

// --- State Machine Additional Tests ---

func TestStateMachineForceState(t *testing.T) {
	sm := NewStateMachine()
	// ForceState bypasses validation — can force to any state.
	sm.ForceState(StepFailed)
	assert.Equal(t, StepFailed, sm.Current())

	history := sm.History()
	require.Len(t, history, 1)
	assert.Equal(t, StepInit, history[0].From)
	assert.Equal(t, StepFailed, history[0].To)
	assert.Equal(t, "forced", history[0].Trigger)
}

func TestStateMachineShipReviewRedo(t *testing.T) {
	sm := NewStateMachine()
	walkTo(t, sm, StepShipReview)
	require.NoError(t, sm.Transition(StepShipping, "redo_ship"))
	assert.Equal(t, StepShipping, sm.Current())
}

func TestStateMachineValidateReviewToShipping(t *testing.T) {
	sm := NewStateMachine()
	walkTo(t, sm, StepValidateReview)
	require.NoError(t, sm.Transition(StepShipping, "approved"))
	assert.Equal(t, StepShipping, sm.Current())
}

func TestStateMachineInvalidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from WorkflowStep
		to   WorkflowStep
	}{
		{"init_to_done", StepInit, StepDone},
		{"planning_to_done", StepPlanning, StepDone},
		{"plan_review_to_validating", StepPlanReview, StepValidating},
		{"building_to_plan_review", StepBuilding, StepPlanReview},
		{"init_to_build_review", StepInit, StepBuildReview},
		{"planning_to_shipping", StepPlanning, StepShipping},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := NewStateMachine()
			walkTo(t, sm, tt.from)
			err := sm.Transition(tt.to, "invalid")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid transition")
		})
	}
}

func TestStateMachineHistoryTimestamps(t *testing.T) {
	sm := NewStateMachine()
	before := time.Now()
	require.NoError(t, sm.Transition(StepPlanning, "start"))
	after := time.Now()

	history := sm.History()
	require.Len(t, history, 1)
	assert.False(t, history[0].Timestamp.Before(before))
	assert.False(t, history[0].Timestamp.After(after))
}

func TestStateMachineConcurrentAccess(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StepPlanning, "start"))

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sm.Current()
			_ = sm.History()
		}()
	}
	wg.Wait()
}

// --- Workflow Tests ---

// TestWorkflow_ResolveStateIDs verifies state name → ID mapping.
func TestWorkflow_ResolveStateIDs(t *testing.T) {
	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	err := wf.resolveStateIDs(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "state-ip", wf.stateIDs["in_progress"])
	assert.Equal(t, "state-ir", wf.stateIDs["in_review"])
	assert.Equal(t, "state-done", wf.stateIDs["done"])
}

func TestWorkflow_ResolveStateIDs_NoTeamID(t *testing.T) {
	mt := &mockWorkflowTracker{}
	issue := testIssue()
	issue.TeamID = ""

	wf := NewWorkflow(mt, issue, testConfig(), discardLogger())
	err := wf.resolveStateIDs(context.Background())
	require.NoError(t, err)
	// No state IDs resolved — empty map.
	assert.Empty(t, wf.stateIDs)
}

func TestWorkflow_ResolveStateIDs_MissingState(t *testing.T) {
	mt := &mockWorkflowTracker{
		workflowStates: []tracker.WorkflowState{
			{ID: "state-ip", Name: "In Progress", Type: "started"},
			// Missing "In Review" and "Done".
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	err := wf.resolveStateIDs(context.Background())
	require.NoError(t, err)
	// Only "in_progress" resolved.
	assert.Equal(t, "state-ip", wf.stateIDs["in_progress"])
	_, ok := wf.stateIDs["in_review"]
	assert.False(t, ok)
	_, ok = wf.stateIDs["done"]
	assert.False(t, ok)
}

// TestWorkflow_Fail verifies the fail() method transitions to StepFailed.
func TestWorkflow_Fail(t *testing.T) {
	mt := &mockWorkflowTracker{}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	wf.fail(context.Background(), fmt.Errorf("agent crashed"))
	assert.Equal(t, StepFailed, wf.state.Current())
	assert.EqualError(t, wf.lastError, "agent crashed")
}

func TestWorkflow_FailFromInit_ForcesState(t *testing.T) {
	mt := &mockWorkflowTracker{}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	// Init → Failed is not a valid transition, so fail() should force it.
	wf.fail(context.Background(), fmt.Errorf("startup error"))
	assert.Equal(t, StepFailed, wf.state.Current())
	assert.EqualError(t, wf.lastError, "startup error")
}

// TestPostWaitingComment verifies comment format.
func TestPostWaitingComment(t *testing.T) {
	mt := &mockWorkflowTracker{}
	_, err := PostWaitingComment(context.Background(), mt, "issue-1", StepPlanReview)
	require.NoError(t, err)

	calls := mt.getCalls("PostComment")
	require.Len(t, calls, 1)
	assert.Equal(t, "issue-1", calls[0].args[0])
	body := calls[0].args[1]
	assert.Contains(t, body, "plan_review")
	assert.Contains(t, body, "approve")
	assert.Contains(t, body, "redo")
}

// TestWorkflow_TransitionToReview verifies review transition + state update + waiting comment.
func TestWorkflow_TransitionToReview(t *testing.T) {
	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	wf.transitionToReview(context.Background(), StepPlanReview, "plan_complete")

	// State should be PlanReview.
	assert.Equal(t, StepPlanReview, wf.state.Current())

	// Should have updated issue state to "In Review".
	stateCalls := mt.getCalls("UpdateIssueState")
	found := false
	for _, c := range stateCalls {
		if c.args[1] == "state-ir" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected UpdateIssueState with in_review state ID")

	// Should have posted a waiting comment.
	commentCalls := mt.getCalls("PostComment")
	assert.NotEmpty(t, commentCalls)
}

// TestWorkflow_TransitionToReview_SkipsReviewMachineryForNonReviewStep verifies that
// transitioning to a non-review step (e.g. StepCreatingPR) does not update issue state
// or post a waiting comment.
func TestWorkflow_TransitionToReview_SkipsReviewMachineryForNonReviewStep(t *testing.T) {
	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))
	require.NoError(t, wf.state.Transition(StepBuilding, "approved"))

	wf.transitionToReview(context.Background(), StepCreatingPR, "build_complete")

	assert.Equal(t, StepCreatingPR, wf.state.Current())

	// Should NOT have updated issue state or posted a waiting comment.
	assert.Empty(t, mt.getCalls("UpdateIssueState"), "should not update issue state for non-review step")
	assert.Empty(t, mt.getCalls("PostComment"), "should not post waiting comment for non-review step")
}

// TestWorkflow_RunReview_Approve tests that an approve comment advances to the next step.
func TestWorkflow_RunReview_Approve(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "lgtm", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.runReview(context.Background(), StepBuilding, StepPlanning)

	assert.Equal(t, StepBuilding, wf.state.Current())
	assert.Empty(t, wf.feedback) // Feedback cleared on approve.
}

// TestWorkflow_RunReview_Redo tests that a redo comment goes back to the redo target.
func TestWorkflow_RunReview_Redo(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "redo\n\nPlease fix the tests", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.runReview(context.Background(), StepBuilding, StepPlanning)

	assert.Equal(t, StepPlanning, wf.state.Current())
	assert.Contains(t, wf.feedback, "redo")
	assert.Contains(t, wf.feedback, "Please fix the tests")
}

// TestWorkflow_RunReview_Comment tests that a generic comment is treated as feedback.
func TestWorkflow_RunReview_Comment(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "Can you also handle the edge case?", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.runReview(context.Background(), StepBuilding, StepPlanning)

	// Generic comment also goes to redo target.
	assert.Equal(t, StepPlanning, wf.state.Current())
	assert.Equal(t, "Can you also handle the edge case?", wf.feedback)
}

// TestWorkflow_RunReview_ApproveVariants tests all approve keywords in review context.
func TestWorkflow_RunReview_ApproveVariants(t *testing.T) {
	variants := []string{"approve", "APPROVE", "lgtm", "LGTM", "ship it", "approved"}

	for _, keyword := range variants {
		t.Run(keyword, func(t *testing.T) {
			mt := &mockWorkflowTracker{
				commentSets: [][]tracker.Comment{
					{{ID: "c1", Body: keyword, IsSelf: false, CreatedAt: time.Now()}},
				},
			}

			wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
			require.NoError(t, wf.state.Transition(StepPlanning, "start"))
			require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

			wf.runReview(context.Background(), StepBuilding, StepPlanning)

			assert.Equal(t, StepBuilding, wf.state.Current(), "keyword %q should approve", keyword)
		})
	}
}

// TestWorkflow_RunReview_LGTMWithNotes tests that "lgtm\n\nsome notes" is still approved.
func TestWorkflow_RunReview_LGTMWithNotes(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "lgtm\n\nGreat work, minor nit on line 42 but ship it", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.runReview(context.Background(), StepBuilding, StepPlanning)
	assert.Equal(t, StepBuilding, wf.state.Current())
}

// TestWorkflow_RunReview_RedoVariants tests all redo keywords.
func TestWorkflow_RunReview_RedoVariants(t *testing.T) {
	variants := []string{"redo", "REDO", "Redo", "retry", "RETRY", "redo: fix it", "retry please"}

	for _, keyword := range variants {
		t.Run(keyword, func(t *testing.T) {
			mt := &mockWorkflowTracker{
				commentSets: [][]tracker.Comment{
					{{ID: "c1", Body: keyword, IsSelf: false, CreatedAt: time.Now()}},
				},
			}

			wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
			require.NoError(t, wf.state.Transition(StepPlanning, "start"))
			require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

			wf.runReview(context.Background(), StepBuilding, StepPlanning)

			assert.Equal(t, StepPlanning, wf.state.Current(), "keyword %q should redo", keyword)
			assert.NotEmpty(t, wf.feedback)
		})
	}
}

// TestWorkflow_SessionIDTracking verifies session IDs are stored per-step.
func TestWorkflow_SessionIDTracking(t *testing.T) {
	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), testConfig(), discardLogger())

	// Simulate session IDs being stored.
	wf.sessionIDs[StepPlanning] = "session-plan-1"
	wf.sessionIDs[StepBuilding] = "session-build-1"

	assert.Equal(t, "session-plan-1", wf.sessionIDs[StepPlanning])
	assert.Equal(t, "session-build-1", wf.sessionIDs[StepBuilding])
	assert.Empty(t, wf.sessionIDs[StepValidating])
}

// TestWorkflow_FeedbackClearedOnApprove verifies feedback is cleared after approval.
func TestWorkflow_FeedbackClearedOnApprove(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "approve", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	wf.feedback = "previous feedback from redo"
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.runReview(context.Background(), StepBuilding, StepPlanning)

	assert.Equal(t, StepBuilding, wf.state.Current())
	assert.Empty(t, wf.feedback, "feedback should be cleared after approval")
}

// TestWorkflow_FeedbackPreservedOnRedo verifies feedback is set after redo.
func TestWorkflow_FeedbackPreservedOnRedo(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "redo\n\nUse a different approach", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.runReview(context.Background(), StepBuilding, StepPlanning)

	assert.Equal(t, StepPlanning, wf.state.Current())
	assert.Equal(t, "redo\n\nUse a different approach", wf.feedback)
}

// TestWorkflow_BuildReviewBackToPlanning verifies backtracking from build review.
func TestWorkflow_BuildReviewBackToPlanning(t *testing.T) {
	sm := NewStateMachine()
	walkTo(t, sm, StepBuildReview)
	require.NoError(t, sm.Transition(StepPlanning, "back_to_plan"))
	assert.Equal(t, StepPlanning, sm.Current())
}

// TestWorkflow_ValidateReviewToBuilding verifies that validate review can go to building.
func TestWorkflow_ValidateReviewToBuilding(t *testing.T) {
	sm := NewStateMachine()
	walkTo(t, sm, StepValidateReview)
	require.NoError(t, sm.Transition(StepBuilding, "fix_failures"))
	assert.Equal(t, StepBuilding, sm.Current())
}

// TestWorkflow_MultipleRedoLoops verifies multiple redo cycles.
func TestWorkflow_MultipleRedoLoops(t *testing.T) {
	sm := NewStateMachine()
	require.NoError(t, sm.Transition(StepPlanning, "start"))
	require.NoError(t, sm.Transition(StepPlanReview, "plan_done"))

	// Redo loop 1.
	require.NoError(t, sm.Transition(StepPlanning, "redo"))
	require.NoError(t, sm.Transition(StepPlanReview, "plan_done"))

	// Redo loop 2.
	require.NoError(t, sm.Transition(StepPlanning, "redo"))
	require.NoError(t, sm.Transition(StepPlanReview, "plan_done"))

	// Finally approve.
	require.NoError(t, sm.Transition(StepBuilding, "approved"))
	assert.Equal(t, StepBuilding, sm.Current())

	// Verify history length: 7 transitions.
	assert.Len(t, sm.History(), 7)
}

// TestWorkflow_TransitionToReview_PreservesLastCommentAt verifies that
// transitionToReview does not overwrite a lastCommentAt that was already set
// by the step result comment, preventing a race where user approval posted
// between the result comment and the waiting comment would be missed.
func TestWorkflow_TransitionToReview_PreservesLastCommentAt(t *testing.T) {
	resultTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	waitingTime := time.Date(2025, 6, 15, 12, 0, 5, 0, time.UTC)
	mt := &mockWorkflowTracker{
		workflowStates:   testWorkflowStates(),
		postCommentReply: &tracker.Comment{CreatedAt: waitingTime},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	// Simulate runStep having set lastCommentAt from the result comment.
	wf.lastCommentAt = resultTime

	wf.transitionToReview(context.Background(), StepPlanReview, "plan_complete")

	assert.Equal(t, resultTime, wf.lastCommentAt, "lastCommentAt should be preserved from step result, not overwritten by waiting comment")
}

// TestWorkflow_TransitionToReview_ZeroTimestampFallback verifies that a zero
// CreatedAt falls back to a recent time (time.Now) instead of staying zero.
func TestWorkflow_TransitionToReview_ZeroTimestampFallback(t *testing.T) {
	mt := &mockWorkflowTracker{
		workflowStates:   testWorkflowStates(),
		postCommentReply: &tracker.Comment{CreatedAt: time.Time{}},
	}

	before := time.Now()
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	wf.transitionToReview(context.Background(), StepPlanReview, "plan_complete")

	assert.False(t, wf.lastCommentAt.IsZero(), "lastCommentAt should not be zero")
	assert.False(t, wf.lastCommentAt.Before(before), "lastCommentAt should be recent (time.Now fallback)")
}

// TestWorkflow_TransitionToReview_FallsBackToWaitingCommentTimestamp verifies that
// when lastCommentAt is zero (step result comment failed or returned no timestamp),
// transitionToReview uses the waiting comment's server timestamp instead of time.Now().
func TestWorkflow_TransitionToReview_FallsBackToWaitingCommentTimestamp(t *testing.T) {
	waitingTime := time.Date(2025, 6, 15, 12, 0, 5, 0, time.UTC)
	mt := &mockWorkflowTracker{
		workflowStates:   testWorkflowStates(),
		postCommentReply: &tracker.Comment{CreatedAt: waitingTime},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	// lastCommentAt is zero — simulates a failed or zero-timestamp step result comment.
	require.True(t, wf.lastCommentAt.IsZero())

	wf.transitionToReview(context.Background(), StepPlanReview, "plan_complete")

	assert.Equal(t, waitingTime, wf.lastCommentAt, "should use waiting comment's server timestamp when lastCommentAt is zero")
}

// TestWorkflow_AllReviewStepsFilterBotComments verifies each review step.
func TestWorkflow_AllReviewStepsFilterBotComments(t *testing.T) {
	reviews := []struct {
		name          string
		reviewStep    WorkflowStep
		approveTarget WorkflowStep
		redoTarget    WorkflowStep
	}{
		{"plan_review", StepPlanReview, StepBuilding, StepPlanning},
		{"build_review", StepBuildReview, StepValidating, StepBuilding},
		{"validate_review", StepValidateReview, StepShipping, StepValidating},
		{"ship_review", StepShipReview, StepDone, StepShipping},
	}

	for _, r := range reviews {
		t.Run(r.name, func(t *testing.T) {
			mt := &mockWorkflowTracker{
				commentSets: [][]tracker.Comment{
					{
						{ID: "bot", Body: "step complete", IsSelf: true, CreatedAt: time.Now()},
						{ID: "human", Body: "approve", IsSelf: false, CreatedAt: time.Now().Add(time.Second)},
					},
				},
			}

			wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
			walkTo(t, wf.state, r.reviewStep)

			wf.runReview(context.Background(), r.approveTarget, r.redoTarget)
			assert.Equal(t, r.approveTarget, wf.state.Current())
		})
	}
}
