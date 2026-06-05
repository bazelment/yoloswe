package jiradozer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestFailingStepFromError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"run-step plan", errors.New("run-step plan: agent execution: API Error: socket closed"), "plan"},
		{"run-step multi-round", errors.New("run-step validate round 2/3: agent execution: stream idle"), "validate"},
		{"plan step", errors.New("plan step: agent execution: Internal server error"), "plan"},
		{"validate round", errors.New("validate round 2/3: agent execution: stream idle"), "validate"},
		{"build step", errors.New("build step: agent execution: turn error"), "build"},
		{"create_pr step", errors.New("create_pr step: agent execution: 403"), "create_pr"},
		{"unrelated prefix", errors.New("load config: parse config: yaml: line 45"), ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FailingStepFromError(tc.err); got != tc.want {
				t.Errorf("FailingStepFromError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestRenderFailureText(t *testing.T) {
	t.Parallel()
	r := FailureReport{
		Tool:          "jiradozer",
		Target:        "INF-703",
		Step:          "plan",
		Err:           errors.New("socket connection was closed unexpectedly"),
		BuildRevision: "abc123def456",
		LogPath:       "/home/ubuntu/.jiradozer/logs/run.log",
	}
	got := r.renderFailureText()
	for _, want := range []string{"INF-703", "`plan`", "socket connection was closed", "abc123def456", "/home/ubuntu/.jiradozer/logs/run.log"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered text missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestSlackWebhookNotifier(t *testing.T) {
	t.Parallel()

	t.Run("empty URL is no-op", func(t *testing.T) {
		t.Parallel()
		n := SlackWebhookNotifier{}
		if err := n.Notify(context.Background(), FailureReport{Tool: "jiradozer"}); err != nil {
			t.Fatalf("expected nil error for empty URL, got %v", err)
		}
	})

	t.Run("posts text payload", func(t *testing.T) {
		t.Parallel()
		var gotBody map[string]string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(body, &gotBody)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		n := SlackWebhookNotifier{WebhookURL: srv.URL, Client: srv.Client()}
		report := FailureReport{Tool: "jiradozer", Target: "INF-703", Step: "validate", Err: errors.New("boom")}
		if err := n.Notify(context.Background(), report); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		if !strings.Contains(gotBody["text"], "INF-703") || !strings.Contains(gotBody["text"], "boom") {
			t.Errorf("payload text = %q", gotBody["text"])
		}
	})

	t.Run("non-2xx is an error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		n := SlackWebhookNotifier{WebhookURL: srv.URL, Client: srv.Client()}
		if err := n.Notify(context.Background(), FailureReport{Tool: "jiradozer", Err: errors.New("x")}); err == nil {
			t.Fatal("expected error on 500, got nil")
		}
	})
}

// fakePoster records comments posted to it and can be made to fail.
type fakePoster struct { //nolint:govet // fieldalignment: test fixture readability
	mu       sync.Mutex
	comments []string
	failWith error
	// blockOnCtx, when true, makes PostComment hang until its context expires
	// (simulating a slow/wedged tracker) and return the ctx error.
	blockOnCtx bool
}

func (f *fakePoster) PostComment(ctx context.Context, issueID, body string) (tracker.Comment, error) {
	if f.blockOnCtx {
		<-ctx.Done()
		return tracker.Comment{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return tracker.Comment{}, f.failWith
	}
	f.comments = append(f.comments, body)
	return tracker.Comment{ID: fmt.Sprintf("c%d", len(f.comments))}, nil
}

// captureNotifier records the reports it receives.
type captureNotifier struct { //nolint:govet // fieldalignment: test fixture readability
	mu       sync.Mutex
	reports  []FailureReport
	failWith error
}

func (c *captureNotifier) Notify(_ context.Context, report FailureReport) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failWith != nil {
		return c.failWith
	}
	c.reports = append(c.reports, report)
	return nil
}

func TestReportFailure(t *testing.T) {
	t.Parallel()
	report := FailureReport{Tool: "jiradozer", Target: "INF-703", Step: "plan", Err: errors.New("boom")}

	t.Run("posts comment and notifies", func(t *testing.T) {
		t.Parallel()
		poster := &fakePoster{}
		notifier := &captureNotifier{}
		ReportFailure(context.Background(), discardLogger(), poster, "issue-id", notifier, report)
		if len(poster.comments) != 1 {
			t.Errorf("got %d comments, want 1", len(poster.comments))
		}
		if len(notifier.reports) != 1 {
			t.Errorf("got %d notifications, want 1", len(notifier.reports))
		}
	})

	t.Run("empty issueID skips comment", func(t *testing.T) {
		t.Parallel()
		poster := &fakePoster{}
		notifier := &captureNotifier{}
		ReportFailure(context.Background(), discardLogger(), poster, "", notifier, report)
		if len(poster.comments) != 0 {
			t.Errorf("expected no comment for empty issueID, got %d", len(poster.comments))
		}
		if len(notifier.reports) != 1 {
			t.Errorf("expected notification to still fire, got %d", len(notifier.reports))
		}
	})

	t.Run("nil sinks are safe", func(t *testing.T) {
		t.Parallel()
		// Must not panic.
		ReportFailure(context.Background(), discardLogger(), nil, "issue-id", nil, report)
	})

	t.Run("sink failures never panic or propagate", func(t *testing.T) {
		t.Parallel()
		poster := &fakePoster{failWith: errors.New("tracker down")}
		notifier := &captureNotifier{failWith: errors.New("slack down")}
		// Both sinks fail; ReportFailure must swallow both.
		ReportFailure(context.Background(), discardLogger(), poster, "issue-id", notifier, report)
	})

	t.Run("fires even when run context is cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		poster := &fakePoster{}
		notifier := &captureNotifier{}
		ReportFailure(ctx, discardLogger(), poster, "issue-id", notifier, report)
		if len(poster.comments) != 1 || len(notifier.reports) != 1 {
			t.Errorf("reporting should detach from cancelled ctx: comments=%d notifications=%d",
				len(poster.comments), len(notifier.reports))
		}
	})

	t.Run("hung tracker does not starve the external alert", func(t *testing.T) {
		// Not parallel: mutates the package-level sinkTimeout.
		prev := sinkTimeout
		sinkTimeout = 50 * time.Millisecond
		t.Cleanup(func() { sinkTimeout = prev })

		poster := &fakePoster{blockOnCtx: true} // hangs until ITS deadline expires
		notifier := &captureNotifier{}
		start := time.Now()
		ReportFailure(context.Background(), discardLogger(), poster, "issue-id", notifier, report)
		elapsed := time.Since(start)

		if len(notifier.reports) != 1 {
			t.Errorf("external alert must fire despite a hung tracker, got %d notifications", len(notifier.reports))
		}
		// The notifier must get its own full budget, not the tracker's leftovers:
		// total time ≈ one sink timeout (tracker hang) + a fast notifier, well
		// under two full budgets.
		if elapsed > 2*sinkTimeout {
			t.Errorf("sinks appear to share a budget: elapsed=%v exceeds 2×sinkTimeout=%v", elapsed, 2*sinkTimeout)
		}
	})
}
