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

func TestCommandLifecycle(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true, true) // verbose=true to see all output

	r.CommandStart("call1", "ls -la")
	r.CommandOutput("call1", "file1.txt\n")
	r.CommandOutput("call1", "file2.txt\n")
	r.CommandEnd("call1", 0, 50)

	output := buf.String()
	if !strings.Contains(output, "ls -la") {
		t.Errorf("Missing command: %q", output)
	}
	if !strings.Contains(output, "file1.txt") {
		t.Errorf("Missing output: %q", output)
	}
	if !strings.Contains(output, "✓") {
		t.Errorf("Missing success indicator: %q", output)
	}
	if !strings.Contains(output, "0.05s") {
		t.Errorf("Missing duration: %q", output)
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

func TestCommandOutputTruncation(t *testing.T) {
	// When verbose=false, no output should be shown (only inline status)
	t.Run("non-verbose mode hides output", func(t *testing.T) {
		var buf bytes.Buffer
		r := NewRenderer(&buf, false, true) // verbose=false

		r.CommandStart("call1", "cat bigfile")
		for i := 0; i < 15; i++ {
			r.CommandOutput("call1", "line "+string(rune('A'+i))+"\n")
		}
		r.CommandEnd("call1", 0, 100)

		output := buf.String()
		// Should show command and inline status, but no output content
		if !strings.Contains(output, "cat bigfile") {
			t.Errorf("Missing command name: %q", output)
		}
		if !strings.Contains(output, "✓") {
			t.Errorf("Missing success indicator: %q", output)
		}
		// Should NOT contain any line output
		if strings.Contains(output, "line A") {
			t.Errorf("Non-verbose mode should not show output: %q", output)
		}
	})

	// When verbose=true, full output should be shown
	t.Run("verbose mode shows output", func(t *testing.T) {
		var buf bytes.Buffer
		r := NewRenderer(&buf, true, true) // verbose=true

		r.CommandStart("call1", "cat bigfile")
		for i := 0; i < 15; i++ {
			r.CommandOutput("call1", "line "+string(rune('A'+i))+"\n")
		}
		r.CommandEnd("call1", 0, 100)

		output := buf.String()
		// Should show command, output, and status
		if !strings.Contains(output, "cat bigfile") {
			t.Errorf("Missing command name: %q", output)
		}
		if !strings.Contains(output, "line A") {
			t.Errorf("Missing output content: %q", output)
		}
		if !strings.Contains(output, "✓") {
			t.Errorf("Missing success indicator: %q", output)
		}
	})
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
