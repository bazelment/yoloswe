package engine

import (
	"encoding/json"
	"strings"

	"github.com/bazelment/yoloswe/medivac/issue"
)

// AgentAnalysis holds the structured analysis extracted from a fix agent's response.
type AgentAnalysis struct {
	Reasoning  string
	RootCause  string
	FixOptions []issue.FixOption
	FixApplied bool
}

// analysisJSON is the JSON structure for analysis blocks.
type analysisJSON struct {
	Reasoning  string            `json:"reasoning"`
	RootCause  string            `json:"root_cause"`
	FixOptions []issue.FixOption `json:"fix_options"`
	FixApplied bool              `json:"fix_applied"`
}

// ParseAnalysis extracts a structured ANALYSIS block from agent text.
// Returns nil if no valid block is found.
// Tries JSON format first, then falls back to legacy key-value format.
func ParseAnalysis(text string) *AgentAnalysis {
	start := strings.Index(text, "<ANALYSIS>")
	if start == -1 {
		return nil
	}
	end := strings.Index(text[start:], "</ANALYSIS>")
	if end == -1 {
		return nil
	}
	block := text[start+len("<ANALYSIS>") : start+end]
	block = strings.TrimSpace(block)

	// Try JSON format first
	var ajson analysisJSON
	if err := json.Unmarshal([]byte(block), &ajson); err == nil {
		return &AgentAnalysis{
			Reasoning:  ajson.Reasoning,
			RootCause:  ajson.RootCause,
			FixApplied: ajson.FixApplied,
			FixOptions: ajson.FixOptions,
		}
	}

	// Fall back to legacy key-value format
	a := &AgentAnalysis{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if v, ok := cutField(line, "reasoning:"); ok {
			a.Reasoning = v
		} else if v, ok := cutField(line, "root_cause:"); ok {
			a.RootCause = v
		} else if v, ok := cutField(line, "fix_applied:"); ok {
			a.FixApplied = strings.EqualFold(strings.TrimSpace(v), "yes")
		} else if strings.HasPrefix(line, "- ") {
			opt := parseFixOption(line[2:])
			if opt.Label != "" {
				a.FixOptions = append(a.FixOptions, opt)
			}
		}
	}
	return a
}

// cutField checks if line starts with prefix (case-insensitive) and returns the value.
func cutField(line, prefix string) (string, bool) {
	if len(line) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(line[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(line[len(prefix):]), true
}

// parseFixOption parses "label: description" from a fix option line.
func parseFixOption(s string) issue.FixOption {
	label, desc, ok := strings.Cut(s, ":")
	if !ok {
		return issue.FixOption{Label: strings.TrimSpace(s)}
	}
	return issue.FixOption{
		Label:       strings.TrimSpace(label),
		Description: strings.TrimSpace(desc),
	}
}
