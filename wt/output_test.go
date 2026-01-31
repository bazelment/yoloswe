package wt

import (
	"bytes"
	"strings"
	"testing"
)

func TestOutput(t *testing.T) {
	t.Run("colorized output", func(t *testing.T) {
		buf := &bytes.Buffer{}
		o := NewOutput(buf, true)

		o.Success("test message")
		output := buf.String()

		if !strings.Contains(output, ColorGreen) {
			t.Error("expected green color code in output")
		}
		if !strings.Contains(output, "✓") {
			t.Error("expected checkmark in output")
		}
		if !strings.Contains(output, "test message") {
			t.Error("expected message in output")
		}
	})

	t.Run("non-colorized output", func(t *testing.T) {
		buf := &bytes.Buffer{}
		o := NewOutput(buf, false)

		o.Success("test message")
		output := buf.String()

		if strings.Contains(output, ColorGreen) {
			t.Error("unexpected color code in non-colorized output")
		}
		if !strings.Contains(output, "✓") {
			t.Error("expected checkmark in output")
		}
	})

	t.Run("error output", func(t *testing.T) {
		buf := &bytes.Buffer{}
		o := NewOutput(buf, true)

		o.Error("error message")
		output := buf.String()

		if !strings.Contains(output, ColorRed) {
			t.Error("expected red color code in output")
		}
		if !strings.Contains(output, "✗") {
			t.Error("expected X mark in output")
		}
	})

	t.Run("info output", func(t *testing.T) {
		buf := &bytes.Buffer{}
		o := NewOutput(buf, true)

		o.Info("info message")
		output := buf.String()

		if !strings.Contains(output, "→") {
			t.Error("expected arrow in output")
		}
	})

	t.Run("warn output", func(t *testing.T) {
		buf := &bytes.Buffer{}
		o := NewOutput(buf, true)

		o.Warn("warning message")
		output := buf.String()

		if !strings.Contains(output, ColorYellow) {
			t.Error("expected yellow color code in output")
		}
		if !strings.Contains(output, "!") {
			t.Error("expected exclamation mark in output")
		}
	})

	t.Run("colorize helper", func(t *testing.T) {
		o := NewOutput(nil, true)
		result := o.Colorize(ColorCyan, "test")
		if result != ColorCyan+"test"+ColorReset {
			t.Errorf("Colorize() = %q, want %q", result, ColorCyan+"test"+ColorReset)
		}

		o2 := NewOutput(nil, false)
		result2 := o2.Colorize(ColorCyan, "test")
		if result2 != "test" {
			t.Errorf("Colorize() with no color = %q, want %q", result2, "test")
		}
	})
}
