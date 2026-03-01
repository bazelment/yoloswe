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
			return fmt.Sprintf("%s: %s", name, TruncateForDisplay(cmd, 50))
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("%s %s", name, pattern)
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("%s %s", name, TruncateForDisplay(pattern, 40))
		}
	case "Task":
		if desc, ok := input["description"].(string); ok {
			return fmt.Sprintf("%s: %s", name, TruncateForDisplay(desc, 40))
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

// TruncateForDisplay truncates s to at most max Unicode code points, appending
// "..." if truncation occurred (the suffix counts toward max). Rune-based
// indexing avoids splitting multi-byte UTF-8 sequences that byte-based slicing
// would corrupt. If max is less than 3, the string is truncated to max runes
// with no suffix.
func TruncateForDisplay(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
