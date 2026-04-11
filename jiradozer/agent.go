package jiradozer

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// PromptData is the template context for rendering step prompts.
type PromptData struct {
	Identifier  string // e.g. "ENG-123"
	Title       string
	Description string // empty string if issue.Description is nil
	URL         string // empty string if issue.URL is nil
	Labels      string // comma-separated
	BaseBranch  string // e.g. "main"
	Plan        string // plan output from the planning step
	BuildOutput string // build output from the build step
}

// Default prompt templates for each step.

const defaultPlanPrompt = `Issue: {{.Identifier}} — {{.Title}}
{{- if .Description}}

Description:
{{.Description}}
{{- end}}
{{- if .URL}}

URL: {{.URL}}
{{- end}}
{{- if .Labels}}
Labels: {{.Labels}}
{{- end}}

Create a detailed implementation plan for this issue. Include: files to modify, approach, testing strategy, and any risks.`

const defaultBuildPrompt = `Issue: {{.Identifier}} — {{.Title}}
{{- if .Description}}

Description:
{{.Description}}
{{- end}}
{{- if .Plan}}

Approved Plan:
{{.Plan}}

Implement the changes described in the approved plan above.
{{- else}}

No plan is available. Implement the changes based on the issue description above.
{{- end}}`

const defaultValidatePrompt = `Issue: {{.Identifier}} — {{.Title}}

Run the project's tests and linters to validate the changes. Fix any failures you find. Report what passed and what you fixed.`

const defaultCreatePRPrompt = `First, check for any uncommitted changes (staged or unstaged, including untracked files).
- If there are uncommitted changes: stage them, commit with a clear message referencing the work done, and push to the remote.
- If there are no uncommitted changes but unpushed commits: push to the remote.

Then, check if a pull request already exists for the current branch against {{.BaseBranch}}.
- If a PR exists: update its description to reflect the current state of the code. Report the PR URL.
- If no PR exists: create one against {{.BaseBranch}} with a clear title and description. Report the PR URL.`

const defaultShipPrompt = `Issue: {{.Identifier}} — {{.Title}}
{{- if .URL}}

Linear: {{.URL}}
{{- end}}

Check if a pull request already exists for the current branch against {{.BaseBranch}}.
- If a PR exists: update its description if needed and ensure it is ready for review. Report the PR URL.
- If no PR exists: create one using gh pr create with "{{.Identifier}}: {{.Title}}" as the title.`

// DefaultPromptForStep returns the built-in default prompt template for a step name.
func DefaultPromptForStep(stepName string) string {
	switch stepName {
	case "plan":
		return defaultPlanPrompt
	case "build":
		return defaultBuildPrompt
	case "validate":
		return defaultValidatePrompt
	case "create_pr":
		return defaultCreatePRPrompt
	case "ship":
		return defaultShipPrompt
	default:
		return ""
	}
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen]) + "..."
	}
	return s
}

// RunStepAgent runs an agent session for the given workflow step.
// On first execution (resumeSessionID == ""), the prompt template is rendered with issue data.
// On follow-up (resumeSessionID != ""), feedback is sent directly to the resumed session.
// If renderer is non-nil, agent events are streamed to the terminal.
func RunStepAgent(ctx context.Context, stepName string, data PromptData, cfg StepConfig, workDir string, feedback string, resumeSessionID string, renderer *render.Renderer, logger *slog.Logger) (string, string, error) {
	prompt, err := resolvePromptForExecution(cfg.Prompt, DefaultPromptForStep(stepName), data, feedback, resumeSessionID)
	if err != nil {
		return "", "", fmt.Errorf("render %s prompt: %w", stepName, err)
	}
	return runAgent(ctx, stepName, prompt, cfg, workDir, resumeSessionID, renderer, logger)
}

// RunCommand runs a shell command template for the given workflow step.
// The commandTmpl is rendered with data, then executed via sh -c in workDir.
// Returns the combined stdout+stderr output and any error.
//
// Security note: command templates are rendered with PromptData fields (Title,
// Description, etc.) which originate from the issue tracker and are
// user-controlled. Interpolating these fields directly into shell commands can
// allow shell injection. Only include tracker fields in command templates when
// the issue source is fully trusted (e.g. an internal tracker with restricted
// write access). Avoid interpolating free-text fields such as Title or
// Description unless you control all issue authors.
func RunCommand(ctx context.Context, stepName string, data PromptData, commandTmpl string, workDir string, logger *slog.Logger) (string, error) {
	rendered, err := renderPrompt(commandTmpl, data)
	if err != nil {
		return "", fmt.Errorf("render %s command: %w", stepName, err)
	}

	logger.Info("running command", "step", stepName, "command", truncate(rendered, 200), "work_dir", workDir)

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", rendered)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		logger.Info("command failed", "step", stepName, "duration", time.Since(start), "error", err)
		return output, fmt.Errorf("command failed: %w", err)
	}

	logger.Info("command completed", "step", stepName, "duration", time.Since(start), "output", truncate(output, 200))
	return output, nil
}

// resolvePromptForExecution determines the prompt to send to the agent.
func resolvePromptForExecution(configPrompt, defaultPrompt string, data PromptData, feedback, resumeSessionID string) (string, error) {
	// Resume: send feedback directly as the prompt.
	if resumeSessionID != "" && feedback != "" {
		return feedback, nil
	}

	// First execution: render template.
	tmplStr := configPrompt
	if tmplStr == "" {
		tmplStr = defaultPrompt
	}
	prompt, err := renderPrompt(tmplStr, data)
	if err != nil {
		return "", err
	}

	// Fallback: no session to resume but have feedback — append it.
	if feedback != "" {
		prompt += "\n\nPrevious feedback to incorporate:\n" + feedback
	}
	return prompt, nil
}

// logEventHandler logs agent events to the log file with high signal-to-noise ratio.
// Text is accumulated and flushed at semantic boundaries. Tool start/complete are
// merged into a single log line with input summary and duration. Tool IDs are omitted.
type logEventHandler struct {
	logger       *slog.Logger
	toolStarts   map[string]time.Time
	step         string
	planFilePath string
	lastWriteMD  string
	textBuf      strings.Builder
}

func newLogEventHandler(logger *slog.Logger, step string) *logEventHandler {
	return &logEventHandler{
		logger:     logger,
		step:       step,
		toolStarts: make(map[string]time.Time),
	}
}

func (h *logEventHandler) OnSessionInit(sessionID string) {
	h.logger.Info("agent session init", "step", h.step, "session_id", sessionID)
}

// flushText logs accumulated text and resets the buffer.
func (h *logEventHandler) flushText() {
	if h.textBuf.Len() > 0 {
		h.logger.Debug("agent text", "step", h.step, "text", truncate(h.textBuf.String(), 200))
		h.textBuf.Reset()
	}
}

func (h *logEventHandler) OnText(text string) {
	h.textBuf.WriteString(text)
	if strings.Contains(text, "\n") || h.textBuf.Len() > 200 {
		h.flushText()
	}
}

func (h *logEventHandler) OnThinking(thinking string) {
	h.flushText()
	h.logger.Debug("agent thinking", "step", h.step, "thinking", truncate(thinking, 200))
}

func (h *logEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	h.flushText()
	h.toolStarts[id] = time.Now()
}

func (h *logEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	attrs := []any{"step", h.step, "tool", name}
	if inputSummary := render.FormatToolInput(name, input); inputSummary != "" {
		attrs = append(attrs, "input", inputSummary)
	}
	if start, ok := h.toolStarts[id]; ok {
		attrs = append(attrs, "duration", time.Since(start).Round(100*time.Millisecond))
		delete(h.toolStarts, id)
	}
	if isError {
		attrs = append(attrs, "error", true)
	}
	h.logger.Debug("tool", attrs...)

	// Track .md file writes — the last one before ExitPlanMode is the plan file.
	if name == "Write" && !isError {
		if filePath, ok := input["file_path"].(string); ok {
			if strings.HasSuffix(filePath, ".md") {
				h.lastWriteMD = filePath
			}
		}
	}
	if name == "ExitPlanMode" && h.lastWriteMD != "" {
		h.planFilePath = h.lastWriteMD
		h.logger.Debug("plan file confirmed", "step", h.step, "path", h.planFilePath)
	}
}

func (h *logEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.flushText()
	h.logger.Debug("turn complete",
		"step", h.step,
		"turn", turnNumber,
		"success", success,
		"duration", fmt.Sprintf("%.1fs", float64(durationMs)/1000),
		"cost", fmt.Sprintf("$%.4f", costUSD),
	)
}

func (h *logEventHandler) OnError(err error, context string) {
	h.flushText()
	clear(h.toolStarts)
	h.logger.Debug("agent error", "step", h.step, "error", err, "context", context)
}

// rendererEventHandler adapts agent.EventHandler to a render.Renderer for
// terminal display.
type rendererEventHandler struct {
	r *render.Renderer
}

func (h *rendererEventHandler) OnText(text string) {
	h.r.Text(text)
}

func (h *rendererEventHandler) OnThinking(thinking string) {
	h.r.Thinking(thinking)
}

func (h *rendererEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	h.r.ToolStart(name, id)
}

func (h *rendererEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	h.r.ToolComplete(name, input)
	if isError {
		h.r.ToolResult(result, isError)
	}
}

func (h *rendererEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.r.TurnSummary(turnNumber, success, durationMs, costUSD)
}

func (h *rendererEventHandler) OnError(err error, ctx string) {
	h.r.Error(err, ctx)
}

// compositeEventHandler fans out events to multiple handlers.
type compositeEventHandler struct {
	handlers []agent.EventHandler
}

func (c *compositeEventHandler) OnText(text string) {
	for _, h := range c.handlers {
		h.OnText(text)
	}
}

func (c *compositeEventHandler) OnThinking(thinking string) {
	for _, h := range c.handlers {
		h.OnThinking(thinking)
	}
}

func (c *compositeEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	for _, h := range c.handlers {
		h.OnToolStart(name, id, input)
	}
}

func (c *compositeEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	for _, h := range c.handlers {
		h.OnToolComplete(name, id, input, result, isError)
	}
}

func (c *compositeEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	for _, h := range c.handlers {
		h.OnTurnComplete(turnNumber, success, durationMs, costUSD)
	}
}

func (c *compositeEventHandler) OnError(err error, ctx string) {
	for _, h := range c.handlers {
		h.OnError(err, ctx)
	}
}

func (c *compositeEventHandler) OnSessionInit(sessionID string) {
	for _, h := range c.handlers {
		if sh, ok := h.(agent.SessionInitHandler); ok {
			sh.OnSessionInit(sessionID)
		}
	}
}

// runAgent runs an agent with the given prompt and step configuration.
// If renderer is non-nil, agent events are rendered to the terminal in addition
// to being logged to the log file.
func runAgent(ctx context.Context, stepName, prompt string, cfg StepConfig, workDir string, resumeSessionID string, renderer *render.Renderer, logger *slog.Logger) (string, string, error) {
	model, ok := agent.ModelByID(cfg.Model)
	if !ok {
		return "", "", fmt.Errorf("unknown model: %q", cfg.Model)
	}
	provider, err := agent.NewProviderForModel(model)
	if err != nil {
		return "", "", fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	logAttrs := []any{
		"step", stepName,
		"mode", cfg.PermissionMode,
		"model", cfg.Model,
		"work_dir", workDir,
	}
	if resumeSessionID != "" {
		logAttrs = append(logAttrs, "resume_session_id", resumeSessionID)
	}
	logger.Info("running agent", logAttrs...)
	logger.Debug("agent prompt", "step", stepName, "prompt", truncate(prompt, 500))

	logHandler := newLogEventHandler(logger, stepName)
	var handler agent.EventHandler = logHandler
	if renderer != nil {
		handler = &compositeEventHandler{handlers: []agent.EventHandler{logHandler, &rendererEventHandler{r: renderer}}}
	}

	var opts []agent.ExecuteOption
	opts = append(opts,
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderPermissionMode(cfg.PermissionMode),
		agent.WithProviderModel(cfg.Model),
		agent.WithProviderKeepUserSettings(),
		agent.WithProviderEventHandler(handler),
	)
	if cfg.SystemPrompt != "" {
		opts = append(opts, agent.WithProviderSystemPrompt(cfg.SystemPrompt))
	}
	if cfg.MaxTurns > 0 {
		opts = append(opts, agent.WithProviderMaxTurns(cfg.MaxTurns))
	}
	if cfg.MaxBudgetUSD > 0 {
		opts = append(opts, agent.WithProviderMaxBudgetUSD(cfg.MaxBudgetUSD))
	}
	if resumeSessionID != "" {
		opts = append(opts, agent.WithProviderResumeSessionID(resumeSessionID))
	}

	result, err := provider.Execute(ctx, prompt, nil, opts...)
	if err != nil {
		return "", "", fmt.Errorf("agent execution: %w", err)
	}
	if !result.Success {
		if result.Error != nil {
			return "", "", result.Error
		}
		return "", "", fmt.Errorf("agent failed")
	}

	logger.Info("agent completed",
		"step", stepName,
		"session_id", result.SessionID,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cost_usd", result.Usage.CostUSD,
		"duration_ms", result.DurationMs,
	)
	if result.Text != "" {
		logger.Debug("agent response", "step", stepName, "response", truncate(result.Text, 100))
	}

	logHandler.flushText()
	return resolveOutput(result.Text, logHandler, logger), result.SessionID, nil
}

// resolveOutput returns the plan file content if one was detected, otherwise the agent's text output.
func resolveOutput(agentText string, handler *logEventHandler, logger *slog.Logger) string {
	return resolveOutputFromPath(agentText, handler.planFilePath, logger)
}

// resolveOutputFromPath reads a plan file if the path is non-empty, falling back to agentText.
func resolveOutputFromPath(agentText, planFilePath string, logger *slog.Logger) string {
	if planFilePath == "" {
		return agentText
	}
	planContent, err := os.ReadFile(planFilePath)
	if err != nil {
		logger.Warn("could not read plan file, using agent text output", "path", planFilePath, "error", err)
		return agentText
	}
	logger.Debug("using plan file content", "path", planFilePath)
	return string(planContent)
}

// JoinRoundOutputs filters empty outputs and joins non-empty ones with a separator.
func JoinRoundOutputs(outputs []string) string {
	var nonEmpty []string
	for _, o := range outputs {
		if strings.TrimSpace(o) != "" {
			nonEmpty = append(nonEmpty, o)
		}
	}
	return strings.Join(nonEmpty, "\n\n---\n\n")
}

func NewPromptData(issue *tracker.Issue, baseBranch string) PromptData {
	d := PromptData{
		Identifier: issue.Identifier,
		Title:      issue.Title,
		Labels:     strings.Join(issue.Labels, ", "),
		BaseBranch: baseBranch,
	}
	if issue.Description != nil {
		d.Description = *issue.Description
	}
	if issue.URL != nil {
		d.URL = *issue.URL
	}
	return d
}

// PlanFilePath returns .jiradozer/plan.md within workDir, enabling plan
// reuse across separate process invocations (e.g. plan step then build step).
func PlanFilePath(workDir string) string {
	return filepath.Join(workDir, ".jiradozer", "plan.md")
}

// PersistPlan writes plan output to PlanFilePath so --run-step=build can load it.
// Empty output is skipped (with a warning log) to avoid overwriting a previously valid plan.
func PersistPlan(workDir, output string, logger *slog.Logger) {
	if strings.TrimSpace(output) == "" {
		logger.Warn("skipping plan persistence — output is empty")
		return
	}
	planPath := PlanFilePath(workDir)
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		logger.Warn("failed to create plan directory", "error", err)
	} else if err := os.WriteFile(planPath, []byte(output), 0o644); err != nil {
		logger.Warn("failed to persist plan", "error", err)
	} else {
		logger.Debug("persisted plan to disk", "path", planPath)
	}
}

// GenerateTitle creates a short title from the first words of a description,
// truncating at word boundaries to fit within 80 characters.
func GenerateTitle(description string) string {
	const maxLen = 80
	words := strings.Fields(description)
	var b strings.Builder
	for _, w := range words {
		if b.Len()+len(w)+1 > maxLen {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	if b.Len() == 0 && description != "" {
		if len(description) > maxLen-3 {
			return description[:maxLen-3] + "..."
		}
		return description
	}
	return b.String()
}

func renderPrompt(tmplStr string, data PromptData) (string, error) {
	t, err := template.New("prompt").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}
