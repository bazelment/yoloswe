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

// AgentSession is an interface for agent session execution.
// This enables testing by allowing mock implementations.
type AgentSession interface {
	ExecuteWithFiles(ctx context.Context, prompt string) (*agent.AgentResult, *agent.ExecuteResult, string, error)
	TotalCost() float64
}

// SessionFactory creates new agent sessions.
// This is injectable for testing.
type SessionFactory func(config agent.AgentConfig, sessionID string) AgentSession

// defaultSessionFactory creates real ephemeral sessions.
// It resolves the provider from the model ID so that non-Claude models
// (e.g. Gemini, Codex) are routed to the correct provider.
func defaultSessionFactory(config agent.AgentConfig, sessionID string) AgentSession {
	sess := agent.NewEphemeralSession(config, sessionID)
	if m, ok := agent.ModelByID(config.Model); ok {
		sess.SetProviderName(m.Provider)
	}
	return sess
}

// FixAgentConfig configures a single fix agent.
type FixAgentConfig struct {
	Issue          *issue.Issue
	WTManager      *wt.Manager
	GHRunner       wt.GHRunner
	SessionFactory SessionFactory // optional; defaults to real sessions
	Logger         *slog.Logger
	Model          string
	BaseBranch     string
	SessionDir     string
	BudgetUSD      float64
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

// agentCoreResult holds the results of a runAgentCore execution.
type agentCoreResult struct {
	Error        error
	Analysis     *AgentAnalysis
	Branch       string
	WorktreePath string
	PRURL        string
	FilesChanged []string
	AgentCost    float64
	PRNumber     int
	Success      bool
}

// runAgentCore is the unified implementation for both single and group fix agents.
// It handles worktree creation, session execution, and PR creation for any number of issues.
func runAgentCore(
	ctx context.Context,
	issues []*issue.Issue,
	prompt string,
	sessionID string,
	wtManager *wt.Manager,
	ghRunner wt.GHRunner,
	logger *slog.Logger,
	model string,
	baseBranch string,
	sessionDir string,
	budgetUSD float64,
	sessionFactory SessionFactory,
) agentCoreResult {
	if logger == nil {
		logger = slog.Default()
	}

	r := agentCoreResult{}

	// Use the first issue as the "leader" for branch naming
	leader := issues[0]
	r.Branch = fixBranchName(leader)

	logger.Info("creating worktree",
		"branch", r.Branch,
		"base", baseBranch,
		"issueCount", len(issues),
	)

	// Create worktree (fetch is done once by launchAgents, skip here)
	var err error
	r.WorktreePath, err = wtManager.New(ctx, r.Branch, baseBranch, leader.Summary, wt.NewOptions{SkipFetch: true})
	if err != nil {
		r.Error = fmt.Errorf("create worktree: %w", err)
		return r
	}

	logger.Info("worktree created", "path", r.WorktreePath)

	// Ensure worktree is cleaned up on error paths.
	// On success, the worktree will be cleaned up later after successful merge.
	defer func() {
		if !r.Success {
			if removeErr := wtManager.Remove(ctx, r.Branch, true); removeErr != nil {
				logger.Warn("failed to cleanup worktree on error path",
					"branch", r.Branch,
					"error", removeErr,
				)
			}
		}
	}()

	// Create ephemeral session
	if sessionFactory == nil {
		sessionFactory = defaultSessionFactory
	}
	sess := sessionFactory(agent.AgentConfig{
		Logger:     logger,
		Role:       agent.RoleBuilder,
		Model:      model,
		WorkDir:    r.WorktreePath,
		SessionDir: sessionDir,
		BudgetUSD:  budgetUSD,
	}, sessionID)

	logger.Debug("fix prompt built", "chars", len(prompt), "issueCount", len(issues))
	logger.Log(ctx, LevelTrace, "fix prompt", "content", prompt)

	logger.Info("running fix agent",
		"issueCount", len(issues),
		"model", model,
	)

	// Execute the fix
	agentResult, execResult, _, execErr := sess.ExecuteWithFiles(ctx, prompt)
	r.AgentCost = sess.TotalCost()

	if execErr != nil {
		r.Error = fmt.Errorf("agent execution: %w", execErr)
		return r
	}

	if agentResult != nil {
		logger.Debug("agent response received", "responseChars", len(agentResult.Text), "success", agentResult.Success)
		logger.Log(ctx, LevelTrace, "agent response", "text", agentResult.Text)
	}

	// Parse analysis from agent response (best-effort).
	if agentResult != nil && agentResult.Text != "" {
		r.Analysis = ParseAnalysis(agentResult.Text)
	}

	if agentResult == nil || !agentResult.Success {
		errMsg := "agent reported failure"
		if agentResult != nil && agentResult.Text != "" {
			errMsg = agentResult.Text
		}
		r.Error = fmt.Errorf("agent failed: %s", errMsg)
		return r
	}

	if execResult != nil {
		r.FilesChanged = append(r.FilesChanged, execResult.FilesCreated...)
		r.FilesChanged = append(r.FilesChanged, execResult.FilesModified...)
	}

	// If no file changes but analysis says fix not applied, treat as analysis_only (not an error).
	if len(r.FilesChanged) == 0 {
		if r.Analysis != nil && !r.Analysis.FixApplied {
			// Analysis-only: no PR will be created, cleanup worktree immediately
			if removeErr := wtManager.Remove(ctx, r.Branch, true); removeErr != nil {
				logger.Warn("failed to cleanup worktree after analysis-only outcome",
					"branch", r.Branch,
					"error", removeErr,
				)
			}
			r.Success = true // analysis-only is a valid outcome
			logger.Info("agent completed with analysis only (no code fix possible)",
				"issueCount", len(issues),
				"rootCause", r.Analysis.RootCause,
			)
			return r
		}
		r.Error = fmt.Errorf("agent made no file changes")
		return r
	}

	logger.Info("agent completed, creating PR",
		"filesChanged", len(r.FilesChanged),
		"cost", fmt.Sprintf("$%.4f", r.AgentCost),
	)

	// Create PR
	var title, body string
	if len(issues) == 1 {
		// Single issue PR
		iss := issues[0]
		title = fmt.Sprintf("fix(%s): %s", iss.Category, truncate(iss.Summary, 60))
		body = fmt.Sprintf("Automated fix for CI failure.\n\n**Category:** %s\n**Signature:** `%s`\n**File:** %s\n\n%s",
			iss.Category, iss.Signature, iss.File, iss.Details)
	} else {
		// Group PR
		title = fmt.Sprintf("fix(%s): %s (%d issues)", leader.Category, truncate(leader.Summary, 40), len(issues))
		var bodyBuilder strings.Builder
		bodyBuilder.WriteString("Automated fix for a group of related CI failures.\n\n")
		bodyBuilder.WriteString(fmt.Sprintf("**Issues fixed:** %d\n\n", len(issues)))
		for _, iss := range issues {
			bodyBuilder.WriteString(fmt.Sprintf("- `%s` %s — %s", iss.ID, iss.Category, truncate(iss.Summary, 80)))
			if iss.File != "" {
				bodyBuilder.WriteString(fmt.Sprintf(" (`%s`)", iss.File))
			}
			bodyBuilder.WriteString("\n")
		}
		body = bodyBuilder.String()
	}

	if len(body) > 4000 {
		body = body[:4000] + "\n... (truncated)"
	}

	prInfo, prErr := wt.CreatePR(ctx, ghRunner, title, body, baseBranch, r.Branch, false, r.WorktreePath)
	if prErr != nil {
		r.Error = fmt.Errorf("create PR: %w", prErr)
		return r
	}

	r.PRURL = prInfo.URL
	r.PRNumber = prInfo.Number
	r.Success = true

	logger.Info("fix PR created",
		"pr", prInfo.URL,
		"number", prInfo.Number,
	)

	return r
}

// RunFixAgent runs a single fix agent: creates worktree, runs Claude session, creates PR.
func RunFixAgent(ctx context.Context, config FixAgentConfig) *FixAgentResult {
	prompt := buildFixPrompt(config.Issue, config.BaseBranch)
	sessionID := fmt.Sprintf("medivac-%s", config.Issue.ID)

	r := runAgentCore(
		ctx,
		[]*issue.Issue{config.Issue},
		prompt,
		sessionID,
		config.WTManager,
		config.GHRunner,
		config.Logger,
		config.Model,
		config.BaseBranch,
		config.SessionDir,
		config.BudgetUSD,
		config.SessionFactory,
	)

	return &FixAgentResult{
		Issue:        config.Issue,
		Error:        r.Error,
		Analysis:     r.Analysis,
		Branch:       r.Branch,
		WorktreePath: r.WorktreePath,
		PRURL:        r.PRURL,
		FilesChanged: r.FilesChanged,
		AgentCost:    r.AgentCost,
		PRNumber:     r.PRNumber,
		Success:      r.Success,
	}
}

// fixBranchName generates a unique branch name for a fix issue.
// The attempt number ensures retries don't collide with previous branches.
func fixBranchName(iss *issue.Issue) string {
	cat := strings.ReplaceAll(string(iss.Category), "/", "-")
	attempt := len(iss.FixAttempts) + 1
	return fmt.Sprintf("fix/%s/%s-v%d", cat, iss.ID, attempt)
}

// recordFixAttempt updates the tracker with the fix agent result for one or more issues.
// For group fixes, the cost is split evenly across all issues.
func recordFixAttempt(tracker *issue.Tracker, issues []*issue.Issue, branch string, prURL string, prNumber int, agentCost float64, analysis *AgentAnalysis, success bool, err error, logFile string) {
	now := time.Now()

	// Split cost evenly if this is a group fix
	costPerIssue := agentCost
	if len(issues) > 1 {
		costPerIssue = agentCost / float64(len(issues))
	}

	attempt := issue.FixAttempt{
		Branch:    branch,
		PRURL:     prURL,
		PRNumber:  prNumber,
		AgentCost: costPerIssue,
		LogFile:   logFile,
		StartedAt: now,
	}

	// Map analysis fields into the attempt.
	if analysis != nil {
		attempt.Reasoning = analysis.Reasoning
		attempt.RootCause = analysis.RootCause
		attempt.FixOptions = analysis.FixOptions
	}

	// Determine outcome and new status
	var newStatus issue.Status
	if success && prURL != "" {
		attempt.Outcome = "pr_created"
		attempt.PRState = "OPEN"
		newStatus = issue.StatusFixPending
	} else if success {
		// analysis_only: agent succeeded but no PR (not code-fixable).
		// Reset to "new" so it can be manually triaged.
		attempt.Outcome = "analysis_only"
		newStatus = issue.StatusNew
	} else {
		attempt.Outcome = "failed"
		if err != nil {
			attempt.Error = err.Error()
		}
		// Reset from in_progress back to new so the issue remains actionable.
		newStatus = issue.StatusNew
	}

	completed := now
	attempt.CompletedAt = &completed

	// Record the attempt for all issues
	for _, iss := range issues {
		tracker.AddFixAttempt(iss.Signature, attempt)
		tracker.UpdateStatus(iss.Signature, newStatus)
	}
}

// GroupFixAgentConfig configures a fix agent for a group of related issues.
type GroupFixAgentConfig struct {
	WTManager      *wt.Manager
	SessionFactory SessionFactory // optional; defaults to real sessions
	Logger         *slog.Logger
	GHRunner       wt.GHRunner
	Model          string
	BaseBranch     string
	SessionDir     string
	Group          IssueGroup
	BudgetUSD      float64
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
	leader := config.Group.Leader()

	prompt := buildGroupFixPrompt(config.Group, config.BaseBranch)
	sessionID := fmt.Sprintf("medivac-group-%s", leader.ID)

	r := runAgentCore(
		ctx,
		config.Group.Issues,
		prompt,
		sessionID,
		config.WTManager,
		config.GHRunner,
		config.Logger,
		config.Model,
		config.BaseBranch,
		config.SessionDir,
		config.BudgetUSD,
		config.SessionFactory,
	)

	return &GroupFixAgentResult{
		Group:        config.Group,
		Error:        r.Error,
		Analysis:     r.Analysis,
		Branch:       r.Branch,
		WorktreePath: r.WorktreePath,
		PRURL:        r.PRURL,
		FilesChanged: r.FilesChanged,
		AgentCost:    r.AgentCost,
		PRNumber:     r.PRNumber,
		Success:      r.Success,
	}
}
