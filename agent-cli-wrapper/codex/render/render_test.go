package render

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNewRenderer(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, false)
	if r == nil {
		t.Fatal("NewRenderer returned nil")
	}
	if r.out != &buf {
		t.Error("Renderer output not set correctly")
	}
	if !r.verbose {
		t.Error("Renderer verbose not set correctly")
	}
}

func TestStatus(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true) // noColor=true for predictable output
	r.Status("test message")

	output := buf.String()
	if !strings.Contains(output, "[Status]") {
		t.Errorf("Status output missing [Status] prefix: %q", output)
	}
	if !strings.Contains(output, "test message") {
		t.Errorf("Status output missing message: %q", output)
	}
}

func TestSessionInfo(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false, true)
	r.SessionInfo("abc-123", "gpt-5")

	output := buf.String()
	if !strings.Contains(output, "session=abc-123") {
		t.Errorf("SessionInfo missing session ID: %q", output)
	}
	if !strings.Contains(output, "model=gpt-5") {
		t.Errorf("SessionInfo missing model: %q", output)
	}
}

func TestText(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)
	r.Text("hello ")
	r.Text("world")

	if buf.String() != "hello world" {
		t.Errorf("Text output: got %q, want %q", buf.String(), "hello world")
	}
}

func TestReasoning(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)
	r.Reasoning("thinking...")

	output := buf.String()
	if !strings.Contains(output, "thinking...") {
		t.Errorf("Reasoning output missing content: %q", output)
	}
}

func TestCommandLifecycle_Verbose(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true) // verbose=true

	r.CommandStart("call1", "ls -la")
	r.CommandOutput("call1", "file1.txt\n") // output is ignored
	r.CommandEnd("call1", 0, 50)

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
	// Command output is never shown
	if strings.Contains(output, "file1.txt") {
		t.Errorf("Command output should not be shown: %q", output)
	}
}

func TestCommandLifecycle_NonVerbose(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false, true) // verbose=false

	r.CommandStart("call1", "ls -la")
	r.CommandEnd("call1", 0, 50)

	output := buf.String()
	// Non-verbose: tool calls are hidden
	if strings.Contains(output, "ls -la") {
		t.Errorf("Non-verbose should hide tool calls: %q", output)
	}
}

func TestCommandError(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)

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
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)

	r.CommandStart("call1", "read file.go")
	r.CommandEnd("call1", 0, 0)

	output := buf.String()
	if !strings.Contains(output, "[read file.go]") {
		t.Errorf("Missing command: %q", output)
	}
	if !strings.Contains(output, "✓") {
		t.Errorf("Missing success indicator: %q", output)
	}
	// Zero duration should be omitted
	if strings.Contains(output, "0.00s") {
		t.Errorf("Zero duration should be omitted: %q", output)
	}
}

func TestTurnComplete(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)

	r.TurnComplete(true, 5000, 1000, 500)

	output := buf.String()
	if !strings.Contains(output, "5.0s") {
		t.Errorf("Missing duration: %q", output)
	}
	if !strings.Contains(output, "1000") {
		t.Errorf("Missing input tokens: %q", output)
	}
	if !strings.Contains(output, "500") {
		t.Errorf("Missing output tokens: %q", output)
	}
	if !strings.Contains(output, "✓") {
		t.Errorf("Missing success indicator: %q", output)
	}
}

func TestTurnCompleteFailed(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)

	r.TurnComplete(false, 3000, 500, 100)

	output := buf.String()
	if !strings.Contains(output, "✗") {
		t.Errorf("Missing failure indicator: %q", output)
	}
}

func TestError(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)

	r.Error(errors.New("something went wrong"), "test context")

	output := buf.String()
	if !strings.Contains(output, "[Error: test context]") {
		t.Errorf("Missing error context: %q", output)
	}
	if !strings.Contains(output, "something went wrong") {
		t.Errorf("Missing error message: %q", output)
	}
}

func TestNoColorMode(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true) // noColor=true

	r.Status("test")
	r.CommandStart("call1", "ls")
	r.CommandEnd("call1", 0, 10)
	r.TurnComplete(true, 1000, 100, 50)

	output := buf.String()
	if strings.Contains(output, "\x1b[") {
		t.Errorf("Color codes present in no-color mode: %q", output)
	}
}

func TestColorMode(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, false) // noColor=false
	// Force noColor off even though buf is not a terminal
	r.noColor = false

	r.Status("test")

	output := buf.String()
	if !strings.Contains(output, "\x1b[") {
		t.Errorf("Color codes missing in color mode: %q", output)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		max      int
	}{
		{"short", "short", 10},
		{"exactly10!", "exactly10!", 10},
		{"this is a long string", "this is...", 10},
		{"abc", "abc", 3},
		{"abcd", "...", 3},
	}

	for _, tt := range tests {
		result := truncate(tt.input, tt.max)
		if result != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, result, tt.expected)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true)

	// Test that concurrent access doesn't panic
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(id int) {
			callID := "call" + string(rune('0'+id))
			r.Status("starting " + callID)
			r.CommandStart(callID, "echo test")
			r.CommandOutput(callID, "output\n")
			r.CommandEnd(callID, 0, 10)
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}
