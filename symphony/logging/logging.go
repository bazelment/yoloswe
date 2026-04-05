package logging

import (
	"log/slog"
	"os"

	"github.com/bazelment/yoloswe/logging/klogfmt"
)

// WithIssue returns a logger with issue_id and issue_identifier attributes.
func WithIssue(logger *slog.Logger, issueID, identifier string) *slog.Logger {
	return logger.With("issue_id", issueID, "issue_identifier", identifier)
}

// WithSession returns a logger with a session_id attribute.
func WithSession(logger *slog.Logger, sessionID string) *slog.Logger {
	return logger.With("session_id", sessionID)
}

// NewLogger creates a klog-formatted logger writing to stderr.
func NewLogger() *slog.Logger {
	return slog.New(klogfmt.New(os.Stderr))
}
