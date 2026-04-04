package jiradozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// RunPlanAgent runs the agent in plan mode and returns the plan text.
func RunPlanAgent(ctx context.Context, model agent.AgentModel, issue *tracker.Issue, cfg StepConfig, workDir string, maxBudgetUSD float64, feedback string, logger *slog.Logger) (string, error) {
	prompt := buildPlanPrompt(issue, feedback)
	return runAgent(ctx, model, "plan", prompt, cfg, workDir, maxBudgetUSD, logger)
}

// RunBuildAgent runs the agent in execution/bypass mode to implement the plan.
func RunBuildAgent(ctx context.Context, model agent.AgentModel, issue *tracker.Issue, plan string, cfg StepConfig, workDir string, maxBudgetUSD float64, feedback string, logger *slog.Logger) (string, error) {
	prompt := buildExecutionPrompt(issue, plan, feedback)
	return runAgent(ctx, model, "bypass", prompt, cfg, workDir, maxBudgetUSD, logger)
}

// runAgent runs an agent with the given permission mode and prompt.
func runAgent(ctx context.Context, model agent.AgentModel, permMode, prompt string, cfg StepConfig, workDir string, maxBudgetUSD float64, logger *slog.Logger) (string, error) {
	provider, err := agent.NewProviderForModel(model)
	if err != nil {
		return "", fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	logger.Info("running agent", "mode", permMode, "model", model.ID, "provider", model.Provider)

	var opts []agent.ExecuteOption
	opts = append(opts,
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderPermissionMode(permMode),
		agent.WithProviderModel(model.ID),
	)
	if cfg.SystemPrompt != "" {
		opts = append(opts, agent.WithProviderSystemPrompt(cfg.SystemPrompt))
	}
	if cfg.MaxTurns > 0 {
		opts = append(opts, agent.WithProviderMaxTurns(cfg.MaxTurns))
	}
	if maxBudgetUSD > 0 {
		opts = append(opts, agent.WithProviderMaxBudgetUSD(maxBudgetUSD))
	}

	result, err := provider.Execute(ctx, prompt, nil, opts...)
	if err != nil {
		return "", fmt.Errorf("%s agent execution: %w", permMode, err)
	}
	if !result.Success {
		if result.Error != nil {
			return "", result.Error
		}
		return "", fmt.Errorf("%s agent failed", permMode)
	}

	logger.Info("agent completed",
		"mode", permMode,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cost_usd", result.Usage.CostUSD,
	)
	return result.Text, nil
}

func buildPlanPrompt(issue *tracker.Issue, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue: %s — %s\n", issue.Identifier, issue.Title)
	if issue.Description != nil && *issue.Description != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", *issue.Description)
	}
	if issue.URL != nil {
		fmt.Fprintf(&b, "\nURL: %s\n", *issue.URL)
	}
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(issue.Labels, ", "))
	}
	b.WriteString("\nCreate a detailed implementation plan for this issue. ")
	b.WriteString("Include: files to modify, approach, testing strategy, and any risks.")
	if feedback != "" {
		fmt.Fprintf(&b, "\n\nPrevious feedback to incorporate:\n%s", feedback)
	}
	return b.String()
}

func buildExecutionPrompt(issue *tracker.Issue, plan, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue: %s — %s\n", issue.Identifier, issue.Title)
	if issue.Description != nil && *issue.Description != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", *issue.Description)
	}
	if plan != "" {
		fmt.Fprintf(&b, "\nApproved Plan:\n%s\n", plan)
		b.WriteString("\nImplement the changes described in the approved plan above.")
	} else {
		b.WriteString("\nNo plan is available. Implement the changes based on the issue description above.")
	}
	if feedback != "" {
		fmt.Fprintf(&b, "\n\nAdditional feedback to incorporate:\n%s", feedback)
	}
	return b.String()
}
