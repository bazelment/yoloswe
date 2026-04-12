package jiradozer

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func testLogger(_ testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type mockDiscoveryTracker struct {
	results  [][]*tracker.Issue // sequence of results for each ListIssues call
	callArgs []tracker.IssueFilter
	mu       sync.Mutex
	callIdx  int
}

func (m *mockDiscoveryTracker) ListIssues(_ context.Context, filter tracker.IssueFilter) ([]*tracker.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callArgs = append(m.callArgs, filter)
	if m.callIdx < len(m.results) {
		issues := m.results[m.callIdx]
		m.callIdx++
		return issues, nil
	}
	return nil, nil
}

func (m *mockDiscoveryTracker) FetchIssue(_ context.Context, _ string) (*tracker.Issue, error) {
	return nil, nil
}
func (m *mockDiscoveryTracker) FetchComments(_ context.Context, _ string, _ time.Time) ([]tracker.Comment, error) {
	return nil, nil
}
func (m *mockDiscoveryTracker) FetchWorkflowStates(_ context.Context, _ string) ([]tracker.WorkflowState, error) {
	return nil, nil
}
func (m *mockDiscoveryTracker) PostComment(_ context.Context, _ string, _ string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}
func (m *mockDiscoveryTracker) UpdateIssueState(_ context.Context, _ string, _ string) error {
	return nil
}
func (m *mockDiscoveryTracker) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}

func TestDiscovery_DeduplicatesIssues(t *testing.T) {
	issueA := &tracker.Issue{ID: "a", Identifier: "ENG-1", Title: "Issue A"}
	issueB := &tracker.Issue{ID: "b", Identifier: "ENG-2", Title: "Issue B"}

	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{
			{issueA},         // poll 1: A
			{issueA, issueB}, // poll 2: A+B (A already seen)
			{issueA, issueB}, // poll 3: both seen
		},
	}

	filter := tracker.IssueFilter{Filters: map[string]string{tracker.FilterTeam: "ENG", tracker.FilterState: "Todo"}}
	d := NewDiscovery(mt, filter, 10*time.Millisecond, testLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	ch := d.Run(ctx)

	var received []*tracker.Issue
	for issue := range ch {
		received = append(received, issue)
	}

	require.Len(t, received, 2)
	require.Equal(t, "ENG-1", received[0].Identifier)
	require.Equal(t, "ENG-2", received[1].Identifier)
}

func TestDiscovery_MarkSeen(t *testing.T) {
	issueA := &tracker.Issue{ID: "a", Identifier: "ENG-1", Title: "Issue A"}
	issueB := &tracker.Issue{ID: "b", Identifier: "ENG-2", Title: "Issue B"}

	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{
			{issueA, issueB},
		},
	}

	d := NewDiscovery(mt, tracker.IssueFilter{}, 10*time.Millisecond, testLogger(t))
	d.MarkSeen("a") // Pre-seed: A already known

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	ch := d.Run(ctx)

	var received []*tracker.Issue
	for issue := range ch {
		received = append(received, issue)
	}

	require.Len(t, received, 1)
	require.Equal(t, "ENG-2", received[0].Identifier)
}

func TestDiscovery_PassesFilter(t *testing.T) {
	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{{}},
	}

	filter := tracker.IssueFilter{
		Filters: map[string]string{
			tracker.FilterTeam:  "INF",
			tracker.FilterState: "Todo,Backlog",
			tracker.FilterLabel: "automated",
		},
	}
	d := NewDiscovery(mt, filter, 50*time.Millisecond, testLogger(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	ch := d.Run(ctx)
	for range ch {
	}

	mt.mu.Lock()
	defer mt.mu.Unlock()
	require.NotEmpty(t, mt.callArgs)
	require.Equal(t, "INF", mt.callArgs[0].Filters[tracker.FilterTeam])
	require.Equal(t, "Todo,Backlog", mt.callArgs[0].Filters[tracker.FilterState])
	require.Equal(t, "automated", mt.callArgs[0].Filters[tracker.FilterLabel])
}

func TestDiscovery_ContextCancellation(t *testing.T) {
	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{{}},
	}

	d := NewDiscovery(mt, tracker.IssueFilter{}, time.Hour, testLogger(t))

	ctx, cancel := context.WithCancel(context.Background())
	ch := d.Run(ctx)
	cancel()

	// Channel should close promptly after cancellation.
	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel should be closed")
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancellation")
	}
}
