// Package render provides ANSI-colored terminal rendering for Claude and agent sessions.
//
// It supports configurable verbosity levels (quiet/normal/verbose/debug),
// color control (auto/always/never), and covers the full Claude SDK event set
// including tools, tasks, hooks, rate limits, and more.
package render

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// ANSI color codes — kept exported for backward compatibility.
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

// Renderer handles terminal output with ANSI colors and verbosity control.
type Renderer struct {
	out          io.Writer
	eventHandler EventHandler
	commands     map[string]string
	outputs      map[string]*strings.Builder
	lastToolName string
	lastToolID   string
	textBuffer   strings.Builder
	mu           sync.Mutex
	verbosity    Verbosity
	palette      Palette
	inToolOutput bool
	inReasoning  bool
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithVerbosity sets the verbosity level.
func WithVerbosity(v Verbosity) Option {
	return func(r *Renderer) { r.verbosity = v }
}

// WithColorMode sets the color output mode.
func WithColorMode(m ColorMode) Option {
	return func(r *Renderer) { r.palette = resolvePalette(m, r.out) }
}

// WithEventHandler sets the semantic event handler.
func WithEventHandler(h EventHandler) Option {
	return func(r *Renderer) { r.eventHandler = h }
}

// New creates a Renderer with functional options.
// Defaults: VerbosityNormal, ColorAuto, no event handler.
func New(out io.Writer, opts ...Option) *Renderer {
	r := &Renderer{
		out:       out,
		palette:   resolvePalette(ColorAuto, out),
		verbosity: VerbosityNormal,
		commands:  make(map[string]string),
		outputs:   make(map[string]*strings.Builder),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ---------------------------------------------------------------------------
// Backward-compatible constructors
// ---------------------------------------------------------------------------

// boolToVerbosity maps the legacy verbose bool to a Verbosity level.
func boolToVerbosity(verbose bool) Verbosity {
	if verbose {
		return VerbosityVerbose
	}
	return VerbosityNormal
}

// NewRenderer creates a renderer writing to the given output.
// If verbose is false, uses VerbosityNormal; if true, VerbosityVerbose.
// Colors are automatically disabled if output is not a terminal.
func NewRenderer(out io.Writer, verbose bool) *Renderer {
	return New(out, WithVerbosity(boolToVerbosity(verbose)))
}

// NewRendererWithOptions creates a renderer with explicit color control.
func NewRendererWithOptions(out io.Writer, verbose, noColor bool) *Renderer {
	mode := ColorAuto
	if noColor {
		mode = ColorNever
	}
	return New(out, WithVerbosity(boolToVerbosity(verbose)), WithColorMode(mode))
}

// NewRendererWithEvents creates a renderer that also emits semantic events.
func NewRendererWithEvents(out io.Writer, verbose bool, handler EventHandler) *Renderer {
	return New(out, WithVerbosity(boolToVerbosity(verbose)), WithEventHandler(handler))
}

// SetEventHandler sets or updates the event handler. Pass nil to disable.
func (r *Renderer) SetEventHandler(handler EventHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventHandler = handler
}

// IsTerminal checks if the writer is backed by a terminal file descriptor.
func IsTerminal(w io.Writer) bool {
	return isTerminalWriter(w)
}

// color returns the color code if colors are enabled, empty string otherwise.
func (r *Renderer) color(c string) string {
	return r.palette.colorFor(c)
}

// ---------------------------------------------------------------------------
// Text and Thinking
// ---------------------------------------------------------------------------

// flushText emits accumulated text as a semantic event.
// Must be called with mutex held.
func (r *Renderer) flushText() {
	if r.eventHandler != nil && r.textBuffer.Len() > 0 {
		r.eventHandler.OnText(r.textBuffer.String())
		r.textBuffer.Reset()
	}
}

// Text prints streaming text output.
func (r *Renderer) Text(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity >= VerbosityNormal {
		r.closeToolOutput()
		r.endReasoning()
		fmt.Fprint(r.out, text)
	}

	// Event handler always receives events regardless of verbosity.
	if r.eventHandler != nil {
		r.textBuffer.WriteString(text)
		if strings.Contains(text, "\n") || r.textBuffer.Len() > 80 {
			r.flushText()
		}
	}
}

// Thinking prints thinking/reasoning output in dim italic.
func (r *Renderer) Thinking(thinking string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Always flush buffered text before a thinking event so the event handler
	// never receives OnThinking while stale text remains in the buffer.
	r.flushText()

	if r.verbosity >= VerbosityVerbose {
		r.closeToolOutput()
		fmt.Fprintf(r.out, "%s%s%s%s", r.color(ColorDim), r.color(ColorItalic), thinking, r.color(ColorReset))
		r.inReasoning = true
	}

	// Event handler always receives events regardless of verbosity.
	if r.eventHandler != nil {
		r.eventHandler.OnThinking(thinking)
	}
}

// Reasoning is an alias for Thinking, matching the Codex renderer API.
func (r *Renderer) Reasoning(text string) {
	r.Thinking(text)
}

// endReasoning adds a newline when transitioning from reasoning to text.
// Must be called with mutex held.
func (r *Renderer) endReasoning() {
	if r.inReasoning {
		fmt.Fprintln(r.out)
		r.inReasoning = false
	}
}

// ---------------------------------------------------------------------------
// Tool lifecycle (Claude-style)
// ---------------------------------------------------------------------------

// ToolStart prints the start of a tool invocation.
func (r *Renderer) ToolStart(name, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastToolName = name
	r.lastToolID = id

	// Always flush buffered text before a tool event so the event handler
	// never receives OnToolStart while stale text remains in the buffer.
	r.flushText()

	if r.verbosity >= VerbosityNormal {
		r.closeToolOutput()
		r.endReasoning()

		// Don't print for interactive tools
		if name != "AskUserQuestion" && name != "ExitPlanMode" {
			fmt.Fprintf(r.out, "\n%s[%s]%s ", r.color(ColorCyan), name, r.color(ColorReset))
			r.inToolOutput = true
		}
	}

	// Event handler always receives events regardless of verbosity,
	// including interactive tools, for paired start/complete tracking.
	if r.eventHandler != nil {
		r.eventHandler.OnToolStart(name, id, nil)
	}
}

// ToolProgress prints streaming tool input chunks.
func (r *Renderer) ToolProgress(chunk string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity < VerbosityDebug {
		return
	}

	fmt.Fprintf(r.out, "%s%s%s", r.color(ColorYellow), chunk, r.color(ColorReset))
}

// ToolComplete prints the completed tool input.
func (r *Renderer) ToolComplete(name string, input map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Don't print for interactive tools, but still notify the event handler.
	if name == "AskUserQuestion" || name == "ExitPlanMode" {
		r.inToolOutput = false
		if r.eventHandler != nil {
			r.eventHandler.OnToolComplete(name, r.lastToolID, input, nil, false)
		}
		r.lastToolID = ""
		return
	}

	if r.verbosity >= VerbosityNormal {
		summary := formatToolInput(name, input)
		if summary != "" {
			fmt.Fprintf(r.out, "%s%s%s\n", r.color(ColorYellow), summary, r.color(ColorReset))
		} else {
			fmt.Fprintln(r.out)
		}
	}

	r.inToolOutput = false

	if r.eventHandler != nil {
		r.eventHandler.OnToolComplete(name, r.lastToolID, input, nil, false)
	}
	r.lastToolID = ""
}

// ToolResult prints the result of a tool execution.
func (r *Renderer) ToolResult(content interface{}, isError bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Errors always shown (even in Quiet); success results at Verbose+
	if !isError && r.verbosity < VerbosityVerbose {
		return
	}

	contentStr := formatContent(content)
	if contentStr == "" {
		return
	}

	// Skip internal AskUserQuestion/ExitPlanMode error results
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

	// Truncate long output unless debug
	lines := strings.Split(contentStr, "\n")
	if r.verbosity < VerbosityDebug && len(lines) > 10 {
		contentStr = strings.Join(lines[:10], "\n") + fmt.Sprintf("\n  ... (%d more lines)", len(lines)-10)
	}

	indented := strings.ReplaceAll(contentStr, "\n", "\n    ")
	fmt.Fprintf(r.out, "%s%s%s%s\n", r.color(colorCode), prefix, indented, r.color(ColorReset))
}

// ToolExecutionProgress prints elapsed time for a running tool.
func (r *Renderer) ToolExecutionProgress(name, id string, elapsedSec float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity < VerbosityVerbose {
		return
	}

	fmt.Fprintf(r.out, "%s[%s]%s running %.0fs...\r",
		r.color(ColorGray), name, r.color(ColorReset), elapsedSec)
}

// ---------------------------------------------------------------------------
// Command lifecycle (Codex-style)
// ---------------------------------------------------------------------------

// CommandStart records the start of a command execution.
func (r *Renderer) CommandStart(callID, command string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.commands[callID] = command
}

// CommandOutput accumulates streaming command output for a given call.
func (r *Renderer) CommandOutput(callID, chunk string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	b, ok := r.outputs[callID]
	if !ok {
		b = &strings.Builder{}
		r.outputs[callID] = b
	}
	b.WriteString(chunk)
}

// HasOutput reports whether any command output has been accumulated for callID.
func (r *Renderer) HasOutput(callID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.outputs[callID]
	return ok
}

// CommandEnd prints the completion of a command execution.
// In verbose mode, prints one line per tool: [command] ✓ or [command] ✗ exit N
func (r *Renderer) CommandEnd(callID string, exitCode int, durationMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	command, ok := r.commands[callID]
	if !ok {
		return
	}
	delete(r.commands, callID)
	delete(r.outputs, callID)

	if r.verbosity < VerbosityVerbose {
		return
	}

	durationStr := ""
	if durationMs > 0 {
		durationStr = fmt.Sprintf(" %.2fs", float64(durationMs)/1000)
	}

	if exitCode == 0 {
		fmt.Fprintf(r.out, "%s[%s]%s %s✓%s%s\n",
			r.color(ColorCyan), TruncateForDisplay(command, 60), r.color(ColorReset),
			r.color(ColorGreen), durationStr, r.color(ColorReset))
	} else {
		fmt.Fprintf(r.out, "%s[%s]%s %s✗ exit %d%s%s\n",
			r.color(ColorCyan), TruncateForDisplay(command, 60), r.color(ColorReset),
			r.color(ColorRed), exitCode, durationStr, r.color(ColorReset))
	}
}

// ---------------------------------------------------------------------------
// Questions
// ---------------------------------------------------------------------------

// Question prints a question prompt with simple string options.
func (r *Renderer) Question(question string, options []string) {
	opts := make([]QuestionOption, len(options))
	for i, o := range options {
		opts[i] = QuestionOption{Label: o}
	}
	r.QuestionWithOptions(question, "", opts)
}

// printQuestionHeader prints the bracketed header line for question prompts.
// Must be called with mutex held.
func (r *Renderer) printQuestionHeader(question, header string) {
	if header != "" {
		fmt.Fprintf(r.out, "\n%s[%s]%s %s\n", r.color(ColorMagenta), header, r.color(ColorReset), question)
	} else {
		fmt.Fprintf(r.out, "\n%s[Question]%s %s\n", r.color(ColorMagenta), r.color(ColorReset), question)
	}
}

// QuestionWithOptions prints a question prompt with labeled options.
func (r *Renderer) QuestionWithOptions(question, header string, options []QuestionOption) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()
	r.printQuestionHeader(question, header)

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

// QuestionAutoAnswer renders a question with all options, highlighting the auto-selected answer.
func (r *Renderer) QuestionAutoAnswer(question, header string, options []QuestionOption, selectedIdx int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()
	r.printQuestionHeader(question, header)

	for i, opt := range options {
		if i == selectedIdx {
			fmt.Fprintf(r.out, "  %s→ %d. %s%s", r.color(ColorGreen), i+1, opt.Label, r.color(ColorReset))
			fmt.Fprintf(r.out, " %s(auto-selected)%s\n", r.color(ColorGray), r.color(ColorReset))
		} else {
			fmt.Fprintf(r.out, "  %s  %d. %s%s\n", r.color(ColorGray), i+1, opt.Label, r.color(ColorReset))
		}
		if opt.Description != "" {
			fmt.Fprintf(r.out, "  %s     %s%s\n", r.color(ColorGray), opt.Description, r.color(ColorReset))
		}
	}
}

// ---------------------------------------------------------------------------
// Turn lifecycle and status
// ---------------------------------------------------------------------------

// Status prints a status message. Shown at Normal+ verbosity.
func (r *Renderer) Status(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flushText()
	r.closeToolOutput()
	if r.verbosity >= VerbosityNormal {
		fmt.Fprintf(r.out, "%s[Status]%s %s\n", r.color(ColorGray), r.color(ColorReset), msg)
	}

	if r.eventHandler != nil {
		r.eventHandler.OnStatus(msg)
	}
}

// SessionInfo prints session metadata (session ID, model).
func (r *Renderer) SessionInfo(sessionID, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity < VerbosityNormal {
		return
	}

	var parts []string
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

// successIcon returns a status icon and color code for success/failure.
func successIcon(success bool) (icon, colorCode string) {
	if success {
		return "✓", ColorGreen
	}
	return "✗", ColorRed
}

// TurnSummary prints a summary of the completed turn.
func (r *Renderer) TurnSummary(turnNumber int, success bool, durationMs int64, costUSD float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flushText()
	r.closeToolOutput()

	icon, colorCode := successIcon(success)
	fmt.Fprintf(r.out, "\n%s%s Turn %d complete (%.1fs, $%.4f)%s\n",
		r.color(colorCode), icon, turnNumber, float64(durationMs)/1000, costUSD, r.color(ColorReset))

	if r.eventHandler != nil {
		r.eventHandler.OnTurnComplete(turnNumber, success, durationMs, costUSD)
	}
}

// TurnCompleteWithTokens prints a turn summary with token counts (Codex-style).
func (r *Renderer) TurnCompleteWithTokens(success bool, durationMs, inputTokens, outputTokens int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flushText()
	r.closeToolOutput()

	fmt.Fprintf(r.out, "\n%s───────────────────────────────────────────────────────%s\n", r.color(ColorDim), r.color(ColorReset))

	icon, colorCode := successIcon(success)
	fmt.Fprintf(r.out, "%s%s Turn complete (%.1fs, %d input / %d output tokens)%s\n",
		r.color(colorCode), icon, float64(durationMs)/1000, inputTokens, outputTokens, r.color(ColorReset))
}

// PlanComplete prints the plan completion summary.
func (r *Renderer) PlanComplete(input map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeToolOutput()

	fmt.Fprintf(r.out, "\n%s%s%s\n", r.color(ColorGreen), strings.Repeat("═", 60), r.color(ColorReset))
	fmt.Fprintf(r.out, "%s[Plan Complete]%s\n", r.color(ColorGreen), r.color(ColorReset))

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

	r.flushText()
	r.closeToolOutput()

	fmt.Fprintf(r.out, "\n%s[Error: %s]%s %v\n", r.color(ColorRed), context, r.color(ColorReset), err)

	if r.eventHandler != nil {
		r.eventHandler.OnError(err, context)
	}
}

// ---------------------------------------------------------------------------
// New event types — tasks, hooks, rate limits, etc.
// ---------------------------------------------------------------------------

// tagged prints a bracketed, colored one-liner if verbosity is at or above minV.
// Must NOT be called with mu held — it acquires the lock itself.
func (r *Renderer) tagged(minV Verbosity, colorCode, label, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity < minV {
		return
	}

	r.closeToolOutput()
	fmt.Fprintf(r.out, "%s[%s]%s %s\n", r.color(colorCode), label, r.color(ColorReset), msg)
}

// TaskStarted prints a task/sub-agent start notification.
func (r *Renderer) TaskStarted(taskID, description string) {
	r.tagged(VerbosityVerbose, ColorBlue, "Task "+taskID, TruncateForDisplay(description, 80))
}

// TaskProgress prints a task progress update.
func (r *Renderer) TaskProgress(taskID, description string) {
	r.tagged(VerbosityVerbose, ColorGray, "Task "+taskID, TruncateForDisplay(description, 80))
}

// TaskNotification prints a task completion notification.
func (r *Renderer) TaskNotification(taskID, status, summary string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity < VerbosityVerbose {
		return
	}

	r.closeToolOutput()
	colorCode := ColorGreen
	icon := "✓"
	if status == "failed" {
		colorCode = ColorRed
		icon = "✗"
	} else if status == "stopped" {
		colorCode = ColorYellow
		icon = "⊘"
	}

	fmt.Fprintf(r.out, "%s%s [Task %s] %s%s %s\n",
		r.color(colorCode), icon, taskID, status, r.color(ColorReset),
		TruncateForDisplay(summary, 70))
}

// HookLifecycle prints a hook execution event.
func (r *Renderer) HookLifecycle(phase, hookName string) {
	r.tagged(VerbosityVerbose, ColorGray, "Hook: "+hookName, phase)
}

// RateLimit prints a rate limit notification.
func (r *Renderer) RateLimit(status string, utilization *float64) {
	msg := status
	if utilization != nil {
		msg = fmt.Sprintf("%s (%.0f%% utilized)", status, *utilization*100)
	}
	r.tagged(VerbosityNormal, ColorYellow, "Rate Limit", msg)
}

// APIRetry prints an API retry notification.
func (r *Renderer) APIRetry(attempt, maxRetries int, errorMsg string) {
	r.tagged(VerbosityVerbose, ColorYellow, fmt.Sprintf("API Retry %d/%d", attempt, maxRetries), errorMsg)
}

// CompactBoundary prints a conversation compaction event.
func (r *Renderer) CompactBoundary(trigger string) {
	r.tagged(VerbosityVerbose, ColorGray, "Compact", trigger)
}

// PostTurnSummary prints a background post-turn summary.
func (r *Renderer) PostTurnSummary(title, description string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity < VerbosityVerbose {
		return
	}

	r.closeToolOutput()
	fmt.Fprintf(r.out, "%s[Summary]%s %s\n",
		r.color(ColorGray), r.color(ColorReset), title)
	if description != "" {
		fmt.Fprintf(r.out, "  %s%s%s\n", r.color(ColorGray), description, r.color(ColorReset))
	}
}

// AuthStatus prints an authentication status update.
func (r *Renderer) AuthStatus(isAuthenticating bool, output []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.verbosity < VerbosityNormal {
		return
	}

	r.closeToolOutput()
	if isAuthenticating {
		fmt.Fprintf(r.out, "%s[Auth]%s Authenticating...\n", r.color(ColorGray), r.color(ColorReset))
	}
	for _, line := range output {
		fmt.Fprintf(r.out, "%s[Auth]%s %s\n", r.color(ColorGray), r.color(ColorReset), line)
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// closeToolOutput closes any open tool output block.
func (r *Renderer) closeToolOutput() {
	if r.inToolOutput {
		fmt.Fprintln(r.out)
		r.inToolOutput = false
	}
}
