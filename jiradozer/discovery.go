package jiradozer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// Discovery polls the issue tracker for new issues matching a filter
// and emits them on a channel for the orchestrator to pick up.
//
//nolint:govet // fieldalignment: keep related discovery state grouped.
type Discovery struct {
	tracker  tracker.IssueTracker
	logger   *slog.Logger
	seen     map[string]bool
	filter   tracker.IssueFilter
	mu       sync.Mutex
	interval time.Duration
	reload   chan struct{}
}

// NewDiscovery creates a new issue discovery poller.
func NewDiscovery(t tracker.IssueTracker, filter tracker.IssueFilter, interval time.Duration, logger *slog.Logger) *Discovery {
	return &Discovery{
		tracker:  t,
		filter:   filter,
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
	d.filter = filter
	d.mu.Unlock()
	d.notifyReload()
}

// UpdateInterval replaces the polling interval and wakes the poll loop so the
// new interval takes effect immediately.
func (d *Discovery) UpdateInterval(interval time.Duration) {
	d.mu.Lock()
	d.interval = interval
	d.mu.Unlock()
	d.notifyReload()
}

// MarkSeen marks an issue ID as already seen, preventing it from being
// emitted on future polls. Useful for pre-seeding with in-progress issues.
// Must be called before Run.
func (d *Discovery) MarkSeen(issueID string) {
	d.mu.Lock()
	d.seen[issueID] = true
	d.mu.Unlock()
}

// ClearSeen removes an issue ID from the seen set so it will be
// re-emitted on the next poll. Use this when Start fails transiently.
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
	var newIssues []*tracker.Issue
	for _, issue := range issues {
		if d.seen[issue.ID] {
			continue
		}
		d.seen[issue.ID] = true
		newIssues = append(newIssues, issue)
	}
	d.mu.Unlock()

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
	if filter.Filters != nil {
		cp.Filters = make(map[string]string, len(filter.Filters))
		for k, v := range filter.Filters {
			cp.Filters[k] = v
		}
	}
	return cp
}
