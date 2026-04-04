package jiradozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// FeedbackAction represents the human's intent from a comment.
type FeedbackAction int

const (
	FeedbackApprove FeedbackAction = iota
	FeedbackRedo
	FeedbackComment // general feedback to incorporate
)

// FeedbackResult is the parsed result of a human comment.
type FeedbackResult struct {
	Message string
	Comment tracker.Comment
	Action  FeedbackAction
}

// PollForFeedback polls the tracker for new human comments on the issue.
// It blocks until a non-bot comment is found, then parses the action.
func PollForFeedback(ctx context.Context, t tracker.IssueTracker, issueID string, since time.Time, interval time.Duration, logger *slog.Logger) (*FeedbackResult, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Check immediately on the first iteration, then wait for ticker.
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
		first = false

		comments, err := t.FetchComments(ctx, issueID, since)
		if err != nil {
			logger.Warn("failed to fetch comments, will retry", "error", err)
			continue
		}

		for _, c := range comments {
			if c.IsSelf {
				continue
			}
			action := ParseCommentAction(c.Body)
			return &FeedbackResult{
				Action:  action,
				Message: c.Body,
				Comment: c,
			}, nil
		}
	}
}

// ParseCommentAction determines the feedback action from a comment body.
func ParseCommentAction(body string) FeedbackAction {
	lower := strings.ToLower(strings.TrimSpace(body))
	switch {
	case lower == "approve" || lower == "lgtm" || lower == "ship it" || lower == "approved":
		return FeedbackApprove
	case strings.HasPrefix(lower, "redo") || strings.HasPrefix(lower, "retry"):
		return FeedbackRedo
	default:
		return FeedbackComment
	}
}

// PostWaitingComment posts a standardized "waiting for review" comment.
func PostWaitingComment(ctx context.Context, t tracker.IssueTracker, issueID string, step WorkflowStep) error {
	body := fmt.Sprintf("**%s** — Waiting for review.\n\nReply with:\n- `approve` to proceed to the next step\n- `redo` to re-run this step\n- Any other comment to provide feedback for revision", step)
	return t.PostComment(ctx, issueID, body)
}
