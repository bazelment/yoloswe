package render

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// newTestRenderer creates a renderer with colors disabled (predictable output).
func newTestRenderer(v Verbosity) (*Renderer, *bytes.Buffer) {
	var buf bytes.Buffer
	r := New(&buf, WithVerbosity(v), WithColorMode(ColorNever))
	return r, &buf
}

// --- Constructor tests ---

func TestNewRenderer_Defaults(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	if r.verbosity != VerbosityNormal {
		t.Errorf("default verbosity = %v, want Normal", r.verbosity)
	}
}

func TestNewRendererWithOptions_BackwardCompat(t *testing.T) {
	var buf bytes.Buffer
	r := NewRendererWithOptions(&buf, true, true)
	if r.verbosity != VerbosityVerbose {
		t.Errorf("verbose=true should map to VerbosityVerbose, got %v", r.verbosity)
	}
}

// --- Text output ---

func TestText_Normal(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.Text("hello ")
	r.Text("world")
	if buf.String() != "hello world" {
		t.Errorf("Text output: got %q, want %q", buf.String(), "hello world")
	}
}

func TestText_Quiet_Suppressed(t *testing.T) {
	r, buf := newTestRenderer(VerbosityQuiet)
	r.Text("should not appear")
	if buf.Len() != 0 {
		t.Errorf("Quiet mode should suppress text, got %q", buf.String())
	}
}

// --- Thinking / Reasoning ---

func TestThinking_Verbose(t *testing.T) {
	r, buf := newTestRenderer(VerbosityVerbose)
	r.Thinking("pondering...")
	if !strings.Contains(buf.String(), "pondering...") {
		t.Errorf("Verbose thinking missing content: %q", buf.String())
	}
}

func TestThinking_Normal_Suppressed(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.Thinking("pondering...")
	if buf.Len() != 0 {
		t.Errorf("Normal mode should suppress thinking, got %q", buf.String())
	}
}

func TestReasoning_AliasForThinking(t *testing.T) {
	r, buf := newTestRenderer(VerbosityVerbose)
	r.Reasoning("thinking via alias")
	if !strings.Contains(buf.String(), "thinking via alias") {
		t.Errorf("Reasoning output missing content: %q", buf.String())
	}
}

// --- Status ---

func TestStatus_Normal(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.Status("test message")
	if !strings.Contains(buf.String(), "[Status]") || !strings.Contains(buf.String(), "test message") {
		t.Errorf("Status output: %q", buf.String())
	}
}

func TestStatus_Quiet_Suppressed(t *testing.T) {
	r, buf := newTestRenderer(VerbosityQuiet)
	r.Status("should not appear")
	if buf.Len() != 0 {
		t.Errorf("Quiet mode should suppress status, got %q", buf.String())
	}
}

// --- Tool lifecycle ---

func TestToolStart_Normal(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.ToolStart("Read", "tool-1")
	if !strings.Contains(buf.String(), "[Read]") {
		t.Errorf("ToolStart missing tool name: %q", buf.String())
	}
}

func TestToolStart_Quiet_Suppressed(t *testing.T) {
	r, buf := newTestRenderer(VerbosityQuiet)
	r.ToolStart("Read", "tool-1")
	if buf.Len() != 0 {
		t.Errorf("Quiet mode should suppress tool start, got %q", buf.String())
	}
}

func TestToolComplete_Normal(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.ToolStart("Read", "tool-1")
	buf.Reset()
	r.ToolComplete("Read", map[string]interface{}{"file_path": "/tmp/test.go"})
	if !strings.Contains(buf.String(), "/tmp/test.go") {
		t.Errorf("ToolComplete missing file path: %q", buf.String())
	}
}

func TestToolComplete_InteractiveTool_EventHandler(t *testing.T) {
	r, _ := newTestRenderer(VerbosityNormal)
	var startedName, completedName string
	handler := &testEventHandler{
		onToolStart: func(name, id string, input map[string]interface{}) {
			startedName = name
		},
		onToolComplete: func(name, id string, input map[string]interface{}, result interface{}, isError bool) {
			completedName = name
		},
	}
	r.SetEventHandler(handler)
	r.ToolStart("AskUserQuestion", "q-1")
	r.ToolComplete("AskUserQuestion", nil)
	if startedName != "AskUserQuestion" {
		t.Errorf("EventHandler should receive OnToolStart for interactive tools, got %q", startedName)
	}
	if completedName != "AskUserQuestion" {
		t.Errorf("EventHandler should receive OnToolComplete for interactive tools, got %q", completedName)
	}
}

// --- ToolResult with verbosity ---

func TestToolResult_Error_AlwaysShown(t *testing.T) {
	for _, v := range []Verbosity{VerbosityQuiet, VerbosityNormal, VerbosityVerbose, VerbosityDebug} {
		r, buf := newTestRenderer(v)
		r.ToolResult("something failed", true)
		if !strings.Contains(buf.String(), "something failed") {
			t.Errorf("verbosity=%v: error tool result should always be shown, got %q", v, buf.String())
		}
	}
}

func TestToolResult_Success_VerboseOnly(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.ToolResult("success output", false)
	if buf.Len() != 0 {
		t.Errorf("Normal mode should suppress success results, got %q", buf.String())
	}

	r2, buf2 := newTestRenderer(VerbosityVerbose)
	r2.ToolResult("success output", false)
	if !strings.Contains(buf2.String(), "success output") {
		t.Errorf("Verbose mode should show success results, got %q", buf2.String())
	}
}

func TestToolResult_SkipsInternalErrors(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.ToolResult("Answer questions?", true)
	if buf.Len() != 0 {
		t.Errorf("Internal AskUserQuestion errors should be skipped, got %q", buf.String())
	}
}

// --- ToolProgress ---

func TestToolProgress_DebugOnly(t *testing.T) {
	r, buf := newTestRenderer(VerbosityVerbose)
	r.ToolProgress("chunk")
	if buf.Len() != 0 {
		t.Errorf("Verbose should suppress tool progress, got %q", buf.String())
	}

	r2, buf2 := newTestRenderer(VerbosityDebug)
	r2.ToolProgress("chunk")
	if !strings.Contains(buf2.String(), "chunk") {
		t.Errorf("Debug should show tool progress, got %q", buf2.String())
	}
}

// --- Command lifecycle (Codex-style) ---

func TestCommandLifecycle_Verbose(t *testing.T) {
	r, buf := newTestRenderer(VerbosityVerbose)
	r.CommandStart("call1", "ls -la")
	r.CommandOutput("call1", "file1.txt\n")
	if !r.HasOutput("call1") {
		t.Error("HasOutput should return true after CommandOutput")
	}
	r.CommandEnd("call1", 0, 50)
	if r.HasOutput("call1") {
		t.Error("HasOutput should return false after CommandEnd")
	}

	output := buf.String()
	if !strings.Contains(output, "ls -la") {
		t.Errorf("Missing command: %q", output)
	}
	if !strings.Contains(output, "✓") {
		t.Errorf("Missing success indicator: %q", output)
	}
	if !strings.Contains(output, "0.05s") {
		t.Errorf("Missing duration: %q", output)
	}
}

func TestCommandLifecycle_Normal_Suppressed(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.CommandStart("call1", "ls -la")
	r.CommandEnd("call1", 0, 50)
	if strings.Contains(buf.String(), "ls -la") {
		t.Errorf("Normal should hide command lifecycle: %q", buf.String())
	}
}

func TestCommandEnd_Error(t *testing.T) {
	r, buf := newTestRenderer(VerbosityVerbose)
	r.CommandStart("call1", "false")
	r.CommandEnd("call1", 1, 10)
	output := buf.String()
	if !strings.Contains(output, "✗") {
		t.Errorf("Missing error indicator: %q", output)
	}
	if !strings.Contains(output, "exit 1") {
		t.Errorf("Missing exit code: %q", output)
	}
}

func TestCommandEnd_ZeroDuration(t *testing.T) {
	r, buf := newTestRenderer(VerbosityVerbose)
	r.CommandStart("call1", "read file.go")
	r.CommandEnd("call1", 0, 0)
	output := buf.String()
	if !strings.Contains(output, "✓") {
		t.Errorf("Missing success indicator: %q", output)
	}
	if strings.Contains(output, "0.00s") {
		t.Errorf("Zero duration should be omitted: %q", output)
	}
}

// --- Turn summaries ---

func TestTurnSummary(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.TurnSummary(1, true, 5000, 0.0123)
	output := buf.String()
	if !strings.Contains(output, "✓") || !strings.Contains(output, "Turn 1") {
		t.Errorf("TurnSummary output: %q", output)
	}
}

func TestTurnCompleteWithTokens(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.TurnCompleteWithTokens(true, 5000, 1000, 500)
	output := buf.String()
	if !strings.Contains(output, "5.0s") {
		t.Errorf("Missing duration: %q", output)
	}
	if !strings.Contains(output, "1000") {
		t.Errorf("Missing input tokens: %q", output)
	}
}

// --- Session info ---

func TestSessionInfo_Normal(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.SessionInfo("abc-123", "gpt-5")
	output := buf.String()
	if !strings.Contains(output, "session=abc-123") || !strings.Contains(output, "model=gpt-5") {
		t.Errorf("SessionInfo output: %q", output)
	}
}

func TestSessionInfo_Quiet_Suppressed(t *testing.T) {
	r, buf := newTestRenderer(VerbosityQuiet)
	r.SessionInfo("abc-123", "gpt-5")
	if buf.Len() != 0 {
		t.Errorf("Quiet mode should suppress session info, got %q", buf.String())
	}
}

// --- Error ---

func TestError(t *testing.T) {
	r, buf := newTestRenderer(VerbosityNormal)
	r.Error(errors.New("something broke"), "test context")
	output := buf.String()
	if !strings.Contains(output, "[Error: test context]") || !strings.Contains(output, "something broke") {
		t.Errorf("Error output: %q", output)
	}
}

// --- Color control ---

func TestNoColorMode(t *testing.T) {
	r, buf := newTestRenderer(VerbosityVerbose)
	r.Status("test")
	r.CommandStart("call1", "ls")
	r.CommandEnd("call1", 0, 10)
	r.TurnCompleteWithTokens(true, 1000, 100, 50)
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("Color codes present in no-color mode: %q", buf.String())
	}
}

func TestColorAlwaysMode(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, WithVerbosity(VerbosityNormal), WithColorMode(ColorAlways))
	r.Status("test")
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("Color codes missing in always-color mode: %q", buf.String())
	}
}

// --- Verbosity helpers ---

func TestParseVerbosity(t *testing.T) {
	tests := []struct {
		input string
		want  Verbosity
	}{
		{"quiet", VerbosityQuiet},
		{"q", VerbosityQuiet},
		{"normal", VerbosityNormal},
		{"", VerbosityNormal},
		{"verbose", VerbosityVerbose},
		{"v", VerbosityVerbose},
		{"debug", VerbosityDebug},
		{"d", VerbosityDebug},
		{"unknown", VerbosityNormal},
	}
	for _, tt := range tests {
		got := ParseVerbosity(tt.input)
		if got != tt.want {
			t.Errorf("ParseVerbosity(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestVerbosity_String(t *testing.T) {
	if VerbosityQuiet.String() != "quiet" {
		t.Errorf("VerbosityQuiet.String() = %q", VerbosityQuiet.String())
	}
	if VerbosityDebug.String() != "debug" {
		t.Errorf("VerbosityDebug.String() = %q", VerbosityDebug.String())
	}
}

// --- Format helpers ---

func TestTruncateForDisplay(t *testing.T) {
	if got := TruncateForDisplay("hello", 10); got != "hello" {
		t.Errorf("short string: got %q", got)
	}
	if got := TruncateForDisplay("hello world", 8); got != "hello..." {
		t.Errorf("truncated: got %q", got)
	}
}

func TestTruncatePath(t *testing.T) {
	short := "/tmp/test.go"
	if got := TruncatePath(short, 50); got != short {
		t.Errorf("short path: got %q", got)
	}
	long := "/very/long/path/to/some/deeply/nested/file.go"
	got := TruncatePath(long, 25)
	if got != ".../nested/file.go" {
		t.Errorf("truncated path should keep last two components: got %q", got)
	}
}

// --- Event handler ---

func TestEventHandler_TextFlush(t *testing.T) {
	r, _ := newTestRenderer(VerbosityNormal)
	var received []string
	handler := &testEventHandler{
		onText: func(text string) { received = append(received, text) },
	}
	r.SetEventHandler(handler)

	r.Text("hello\n")
	if len(received) != 1 || received[0] != "hello\n" {
		t.Errorf("Expected text flush at newline, got %v", received)
	}
}

func TestEventHandler_StatusAlwaysEmitted(t *testing.T) {
	r, _ := newTestRenderer(VerbosityQuiet)
	var received string
	handler := &testEventHandler{
		onStatus: func(msg string) { received = msg },
	}
	r.SetEventHandler(handler)
	r.Status("quiet status")
	if received != "quiet status" {
		t.Errorf("EventHandler should receive status even in quiet mode, got %q", received)
	}
}

// testEventHandler is a flexible EventHandler for testing.
type testEventHandler struct {
	NoOpEventHandler
	onText         func(string)
	onToolStart    func(name, id string, input map[string]interface{})
	onToolComplete func(name, id string, input map[string]interface{}, result interface{}, isError bool)
	onStatus       func(string)
}

func (h *testEventHandler) OnText(text string) {
	if h.onText != nil {
		h.onText(text)
	}
}

func (h *testEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	if h.onToolStart != nil {
		h.onToolStart(name, id, input)
	}
}

func (h *testEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	if h.onToolComplete != nil {
		h.onToolComplete(name, id, input, result, isError)
	}
}

func (h *testEventHandler) OnStatus(msg string) {
	if h.onStatus != nil {
		h.onStatus(msg)
	}
}
