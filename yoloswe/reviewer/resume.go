package reviewer

import "strings"

func isResumeUnavailableMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "session not found") ||
		strings.Contains(msg, "thread not found") ||
		strings.Contains(msg, "chat not found") ||
		strings.Contains(msg, "session expired") ||
		strings.Contains(msg, "thread expired") ||
		strings.Contains(msg, "chat expired")
}

func reviewErrorResult(resumeStatus ResumeStatus, err error) (*ReviewResult, error) {
	return &ReviewResult{
		Success:      false,
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
