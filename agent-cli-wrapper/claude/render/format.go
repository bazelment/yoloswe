package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/displaytext"
)

// FormatToolInput formats tool input for inline display after the tool name bracket.
// Returns an empty string for tools that should be handled specially.
func FormatToolInput(name string, input map[string]interface{}) string {
	switch name {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return displaytext.TruncatePath(path, 80)
		}
	case "Write", "Edit":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("→ %s", displaytext.TruncatePath(path, 70))
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return displaytext.Truncate(cmd, 80)
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return pattern
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return displaytext.Truncate(pattern, 60)
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
			return displaytext.Truncate(string(data), 100)
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
