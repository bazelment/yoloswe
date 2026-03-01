package replay

import (
	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/sessionmodel"
)

// parseRawJSONL loads a ~/.claude/projects/ JSONL session file using the
// sessionmodel pipeline (FromRawJSONL → MessageParser → SessionModel).
func parseRawJSONL(path string) (*Result, error) {
	model, err := sessionmodel.LoadFromRawJSONL(path)
	if err != nil {
		return nil, err
	}

	lines := model.Output()
	meta := model.Meta()

	// Extract prompt from the first text line (often the user's initial message).
	prompt := extractPrompt(lines)

	return &Result{
		Lines:  lines,
		Prompt: prompt,
		Status: session.SessionStatus(meta.Status),
		Format: FormatRawJSONL,
	}, nil
}

// extractPrompt finds the user's initial prompt for display.
// It prefers lines explicitly marked as user prompts (IsUserPrompt), falling
// back to the first text line if none are found.
func extractPrompt(lines []session.OutputLine) string {
	fallback := ""
	for i := range lines {
		line := &lines[i]
		if line.Type != session.OutputTypeText || line.Content == "" {
			continue
		}
		// Truncate to 203 so that TruncateForDisplay keeps 200 content runes + "...".
		s := sessionmodel.TruncateForDisplay(line.Content, 203)
		if line.IsUserPrompt {
			return s
		}
		if fallback == "" {
			fallback = s
		}
	}
	return fallback
}
