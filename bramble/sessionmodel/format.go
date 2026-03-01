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
	runes := []rune(path)
	if len(runes) <= 60 {
		return path
	}
	lastSlash := -1
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == '/' {
			lastSlash = i
			break
		}
	}
	if lastSlash > 0 && len(runes)-lastSlash <= 50 {
		return "..." + string(runes[lastSlash:])
	}
	return string(runes[:57]) + "..."
}

// truncateForDisplay truncates s to at most max Unicode code points, appending
// "..." if truncation occurred. Rune-based indexing avoids splitting multi-byte
// UTF-8 sequences that byte-based slicing would corrupt.
func truncateForDisplay(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-3]) + "..."
}
