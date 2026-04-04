package jiradozer

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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

const defaultShipPrompt = `Issue: {{.Identifier}} — {{.Title}}
{{- if .URL}}

Linear: {{.URL}}
{{- end}}

Create a pull request for the changes on the current branch against {{.BaseBranch}}. Use the issue title for the PR title (prefixed with the issue identifier) and write a clear PR description.`

// DefaultPromptForStep returns the built-in default prompt template for a step name.
func DefaultPromptForStep(stepName string) string {
	switch stepName {
	case "plan":
		return defaultPlanPrompt
	case "build":
		return defaultBuildPrompt
	case "validate":
		return defaultValidatePrompt
	case "ship":
		return defaultShipPrompt
	default:
		return ""
	}
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
		"resume", resumeSessionID != "",
	)

	var opts []agent.ExecuteOption
	opts = append(opts,
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderPermissionMode(cfg.PermissionMode),
		agent.WithProviderModel(cfg.Model),
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
	return result.Text, result.SessionID, nil
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
