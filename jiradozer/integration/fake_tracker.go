//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// FakeTracker is a stateful in-memory implementation of tracker.IssueTracker
// for integration tests. It records all calls for assertion and maintains
// mutable issue state, including comments with since-filtering.
type FakeTracker struct {
	mu     sync.Mutex
	issues map[string]*fakeIssue // keyed by issue.ID
	states []tracker.WorkflowState
	calls  []FakeTrackerCall
	nextID int
}

type fakeIssue struct {
	issue    tracker.Issue
	stateID  string
	comments []tracker.Comment
}

// FakeTrackerCall records a single method invocation on the FakeTracker.
type FakeTrackerCall struct {
	Method string
	Args   []string
}

// NewFakeTracker creates a new FakeTracker with the given workflow states.
func NewFakeTracker(states []tracker.WorkflowState) *FakeTracker {
	return &FakeTracker{
		issues: make(map[string]*fakeIssue),
		states: states,
	}
}

// AddIssue stores an issue in the fake tracker.
func (f *FakeTracker) AddIssue(issue tracker.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[issue.ID] = &fakeIssue{issue: issue}
}

// Calls returns all recorded method calls.
func (f *FakeTracker) Calls() []FakeTrackerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeTrackerCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallsFor returns recorded calls filtered by method name.
func (f *FakeTracker) CallsFor(method string) []FakeTrackerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FakeTrackerCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// IssueStateID returns the current tracker state ID for the given issue.
func (f *FakeTracker) IssueStateID(issueID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fi, ok := f.issues[issueID]; ok {
		return fi.stateID
	}
	return ""
}

// IssueComments returns all stored comments for the given issue.
func (f *FakeTracker) IssueComments(issueID string) []tracker.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, ok := f.issues[issueID]
	if !ok {
		return nil
	}
	out := make([]tracker.Comment, len(fi.comments))
	copy(out, fi.comments)
	return out
}

// InjectHumanComment adds a comment with IsSelf=false, simulating human feedback.
func (f *FakeTracker) InjectHumanComment(issueID, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, ok := f.issues[issueID]
	if !ok {
		return
	}
	f.nextID++
	fi.comments = append(fi.comments, tracker.Comment{
		ID:        fmt.Sprintf("human-%d", f.nextID),
		Body:      body,
		UserName:  "human-reviewer",
		IsSelf:    false,
		CreatedAt: time.Now(),
	})
}

func (f *FakeTracker) record(method string, args ...string) {
	f.calls = append(f.calls, FakeTrackerCall{Method: method, Args: args})
}

// --- IssueTracker interface implementation ---

func (f *FakeTracker) FetchIssue(_ context.Context, identifier string) (*tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FetchIssue", identifier)
	for _, fi := range f.issues {
		if fi.issue.Identifier == identifier {
			issue := fi.issue // copy
			return &issue, nil
		}
	}
	return nil, fmt.Errorf("issue %q not found", identifier)
}

func (f *FakeTracker) ListIssues(_ context.Context, _ tracker.IssueFilter) ([]*tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ListIssues")
	var out []*tracker.Issue
	for _, fi := range f.issues {
		issue := fi.issue // copy
		out = append(out, &issue)
	}
	return out, nil
}

func (f *FakeTracker) FetchComments(_ context.Context, issueID string, since time.Time) ([]tracker.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FetchComments", issueID, since.Format(time.RFC3339Nano))
	fi, ok := f.issues[issueID]
	if !ok {
		return nil, nil
	}
	var out []tracker.Comment
	for _, c := range fi.comments {
		if !c.CreatedAt.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *FakeTracker) FetchWorkflowStates(_ context.Context, teamID string) ([]tracker.WorkflowState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("FetchWorkflowStates", teamID)
	out := make([]tracker.WorkflowState, len(f.states))
	copy(out, f.states)
	return out, nil
}

func (f *FakeTracker) PostComment(_ context.Context, issueID string, body string) (tracker.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("PostComment", issueID, body)
	fi, ok := f.issues[issueID]
	if !ok {
		return tracker.Comment{}, fmt.Errorf("issue %q not found", issueID)
	}
	f.nextID++
	comment := tracker.Comment{
		ID:        fmt.Sprintf("comment-%d", f.nextID),
		Body:      body,
		UserName:  "jiradozer-bot",
		IsSelf:    true,
		CreatedAt: time.Now(),
	}
	fi.comments = append(fi.comments, comment)
	return comment, nil
}

func (f *FakeTracker) UpdateIssueState(_ context.Context, issueID string, stateID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("UpdateIssueState", issueID, stateID)
	fi, ok := f.issues[issueID]
	if !ok {
		return fmt.Errorf("issue %q not found", issueID)
	}
	fi.stateID = stateID
	return nil
}
