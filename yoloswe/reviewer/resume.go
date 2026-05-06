package reviewer

import "strings"

func isResumeUnavailableMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "missing") ||
		strings.Contains(msg, "expired")
}
