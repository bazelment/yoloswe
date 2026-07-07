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
func (m *mockDiscoveryTracker) RemoveLabel(_ context.Context, _ string, _ string) error {
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

func TestDiscovery_UpdateFilterAppliesToFuturePolls(t *testing.T) {
	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{{}, {}},
	}
	d := NewDiscovery(mt, tracker.IssueFilter{Filters: map[string]string{tracker.FilterTeam: "ENG"}}, time.Hour, testLogger(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := d.Run(ctx)
	defer func() {
		cancel()
		for range ch {
		}
	}()

	require.Eventually(t, func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		return len(mt.callArgs) >= 1
	}, time.Second, 10*time.Millisecond)

	d.UpdateFilter(tracker.IssueFilter{Filters: map[string]string{tracker.FilterTeam: "INF"}})

	require.Eventually(t, func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		return len(mt.callArgs) >= 2 && mt.callArgs[len(mt.callArgs)-1].Filters[tracker.FilterTeam] == "INF"
	}, time.Second, 10*time.Millisecond)
}

func TestDiscovery_UpdateAppliesFilterAndInterval(t *testing.T) {
	mt := &mockDiscoveryTracker{
		results: [][]*tracker.Issue{{}, {}, {}},
	}
	d := NewDiscovery(mt, tracker.IssueFilter{Filters: map[string]string{tracker.FilterTeam: "ENG"}}, time.Hour, testLogger(t))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := d.Run(ctx)
	defer func() {
		cancel()
		for range ch {
		}
	}()

	require.Eventually(t, func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		return len(mt.callArgs) >= 1
	}, time.Second, 10*time.Millisecond)

	d.Update(tracker.IssueFilter{Filters: map[string]string{tracker.FilterTeam: "INF"}}, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		return len(mt.callArgs) >= 2 && mt.callArgs[len(mt.callArgs)-1].Filters[tracker.FilterTeam] == "INF"
	}, time.Second, 10*time.Millisecond)
}

// collectPoll runs a single poll() against a channel drained concurrently and
// returns the identifiers emitted during that poll. It drives poll() directly
// (rather than through Run's timer) so the active set can be mutated
// deterministically between polls.
func collectPoll(t *testing.T, d *Discovery) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan *tracker.Issue)
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.poll(ctx, ch)
		close(ch)
	}()
	for issue := range ch {
		got = append(got, issue.Identifier)
	}
	<-done
	return got
}

// TestDiscovery_ReconcilesAgainstActiveSet exercises the INF-1808 lifecycle: an
// issue is discovered once, suppressed while actively worked, and re-emitted
// once it returns to the filter state and is no longer active.
func TestDiscovery_ReconcilesAgainstActiveSet(t *testing.T) {
	t.Parallel()
	issue := &tracker.Issue{ID: "a", Identifier: "INF-1808", Title: "Reservation lifecycle"}

	// The filter always returns the issue (it's back in Gate Approved after the
	// runtime failure and re-queue).
	mt := &mockDiscoveryTracker{results: [][]*tracker.Issue{
		{issue}, {issue}, {issue}, {issue}, {issue},
	}}

	var active map[string]bool
	var released []string // drained (and cleared) on each poll
	d := NewDiscovery(mt, tracker.IssueFilter{}, time.Hour, testLogger(t))
	d.SetReconcileProviders(
		func() map[string]bool { return active },
		func() []string { r := released; released = nil; return r },
	)

	// Poll 1: not active → emitted once.
	require.Equal(t, []string{"INF-1808"}, collectPoll(t, d))

	// Poll 2: now active (claimed and running) → suppressed.
	active = map[string]bool{"a": true}
	require.Empty(t, collectPoll(t, d))

	// Poll 3: still active → still suppressed (no double-claim).
	require.Empty(t, collectPoll(t, d))

	// Runtime-failed and re-queued: orchestrator drops it from active AND
	// releases it for re-pickup (the claim+fail may have happened between polls).
	// Poll 4: released is drained → re-emitted once.
	active = nil
	released = []string{"a"}
	require.Equal(t, []string{"INF-1808"}, collectPoll(t, d))

	// Poll 5: still returned, still not active, released already drained →
	// suppressed. The re-admission must fire exactly once, not every poll.
	require.Empty(t, collectPoll(t, d))
}

// TestDiscovery_ClaimWindowNeverDoubleEmits verifies that an issue held in the
// active set across consecutive polls (e.g. a placeholder during the claim
// window before the In Progress transition lands) is never emitted twice.
func TestDiscovery_ClaimWindowNeverDoubleEmits(t *testing.T) {
	t.Parallel()
	issue := &tracker.Issue{ID: "a", Identifier: "ENG-1", Title: "Claiming"}
	mt := &mockDiscoveryTracker{results: [][]*tracker.Issue{
		{issue}, {issue}, {issue},
	}}

	// Active from the very first poll (placeholder inserted before this poll saw
	// the issue) and stays active — filter still returns it because the state
	// transition is deferred.
	d := NewDiscovery(mt, tracker.IssueFilter{}, time.Hour, testLogger(t))
	d.SetReconcileProviders(
		func() map[string]bool { return map[string]bool{"a": true} },
		func() []string { return nil },
	)

	require.Empty(t, collectPoll(t, d))
	require.Empty(t, collectPoll(t, d))
	require.Empty(t, collectPoll(t, d))
}

// TestDiscovery_NilActiveProvider verifies that with no provider wired the
// active set is treated as empty: every filter issue emits exactly once and
// stays suppressed while the filter keeps returning it.
func TestDiscovery_NilActiveProvider(t *testing.T) {
	t.Parallel()
	issue := &tracker.Issue{ID: "a", Identifier: "ENG-1", Title: "Solo"}
	mt := &mockDiscoveryTracker{results: [][]*tracker.Issue{
		{issue}, {issue},
	}}
	d := NewDiscovery(mt, tracker.IssueFilter{}, time.Hour, testLogger(t))
	// No SetReconcileProviders call — activeIDs is nil.

	require.Equal(t, []string{"ENG-1"}, collectPoll(t, d))
	require.Empty(t, collectPoll(t, d)) // still returned, still seen → no re-emit
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
