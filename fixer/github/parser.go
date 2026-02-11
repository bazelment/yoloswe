package github

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// FailureCategory classifies the type of CI failure.
type FailureCategory string

const (
	CategoryLintGo      FailureCategory = "lint/go"
	CategoryLintBazel   FailureCategory = "lint/bazel"
	CategoryLintTS      FailureCategory = "lint/ts"
	CategoryLintPython  FailureCategory = "lint/python"
	CategoryBuild       FailureCategory = "build"
	CategoryBuildDocker FailureCategory = "build/docker"
	CategoryTest        FailureCategory = "test"
	CategoryInfraDepbot FailureCategory = "infra/dependabot"
	CategoryInfraCI     FailureCategory = "infra/ci"
	CategoryUnknown     FailureCategory = "unknown"
)

// ValidCategories is the set of valid failure categories for LLM triage.
var ValidCategories = map[FailureCategory]bool{
	CategoryLintGo:      true,
	CategoryLintBazel:   true,
	CategoryLintTS:      true,
	CategoryLintPython:  true,
	CategoryBuild:       true,
	CategoryBuildDocker: true,
	CategoryTest:        true,
	CategoryInfraDepbot: true,
	CategoryInfraCI:     true,
	CategoryUnknown:     true,
}

// CIFailure represents a single categorized CI failure.
type CIFailure struct {
	Timestamp time.Time
	RunURL    string
	HeadSHA   string
	Branch    string
	JobName   string
	Category  FailureCategory
	Signature string
	Summary   string
	Details   string
	File      string
	RunID     int64
	Line      int
}

// Log cleaning patterns.
var (
	ansiPattern      = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	ghActionsMarker  = regexp.MustCompile(`##\[(error|group|endgroup|warning|notice|debug)\]`)
	ciTimestampPat   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[\d:.]+Z `)
	normalizeLineCol = regexp.MustCompile(`:\d+:\d+`)
	normalizeHex     = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	normalizeTS      = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`)
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

// ComputeSignature generates a stable dedup key for a failure.
// Format: {category}:{normalized-message-hash}:{file}
// When summary is empty, falls back to details, then job name.
// Job name is only used in the hash when it's the sole identifier
// (empty summary + empty details), so the same error across different
// jobs deduplicates correctly.
func ComputeSignature(category FailureCategory, file, summary, jobName, details string) string {
	msg := summary
	if msg == "" {
		msg = details
	}
	if msg == "" {
		// Only include job name when there's no other content to hash.
		msg = jobName
	}
	normalized := normalizeMessage(msg)
	h := sha256.Sum256([]byte(normalized))
	shortHash := fmt.Sprintf("%x", h[:8])
	return fmt.Sprintf("%s:%s:%s", category, shortHash, file)
}

// normalizeMessage strips volatile parts (line numbers, hashes, timestamps) from error messages.
func normalizeMessage(msg string) string {
	msg = normalizeLineCol.ReplaceAllString(msg, "")
	msg = normalizeHex.ReplaceAllString(msg, "")
	msg = normalizeTS.ReplaceAllString(msg, "")
	return strings.TrimSpace(msg)
}
