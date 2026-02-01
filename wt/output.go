// Package wt provides Git worktree management operations.
package wt

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ANSI color codes.
const (
	ColorReset  = "\033[0m"
	ColorBold   = "\033[1m"
	ColorDim    = "\033[2m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorCyan   = "\033[36m"
)

// Output provides colored console output.
type Output struct {
	w         io.Writer
	colorized bool
}

// NewOutput creates an Output that writes to w.
// If colorized is true, output will include ANSI color codes.
func NewOutput(w io.Writer, colorized bool) *Output {
	return &Output{w: w, colorized: colorized}
}

// DefaultOutput creates an Output for stdout with auto-detected color support.
func DefaultOutput() *Output {
	colorized := isTerminal() && os.Getenv("NO_COLOR") == ""
	return NewOutput(os.Stdout, colorized)
}

// isTerminal checks if stdout is a terminal.
func isTerminal() bool {
	stat, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// Colorize wraps text with the given color code.
func (o *Output) Colorize(color, text string) string {
	if o.colorized {
		return color + text + ColorReset
	}
	return text
}

// Success prints a success message with a green checkmark.
func (o *Output) Success(msg string) {
	fmt.Fprintf(o.w, "%s %s\n", o.Colorize(ColorGreen, "✓"), msg)
}

// Error prints an error message with a red X.
func (o *Output) Error(msg string) {
	fmt.Fprintf(o.w, "%s %s\n", o.Colorize(ColorRed, "✗"), msg)
}

// Info prints an info message with a dim arrow.
func (o *Output) Info(msg string) {
	fmt.Fprintf(o.w, "%s %s\n", o.Colorize(ColorDim, "→"), msg)
}

// Warn prints a warning message with a yellow exclamation.
func (o *Output) Warn(msg string) {
	fmt.Fprintf(o.w, "%s %s\n", o.Colorize(ColorYellow, "!"), msg)
}

// Print prints a message without any prefix.
func (o *Output) Print(msg string) {
	fmt.Fprintln(o.w, msg)
}

// Printf prints a formatted message without any prefix.
func (o *Output) Printf(format string, args ...any) {
	fmt.Fprintf(o.w, format, args...)
}

// Pad pads a string (which may contain ANSI codes) to the specified visible width.
func Pad(s string, width int) string {
	visible := visibleLen(s)
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

// visibleLen returns the visible length of a string, ignoring ANSI escape codes.
func visibleLen(s string) int {
	length := 0
	inEscape := false
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		length++
	}
	return length
}
