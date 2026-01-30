package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
)

// OutputMode controls verbosity level.
type OutputMode int

const (
	// OutputMinimal shows phase changes and final cost only.
	OutputMinimal OutputMode = iota
	// OutputNormal adds agent activity and iterations.
	OutputNormal
	// OutputVerbose adds tool activity and streaming progress.
	OutputVerbose
)

// ConsoleReporter writes progress to the console.
type ConsoleReporter struct {
	startTime    time.Time
	lastActivity time.Time
	out          io.Writer
	currentPhase checkpoint.Phase
	currentAgent agent.AgentRole
	mode         OutputMode
	toolsActive  int
	mu           sync.Mutex
}

// ConsoleOption configures the console reporter.
type ConsoleOption func(*ConsoleReporter)

// WithOutput sets the output writer.
func WithOutput(w io.Writer) ConsoleOption {
	return func(r *ConsoleReporter) { r.out = w }
}

// WithMode sets the output verbosity.
func WithMode(mode OutputMode) ConsoleOption {
	return func(r *ConsoleReporter) { r.mode = mode }
}

// NewConsoleReporter creates a new console progress reporter.
func NewConsoleReporter(opts ...ConsoleOption) *ConsoleReporter {
	r := &ConsoleReporter{
		out:       os.Stdout,
		mode:      OutputNormal,
		startTime: time.Now(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Event handles a progress event.
func (r *ConsoleReporter) Event(event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastActivity = event.Timestamp()

	switch e := event.(type) {
	case PhaseChangeEvent:
		r.handlePhaseChange(e)
	case AgentStartEvent:
		r.handleAgentStart(e)
	case AgentCompleteEvent:
		r.handleAgentComplete(e)
	case AgentThinkingEvent:
		r.handleAgentThinking(e)
	case ToolActivityEvent:
		r.handleToolActivity(e)
	case IterationEvent:
		r.handleIteration(e)
	case CostUpdateEvent:
		r.handleCostUpdate(e)
	case ErrorEvent:
		r.handleError(e)
	}
}

// Close closes the reporter.
func (r *ConsoleReporter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	elapsed := time.Since(r.startTime)
	fmt.Fprintf(r.out, "\nTotal time: %s\n", formatDuration(elapsed))
}

func (r *ConsoleReporter) handlePhaseChange(e PhaseChangeEvent) {
	r.currentPhase = e.To

	symbol := r.phaseSymbol(e.To)
	desc := r.phaseDescription(e.To)

	if e.Iteration > 1 {
		fmt.Fprintf(r.out, "\n%s %s (iteration %d)\n", symbol, desc, e.Iteration)
	} else {
		fmt.Fprintf(r.out, "\n%s %s\n", symbol, desc)
	}
}

func (r *ConsoleReporter) handleAgentStart(e AgentStartEvent) {
	if r.mode < OutputNormal {
		return
	}

	r.currentAgent = e.Role
	role := r.formatRole(e.Role)

	fmt.Fprintf(r.out, "  %s %s...\n", role, e.TaskDesc)
}

func (r *ConsoleReporter) handleAgentComplete(e AgentCompleteEvent) {
	if r.mode < OutputNormal {
		return
	}

	status := "done"
	if !e.Success {
		status = "failed"
	}

	fmt.Fprintf(r.out, "  %s %s (%.1fs, $%.4f)\n",
		r.formatRole(e.Role), status, e.Duration.Seconds(), e.CostUSD)
}

func (r *ConsoleReporter) handleAgentThinking(e AgentThinkingEvent) {
	r.currentAgent = e.Role
	role := r.formatRole(e.Role)

	fmt.Fprintf(r.out, "\n%s %s...\n", role, e.Message)
}

func (r *ConsoleReporter) handleToolActivity(e ToolActivityEvent) {
	if r.mode < OutputVerbose {
		return
	}

	if e.Started {
		r.toolsActive++
		fmt.Fprintf(r.out, "    [%s] starting...\n", e.ToolName)
	} else {
		r.toolsActive--
		fmt.Fprintf(r.out, "    [%s] %s\n", e.ToolName, r.formatToolInput(e.ToolName, e.Input))
	}
}

func (r *ConsoleReporter) handleIteration(e IterationEvent) {
	fmt.Fprintf(r.out, "\n>>> Iteration %d/%d: %s\n", e.Number, e.MaxIterations, e.Reason)
}

func (r *ConsoleReporter) handleCostUpdate(e CostUpdateEvent) {
	if r.mode < OutputVerbose {
		return
	}

	elapsed := time.Since(r.startTime)
	pct := float64(0)
	if e.BudgetUSD > 0 {
		pct = (e.TotalCostUSD / e.BudgetUSD) * 100
	}

	fmt.Fprintf(r.out, "  [%s] Cost: $%.4f / $%.2f (%.1f%%)\n",
		formatDuration(elapsed), e.TotalCostUSD, e.BudgetUSD, pct)
}

func (r *ConsoleReporter) handleError(e ErrorEvent) {
	fmt.Fprintf(r.out, "  [ERROR] %s: %v\n", e.Context, e.Err)
}

// Helper methods

func (r *ConsoleReporter) phaseSymbol(phase checkpoint.Phase) string {
	switch phase {
	case checkpoint.PhaseDesigning:
		return "[DESIGN]"
	case checkpoint.PhaseBuilding:
		return "[BUILD]"
	case checkpoint.PhaseReviewing:
		return "[REVIEW]"
	case checkpoint.PhaseCompleted:
		return "[DONE]"
	case checkpoint.PhaseFailed:
		return "[FAILED]"
	default:
		return "[...]"
	}
}

func (r *ConsoleReporter) phaseDescription(phase checkpoint.Phase) string {
	switch phase {
	case checkpoint.PhaseDesigning:
		return "Designing solution"
	case checkpoint.PhaseBuilding:
		return "Implementing changes"
	case checkpoint.PhaseReviewing:
		return "Reviewing implementation"
	case checkpoint.PhaseCompleted:
		return "Mission completed"
	case checkpoint.PhaseFailed:
		return "Mission failed"
	default:
		return "Processing"
	}
}

func (r *ConsoleReporter) formatRole(role agent.AgentRole) string {
	switch role {
	case agent.RoleDesigner:
		return "[Designer]"
	case agent.RoleBuilder:
		return "[Builder]"
	case agent.RoleReviewer:
		return "[Reviewer]"
	case agent.RolePlanner:
		return "[Planner]"
	case agent.RoleOrchestrator:
		return "[Orchestrator]"
	default:
		return fmt.Sprintf("[%s]", role)
	}
}

func (r *ConsoleReporter) formatToolInput(name string, input map[string]interface{}) string {
	switch name {
	case "Write", "Edit":
		if path, ok := input["file_path"].(string); ok {
			return truncatePath(path, 50)
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return truncate(cmd, 60)
		}
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return truncatePath(path, 50)
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return pattern
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return truncate(pattern, 40)
		}
	}
	return "completed"
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func truncatePath(path string, max int) string {
	if len(path) <= max {
		return path
	}
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		return truncate(path, max)
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}
