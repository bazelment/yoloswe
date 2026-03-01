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
	for _, line := range lines {
		if line.Type != session.OutputTypeText || line.Content == "" {
			continue
		}
		s := truncateRunes(line.Content, 200)
		if line.IsUserPrompt {
			return s
		}
		if fallback == "" {
			fallback = s
		}
	}
	return fallback
}

// truncateRunes truncates s to at most maxRunes Unicode code points, appending
// "..." if truncation occurred. Using rune-based indexing avoids splitting
// multi-byte UTF-8 sequences that byte-based slicing would corrupt.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
