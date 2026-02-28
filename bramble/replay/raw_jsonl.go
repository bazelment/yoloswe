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

// extractPrompt finds the first user-visible text content for display.
func extractPrompt(lines []session.OutputLine) string {
	for _, line := range lines {
		if line.Type == session.OutputTypeText && line.Content != "" {
			s := line.Content
			if len(s) > 200 {
				s = s[:200] + "..."
			}
			return s
		}
	}
	return ""
}
