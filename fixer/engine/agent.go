package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/fixer/issue"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

// FixAgentConfig configures a single fix agent.
type FixAgentConfig struct {
	Issue      *issue.Issue
	WTManager  *wt.Manager
	GHRunner   wt.GHRunner
	Logger     *slog.Logger
	Model      string
	BaseBranch string
	SessionDir string
	BudgetUSD  float64
}

// FixAgentResult reports the outcome of a fix agent.
type FixAgentResult struct {
	Error        error
	Issue        *issue.Issue
	Branch       string
	WorktreePath string
	PRURL        string
	FilesChanged []string
	AgentCost    float64
	PRNumber     int
	Success      bool
}

// RunFixAgent runs a single fix agent: creates worktree, runs Claude session, creates PR.
func RunFixAgent(ctx context.Context, config FixAgentConfig) *FixAgentResult {
	result := &FixAgentResult{Issue: config.Issue}
	log := config.Logger
	if log == nil {
		log = slog.Default()
	}

	// Generate branch name from issue
	branchName := fixBranchName(config.Issue)
	result.Branch = branchName

	log.Info("creating worktree",
		"branch", branchName,
		"base", config.BaseBranch,
		"issue", config.Issue.ID,
	)

	// Create worktree
	wtPath, err := config.WTManager.New(ctx, branchName, config.BaseBranch, config.Issue.Summary)
	if err != nil {
		result.Error = fmt.Errorf("create worktree: %w", err)
		return result
	}
	result.WorktreePath = wtPath

	log.Info("worktree created",
		"path", wtPath,
	)

	// Build prompt
	prompt := buildFixPrompt(config.Issue, config.BaseBranch)

	// Create ephemeral session
	sess := agent.NewEphemeralSession(agent.AgentConfig{
		Logger:     log,
		Role:       agent.RoleBuilder,
		Model:      config.Model,
		WorkDir:    wtPath,
		SessionDir: config.SessionDir,
		BudgetUSD:  config.BudgetUSD,
	}, fmt.Sprintf("fixer-%s", config.Issue.ID))

	log.Info("running fix agent",
		"issue", config.Issue.ID,
		"model", config.Model,
	)

	// Execute the fix
	agentResult, execResult, _, err := sess.ExecuteWithFiles(ctx, prompt)
	result.AgentCost = sess.TotalCost()

	if err != nil {
		result.Error = fmt.Errorf("agent execution: %w", err)
		return result
	}

	if agentResult == nil || !agentResult.Success {
		errMsg := "agent reported failure"
		if agentResult != nil && agentResult.Text != "" {
			errMsg = agentResult.Text
		}
		result.Error = fmt.Errorf("agent failed: %s", errMsg)
		return result
	}

	if execResult != nil {
		result.FilesChanged = append(result.FilesChanged, execResult.FilesCreated...)
		result.FilesChanged = append(result.FilesChanged, execResult.FilesModified...)
	}

	if len(result.FilesChanged) == 0 {
		result.Error = fmt.Errorf("agent made no file changes")
		return result
	}

	log.Info("agent completed, creating PR",
		"filesChanged", len(result.FilesChanged),
		"cost", fmt.Sprintf("$%.4f", result.AgentCost),
	)

	// Create PR
	title := fmt.Sprintf("fix(%s): %s", config.Issue.Category, truncate(config.Issue.Summary, 60))
	body := fmt.Sprintf("Automated fix for CI failure.\n\n**Category:** %s\n**Signature:** `%s`\n**File:** %s\n\n%s",
		config.Issue.Category, config.Issue.Signature, config.Issue.File, config.Issue.Details)
	if len(body) > 4000 {
		body = body[:4000] + "\n... (truncated)"
	}

	prInfo, err := wt.CreatePR(ctx, config.GHRunner, title, body, config.BaseBranch, false, wtPath)
	if err != nil {
		result.Error = fmt.Errorf("create PR: %w", err)
		return result
	}

	result.PRURL = prInfo.URL
	result.PRNumber = prInfo.Number
	result.Success = true

	log.Info("fix PR created",
		"pr", prInfo.URL,
		"number", prInfo.Number,
	)

	return result
}

// fixBranchName generates a branch name for a fix issue.
func fixBranchName(iss *issue.Issue) string {
	// fix/<category>/<short-id>
	cat := strings.ReplaceAll(string(iss.Category), "/", "-")
	return fmt.Sprintf("fix/%s/%s", cat, iss.ID)
}

// truncate shortens a string with ellipsis if too long.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// recordFixAttempt updates the tracker with the fix agent result.
func recordFixAttempt(tracker *issue.Tracker, result *FixAgentResult) {
	now := time.Now()
	attempt := issue.FixAttempt{
		Branch:    result.Branch,
		PRURL:     result.PRURL,
		PRNumber:  result.PRNumber,
		AgentCost: result.AgentCost,
		StartedAt: now,
	}

	if result.Success {
		attempt.Outcome = "pr_created"
		attempt.PRState = "OPEN"
		tracker.UpdateStatus(result.Issue.Signature, issue.StatusFixPending)
	} else {
		attempt.Outcome = "failed"
		if result.Error != nil {
			attempt.Error = result.Error.Error()
		}
	}

	completed := now
	attempt.CompletedAt = &completed
	tracker.AddFixAttempt(result.Issue.Signature, attempt)
}
