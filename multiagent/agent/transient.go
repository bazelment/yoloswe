package agent

import (
	"errors"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	transientmeta "github.com/bazelment/yoloswe/agent-cli-wrapper/transient"
)

// IsOutOfCredits reports whether err is a hard usage-exhaustion failure that a
// same-model retry cannot clear — either a codex/cursor workspace running out
// of credits or a Claude.ai plan hitting one of its limit windows. It is
// distinct from a transient error: refilling the workspace or waiting for the
// window to reset is not something a retry can do, so callers use it to trigger
// a fallback to a different provider's model rather than to retry. It is
// text-based and provider-agnostic — both claude.TurnError and codex.TurnError
// render the upstream message into Error(), so matching the rendered string
// works across providers.
//
// The Claude.ai plan limit surfaces across several concurrent windows (the
// 5-hour session window, the weekly all-models window, and per-model weekly
// scoped windows) and the wording varies by which window tripped and by CLI
// version — "You've hit your session limit · resets …", "You've hit your limit
// · resets …", "You've hit your usage limit · resets …", and the phrasing-
// inverted "Session limit reached · resets …" (claude-code#8926). The invariant
// across every variant is a "limit" clause co-occurring with a "· resets …
// (UTC)" reset clause, so we match that shape rather than enumerating each
// window/phrasing — a new window kind or reworded message is covered without a
// code change. The reset time is deliberately not parsed: per product decision
// we fall back to another model immediately rather than waiting for the window
// to reset. Requiring the "resets" clause excludes unrelated text such as
// "reached your limit of 5 organization memberships", which has no reset clause
// and is not a usage-exhaustion failure.
func IsOutOfCredits(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "out of credits") ||
		strings.Contains(s, "workspace owner to refill") {
		return true
	}
	if strings.Contains(s, "limit") && strings.Contains(s, "resets") {
		return true
	}
	return false
}

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
