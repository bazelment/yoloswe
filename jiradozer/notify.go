package jiradozer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// FailureReport is the human-facing summary of a run that aborted. It is built
// once, at the single top-level failure point, and fanned out to every
// configured sink (tracker comment, external notifier). The fields are chosen
// so an on-call human can act without opening the log first: which run, which
// step, the error, which build produced it, and where the full log lives.
type FailureReport struct {
	// Tool is the binary name, e.g. "jiradozer".
	Tool string
	// Target identifies the run: an issue identifier ("INF-703") or, for
	// --description runs, a short description of the task.
	Target string
	// Step is the workflow step that failed ("plan", "validate", ...),
	// best-effort parsed from the error. Empty when it can't be determined.
	Step string
	// Err is the error that aborted the run.
	Err error
	// BuildRevision is the VCS commit of the running binary (cliapp.BuildInfo
	// ShortRevision), so a stale-deploy failure is obvious from the alert.
	BuildRevision string
	// LogPath is the absolute path to the run's log file.
	LogPath string
}

// FailingStepFromError extracts the workflow step name from a wrapped run
// error, or "" when no known prefix is present. Recognized steps only, so an
// unrelated "xyz: …" message does not masquerade as a step.
func FailingStepFromError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Try the explicit "<step> step:" / "<step> round N/M:" / "run-step <step>:" shapes.
	for _, step := range StepNames {
		switch {
		case strings.HasPrefix(msg, "run-step "+step+":"),
			strings.HasPrefix(msg, step+" step:"),
			strings.HasPrefix(msg, step+" round "):
			return step
		}
	}
	return ""
}

// renderFailureText builds the one-paragraph message shared by the tracker
// comment and the external notification.
func (r FailureReport) renderFailureText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚨 %s run failed", r.Tool)
	if r.Target != "" {
		fmt.Fprintf(&b, " for %s", r.Target)
	}
	if r.Step != "" {
		fmt.Fprintf(&b, " at step `%s`", r.Step)
	}
	b.WriteString(".\n")
	fmt.Fprintf(&b, "Error: %s\n", r.Err)
	if r.BuildRevision != "" {
		fmt.Fprintf(&b, "Build: %s\n", r.BuildRevision)
	}
	if r.LogPath != "" {
		fmt.Fprintf(&b, "Log: %s\n", r.LogPath)
	}
	return b.String()
}

// Notifier delivers a failure report to an external destination. Implementations
// must be safe to call with a context deadline and must not panic on partial
// configuration.
type Notifier interface {
	Notify(ctx context.Context, report FailureReport) error
}

// SlackWebhookNotifier posts failure reports to a Slack incoming webhook. It is
// the first (and currently only) external sink; the Notifier interface keeps the
// reporting call site provider-agnostic so a different backend can be added
// without touching the workflow.
type SlackWebhookNotifier struct {
	Client     *http.Client
	WebhookURL string
}

// Notify posts the report's text to the Slack webhook as a simple message.
func (n SlackWebhookNotifier) Notify(ctx context.Context, report FailureReport) error {
	if n.WebhookURL == "" {
		return nil
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	payload, err := json.Marshal(map[string]string{"text": report.renderFailureText()})
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post to slack: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// ReportFailure fans a failure report out to all configured sinks. It is
// best-effort and never returns an error: a notification failure is logged but
// must not mask the original run failure or change the exit path. A nil
// notifier or empty issueID simply skips that sink.
//
// poster posts a comment on issueID (skipped when issueID is empty, e.g.
// --description runs); notifier delivers the external alert (skipped when nil).
func ReportFailure(ctx context.Context, logger *slog.Logger, poster CommentPoster, issueID string, notifier Notifier, report FailureReport) {
	// Detach from a possibly-cancelled run context so the alert still sends
	// after a ctx-cancellation, but keep a tight bound.
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	if poster != nil && issueID != "" {
		if _, err := poster.PostComment(notifyCtx, issueID, report.renderFailureText()); err != nil {
			logger.Warn("failed to post failure comment to tracker", "issue", issueID, "error", err)
		} else {
			logger.Info("posted failure comment to tracker", "issue", issueID)
		}
	}

	if notifier != nil {
		if err := notifier.Notify(notifyCtx, report); err != nil {
			logger.Warn("failed to send failure notification", "error", err)
		} else {
			logger.Info("sent failure notification")
		}
	}
}
