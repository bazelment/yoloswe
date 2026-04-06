package jiradozer

import (
	"context"
	"log/slog"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// Discovery polls the issue tracker for new issues matching a filter
// and emits them on a channel for the orchestrator to pick up.
type Discovery struct {
	tracker  tracker.IssueTracker
	logger   *slog.Logger
	seen     map[string]bool
	filter   tracker.IssueFilter
	interval time.Duration
}

// NewDiscovery creates a new issue discovery poller.
func NewDiscovery(t tracker.IssueTracker, filter tracker.IssueFilter, interval time.Duration, logger *slog.Logger) *Discovery {
	return &Discovery{
		tracker:  t,
		filter:   filter,
		interval: interval,
		seen:     make(map[string]bool),
		logger:   logger,
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
		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.poll(ctx, ch)
			}
		}
	}()
	return ch
}

// MarkSeen marks an issue ID as already seen, preventing it from being
// emitted on future polls. Useful for pre-seeding with in-progress issues.
func (d *Discovery) MarkSeen(issueID string) {
	d.seen[issueID] = true
}

// ClearSeen removes an issue ID from the seen set so it will be
// re-emitted on the next poll. Use this when Start fails transiently.
func (d *Discovery) ClearSeen(issueID string) {
	delete(d.seen, issueID)
}

func (d *Discovery) poll(ctx context.Context, ch chan<- *tracker.Issue) {
	issues, err := d.tracker.ListIssues(ctx, d.filter)
	if err != nil {
		d.logger.Warn("discovery poll failed", "error", err)
		return
	}
	for _, issue := range issues {
		if d.seen[issue.ID] {
			continue
		}
		d.seen[issue.ID] = true
		d.logger.Info("discovered new issue", "identifier", issue.Identifier, "title", issue.Title)
		select {
		case ch <- issue:
		case <-ctx.Done():
			return
		}
	}
}
