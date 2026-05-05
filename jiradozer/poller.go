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
	FeedbackApproveAll
	FeedbackRedo
	FeedbackComment // general feedback to incorporate
)

var approveAllActionAliases = []string{"approve all", "approve_all", "yolo"}

const approveAllActionDisplay = "`approve all`, `approve_all`, or `yolo`"

// FeedbackResult is the parsed result of a human comment.
type FeedbackResult struct {
	Message string
	Comment tracker.Comment
	Action  FeedbackAction
}

// PollForFeedback polls the tracker for new human comments on the issue.
// It blocks until a comment not in excludeIDs is found, then parses the action.
// excludeIDs contains IDs of comments posted by the bot (e.g., step result and
// waiting comments) so they are skipped even when the bot and human share the
// same API key (where IsSelf would be true for both).
func PollForFeedback(ctx context.Context, t tracker.IssueTracker, issueID string, since time.Time, interval time.Duration, logger *slog.Logger, excludeIDs []string) (*FeedbackResult, error) {
	exclude := make(map[string]bool, len(excludeIDs))
	for _, id := range excludeIDs {
		exclude[id] = true
	}

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

		latest := latestFeedbackComment(comments, exclude)
		if latest != nil {
			action := ParseCommentAction(latest.Body)
			return &FeedbackResult{
				Action:  action,
				Message: latest.Body,
				Comment: *latest,
			}, nil
		}
	}
}

func latestFeedbackComment(comments []tracker.Comment, exclude map[string]bool) *tracker.Comment {
	for i := len(comments) - 1; i >= 0; i-- {
		if !exclude[comments[i].ID] {
			return &comments[i]
		}
	}
	return nil
}

// ParseCommentAction determines the feedback action from a comment body.
// Only the first line is checked for action keywords, so "approve\n\nsome notes"
// is correctly recognized as an approval. Trailing punctuation (., !, ?) is
// stripped before matching, so "lgtm!", "approved.", "ship it!" all work.
func ParseCommentAction(body string) FeedbackAction {
	firstLine := strings.TrimSpace(body)
	if idx := strings.IndexAny(firstLine, "\r\n"); idx >= 0 {
		firstLine = strings.TrimSpace(firstLine[:idx])
	}
	lower := strings.ToLower(firstLine)
	// Strip trailing punctuation for keyword matching.
	normalized := strings.TrimRight(lower, ".!?")
	switch {
	case isApproveAllAction(normalized):
		return FeedbackApproveAll
	case normalized == "approve" || normalized == "lgtm" || normalized == "ship it" || normalized == "approved":
		return FeedbackApprove
	case strings.HasPrefix(lower, "redo") || strings.HasPrefix(lower, "retry"):
		return FeedbackRedo
	default:
		return FeedbackComment
	}
}

func isApproveAllAction(normalized string) bool {
	for _, alias := range approveAllActionAliases {
		if normalized == alias {
			return true
		}
	}
	return false
}

// PostWaitingComment posts a standardized "waiting for review" comment and
// returns the created comment with its server-assigned timestamp.
func PostWaitingComment(ctx context.Context, t tracker.IssueTracker, issueID string, step WorkflowStep) (tracker.Comment, error) {
	body := fmt.Sprintf("**%s** — Waiting for review.\n\nReply with:\n- `approve` to proceed to the next step\n- %s to approve this and all remaining review gates in the current run\n- `redo` to re-run this step\n- Any other comment to provide feedback for revision", step, approveAllActionDisplay)
	return t.PostComment(ctx, issueID, body)
}
