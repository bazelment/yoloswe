package reviewer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/framelog"
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

func TestSummarizeToolInput_RedactsCursorSearchKeys(t *testing.T) {
	// Cursor's grepToolCall and globToolCall pass `pattern` and `globPattern`
	// as inputs (yoloswe/reviewer/backend_cursor.go grep/glob input keys).
	// Patterns can carry secrets (e.g. searching for an API key prefix) or
	// reveal what the reviewer was looking for; redact them.
	input := map[string]interface{}{
		"pattern":     "AKIA[0-9A-Z]{16}",
		"globPattern": "**/credentials.*",
	}
	got := summarizeToolInput(input)
	for _, leaked := range []string{"AKIA", "credentials"} {
		if strings.Contains(got, leaked) {
			t.Errorf("summarizeToolInput leaked %q: %s", leaked, got)
		}
	}
	for _, want := range []string{"pattern=<redacted:", "globPattern=<redacted:"} {
		if !strings.Contains(got, want) {
			t.Errorf("summarizeToolInput should mark %s: %s", want, got)
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

// The raw frame that failed to parse must reach the log. Without it the record
// names a Go struct field but never what the backend actually sent — the gap
// that forced the cursor protocol-error investigation to infer the wire shape
// from a decode error instead of reading the frame.
func TestProtocolErrorLine_ExtractsAndBounds(t *testing.T) {
	t.Run("extracts from a wrapped error", func(t *testing.T) {
		inner := &cursor.ProtocolError{
			Message: "failed to parse message",
			Line:    `{"type":"tool_call","tool_call":[{"readToolCall":{}}]}`,
		}
		line, n, ok := protocolErrorLine(fmt.Errorf("cursor: %w", inner))
		if !ok {
			t.Fatal("expected the offending line to be recovered through the wrap")
		}
		if !strings.Contains(line, `"readToolCall"`) {
			t.Errorf("line lost the offending shape: %q", line)
		}
		if n != len(inner.Line) {
			t.Errorf("line_len = %d, want %d", n, len(inner.Line))
		}
	})

	t.Run("truncates an oversized frame but reports true length", func(t *testing.T) {
		full := strings.Repeat("x", framelog.MaxLen*3)
		line, n, ok := protocolErrorLine(&codex.ProtocolError{Message: "boom", Line: full})
		if !ok {
			t.Fatal("expected ok")
		}
		if len(line) >= len(full) {
			t.Errorf("oversized frame was not truncated: got %d bytes", len(line))
		}
		if !strings.HasSuffix(line, "...[truncated]") {
			t.Errorf("truncation must be visible in the log, got suffix %q", line[len(line)-20:])
		}
		if n != len(full) {
			t.Errorf("line_len = %d, want the untruncated %d", n, len(full))
		}
	})

	t.Run("covers all three backends", func(t *testing.T) {
		// The contract is only useful if every backend that carries a Line
		// implements it. claude was missed on the first pass and its parse
		// errors reach the same sink, so pin all three.
		for name, err := range map[string]error{
			"cursor": &cursor.ProtocolError{Message: "m", Line: `{"k":1}`},
			"codex":  &codex.ProtocolError{Message: "m", Line: `{"k":1}`},
			"claude": &claude.ProtocolError{Message: "m", Line: `{"k":1}`},
		} {
			if _, _, ok := protocolErrorLine(err); !ok {
				t.Errorf("%s: ProtocolLine not reachable — that backend's frames are still lost", name)
			}
		}
	})

	t.Run("redacts string values but keeps structure", func(t *testing.T) {
		// tool_call frames are the ones that fail to parse, and they carry
		// command/file_path/content — the values sensitiveToolInputKeys exists
		// to keep out of the log. Shape is what makes a frame diagnostic.
		err := &cursor.ProtocolError{
			Message: "failed to parse message",
			Line:    `{"type":"tool_call","tool_call":[{"readToolCall":{"args":{"path":"/home/alice/.ssh/id_rsa"}}}]}`,
		}
		line, _, ok := protocolErrorLine(err)
		if !ok {
			t.Fatal("expected a line")
		}
		if strings.Contains(line, "/home/alice/.ssh/id_rsa") {
			t.Errorf("secret value leaked into the log: %s", line)
		}
		for _, key := range []string{"tool_call", "readToolCall", "args", "path"} {
			if !strings.Contains(line, key) {
				t.Errorf("key %q was redacted away — the frame is no longer diagnostic: %s", key, line)
			}
		}
		if !strings.Contains(line, "[") {
			t.Errorf("array shape lost — that shape IS the bug being diagnosed: %s", line)
		}
		// The frame-kind discriminator is the single most diagnostic value in
		// the frame; redacting it to "<str:9>" would defeat the logging.
		if !strings.Contains(line, `"type":"tool_call"`) {
			t.Errorf("frame-kind discriminator was redacted away: %s", line)
		}
	})

	t.Run("redacts a truncated trailing string", func(t *testing.T) {
		err := &cursor.ProtocolError{Message: "m", Line: `{"content":"SECRET_TOKEN=abcdef`}
		line, _, ok := protocolErrorLine(err)
		if !ok {
			t.Fatal("expected a line")
		}
		if strings.Contains(line, "SECRET_TOKEN") {
			t.Errorf("truncated string value leaked: %s", line)
		}
	})

	t.Run("absent for errors carrying no line", func(t *testing.T) {
		if _, _, ok := protocolErrorLine(errors.New("plain")); ok {
			t.Error("a non-protocol error must not report a line")
		}
		if _, _, ok := protocolErrorLine(&claude.ProtocolError{Message: "no line"}); ok {
			t.Error("an empty Line must be reported absent, not as an empty field")
		}
	})
}
