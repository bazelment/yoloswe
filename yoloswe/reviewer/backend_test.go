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

func TestSummarizeToolInput_Empty(t *testing.T) {
	if got := summarizeToolInput(nil); got != "" {
		t.Errorf("summarizeToolInput(nil) = %q, want empty", got)
	}
	if got := summarizeToolInput(map[string]interface{}{}); got != "" {
		t.Errorf("summarizeToolInput({}) = %q, want empty", got)
	}
}
