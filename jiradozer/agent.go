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
func RunStepAgent(ctx context.Context, stepName string, data PromptData, cfg StepConfig, workDir string, feedback string, resumeSessionID string, logger *slog.Logger) (string, string, error) {
	prompt, err := resolvePromptForExecution(cfg.Prompt, DefaultPromptForStep(stepName), data, feedback, resumeSessionID)
	if err != nil {
		return "", "", fmt.Errorf("render %s prompt: %w", stepName, err)
	}
	return runAgent(ctx, stepName, prompt, cfg, workDir, resumeSessionID, logger)
}

// RunCommand runs a shell command template for the given workflow step.
// The commandTmpl is rendered with data, then executed via sh -c in workDir.
// Returns the combined stdout+stderr output and any error.
func RunCommand(ctx context.Context, stepName string, data PromptData, commandTmpl string, workDir string, logger *slog.Logger) (string, error) {
	rendered, err := renderPrompt(commandTmpl, data)
	if err != nil {
		return "", fmt.Errorf("render %s command: %w", stepName, err)
	}

	logger.Info("running command", "step", stepName, "command", truncate(rendered, 200), "work_dir", workDir)

	cmd := exec.CommandContext(ctx, "sh", "-c", rendered)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		return output, fmt.Errorf("command failed: %w", err)
	}

	logger.Info("command completed", "step", stepName, "output", truncate(output, 200))
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

// logEventHandler logs agent events at debug level for verbose output.
// It also tracks plan file writes for the plan step.
type logEventHandler struct {
	logger       *slog.Logger
	step         string
	planFilePath string // set when Claude writes a plan .md file before ExitPlanMode
	lastWriteMD  string // tracks the most recent .md Write for ExitPlanMode correlation
}

func (h *logEventHandler) OnText(text string) {
	h.logger.Debug("agent text", "step", h.step, "text", text)
}

func (h *logEventHandler) OnThinking(thinking string) {
	h.logger.Debug("agent thinking", "step", h.step, "thinking", thinking)
}

func (h *logEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	h.logger.Debug("agent tool start", "step", h.step, "tool", name, "id", id)
}

func (h *logEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	h.logger.Debug("agent tool complete", "step", h.step, "tool", name, "id", id, "is_error", isError)
	// Track .md file writes — the last one before ExitPlanMode is the plan file.
	// The plan file location varies: .claude/plans/, docs/plans/, etc. depending on
	// user settings, so we track any .md Write and confirm when ExitPlanMode fires.
	if name == "Write" && !isError {
		if filePath, ok := input["file_path"].(string); ok {
			if strings.HasSuffix(filePath, ".md") {
				h.lastWriteMD = filePath
				h.logger.Debug("tracked md write", "step", h.step, "path", filePath)
			}
		}
	}
	if name == "ExitPlanMode" && h.lastWriteMD != "" {
		h.planFilePath = h.lastWriteMD
		h.logger.Debug("plan file confirmed", "step", h.step, "path", h.planFilePath)
	}
}

func (h *logEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.logger.Debug("agent turn complete", "step", h.step, "turn", turnNumber, "success", success, "duration_ms", durationMs, "cost_usd", costUSD)
}

func (h *logEventHandler) OnError(err error, context string) {
	h.logger.Debug("agent error", "step", h.step, "error", err, "context", context)
}

// runAgent runs an agent with the given prompt and step configuration.
func runAgent(ctx context.Context, stepName, prompt string, cfg StepConfig, workDir string, resumeSessionID string, logger *slog.Logger) (string, string, error) {
	model, ok := agent.ModelByID(cfg.Model)
	if !ok {
		return "", "", fmt.Errorf("unknown model: %q", cfg.Model)
	}
	provider, err := agent.NewProviderForModel(model)
	if err != nil {
		return "", "", fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	logger.Info("running agent",
		"step", stepName,
		"mode", cfg.PermissionMode,
		"model", cfg.Model,
		"work_dir", workDir,
		"resume", resumeSessionID != "",
	)
	logger.Debug("agent prompt", "step", stepName, "prompt", prompt)
	logger.Info("agent prompt", "step", stepName, "prompt", truncate(prompt, 200))

	handler := &logEventHandler{logger: logger, step: stepName}
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
		"mode", cfg.PermissionMode,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cost_usd", result.Usage.CostUSD,
	)
	if result.Text != "" {
		logger.Info("agent response", "step", stepName, "response", truncate(result.Text, 100))
	}

	output := resolveOutput(result.Text, handler, logger)
	return output, result.SessionID, nil
}

// resolveOutput returns the plan file content if one was detected, otherwise the agent's text output.
func resolveOutput(agentText string, handler *logEventHandler, logger *slog.Logger) string {
	if handler.planFilePath == "" {
		return agentText
	}
	planContent, err := os.ReadFile(handler.planFilePath)
	if err != nil {
		logger.Warn("could not read plan file, using agent text output", "path", handler.planFilePath, "error", err)
		return agentText
	}
	logger.Info("using plan file content", "path", handler.planFilePath)
	return string(planContent)
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
		logger.Info("persisted plan to disk", "path", planPath)
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
