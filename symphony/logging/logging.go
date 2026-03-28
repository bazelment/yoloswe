package logging

import (
	"log/slog"
	"os"
)

// WithIssue returns a logger with issue_id and issue_identifier attributes.
func WithIssue(logger *slog.Logger, issueID, identifier string) *slog.Logger {
	return logger.With("issue_id", issueID, "issue_identifier", identifier)
}

// WithSession returns a logger with a session_id attribute.
func WithSession(logger *slog.Logger, sessionID string) *slog.Logger {
	return logger.With("session_id", sessionID)
}

// NewLogger creates a structured text logger writing to stderr.
func NewLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}
