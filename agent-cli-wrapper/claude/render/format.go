package render

import (
	"encoding/json"
	"fmt"
	"strings"
)

// formatToolInput formats tool input for inline display after the tool name bracket.
// Returns an empty string for tools that should be handled specially.
func formatToolInput(name string, input map[string]interface{}) string {
	switch name {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return TruncatePath(path, 80)
		}
	case "Write", "Edit":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("→ %s", TruncatePath(path, 70))
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return TruncateForDisplay(cmd, 80)
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return pattern
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return TruncateForDisplay(pattern, 60)
		}
	case "Task":
		if desc, ok := input["description"].(string); ok {
			return desc
		}
	case "AskUserQuestion", "ExitPlanMode":
		return ""
	default:
		if len(input) > 0 {
			data, _ := json.Marshal(input)
			return TruncateForDisplay(string(data), 100)
		}
	}
	return ""
}

// formatContent formats tool result content for display.
func formatContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, block := range c {
			if bMap, ok := block.(map[string]interface{}); ok {
				if text, ok := bMap["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		data, _ := json.Marshal(content)
		return string(data)
	}
}

// TruncateForDisplay truncates s to at most max Unicode code points,
// appending "..." if truncation occurred (the suffix counts toward max).
// Rune-based indexing avoids splitting multi-byte UTF-8 sequences.
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

// Truncate truncates a string to the given max byte length.
// Kept for backward compatibility; prefer TruncateForDisplay for new code.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// TruncatePath truncates a file path, keeping the end visible.
// For paths longer than max, shows ".../" plus the last path components.
func TruncatePath(path string, max int) string {
	runes := []rune(path)
	if len(runes) <= max {
		return path
	}
	if lastSlash := strings.LastIndexByte(path, '/'); lastSlash > 0 {
		suffix := path[lastSlash:]
		if len([]rune(suffix)) <= max-3 {
			return "..." + suffix
		}
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
