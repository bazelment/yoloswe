package model

import (
	"fmt"
	"regexp"
	"strings"
)

// allowedChars matches characters permitted in workspace keys: [A-Za-z0-9._-]
var allowedChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// SanitizeIdentifier replaces any character not in [A-Za-z0-9._-] with underscore.
// Spec Section 4.2: Workspace Key derivation.
func SanitizeIdentifier(identifier string) string {
	return allowedChars.ReplaceAllString(identifier, "_")
}

// NormalizeState lowercases an issue state for comparison.
// Spec Section 4.2: Normalized Issue State.
func NormalizeState(state string) string {
	return strings.ToLower(state)
}

// ComposeSessionID creates a session ID from thread and turn IDs.
// Spec Section 4.2: Session ID = "<thread_id>-<turn_id>".
func ComposeSessionID(threadID, turnID string) string {
	return fmt.Sprintf("%s-%s", threadID, turnID)
}
