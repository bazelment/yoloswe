package reviewer

import (
	"strings"
	"testing"
)

func TestSummarizeToolInput_RedactsSensitiveValues(t *testing.T) {
	// The per-run log lives on disk and is keyed by developer pid; tool
	// inputs can contain shell commands, file paths, and edit payloads. The
	// summarizer must never write those values verbatim.
	input := map[string]interface{}{
		"command":          "aws s3 cp s3://secret/key ./creds",
		"file_path":        "/home/alice/.config/creds.toml",
		"content":          "SECRET_TOKEN=abcdef",
		"new_string":       "password=hunter2",
		"old_string":       "password=opensesame",
		"isBackground":     false,
		"timeout":          30000,
		"workingDirectory": "/home/alice/project",
	}
	got := summarizeToolInput(input)

	forbidden := []string{
		"aws s3 cp",
		"creds.toml",
		"SECRET_TOKEN",
		"hunter2",
		"opensesame",
		"/home/alice",
	}
	for _, s := range forbidden {
		if strings.Contains(got, s) {
			t.Errorf("summarizeToolInput leaked %q:\n%s", s, got)
		}
	}

	required := []string{
		"command=<redacted:",
		"file_path=<redacted:",
		"content=<redacted:",
		"isBackground=",
		"timeout=",
	}
	for _, s := range required {
		if !strings.Contains(got, s) {
			t.Errorf("summarizeToolInput missing %q:\n%s", s, got)
		}
	}
}

func TestSummarizeToolInput_RedactsCWD(t *testing.T) {
	// Codex shell tool start payloads include a `cwd` key with the absolute
	// workspace path. Without redaction, every shell-tool log line persists
	// the developer's full path. See agent-cli-wrapper/codex/events.go.
	input := map[string]interface{}{"cwd": "/home/alice/secret-project"}
	got := summarizeToolInput(input)
	if strings.Contains(got, "/home/alice") {
		t.Errorf("summarizeToolInput leaked cwd path: %s", got)
	}
	if !strings.Contains(got, "cwd=<redacted:") {
		t.Errorf("summarizeToolInput should mark cwd as redacted: %s", got)
	}
}

func TestSummarizeToolInput_Empty(t *testing.T) {
	if got := summarizeToolInput(nil); got != "" {
		t.Errorf("summarizeToolInput(nil) = %q, want empty", got)
	}
	if got := summarizeToolInput(map[string]interface{}{}); got != "" {
		t.Errorf("summarizeToolInput({}) = %q, want empty", got)
	}
}
