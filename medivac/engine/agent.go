package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/medivac/issue"
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
	RepoDir    string
	BudgetUSD  float64
}

// FixAgentResult reports the outcome of a fix agent.
type FixAgentResult struct {
	Error        error
	Issue        *issue.Issue
	Analysis     *AgentAnalysis
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

	// Detect build system and build prompt
	buildInfo := DetectBuildInfo(config.RepoDir)
	prompt := buildFixPrompt(config.Issue, config.BaseBranch, buildInfo)

	// Create ephemeral session
	sess := agent.NewEphemeralSession(agent.AgentConfig{
		Logger:     log,
		Role:       agent.RoleBuilder,
		Model:      config.Model,
		WorkDir:    wtPath,
		SessionDir: config.SessionDir,
		BudgetUSD:  config.BudgetUSD,
	}, fmt.Sprintf("medivac-%s", config.Issue.ID))

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

	// Parse analysis from agent response (best-effort).
	if agentResult != nil && agentResult.Text != "" {
		result.Analysis = ParseAnalysis(agentResult.Text)
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

	// If no file changes but analysis says fix not applied, treat as analysis_only (not an error).
	if len(result.FilesChanged) == 0 {
		if result.Analysis != nil && !result.Analysis.FixApplied {
			result.Success = true // analysis-only is a valid outcome
			log.Info("agent completed with analysis only (no code fix possible)",
				"issue", config.Issue.ID,
				"rootCause", result.Analysis.RootCause,
			)
			return result
		}
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

	prInfo, err := wt.CreatePR(ctx, config.GHRunner, title, body, config.BaseBranch, branchName, false, wtPath)
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

// recordFixAttempt updates the tracker with the fix agent result.
func recordFixAttempt(tracker *issue.Tracker, result *FixAgentResult, logFile string) {
	now := time.Now()
	attempt := issue.FixAttempt{
		Branch:    result.Branch,
		PRURL:     result.PRURL,
		PRNumber:  result.PRNumber,
		AgentCost: result.AgentCost,
		LogFile:   logFile,
		StartedAt: now,
	}

	// Map analysis fields into the attempt.
	if result.Analysis != nil {
		attempt.Reasoning = result.Analysis.Reasoning
		attempt.RootCause = result.Analysis.RootCause
		attempt.FixOptions = result.Analysis.FixOptions
	}

	if result.Success && result.PRURL != "" {
		attempt.Outcome = "pr_created"
		attempt.PRState = "OPEN"
		tracker.UpdateStatus(result.Issue.Signature, issue.StatusFixPending)
	} else if result.Success {
		// analysis_only: agent succeeded but no PR (not code-fixable).
		// Reset to "new" so it can be manually triaged.
		attempt.Outcome = "analysis_only"
		tracker.UpdateStatus(result.Issue.Signature, issue.StatusNew)
	} else {
		attempt.Outcome = "failed"
		if result.Error != nil {
			attempt.Error = result.Error.Error()
		}
		// Reset from in_progress back to new so the issue remains actionable.
		tracker.UpdateStatus(result.Issue.Signature, issue.StatusNew)
	}

	completed := now
	attempt.CompletedAt = &completed
	tracker.AddFixAttempt(result.Issue.Signature, attempt)
}

// GroupFixAgentConfig configures a fix agent for a group of related issues.
type GroupFixAgentConfig struct {
	WTManager  *wt.Manager
	Logger     *slog.Logger
	GHRunner   wt.GHRunner
	Model      string
	BaseBranch string
	SessionDir string
	RepoDir    string
	Group      IssueGroup
	BudgetUSD  float64
}

// GroupFixAgentResult reports the outcome of a grouped fix agent.
type GroupFixAgentResult struct {
	Error        error
	Group        IssueGroup
	Analysis     *AgentAnalysis
	Branch       string
	WorktreePath string
	PRURL        string
	FilesChanged []string
	AgentCost    float64
	PRNumber     int
	Success      bool
}

// RunGroupFixAgent runs a single fix agent for a group of related issues.
func RunGroupFixAgent(ctx context.Context, config GroupFixAgentConfig) *GroupFixAgentResult {
	result := &GroupFixAgentResult{Group: config.Group}
	leader := config.Group.Leader()
	log := config.Logger
	if log == nil {
		log = slog.Default()
	}

	branchName := fixBranchName(leader)
	result.Branch = branchName

	log.Info("creating worktree for issue group",
		"branch", branchName,
		"groupKey", config.Group.Key,
		"issueCount", len(config.Group.Issues),
	)

	wtPath, err := config.WTManager.New(ctx, branchName, config.BaseBranch, leader.Summary)
	if err != nil {
		result.Error = fmt.Errorf("create worktree: %w", err)
		return result
	}
	result.WorktreePath = wtPath

	buildInfo := DetectBuildInfo(config.RepoDir)
	prompt := buildGroupFixPrompt(config.Group, config.BaseBranch, buildInfo)

	sess := agent.NewEphemeralSession(agent.AgentConfig{
		Logger:     log,
		Role:       agent.RoleBuilder,
		Model:      config.Model,
		WorkDir:    wtPath,
		SessionDir: config.SessionDir,
		BudgetUSD:  config.BudgetUSD,
	}, fmt.Sprintf("medivac-group-%s", leader.ID))

	log.Info("running group fix agent",
		"groupKey", config.Group.Key,
		"issueCount", len(config.Group.Issues),
		"model", config.Model,
	)

	agentResult, execResult, _, err := sess.ExecuteWithFiles(ctx, prompt)
	result.AgentCost = sess.TotalCost()

	// Parse analysis from agent response (best-effort).
	if agentResult != nil && agentResult.Text != "" {
		result.Analysis = ParseAnalysis(agentResult.Text)
	}

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

	// If no file changes but analysis says fix not applied, treat as analysis_only.
	if len(result.FilesChanged) == 0 {
		if result.Analysis != nil && !result.Analysis.FixApplied {
			result.Success = true
			log.Info("group agent completed with analysis only (no code fix possible)",
				"groupKey", config.Group.Key,
				"rootCause", result.Analysis.RootCause,
			)
			return result
		}
		result.Error = fmt.Errorf("agent made no file changes")
		return result
	}

	log.Info("group agent completed, creating PR",
		"filesChanged", len(result.FilesChanged),
		"cost", fmt.Sprintf("$%.4f", result.AgentCost),
	)

	// PR title/body list all issues in the group
	title := fmt.Sprintf("fix(%s): %s (%d issues)", leader.Category, truncate(leader.Summary, 40), len(config.Group.Issues))
	var bodyBuilder strings.Builder
	bodyBuilder.WriteString("Automated fix for a group of related CI failures.\n\n")
	bodyBuilder.WriteString(fmt.Sprintf("**Group key:** `%s`\n", config.Group.Key))
	bodyBuilder.WriteString(fmt.Sprintf("**Issues fixed:** %d\n\n", len(config.Group.Issues)))
	for _, iss := range config.Group.Issues {
		bodyBuilder.WriteString(fmt.Sprintf("- `%s` %s — %s", iss.ID, iss.Category, truncate(iss.Summary, 80)))
		if iss.File != "" {
			bodyBuilder.WriteString(fmt.Sprintf(" (`%s`)", iss.File))
		}
		bodyBuilder.WriteString("\n")
	}
	body := bodyBuilder.String()
	if len(body) > 4000 {
		body = body[:4000] + "\n... (truncated)"
	}

	prInfo, err := wt.CreatePR(ctx, config.GHRunner, title, body, config.BaseBranch, branchName, false, result.WorktreePath)
	if err != nil {
		result.Error = fmt.Errorf("create PR: %w", err)
		return result
	}

	result.PRURL = prInfo.URL
	result.PRNumber = prInfo.Number
	result.Success = true

	log.Info("group fix PR created",
		"pr", prInfo.URL,
		"number", prInfo.Number,
		"issueCount", len(config.Group.Issues),
	)

	return result
}

// recordGroupFixAttempt updates the tracker with a group fix agent result,
// recording the attempt on ALL issues in the group.
func recordGroupFixAttempt(tracker *issue.Tracker, result *GroupFixAgentResult, logFile string) {
	now := time.Now()
	attempt := issue.FixAttempt{
		Branch:    result.Branch,
		PRURL:     result.PRURL,
		PRNumber:  result.PRNumber,
		AgentCost: result.AgentCost / float64(len(result.Group.Issues)), // split cost
		LogFile:   logFile,
		StartedAt: now,
	}

	// Map analysis fields into the attempt.
	if result.Analysis != nil {
		attempt.Reasoning = result.Analysis.Reasoning
		attempt.RootCause = result.Analysis.RootCause
		attempt.FixOptions = result.Analysis.FixOptions
	}

	if result.Success && result.PRURL != "" {
		attempt.Outcome = "pr_created"
		attempt.PRState = "OPEN"
	} else if result.Success {
		attempt.Outcome = "analysis_only"
	} else {
		attempt.Outcome = "failed"
		if result.Error != nil {
			attempt.Error = result.Error.Error()
		}
	}

	completed := now
	attempt.CompletedAt = &completed

	for _, iss := range result.Group.Issues {
		tracker.AddFixAttempt(iss.Signature, attempt)
		if result.Success && result.PRURL != "" {
			tracker.UpdateStatus(iss.Signature, issue.StatusFixPending)
		} else {
			// analysis_only or failed: reset to "new" so issue remains actionable.
			tracker.UpdateStatus(iss.Signature, issue.StatusNew)
		}
	}
}
