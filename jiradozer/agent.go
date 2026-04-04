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
func RunPlanAgent(ctx context.Context, model agent.AgentModel, issue *tracker.Issue, cfg StepConfig, workDir string, budget float64, feedback string, logger *slog.Logger) (string, error) {
	provider, err := agent.NewProviderForModel(model)
	if err != nil {
		return "", fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	prompt := buildPlanPrompt(issue, feedback)

	logger.Info("running plan agent", "model", model.ID, "provider", model.Provider)

	var opts []agent.ExecuteOption
	opts = append(opts,
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderPermissionMode("plan"),
		agent.WithProviderModel(model.ID),
	)
	if cfg.SystemPrompt != "" {
		opts = append(opts, agent.WithProviderSystemPrompt(cfg.SystemPrompt))
	}

	result, err := provider.Execute(ctx, prompt, nil, opts...)
	if err != nil {
		return "", fmt.Errorf("plan agent execution: %w", err)
	}
	if !result.Success {
		errMsg := "plan agent failed"
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		return "", fmt.Errorf("%s", errMsg)
	}

	logger.Info("plan agent completed",
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"cost_usd", result.Usage.CostUSD,
	)
	return result.Text, nil
}

// RunBuildAgent runs the agent in execution/bypass mode to implement the plan.
func RunBuildAgent(ctx context.Context, model agent.AgentModel, issue *tracker.Issue, plan string, cfg StepConfig, workDir string, budget float64, feedback string, logger *slog.Logger) (string, error) {
	provider, err := agent.NewProviderForModel(model)
	if err != nil {
		return "", fmt.Errorf("create provider: %w", err)
	}
	defer provider.Close()

	prompt := buildExecutionPrompt(issue, plan, feedback)

	logger.Info("running build agent", "model", model.ID, "provider", model.Provider)

	var opts []agent.ExecuteOption
	opts = append(opts,
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderPermissionMode("bypass"),
		agent.WithProviderModel(model.ID),
	)
	if cfg.SystemPrompt != "" {
		opts = append(opts, agent.WithProviderSystemPrompt(cfg.SystemPrompt))
	}

	result, err := provider.Execute(ctx, prompt, nil, opts...)
	if err != nil {
		return "", fmt.Errorf("build agent execution: %w", err)
	}
	if !result.Success {
		errMsg := "build agent failed"
		if result.Error != nil {
			errMsg = result.Error.Error()
		}
		return "", fmt.Errorf("%s", errMsg)
	}

	logger.Info("build agent completed",
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
	fmt.Fprintf(&b, "\nApproved Plan:\n%s\n", plan)
	b.WriteString("\nImplement the changes described in the approved plan above.")
	if feedback != "" {
		fmt.Fprintf(&b, "\n\nAdditional feedback to incorporate:\n%s", feedback)
	}
	return b.String()
}
