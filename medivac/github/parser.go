package github

import (
	"fmt"
	"regexp"
	"strings"
)

// Log cleaning patterns.
var (
	ansiPattern     = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	ghActionsMarker = regexp.MustCompile(`##\[(error|group|endgroup|warning|notice|debug)\]`)
	ciTimestampPat  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[\d:.]+Z `)
)

// MaxLogSize is the maximum log size (in bytes) to send to the LLM for triage.
const MaxLogSize = 50 * 1024

// CleanLog strips ANSI escapes, GitHub Actions markers, CI timestamps, and
// truncates to the last MaxLogSize bytes for LLM input.
func CleanLog(raw string) string {
	s := ansiPattern.ReplaceAllString(raw, "")
	s = ghActionsMarker.ReplaceAllString(s, "")
	s = ciTimestampPat.ReplaceAllString(s, "")

	if len(s) > MaxLogSize {
		s = s[len(s)-MaxLogSize:]
		// Trim to the next newline to avoid partial lines.
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
	}
	return s
}

// TrimLog keeps the first headLines and last tailLines of a log, inserting
// a marker in between. If the log has fewer lines than head+tail, it is
// returned unchanged.
func TrimLog(log string, headLines, tailLines int) string {
	lines := strings.Split(log, "\n")
	total := len(lines)
	if total <= headLines+tailLines {
		return log
	}
	head := lines[:headLines]
	tail := lines[total-tailLines:]
	trimmed := total - headLines - tailLines
	result := make([]string, 0, headLines+tailLines+1)
	result = append(result, head...)
	result = append(result, fmt.Sprintf("\n... (%d lines trimmed) ...\n", trimmed))
	result = append(result, tail...)
	return strings.Join(result, "\n")
}
