package sessionmodel

import "fmt"

// FormatToolContent creates a display-friendly content string for a tool call.
// Ported from bramble/session/event_handler.go:formatToolContent.
func FormatToolContent(name string, input map[string]interface{}) string {
	if input == nil {
		return name
	}

	switch name {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("%s %s", name, truncatePathForDisplay(path))
		}
	case "Write", "Edit":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("%s → %s", name, truncatePathForDisplay(path))
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return fmt.Sprintf("%s: %s", name, truncateForDisplay(cmd, 50))
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("%s %s", name, pattern)
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("%s %s", name, truncateForDisplay(pattern, 40))
		}
	case "Task":
		if desc, ok := input["description"].(string); ok {
			return fmt.Sprintf("%s: %s", name, truncateForDisplay(desc, 40))
		}
	}
	return name
}

// truncatePathForDisplay keeps the filename visible when truncating paths.
func truncatePathForDisplay(path string) string {
	if len(path) <= 60 {
		return path
	}
	lastSlash := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			lastSlash = i
			break
		}
	}
	if lastSlash > 0 && len(path)-lastSlash <= 50 {
		return "..." + path[lastSlash:]
	}
	return path[:57] + "..."
}

// truncateForDisplay truncates a string to max length.
func truncateForDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
