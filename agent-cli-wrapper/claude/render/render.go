// Package render provides ANSI-colored terminal rendering for Claude sessions.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ANSI color codes
const (
	ColorReset   = "\x1b[0m"
	ColorDim     = "\x1b[2m"
	ColorItalic  = "\x1b[3m"
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
	mu      sync.Mutex
	out     io.Writer
	verbose bool // When false, only error tool results are shown
	noColor bool // When true, ANSI color codes are suppressed

	// State tracking
	inToolOutput bool
	lastToolName string
}

// NewRenderer creates a new renderer writing to the given output.
// If verbose is false, only error tool results are displayed.
// Colors are automatically disabled if output is not a terminal.
func NewRenderer(out io.Writer, verbose bool) *Renderer {
	noColor := !isTerminal(out)
	return &Renderer{out: out, verbose: verbose, noColor: noColor}
}

// NewRendererWithOptions creates a new renderer with explicit color control.
func NewRendererWithOptions(out io.Writer, verbose, noColor bool) *Renderer {
	return &Renderer{out: out, verbose: verbose, noColor: noColor}
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

	fmt.Fprintf(r.out, "%s[Status]%s %s\n", r.color(ColorGray), r.color(ColorReset), msg)
}

// Text prints streaming text output.
func (r *Renderer) Text(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()
	fmt.Fprint(r.out, text)
}

// Thinking prints thinking/reasoning output in dim italic.
func (r *Renderer) Thinking(thinking string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()
	fmt.Fprintf(r.out, "%s%s%s%s", r.color(ColorDim), r.color(ColorItalic), thinking, r.color(ColorReset))
}

// ToolStart prints the start of a tool invocation.
func (r *Renderer) ToolStart(name, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()
	// Don't print for interactive tools - they'll be handled specially
	if name == "AskUserQuestion" || name == "ExitPlanMode" {
		r.lastToolName = name
		return
	}
	fmt.Fprintf(r.out, "\n%s[%s]%s ", r.color(ColorCyan), name, r.color(ColorReset))
	r.inToolOutput = true
	r.lastToolName = name
}

// ToolProgress prints streaming tool input chunks.
func (r *Renderer) ToolProgress(chunk string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Tool progress is typically JSON, print in yellow
	fmt.Fprintf(r.out, "%s%s%s", r.color(ColorYellow), chunk, r.color(ColorReset))
}

// ToolComplete prints the completed tool input.
func (r *Renderer) ToolComplete(name string, input map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Don't print for interactive tools
	if name == "AskUserQuestion" || name == "ExitPlanMode" {
		r.inToolOutput = false
		return
	}

	// Format tool-specific output
	summary := r.formatToolInput(name, input)
	if summary != "" {
		fmt.Fprintf(r.out, "%s%s%s\n", r.color(ColorYellow), summary, r.color(ColorReset))
	} else {
		fmt.Fprintln(r.out)
	}

	r.inToolOutput = false
}

// ToolResult prints the result of a tool execution.
// Only errors are shown unless verbose mode is enabled.
func (r *Renderer) ToolResult(content interface{}, isError bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Only show non-error results in verbose mode
	if !isError && !r.verbose {
		return
	}

	// Format content
	contentStr := r.formatContent(content)
	if contentStr == "" {
		return
	}

	// Skip internal AskUserQuestion/ExitPlanMode error results (these are expected when we respond)
	if isError && (contentStr == "Answer questions?" ||
		strings.Contains(contentStr, "AskUserQuestion") ||
		strings.Contains(contentStr, "ExitPlanMode")) {
		return
	}

	colorCode := ColorGreen
	prefix := "  → "
	if isError {
		colorCode = ColorRed
		prefix = "  ✗ "
	}

	// Truncate long output
	lines := strings.Split(contentStr, "\n")
	if len(lines) > 10 {
		contentStr = strings.Join(lines[:10], "\n") + fmt.Sprintf("\n  ... (%d more lines)", len(lines)-10)
	}

	// Indent each line
	indented := strings.ReplaceAll(contentStr, "\n", "\n    ")
	fmt.Fprintf(r.out, "%s%s%s%s\n", r.color(colorCode), prefix, indented, r.color(ColorReset))
}

// Question prints a question prompt with simple string options.
func (r *Renderer) Question(question string, options []string) {
	opts := make([]QuestionOption, len(options))
	for i, o := range options {
		opts[i] = QuestionOption{Label: o}
	}
	r.QuestionWithOptions(question, "", opts)
}

// QuestionWithOptions prints a question prompt with labeled options.
func (r *Renderer) QuestionWithOptions(question, header string, options []QuestionOption) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()

	// Print header if present
	if header != "" {
		fmt.Fprintf(r.out, "\n%s[%s]%s %s\n", r.color(ColorMagenta), header, r.color(ColorReset), question)
	} else {
		fmt.Fprintf(r.out, "\n%s[Question]%s %s\n", r.color(ColorMagenta), r.color(ColorReset), question)
	}

	if len(options) > 0 {
		for i, opt := range options {
			if opt.Description != "" {
				fmt.Fprintf(r.out, "  %s%d.%s %s %s(%s)%s\n",
					r.color(ColorCyan), i+1, r.color(ColorReset),
					opt.Label,
					r.color(ColorGray), opt.Description, r.color(ColorReset))
			} else {
				fmt.Fprintf(r.out, "  %s%d.%s %s\n", r.color(ColorCyan), i+1, r.color(ColorReset), opt.Label)
			}
		}
	}
}

// QuestionAutoAnswer renders a question with all options, highlighting the auto-selected answer.
// Shows full context for transparency even in simple mode.
func (r *Renderer) QuestionAutoAnswer(question, header string, options []QuestionOption, selectedIdx int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()

	// Print header if present
	if header != "" {
		fmt.Fprintf(r.out, "\n%s[%s]%s %s\n", r.color(ColorMagenta), header, r.color(ColorReset), question)
	} else {
		fmt.Fprintf(r.out, "\n%s[Question]%s %s\n", r.color(ColorMagenta), r.color(ColorReset), question)
	}

	// Show all options, highlight the selected one
	for i, opt := range options {
		if i == selectedIdx {
			// Highlight selected option in green
			fmt.Fprintf(r.out, "  %s→ %d. %s%s", r.color(ColorGreen), i+1, opt.Label, r.color(ColorReset))
			fmt.Fprintf(r.out, " %s(auto-selected)%s\n", r.color(ColorGray), r.color(ColorReset))
		} else {
			// Show other options dimmed
			fmt.Fprintf(r.out, "  %s  %d. %s%s\n", r.color(ColorGray), i+1, opt.Label, r.color(ColorReset))
		}
		if opt.Description != "" {
			fmt.Fprintf(r.out, "  %s     %s%s\n", r.color(ColorGray), opt.Description, r.color(ColorReset))
		}
	}
}

// PlanComplete prints the plan completion summary.
func (r *Renderer) PlanComplete(input map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()

	fmt.Fprintf(r.out, "\n%s%s%s\n", r.color(ColorGreen), strings.Repeat("═", 60), r.color(ColorReset))
	fmt.Fprintf(r.out, "%s[Plan Complete]%s\n", r.color(ColorGreen), r.color(ColorReset))

	// Print any plan metadata from the input
	if allowedPrompts, ok := input["allowedPrompts"].([]interface{}); ok && len(allowedPrompts) > 0 {
		fmt.Fprintf(r.out, "\n%sRequested permissions:%s\n", r.color(ColorGray), r.color(ColorReset))
		for _, p := range allowedPrompts {
			if pMap, ok := p.(map[string]interface{}); ok {
				tool, _ := pMap["tool"].(string)
				prompt, _ := pMap["prompt"].(string)
				fmt.Fprintf(r.out, "  • %s: %s\n", tool, prompt)
			}
		}
	}

	fmt.Fprintf(r.out, "%s%s%s\n", r.color(ColorGreen), strings.Repeat("═", 60), r.color(ColorReset))
}

// Error prints an error message.
func (r *Renderer) Error(err error, context string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()

	fmt.Fprintf(r.out, "\n%s[Error: %s]%s %v\n", r.color(ColorRed), context, r.color(ColorReset), err)
}

// TurnSummary prints a summary of the completed turn.
func (r *Renderer) TurnSummary(turnNumber int, success bool, durationMs int64, costUSD float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()

	status := "✓"
	colorCode := ColorGreen
	if !success {
		status = "✗"
		colorCode = ColorRed
	}

	fmt.Fprintf(r.out, "\n%s%s Turn %d complete (%.1fs, $%.4f)%s\n",
		r.color(colorCode), status, turnNumber, float64(durationMs)/1000, costUSD, r.color(ColorReset))
}

// closeToolOutput closes any open tool output block.
func (r *Renderer) closeToolOutput() {
	if r.inToolOutput {
		fmt.Fprintln(r.out)
		r.inToolOutput = false
	}
}

// formatToolInput formats tool input for display.
func (r *Renderer) formatToolInput(name string, input map[string]interface{}) string {
	switch name {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return truncatePath(path, 80)
		}
	case "Write":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("→ %s", truncatePath(path, 70))
		}
	case "Edit":
		if path, ok := input["file_path"].(string); ok {
			return fmt.Sprintf("→ %s", truncatePath(path, 70))
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return Truncate(cmd, 80)
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return pattern
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return Truncate(pattern, 60)
		}
	case "Task":
		if desc, ok := input["description"].(string); ok {
			return desc
		}
	case "AskUserQuestion", "ExitPlanMode":
		// These are handled specially
		return ""
	default:
		// For unknown tools, show a JSON summary
		if len(input) > 0 {
			data, _ := json.Marshal(input)
			return Truncate(string(data), 100)
		}
	}
	return ""
}

// formatContent formats tool result content for display.
func (r *Renderer) formatContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		// Array of content blocks
		var parts []string
		for _, block := range c {
			if bMap, ok := block.(map[string]interface{}); ok {
				if text, ok := bMap["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		data, _ := json.Marshal(content)
		return string(data)
	}
}

// Truncate truncates a string to the given max length.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// truncatePath truncates a path, keeping the end visible.
func truncatePath(path string, max int) string {
	if len(path) <= max {
		return path
	}
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		return Truncate(path, max)
	}
	// Keep last 2 parts
	suffix := strings.Join(parts[len(parts)-2:], "/")
	if len(suffix)+4 >= max {
		return Truncate(path, max)
	}
	return ".../" + suffix
}
