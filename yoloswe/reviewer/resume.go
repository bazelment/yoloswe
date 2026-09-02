package reviewer

import "strings"

// isResumeUnavailableMessage reports whether a backend error means "the
// session you asked me to resume isn't there" — as opposed to a real failure.
// Callers use it to degrade a stale --resume-session-id into a fresh session
// tagged resume_status=fallback instead of losing the whole review round.
//
// The vocabulary is deliberately cross-backend: cursor/codex speak of
// sessions, threads, and chats; the Claude CLI reports an unknown --resume id
// as "No conversation found with session ID …". agy does not use this helper
// — see agyBackend's doc comment for why it has no resume-not-found signal.
func isResumeUnavailableMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "session not found") ||
		strings.Contains(msg, "thread not found") ||
		strings.Contains(msg, "chat not found") ||
		strings.Contains(msg, "conversation not found") ||
		strings.Contains(msg, "no conversation found") ||
		strings.Contains(msg, "session expired") ||
		strings.Contains(msg, "thread expired") ||
		strings.Contains(msg, "chat expired") ||
		strings.Contains(msg, "conversation expired")
}

func reviewErrorResult(resumeStatus ResumeStatus, err error) (*ReviewResult, error) {
	return &ReviewResult{
		Success:      false,
		ErrorMessage: err.Error(),
		ResumeStatus: resumeStatus,
	}, err
}

// reviewPartialResult is reviewErrorResult that keeps whatever the reviewer
// streamed before failing. A run killed mid-stream (idle timeout) may already
// have emitted a complete, schema-valid review body; BuildEnvelope turns that
// into status="partial" so the findings reach the caller instead of being
// discarded with the error. bridged may be nil — failures before the stream
// opened have nothing to preserve and degrade to reviewErrorResult.
func reviewPartialResult(resumeStatus ResumeStatus, bridged *bridgeResult, err error) (*ReviewResult, error) {
	if bridged == nil || bridged.responseText == "" {
		return reviewErrorResult(resumeStatus, err)
	}
	return &ReviewResult{
		Success:      false,
		ResponseText: bridged.responseText,
		// Carry the elapsed time the bridge measured. Setting it on the
		// bridgeResult and dropping it here left the envelope reporting 0ms
		// anyway — the fix has to reach the layer the metric is read from.
		// Token counts stay zero: they live on the TurnComplete event that by
		// definition never arrived, and codex's TokenUsageEvent is outside the
		// agentstream subset, so plumbing them is a cross-backend interface
		// change rather than a carry-through.
		DurationMs:   bridged.durationMs,
		ErrorMessage: err.Error(),
		ResumeStatus: resumeStatus,
	}, err
}

func resumeStatusAfterSessionReady(status ResumeStatus, requestedID, actualID string) ResumeStatus {
	if requestedID == "" {
		return status
	}
	if actualID == requestedID {
		return ResumeStatusOK
	}
	return ResumeStatusFallback
}
