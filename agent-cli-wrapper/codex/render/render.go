// Package render provides ANSI-colored terminal rendering for Codex sessions.
package render

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ANSI color codes - chosen to work on both light and dark backgrounds
const (
	ColorReset   = "\x1b[0m"
	ColorDim     = "\x1b[2m"
	ColorItalic  = "\x1b[3m"
	ColorBold    = "\x1b[1m"
	ColorRed     = "\x1b[31m"
	ColorGreen   = "\x1b[32m"
	ColorYellow  = "\x1b[33m"
	ColorBlue    = "\x1b[34m"
	ColorMagenta = "\x1b[35m"
	ColorCyan    = "\x1b[36m"
	ColorGray    = "\x1b[90m"
)

// Renderer handles terminal output with ANSI colors.
type Renderer struct {
	out         io.Writer
	commands    map[string]*commandState
	mu          sync.Mutex
	verbose     bool
	noColor     bool
	inReasoning bool
}

// maxOutputBytes is the maximum size of command output to buffer (1MB).
const maxOutputBytes = 1024 * 1024

// commandState tracks an active command's state.
type commandState struct {
	command   string
	output    strings.Builder
	truncated bool
}

// NewRenderer creates a new renderer writing to the given output.
// If verbose is false, command output is truncated.
// If noColor is true, ANSI color codes are suppressed.
func NewRenderer(out io.Writer, verbose, noColor bool) *Renderer {
	if !noColor {
		noColor = !isTerminal(out)
	}
	return &Renderer{
		out:      out,
		verbose:  verbose,
		noColor:  noColor,
		commands: make(map[string]*commandState),
	}
}

// isTerminal checks if the writer is a terminal.
func isTerminal(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		stat, err := f.Stat()
		if err != nil {
			return false
		}
		return (stat.Mode() & os.ModeCharDevice) != 0
	}
	return false
}

// color returns the color code if colors are enabled, empty string otherwise.
func (r *Renderer) color(c string) string {
	if r.noColor {
		return ""
	}
	return c
}

// Status prints a status message.
func (r *Renderer) Status(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Use dim for status - visible on both light and dark backgrounds
	fmt.Fprintf(r.out, "%s[Status] %s%s\n", r.color(ColorDim), msg, r.color(ColorReset))
}

// Text prints streaming text output.
func (r *Renderer) Text(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Add newline when transitioning from reasoning to text
	if r.inReasoning {
		fmt.Fprintln(r.out)
		r.inReasoning = false
	}
	fmt.Fprint(r.out, text)
}

// Reasoning prints reasoning/thinking output in italic style.
func (r *Renderer) Reasoning(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Use italic + cyan for reasoning - readable on both light and dark backgrounds
	fmt.Fprintf(r.out, "%s%s%s%s", r.color(ColorItalic), r.color(ColorCyan), text, r.color(ColorReset))
	r.inReasoning = true
}

// CommandStart prints the start of a command execution.
func (r *Renderer) CommandStart(callID, command string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.commands[callID] = &commandState{command: command}
	fmt.Fprintf(r.out, "\n%s[%s]%s ", r.color(ColorCyan), truncate(command, 60), r.color(ColorReset))
}

// HasOutput returns true if any output has been accumulated for the given command.
func (r *Renderer) HasOutput(callID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cmd, ok := r.commands[callID]; ok {
		return cmd.output.Len() > 0
	}
	return false
}

// CommandOutput accumulates command output with memory limit.
// When verbose=false, output is not accumulated (won't be displayed anyway).
func (r *Renderer) CommandOutput(callID, chunk string) {
	// Don't buffer output if we won't display it
	if !r.verbose {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	cmd, ok := r.commands[callID]
	if !ok {
		return
	}

	// Limit buffer size to prevent memory exhaustion
	if cmd.truncated {
		return // Already at limit, ignore further output
	}

	remaining := maxOutputBytes - cmd.output.Len()
	if remaining <= 0 {
		cmd.truncated = true
		return
	}

	if len(chunk) > remaining {
		cmd.output.WriteString(chunk[:remaining])
		cmd.truncated = true
	} else {
		cmd.output.WriteString(chunk)
	}
}

// CommandEnd prints the completion of a command execution.
// When verbose=false, only shows inline status (✓/✗) without output.
// When verbose=true, shows full output followed by status.
func (r *Renderer) CommandEnd(callID string, exitCode int, durationMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cmd, ok := r.commands[callID]
	if !ok {
		return
	}
	delete(r.commands, callID)

	// Only print output when verbose
	if r.verbose {
		output := strings.TrimSpace(cmd.output.String())
		if output != "" {
			lines := strings.Split(output, "\n")
			// No color for command output - works on all backgrounds
			fmt.Fprint(r.out, output)
			// Indicate if output was truncated due to size limit
			if cmd.truncated {
				fmt.Fprintf(r.out, "%s\n  [output truncated at 1MB]%s", r.color(ColorYellow), r.color(ColorReset))
			}
			_ = lines // unused but keeping for potential future use
		}

		// Print status indicator with newline (block format)
		if exitCode == 0 {
			fmt.Fprintf(r.out, "\n  %s✓ %.2fs%s\n", r.color(ColorGreen), float64(durationMs)/1000, r.color(ColorReset))
		} else {
			fmt.Fprintf(r.out, "\n  %s✗ exit %d (%.2fs)%s\n", r.color(ColorRed), exitCode, float64(durationMs)/1000, r.color(ColorReset))
		}
	} else {
		// Print inline status indicator (no output shown)
		if exitCode == 0 {
			fmt.Fprintf(r.out, "%s✓ %.2fs%s\n", r.color(ColorGreen), float64(durationMs)/1000, r.color(ColorReset))
		} else {
			fmt.Fprintf(r.out, "%s✗ exit %d (%.2fs)%s\n", r.color(ColorRed), exitCode, float64(durationMs)/1000, r.color(ColorReset))
		}
	}
}

// TurnComplete prints a summary of the completed turn.
func (r *Renderer) TurnComplete(success bool, durationMs int64, inputTokens, outputTokens int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.out, "\n%s───────────────────────────────────────────────────────%s\n", r.color(ColorDim), r.color(ColorReset))

	status := "✓"
	colorCode := ColorGreen
	if !success {
		status = "✗"
		colorCode = ColorRed
	}

	fmt.Fprintf(r.out, "%s%s Turn complete (%.1fs, %d input / %d output tokens)%s\n",
		r.color(colorCode), status, float64(durationMs)/1000, inputTokens, outputTokens, r.color(ColorReset))
}

// Error prints an error message.
func (r *Renderer) Error(err error, context string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.out, "\n%s[Error: %s]%s %v\n", r.color(ColorRed), context, r.color(ColorReset), err)
}

// truncate truncates a string to the given max length.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
