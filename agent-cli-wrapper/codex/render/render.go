// Package render provides ANSI-colored terminal rendering for agent sessions.
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
	commands    map[string]string // callID → command name
	outputs     map[string]string // callID → accumulated output
	mu          sync.Mutex
	verbose     bool
	noColor     bool
	inReasoning bool
}

// NewRenderer creates a new renderer writing to the given output.
// If verbose is true, tool call names are shown as they execute.
// If noColor is true, ANSI color codes are suppressed.
func NewRenderer(out io.Writer, verbose, noColor bool) *Renderer {
	if !noColor {
		noColor = !isTerminal(out)
	}
	return &Renderer{
		out:      out,
		verbose:  verbose,
		noColor:  noColor,
		commands: make(map[string]string),
		outputs:  make(map[string]string),
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

// SessionInfo prints session metadata (e.g. session ID, model).
func (r *Renderer) SessionInfo(sessionID, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	parts := []string{}
	if sessionID != "" {
		parts = append(parts, "session="+sessionID)
	}
	if model != "" {
		parts = append(parts, "model="+model)
	}
	if len(parts) > 0 {
		fmt.Fprintf(r.out, "%s[%s]%s\n", r.color(ColorGray), strings.Join(parts, " "), r.color(ColorReset))
	}
}

// Status prints a status message.
func (r *Renderer) Status(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(r.out, "%s[Status]%s %s\n", r.color(ColorGray), r.color(ColorReset), msg)
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

	fmt.Fprintf(r.out, "%s%s%s%s", r.color(ColorDim), r.color(ColorItalic), text, r.color(ColorReset))
	r.inReasoning = true
}

// CommandStart records the start of a command execution.
func (r *Renderer) CommandStart(callID, command string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.commands[callID] = command
}

// HasOutput reports whether any command output has been accumulated for callID.
func (r *Renderer) HasOutput(callID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.outputs[callID]
	return ok
}

// CommandOutput accumulates streaming command output for a given call.
// The output is stored but not printed (it is used by sessionplayer for replay).
func (r *Renderer) CommandOutput(callID, chunk string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outputs[callID] += chunk
}

// CommandEnd prints the completion of a command execution.
// In verbose mode, prints one line per tool: [command] ✓ or [command] ✗ exit N
// In non-verbose mode, tool calls are silently tracked.
func (r *Renderer) CommandEnd(callID string, exitCode int, durationMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	command, ok := r.commands[callID]
	if !ok {
		return
	}
	delete(r.commands, callID)
	delete(r.outputs, callID)

	if !r.verbose {
		return
	}

	// Format duration string (omit when zero)
	durationStr := ""
	if durationMs > 0 {
		durationStr = fmt.Sprintf(" %.2fs", float64(durationMs)/1000)
	}

	if exitCode == 0 {
		fmt.Fprintf(r.out, "%s[%s]%s %s✓%s%s\n",
			r.color(ColorCyan), truncate(command, 60), r.color(ColorReset),
			r.color(ColorGreen), durationStr, r.color(ColorReset))
	} else {
		fmt.Fprintf(r.out, "%s[%s]%s %s✗ exit %d%s%s\n",
			r.color(ColorCyan), truncate(command, 60), r.color(ColorReset),
			r.color(ColorRed), exitCode, durationStr, r.color(ColorReset))
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
