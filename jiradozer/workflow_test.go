package jiradozer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
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
	postCommentReplies []*tracker.Comment // if set, PostComment returns these in order (last repeated)
	fetchIssueReply    *tracker.Issue     // if set, FetchIssue returns this (used to feed labels into refreshLabels)
	fetchIssueReplies  []*tracker.Issue   // if set, FetchIssue returns these in order (last repeated)
	calls              []trackerCall
	mu                 sync.Mutex
	commentIdx         int // tracks which comment set to return (for polling)
	postCommentIdx     int // tracks index into postCommentReplies
	fetchIssueIdx      int // tracks index into fetchIssueReplies
}

func (m *mockWorkflowTracker) FetchIssue(_ context.Context, id string) (*tracker.Issue, error) {
	m.mu.Lock()
	m.calls = append(m.calls, trackerCall{method: "FetchIssue", args: []string{id}})
	var reply *tracker.Issue
	if m.fetchIssueReply != nil {
		reply = m.fetchIssueReply
	} else if len(m.fetchIssueReplies) > 0 {
		idx := m.fetchIssueIdx
		if idx >= len(m.fetchIssueReplies) {
			idx = len(m.fetchIssueReplies) - 1
		} else {
			m.fetchIssueIdx++
		}
		reply = m.fetchIssueReplies[idx]
	}
	m.mu.Unlock()
	return reply, nil
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
	var reply *tracker.Comment
	if m.postCommentReply != nil {
		reply = m.postCommentReply
	} else if len(m.postCommentReplies) > 0 {
		idx := m.postCommentIdx
		if idx >= len(m.postCommentReplies) {
			idx = len(m.postCommentReplies) - 1
		} else {
			m.postCommentIdx++
		}
		reply = m.postCommentReplies[idx]
	}
	m.mu.Unlock()

	if reply != nil {
		return *reply, nil
	}
	return tracker.Comment{CreatedAt: time.Now()}, nil
}

func (m *mockWorkflowTracker) UpdateIssueState(_ context.Context, issueID string, stateID string) error {
	m.recordCall("UpdateIssueState", issueID, stateID)
	return nil
}

func (m *mockWorkflowTracker) AddLabel(_ context.Context, issueID string, label string) error {
	m.recordCall("AddLabel", issueID, label)
	return nil
}

func (m *mockWorkflowTracker) RemoveLabel(_ context.Context, issueID string, label string) error {
	m.recordCall("RemoveLabel", issueID, label)
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

// TestWorkflow_RunReview_Comment_PlanReview tests that a generic comment during
// plan review advances to build (approve with feedback), not back to planning.
func TestWorkflow_RunReview_Comment_PlanReview(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "Can you also handle the edge case?", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.runReview(context.Background(), StepBuilding, StepPlanning)

	// During plan review, generic comments advance to build with feedback.
	assert.Equal(t, StepBuilding, wf.state.Current())
	assert.Equal(t, "Can you also handle the edge case?", wf.feedback)
}

// TestWorkflow_RunReview_Comment_BuildReview tests that a generic comment during
// build review goes back to redo target (existing behavior preserved).
func TestWorkflow_RunReview_Comment_BuildReview(t *testing.T) {
	mt := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "Please also fix the formatting", IsSelf: false, CreatedAt: time.Now()}},
		},
	}

	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
	walkTo(t, wf.state, StepBuildReview)

	wf.runReview(context.Background(), StepValidating, StepBuilding)

	// During build review, generic comments redo the build step.
	assert.Equal(t, StepBuilding, wf.state.Current())
	assert.Equal(t, "Please also fix the formatting", wf.feedback)
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

// TestWorkflow_RunStep_SetsLastCommentAt verifies that runStep captures the server
// timestamp from the result PostComment as the polling anchor, and that a stale
// lastCommentAt from a prior review cycle is reset at step start. Uses distinct
// timestamps for the result comment vs the subsequent waiting comment so that the
// test fails if runStep stops capturing from the result comment (instead of silently
// passing via the transitionToReview fallback).
func TestWorkflow_RunStep_SetsLastCommentAt(t *testing.T) {
	resultTime := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	waitingTime := time.Date(2025, 6, 15, 12, 0, 5, 0, time.UTC)

	// First PostComment = result comment, second = waiting comment.
	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
		postCommentReplies: []*tracker.Comment{
			{CreatedAt: resultTime},
			{CreatedAt: waitingTime},
		},
	}

	cfg := testConfig()
	wf := NewWorkflow(mt, testIssue(), cfg, discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	// Inject a stale lastCommentAt simulating a leftover from a prior review cycle.
	staleTime := time.Date(2025, 6, 14, 10, 0, 0, 0, time.UTC)
	wf.lastCommentAt = staleTime

	// Stub runStepAgent so runStep executes without invoking a real agent.
	wf.runStepAgent = func(_ context.Context, _ string, _ PromptData, _ StepConfig, _ string, _ string, _ string, _ *render.Renderer, _ *slog.Logger) (string, string, error) {
		return "step output", "session-1", nil
	}

	wf.runStep(context.Background(), "plan", cfg.Plan, StepPlanReview, "plan_complete")

	// Must be resultTime (result comment), not waitingTime (waiting comment) or staleTime.
	assert.Equal(t, resultTime, wf.lastCommentAt, "lastCommentAt should be set from result comment, not waiting comment or stale value")
}

// TestWorkflow_RunStepRounds_SetsLastCommentAtFromFinalRound verifies that
// runStepRounds captures the server timestamp from the final round's PostComment
// as the polling anchor, discarding any stale value from a prior review cycle.
// Uses distinct timestamps per round so that only the final round's timestamp
// wins — confirming it is the final-round comment, not an earlier one.
func TestWorkflow_RunStepRounds_SetsLastCommentAtFromFinalRound(t *testing.T) {
	round1Time := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	round2Time := time.Date(2025, 6, 15, 12, 1, 0, 0, time.UTC)
	waitingTime := time.Date(2025, 6, 15, 12, 2, 0, 0, time.UTC)

	// Sequence: round1 result comment → round2 result comment → waiting comment.
	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
		postCommentReplies: []*tracker.Comment{
			{CreatedAt: round1Time},
			{CreatedAt: round2Time},
			{CreatedAt: waitingTime},
		},
	}

	cfg := testConfig()
	// Build a two-round step config.
	roundsCfg := StepConfig{
		Rounds: []RoundConfig{
			{Prompt: "round one"},
			{Prompt: "round two"},
		},
	}

	wf := NewWorkflow(mt, testIssue(), cfg, discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	// Inject a stale lastCommentAt simulating a prior review cycle.
	staleTime := time.Date(2025, 6, 14, 10, 0, 0, 0, time.UTC)
	wf.lastCommentAt = staleTime

	// Stub runStepAgent so rounds execute without a real agent.
	wf.runStepAgent = func(_ context.Context, _ string, _ PromptData, _ StepConfig, _ string, _ string, _ string, _ *render.Renderer, _ *slog.Logger) (string, string, error) {
		return "round output", "", nil
	}

	wf.runStepRounds(context.Background(), "plan", roundsCfg, StepPlanReview, "plan_complete")

	// Must be round2Time (final round's result comment), not round1Time, waitingTime, or staleTime.
	assert.Equal(t, round2Time, wf.lastCommentAt, "lastCommentAt should be set from final round's result comment")
}

// TestWorkflow_RunStepRounds_CommandFirstFeedbackInjection verifies that when
// the first round is a command, redo feedback is injected into the first agent
// round (not the command round, which cannot accept feedback).
func TestWorkflow_RunStepRounds_CommandFirstFeedbackInjection(t *testing.T) {
	t.Parallel()

	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
		postCommentReply: &tracker.Comment{
			CreatedAt: time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
		},
	}

	cfg := testConfig()
	// Two-round step: command first, then agent.
	roundsCfg := StepConfig{
		Rounds: []RoundConfig{
			{Command: "echo command-round"},
			{Prompt: "agent round prompt"},
		},
	}

	wf := NewWorkflow(mt, testIssue(), cfg, discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	// Inject redo feedback — should go to the agent round (index 1), not the command round.
	wf.feedback = "please fix the tests"

	var capturedFeedback string
	wf.runStepAgent = func(_ context.Context, _ string, _ PromptData, _ StepConfig, _ string, feedback string, _ string, _ *render.Renderer, _ *slog.Logger) (string, string, error) {
		capturedFeedback = feedback
		return "agent output", "", nil
	}

	wf.runStepRounds(context.Background(), "build", roundsCfg, StepBuildReview, "build_complete")

	assert.Equal(t, "please fix the tests", capturedFeedback, "feedback should be injected into the first agent round")
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

func TestCaptureOutput_PersistsPlanToDisk(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	cfg := testConfig()
	cfg.WorkDir = workDir

	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), cfg, discardLogger())
	planText := "## Plan\n\n1. Do thing A\n2. Do thing B"
	wf.captureOutput("plan", planText)

	// Verify in-memory state.
	assert.Equal(t, planText, wf.plan)

	// Verify persisted file.
	planPath := PlanFilePath(workDir)
	content, err := os.ReadFile(planPath)
	require.NoError(t, err)
	assert.Equal(t, planText, string(content))
}

func TestCaptureOutput_BuildDoesNotPersist(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	cfg := testConfig()
	cfg.WorkDir = workDir

	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), cfg, discardLogger())
	wf.captureOutput("build", "build output")

	assert.Equal(t, "build output", wf.buildOutput)

	// No plan file should be created for build output.
	_, err := os.Stat(PlanFilePath(workDir))
	assert.True(t, errors.Is(err, fs.ErrNotExist))
}

func TestCaptureOutput_EmptyPlanDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	cfg := testConfig()
	cfg.WorkDir = workDir

	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), cfg, discardLogger())

	// Persist a valid plan first.
	wf.captureOutput("plan", "valid plan content")
	planPath := PlanFilePath(workDir)
	content, err := os.ReadFile(planPath)
	require.NoError(t, err)
	assert.Equal(t, "valid plan content", string(content))

	// Now capture empty plan output — should NOT overwrite.
	wf.captureOutput("plan", "")
	content, err = os.ReadFile(planPath)
	require.NoError(t, err)
	assert.Equal(t, "valid plan content", string(content), "empty output should not overwrite valid plan")
}

func TestPlanFilePath(t *testing.T) {
	t.Parallel()
	got := PlanFilePath("/tmp/myproject")
	assert.Equal(t, filepath.Join("/tmp/myproject", ".jiradozer", "plan.md"), got)
}

// --- Circuit Breaker Tests ---

// TestWorkflow_CircuitBreaker_RedoExceedsMax verifies that after maxRedos redo
// attempts, the workflow transitions to StepFailed instead of looping.
func TestWorkflow_CircuitBreaker_RedoExceedsMax(t *testing.T) {
	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), testConfig(), discardLogger())
	wf.maxRedos = 2
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	// First redo — OK.
	wf.feedback = "fix tests"
	require.NoError(t, wf.tryRedo(context.Background(), StepPlanning))
	assert.Equal(t, StepPlanning, wf.state.Current())

	// Back to review.
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	// Second redo — OK.
	require.NoError(t, wf.tryRedo(context.Background(), StepPlanning))
	assert.Equal(t, StepPlanning, wf.state.Current())

	// Back to review.
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	// Third redo — exceeds max, should fail.
	err := wf.tryRedo(context.Background(), StepPlanning)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-run 2 times")
	assert.Contains(t, err.Error(), "max 2")
}

// TestWorkflow_CircuitBreaker_DefaultLimit verifies the default maxRedos value.
func TestWorkflow_CircuitBreaker_DefaultLimit(t *testing.T) {
	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), testConfig(), discardLogger())
	assert.Equal(t, DefaultMaxRedos, wf.maxRedos)
}

// TestWorkflow_CircuitBreaker_IndependentPerStep verifies that redo counts
// are tracked independently per step target.
func TestWorkflow_CircuitBreaker_IndependentPerStep(t *testing.T) {
	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), testConfig(), discardLogger())
	wf.maxRedos = 1

	// One redo for planning.
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))
	require.NoError(t, wf.tryRedo(context.Background(), StepPlanning))

	// Advance through to build review.
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))
	require.NoError(t, wf.state.Transition(StepBuilding, "approved"))
	require.NoError(t, wf.state.Transition(StepCreatingPR, "build_done"))
	require.NoError(t, wf.state.Transition(StepBuildReview, "pr_created"))

	// One redo for building — should be allowed (independent counter).
	require.NoError(t, wf.tryRedo(context.Background(), StepBuilding))
	assert.Equal(t, StepBuilding, wf.state.Current())
}

// --- FeedbackComment Per-Step Tests ---

// TestWorkflow_RunReview_CommentPerStep verifies FeedbackComment transitions
// correctly for each review step: PlanReview advances, others redo.
func TestWorkflow_RunReview_CommentPerStep(t *testing.T) {
	tests := []struct {
		name           string
		reviewStep     WorkflowStep
		approveTarget  WorkflowStep
		redoTarget     WorkflowStep
		expectedTarget WorkflowStep
	}{
		{"plan_review_advances", StepPlanReview, StepBuilding, StepPlanning, StepBuilding},
		{"build_review_redoes", StepBuildReview, StepValidating, StepBuilding, StepBuilding},
		{"validate_review_redoes", StepValidateReview, StepShipping, StepValidating, StepValidating},
		{"ship_review_redoes", StepShipReview, StepDone, StepShipping, StepShipping},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt := &mockWorkflowTracker{
				commentSets: [][]tracker.Comment{
					{{ID: "c1", Body: "some general feedback", IsSelf: false, CreatedAt: time.Now()}},
				},
			}

			wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())
			walkTo(t, wf.state, tt.reviewStep)

			wf.runReview(context.Background(), tt.approveTarget, tt.redoTarget)

			assert.Equal(t, tt.expectedTarget, wf.state.Current(),
				"FeedbackComment at %s should go to %s", tt.reviewStep, tt.expectedTarget)
			assert.Equal(t, "some general feedback", wf.feedback)
		})
	}
}

// --- Full Feedback Loop Simulation ---

// TestWorkflow_FullFeedbackLoop_PlanCommentAdvancesToBuild simulates the exact
// scenario from the bug report: plan completes, user posts a general comment,
// workflow should advance to build (not loop back to planning).
func TestWorkflow_FullFeedbackLoop_PlanCommentAdvancesToBuild(t *testing.T) {
	now := time.Now()
	approveComment := tracker.Comment{ID: "human1", Body: "Looks good, but also handle error cases", IsSelf: false, CreatedAt: now.Add(3 * time.Second)}

	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
		// First call returns bot comments only (plan result + waiting), second returns human comment.
		commentSets: [][]tracker.Comment{
			{
				{ID: "bot1", Body: "## Plan Complete\n\nHere is the plan...", IsSelf: true, CreatedAt: now},
				{ID: "bot2", Body: "**plan_review** — Waiting for review.", IsSelf: true, CreatedAt: now.Add(time.Second)},
			},
			{
				{ID: "bot1", Body: "## Plan Complete\n\nHere is the plan...", IsSelf: true, CreatedAt: now},
				{ID: "bot2", Body: "**plan_review** — Waiting for review.", IsSelf: true, CreatedAt: now.Add(time.Second)},
				approveComment,
			},
		},
	}

	cfg := testConfig()
	wf := NewWorkflow(mt, testIssue(), cfg, discardLogger())
	require.NoError(t, wf.resolveStateIDs(context.Background()))
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	wf.botCommentIDs = []string{"bot1", "bot2"}

	wf.runReview(context.Background(), StepBuilding, StepPlanning)

	// Should advance to building, not loop back to planning.
	assert.Equal(t, StepBuilding, wf.state.Current())
	assert.Equal(t, "Looks good, but also handle error cases", wf.feedback)
}

// TestWorkflow_CircuitBreaker_TripsInRunReview verifies that the circuit breaker
// integrates correctly with runReview — after maxRedos redo comments, the
// workflow transitions to StepFailed.
func TestWorkflow_CircuitBreaker_TripsInRunReview(t *testing.T) {
	cfg := testConfig()
	wf := NewWorkflow(&mockWorkflowTracker{}, testIssue(), cfg, discardLogger())
	wf.maxRedos = 1

	// First redo cycle.
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	mt1 := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c1", Body: "redo: fix approach", IsSelf: false, CreatedAt: time.Now()}},
		},
	}
	wf.tracker = mt1
	wf.runReview(context.Background(), StepBuilding, StepPlanning)
	assert.Equal(t, StepPlanning, wf.state.Current(), "first redo should succeed")

	// Back to review.
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	// Second redo cycle — should trip circuit breaker.
	mt2 := &mockWorkflowTracker{
		commentSets: [][]tracker.Comment{
			{{ID: "c2", Body: "redo: try again", IsSelf: false, CreatedAt: time.Now()}},
		},
	}
	wf.tracker = mt2
	wf.runReview(context.Background(), StepBuilding, StepPlanning)
	assert.Equal(t, StepFailed, wf.state.Current(), "second redo should trip circuit breaker")
	assert.Contains(t, wf.lastError.Error(), "re-run")
}

// --- Phase Label Tests ---

// labelSequence returns the ordered list of label-mutating calls
// (AddLabel/RemoveLabel) with their label args.
func labelSequence(mt *mockWorkflowTracker) []string {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	var out []string
	for _, c := range mt.calls {
		if c.method == "AddLabel" || c.method == "RemoveLabel" {
			out = append(out, c.method+":"+c.args[1])
		}
	}
	return out
}

// TestNewWorkflow_FiltersJiradozerLabelsFromIssue verifies that NewWorkflow
// strips jiradozer-* labels from issue.Labels at construction time so agent
// prompts (which read issue.Labels directly) don't see bookkeeping labels
// before the first successful refreshLabels call.
func TestNewWorkflow_FiltersJiradozerLabelsFromIssue(t *testing.T) {
	t.Parallel()
	issue := testIssue()
	issue.Labels = []string{"bug", "jiradozer-plan-inprogress", "feature", "jiradozer-build-done"}

	wf := NewWorkflow(&mockWorkflowTracker{}, issue, testConfig(), discardLogger())

	// issue.Labels should have jiradozer-* filtered out.
	assert.Equal(t, []string{"bug", "feature"}, wf.issue.Labels)
	// lastLabels should retain the full set for skip decisions.
	assert.Equal(t,
		[]string{"bug", "jiradozer-plan-inprogress", "feature", "jiradozer-build-done"},
		wf.lastLabels)
}

// TestWorkflow_EnterPhase_Idempotent verifies enterPhase adds the inprogress
// label on first call and is a no-op on repeat.
func TestWorkflow_EnterPhase_Idempotent(t *testing.T) {
	mt := &mockWorkflowTracker{}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())

	wf.enterPhase(context.Background(), PhasePlan)
	wf.enterPhase(context.Background(), PhasePlan) // repeat

	assert.Equal(t, []string{"AddLabel:jiradozer-plan-inprogress"}, labelSequence(mt))
	assert.Equal(t, phaseInProgress, wf.phases[PhasePlan])
}

// TestWorkflow_CompletePhase_Idempotent verifies completePhase removes the
// inprogress label and adds the done label, and is a no-op on repeat.
func TestWorkflow_CompletePhase_Idempotent(t *testing.T) {
	mt := &mockWorkflowTracker{}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())

	wf.completePhase(context.Background(), PhasePlan)
	wf.completePhase(context.Background(), PhasePlan) // repeat

	assert.Equal(t,
		[]string{"RemoveLabel:jiradozer-plan-inprogress", "AddLabel:jiradozer-plan-done"},
		labelSequence(mt))
	assert.Equal(t, phaseDone, wf.phases[PhasePlan])
}

// TestWorkflow_ApproveTransition_CrossesPhaseBoundary verifies that a
// plan_review → building transition completes the plan phase and enters the
// build phase in the correct order.
func TestWorkflow_ApproveTransition_CrossesPhaseBoundary(t *testing.T) {
	mt := &mockWorkflowTracker{
		fetchIssueReply: &tracker.Issue{Labels: []string{"bug"}},
	}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())

	// Simulate plan phase already entered (as if by workflow start).
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	wf.phases[PhasePlan] = phaseInProgress
	require.NoError(t, wf.state.Transition(StepPlanReview, "plan_done"))

	require.NoError(t, wf.approveTransition(context.Background(), StepBuilding, "approved"))

	assert.Equal(t, StepBuilding, wf.state.Current())
	assert.Equal(t,
		[]string{
			"RemoveLabel:jiradozer-plan-inprogress",
			"AddLabel:jiradozer-plan-done",
			"AddLabel:jiradozer-build-inprogress",
		},
		labelSequence(mt))
}

// TestWorkflow_ApproveTransition_StaysWithinPhase verifies that a
// build_review → validating transition completes the build phase (started as
// "building" through creating_pr). When re-entering building via redo, the
// phase is not re-entered.
func TestWorkflow_ApproveTransition_BuildToValidate(t *testing.T) {
	mt := &mockWorkflowTracker{
		fetchIssueReply: &tracker.Issue{Labels: []string{}},
	}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())

	walkTo(t, wf.state, StepBuildReview)
	wf.phases[PhaseBuild] = phaseInProgress // simulate build phase already in-flight

	require.NoError(t, wf.approveTransition(context.Background(), StepValidating, "approved"))

	assert.Equal(t, StepValidating, wf.state.Current())
	assert.Equal(t,
		[]string{
			"RemoveLabel:jiradozer-build-inprogress",
			"AddLabel:jiradozer-build-done",
			"AddLabel:jiradozer-validate-inprogress",
		},
		labelSequence(mt))
}

// TestWorkflow_SkipDonePhases_FromInit verifies that skipDonePhases at workflow
// start jumps over phases whose -done label is already present.
func TestWorkflow_SkipDonePhases_FromInit(t *testing.T) {
	workDir := t.TempDir()
	// Seed a persisted plan so the build step has something to work with.
	PersistPlan(workDir, "persisted plan", discardLogger())

	issue := testIssue()
	issue.Labels = []string{"bug", "jiradozer-plan-done", "jiradozer-build-done"}

	mt := &mockWorkflowTracker{
		fetchIssueReply: &tracker.Issue{Labels: issue.Labels},
	}

	cfg := testConfig()
	cfg.WorkDir = workDir
	wf := NewWorkflow(mt, issue, cfg, discardLogger())

	// Workflow starts at StepPlanning (the first step after init transition).
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))
	wf.skipDonePhases(context.Background())

	// Should have jumped forward past plan and build to validating.
	assert.Equal(t, StepValidating, wf.state.Current())
	// Plan should have been loaded from disk since plan phase was skipped.
	assert.Equal(t, "persisted plan", wf.plan)
	// Phases skipped should be marked entered + completed (internal bookkeeping).
	assert.Equal(t, phaseDone, wf.phases[PhasePlan])
	assert.Equal(t, phaseDone, wf.phases[PhaseBuild])
	// No label writes should happen for skipped phases (label already present).
	assert.Empty(t, labelSequence(mt))
}

// TestWorkflow_SkipDonePhases_ClearsStaleInProgress verifies that when a phase
// has both -inprogress and -done labels (e.g. from a prior interrupted run),
// skipDonePhases removes the stale -inprogress label so the issue doesn't end
// up with contradictory phase labels.
func TestWorkflow_SkipDonePhases_ClearsStaleInProgress(t *testing.T) {
	workDir := t.TempDir()
	PersistPlan(workDir, "persisted plan", discardLogger())

	issue := testIssue()
	issue.Labels = []string{"jiradozer-plan-inprogress", "jiradozer-plan-done"}

	mt := &mockWorkflowTracker{
		fetchIssueReply: &tracker.Issue{Labels: issue.Labels},
	}

	cfg := testConfig()
	cfg.WorkDir = workDir
	wf := NewWorkflow(mt, issue, cfg, discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	wf.skipDonePhases(context.Background())

	assert.Equal(t, StepBuilding, wf.state.Current())
	assert.Equal(t, phaseDone, wf.phases[PhasePlan])
	// The stale -inprogress should have been cleared.
	assert.Equal(t,
		[]string{"RemoveLabel:jiradozer-plan-inprogress"},
		labelSequence(mt))
}

// TestWorkflow_SkipDonePhases_AllDone verifies that if every phase is already
// done, the workflow jumps straight to StepDone.
func TestWorkflow_SkipDonePhases_AllDone(t *testing.T) {
	issue := testIssue()
	issue.Labels = []string{
		"jiradozer-plan-done",
		"jiradozer-build-done",
		"jiradozer-validate-done",
		"jiradozer-ship-done",
	}

	mt := &mockWorkflowTracker{
		fetchIssueReply: &tracker.Issue{Labels: issue.Labels},
	}
	wf := NewWorkflow(mt, issue, testConfig(), discardLogger())
	require.NoError(t, wf.state.Transition(StepPlanning, "start"))

	wf.skipDonePhases(context.Background())

	assert.Equal(t, StepDone, wf.state.Current())
}

// TestWorkflow_HandlePhaseBoundary_MidRunSkip simulates a user adding
// `jiradozer-validate-done` mid-run: after build review approves, the
// workflow should complete the build phase and then skip validate, landing on
// StepShipping with the validate-done bookkeeping recorded.
func TestWorkflow_HandlePhaseBoundary_MidRunSkip(t *testing.T) {
	mt := &mockWorkflowTracker{
		// On refresh, the issue now has validate-done.
		fetchIssueReply: &tracker.Issue{Labels: []string{
			"bug",
			"jiradozer-validate-done",
		}},
	}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())

	walkTo(t, wf.state, StepBuildReview)
	wf.phases[PhaseBuild] = phaseInProgress

	require.NoError(t, wf.approveTransition(context.Background(), StepValidating, "approved"))

	// Should have skipped validate and landed on shipping.
	assert.Equal(t, StepShipping, wf.state.Current())
	assert.Equal(t, phaseDone, wf.phases[PhaseValidate])
	// Build is completed (label writes) and ship is entered (label write).
	// Validate phase labels are NOT written — the -done label was user-added.
	assert.Equal(t,
		[]string{
			"RemoveLabel:jiradozer-build-inprogress",
			"AddLabel:jiradozer-build-done",
			"AddLabel:jiradozer-ship-inprogress",
		},
		labelSequence(mt))
}

// TestWorkflow_HandlePhaseBoundary_StepDone verifies that reaching StepDone
// completes the ship phase when ship was entered.
func TestWorkflow_HandlePhaseBoundary_StepDone(t *testing.T) {
	mt := &mockWorkflowTracker{
		fetchIssueReply: &tracker.Issue{Labels: []string{}},
	}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())

	walkTo(t, wf.state, StepShipReview)
	wf.phases[PhaseShip] = phaseInProgress

	require.NoError(t, wf.approveTransition(context.Background(), StepDone, "approved"))

	assert.Equal(t, StepDone, wf.state.Current())
	assert.Equal(t,
		[]string{
			"RemoveLabel:jiradozer-ship-inprogress",
			"AddLabel:jiradozer-ship-done",
		},
		labelSequence(mt))
}

// TestWorkflow_RefreshLabels_FallbackOnError verifies that when FetchIssue
// returns nil (or error), refreshLabels falls back to the cached labels.
func TestWorkflow_RefreshLabels_FallbackOnError(t *testing.T) {
	mt := &mockWorkflowTracker{
		// No fetchIssueReply set — FetchIssue returns (nil, nil).
	}
	issue := testIssue()
	issue.Labels = []string{"bug", "jiradozer-plan-done"}
	wf := NewWorkflow(mt, issue, testConfig(), discardLogger())

	labels := wf.refreshLabels(context.Background())
	assert.Equal(t, []string{"bug", "jiradozer-plan-done"}, labels)
}
