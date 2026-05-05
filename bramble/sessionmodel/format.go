package sessionmodel

import (
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/displaytext"
)

// FormatToolContent creates a display-friendly content string for a tool call.
// Ported from bramble/session/event_handler.go:formatToolContent.
// This structured-output path includes the tool name and uses narrower persisted
// display widths; render.FormatToolInput uses terminal-oriented inline widths.
func FormatToolContent(name string, input map[string]interface{}) string {
	if input == nil {
		return name
	}

	switch name {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("%s %s", name, displaytext.TruncatePathComponents(path, 60, 1))
		}
	case "Write", "Edit":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("%s → %s", name, displaytext.TruncatePathComponents(path, 60, 1))
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return fmt.Sprintf("%s: %s", name, displaytext.Truncate(cmd, 50))
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("%s %s", name, pattern)
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return fmt.Sprintf("%s %s", name, displaytext.Truncate(pattern, 40))
		}
	case "Task":
		if desc, ok := input["description"].(string); ok {
			return fmt.Sprintf("%s: %s", name, displaytext.Truncate(desc, 40))
		}
	}
	return name
}
