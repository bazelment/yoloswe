package claude

import (
	"regexp"
	"strings"
)

var bgTaskIDRegex = regexp.MustCompile(`Command running in background with ID: (\S+)`)

// extractBackgroundTaskID parses the background task ID from a tool_result
// content string returned by the CLI when a Bash command is launched with
// run_in_background: true.
// Returns empty string if no background task ID is found.
func extractBackgroundTaskID(content string) string {
	m := bgTaskIDRegex.FindStringSubmatch(content)
	if len(m) >= 2 {
		return strings.TrimSuffix(m[1], ".")
	}
	return ""
}

var taskNotificationIDRegex = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)

// extractTaskNotificationID parses the task ID from a CLI <task-notification>
// message. The CLI injects these user messages when a background task completes.
// Returns empty string if the content is not a task notification.
func extractTaskNotificationID(content string) string {
	if !strings.Contains(content, "<task-notification>") {
		return ""
	}
	m := taskNotificationIDRegex.FindStringSubmatch(content)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}
