package agent

import (
	"errors"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	transientmeta "github.com/bazelment/yoloswe/agent-cli-wrapper/transient"
)

// IsTransient reports whether err originates from a known retryable provider
// failure, such as stream-idle, rate limiting, or a temporary network break.
func IsTransient(err error) bool {
	ok, _ := ClassifyTransient(err)
	return ok
}

// TransientReason returns a stable, low-cardinality reason for retry logs.
func TransientReason(err error) string {
	if err == nil {
		return transientmeta.ReasonUnknown
	}
	_, reason := ClassifyTransient(err)
	return reason
}

// ClassifyTransient reports whether err is retryable and returns a stable
// reason for logs without reparsing typed provider errors.
func ClassifyTransient(err error) (bool, string) {
	if err == nil {
		return false, transientmeta.ReasonUnknown
	}

	var claudeTransient *claude.TransientError
	if errors.As(err, &claudeTransient) {
		if reason, ok := transientmeta.ClassifyText(claudeTransient.Message); ok {
			return true, reason
		}
		return true, transientmeta.ReasonUnknown
	}

	var codexTransient *codex.TransientError
	if errors.As(err, &codexTransient) {
		if codexTransient.Reason != "" {
			return true, codexTransient.Reason
		}
		if reason, ok := transientmeta.ClassifyText(codexTransient.Message); ok {
			return true, reason
		}
		if codexTransient.Cause != nil {
			if reason, ok := transientmeta.ClassifyText(codexTransient.Cause.Error()); ok {
				return true, reason
			}
		}
		return true, transientmeta.ReasonUnknown
	}

	if reason, ok := transientmeta.ClassifyText(err.Error()); ok {
		return true, reason
	}
	return false, transientmeta.ReasonUnknown
}
