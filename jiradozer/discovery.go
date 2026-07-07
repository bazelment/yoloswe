package jiradozer

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// Discovery polls the issue tracker for new issues matching a filter
// and emits them on a channel for the orchestrator to pick up.
//
//nolint:govet // fieldalignment: keep related discovery state grouped.
type Discovery struct {
	tracker tracker.IssueTracker
	logger  *slog.Logger
	// seen is the set of issue IDs already emitted and not re-admittable. It is
	// rebuilt each poll from only the currently-returned issues (so it never
	// pins an issue that leaves the filter for good), and it is self-reconciling
	// rather than monotonic: drainReleased clears an entry when the orchestrator
	// releases that issue for re-pickup, so a runtime-failed / re-queued issue
	// (INF-1808) comes back automatically without any exit path having to
	// remember to un-suppress it.
	seen map[string]bool
	// activeIDs reports the orchestrator's active/suppressed set: issues it is
	// actively working (placeholders + running children — they must count as
	// active to protect the claim window) plus issues over the runtime-failure
	// cap. Filter issues in this set are never (re-)emitted. Nil when no
	// orchestrator is wired (single-shot mode, most unit tests) — then the
	// active set is empty and suppression rests entirely on seen.
	activeIDs func() map[string]bool
	// drainReleased returns (and atomically clears) the set of issue IDs the
	// orchestrator has released for re-pickup since the last poll — issues that
	// failed or were cancelled and may be back in the filter state. Each is
	// cleared from seen so it is re-emitted once. This is the re-admission
	// signal that survives a claim+fail happening entirely between two polls
	// (INF-1808): a transition the active set alone would never expose. Nil when
	// no orchestrator is wired.
	drainReleased func() []string
	filter        tracker.IssueFilter
	mu            sync.Mutex
	interval      time.Duration
	reload        chan struct{}
}

// NewDiscovery creates a new issue discovery poller.
func NewDiscovery(t tracker.IssueTracker, filter tracker.IssueFilter, interval time.Duration, logger *slog.Logger) *Discovery {
	return &Discovery{
		tracker:  t,
		filter:   cloneIssueFilter(filter),
		interval: interval,
		seen:     make(map[string]bool),
		logger:   logger,
		reload:   make(chan struct{}, 1),
	}
}

// Run polls for new issues and sends them on the returned channel.
// The channel is closed when ctx is cancelled.
func (d *Discovery) Run(ctx context.Context) <-chan *tracker.Issue {
	ch := make(chan *tracker.Issue)
	go func() {
		defer close(ch)
		// Do an immediate poll, then tick.
		d.poll(ctx, ch)
		ticker := time.NewTicker(d.currentInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.poll(ctx, ch)
			case <-d.reload:
				ticker.Reset(d.currentInterval())
				d.poll(ctx, ch)
			}
		}
	}()
	return ch
}

// UpdateFilter replaces the issue filter used on future polls.
func (d *Discovery) UpdateFilter(filter tracker.IssueFilter) {
	d.mu.Lock()
	changed := !reflect.DeepEqual(filter, d.filter)
	d.filter = cloneIssueFilter(filter)
	d.mu.Unlock()
	if changed {
		d.notifyReload()
	}
}

// UpdateInterval replaces the polling interval and wakes the poll loop so the
// new interval takes effect immediately.
func (d *Discovery) UpdateInterval(interval time.Duration) {
	d.mu.Lock()
	changed := interval != d.interval
	d.interval = interval
	d.mu.Unlock()
	if changed {
		d.notifyReload()
	}
}

// Update replaces discovery settings and wakes the poll loop once when they change.
func (d *Discovery) Update(filter tracker.IssueFilter, interval time.Duration) {
	d.mu.Lock()
	changed := interval != d.interval || !reflect.DeepEqual(filter, d.filter)
	d.filter = cloneIssueFilter(filter)
	d.interval = interval
	d.mu.Unlock()
	if changed {
		d.notifyReload()
	}
}

// SetReconcileProviders wires discovery to the orchestrator's runtime state so
// its seen set self-reconciles instead of growing monotonically:
//
//   - activeIDs: issues the orchestrator is working (never re-emitted — this
//     protects the claim window between slot reservation and the In Progress
//     transition) plus issues over the runtime-failure cap.
//   - drainReleased: issues the orchestrator released for re-pickup since the
//     last poll (failed/cancelled). Each is cleared from seen so it re-emits
//     once — this is what re-admits a runtime-failed, re-queued issue even when
//     the whole claim+fail happened between two polls (INF-1808).
//
// Either may be nil (tolerated by poll()). Set before Run to take effect on the
// first poll.
func (d *Discovery) SetReconcileProviders(activeIDs func() map[string]bool, drainReleased func() []string) {
	d.mu.Lock()
	d.activeIDs = activeIDs
	d.drainReleased = drainReleased
	d.mu.Unlock()
}

// MarkSeen marks an issue ID as already seen, preventing it from being
// emitted on the next poll. No longer load-bearing for crash/restart recovery
// (poll() self-reconciles seen against the active set), but retained for
// callers that want to pre-seed before the first poll.
func (d *Discovery) MarkSeen(issueID string) {
	d.mu.Lock()
	d.seen[issueID] = true
	d.mu.Unlock()
}

// ClearSeen removes an issue ID from the seen set so it will be
// re-emitted on the next poll. Retained for the Start-failure retry path in
// RunWithDiscovery; runtime re-admission is handled by poll()'s reconciliation.
func (d *Discovery) ClearSeen(issueID string) {
	d.mu.Lock()
	delete(d.seen, issueID)
	d.mu.Unlock()
}

func (d *Discovery) poll(ctx context.Context, ch chan<- *tracker.Issue) {
	filter := d.currentFilter()
	issues, err := d.tracker.ListIssues(ctx, filter)
	if err != nil {
		d.logger.Warn("discovery poll failed", "error", err)
		return
	}

	d.mu.Lock()
	var active map[string]bool
	if d.activeIDs != nil {
		active = d.activeIDs()
	}
	// Re-admission: clear from seen every issue the orchestrator released for
	// re-pickup since the last poll. This un-suppresses a failed/cancelled issue
	// even when its entire claim+fail happened between two polls — the case the
	// active set alone cannot expose (INF-1808).
	if d.drainReleased != nil {
		for _, id := range d.drainReleased() {
			delete(d.seen, id)
		}
	}
	// seen tracks "already emitted, not re-admittable". Suppress an issue when it
	// is active (in-flight or over the runtime-failure cap) or already seen;
	// otherwise emit it once and mark it seen. seen is rebuilt each poll from
	// only the currently-returned issues so an issue that leaves the filter for
	// good is not pinned in memory forever — and if it later returns it is
	// treated as new. A never-claimed issue that stays in the filter carries its
	// seen bit forward here, so it emits exactly once, not every poll.
	next := make(map[string]bool, len(issues))
	var newIssues []*tracker.Issue
	suppressed := 0
	for _, issue := range issues {
		id := issue.ID
		switch {
		case active[id]:
			// In-flight (placeholder, running, or over the runtime-failure cap):
			// never re-emitted — protects the claim window.
			next[id] = true
			suppressed++
		case d.seen[id]:
			next[id] = true
			suppressed++
		default:
			next[id] = true
			newIssues = append(newIssues, issue)
		}
	}
	d.seen = next
	d.mu.Unlock()

	// Operability heartbeat: when the filter is non-empty but nothing is newly
	// discovered, emit a debug line so a long steady-state (e.g. every issue is
	// active or suppressed) is not indistinguishable from a dead poller. Debug
	// level keeps it out of normal logs; enable debug to diagnose silence.
	if len(issues) > 0 && len(newIssues) == 0 {
		d.logger.Debug("poll: no new issues",
			"in_filter", len(issues),
			"suppressed", suppressed,
		)
	}

	for _, issue := range newIssues {
		d.logger.Info("discovered new issue", "identifier", issue.Identifier, "title", issue.Title)
		select {
		case ch <- issue:
		case <-ctx.Done():
			return
		}
	}
}

func (d *Discovery) currentFilter() tracker.IssueFilter {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneIssueFilter(d.filter)
}

func (d *Discovery) currentInterval() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.interval
}

func (d *Discovery) notifyReload() {
	select {
	case d.reload <- struct{}{}:
	default:
	}
}

func cloneIssueFilter(filter tracker.IssueFilter) tracker.IssueFilter {
	cp := filter
	cp.Filters = cloneStringMap(filter.Filters)
	return cp
}
