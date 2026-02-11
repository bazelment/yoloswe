// Package engine orchestrates the fixer workflow: scan, fix, merge, verify.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bazelment/yoloswe/fixer/github"
	"github.com/bazelment/yoloswe/fixer/issue"
	"github.com/bazelment/yoloswe/wt"
)

// Config configures the fixer engine.
type Config struct {
	WTManager    *wt.Manager
	GHRunner     wt.GHRunner
	TriageQuery  github.QueryFn // injectable for testing; nil = real claude.Query
	Logger       *slog.Logger
	RepoDir      string
	TrackerPath  string
	AgentModel   string
	Branch       string
	SessionDir   string
	TriageModel  string // Claude model for triage (default "haiku")
	AgentBudget  float64
	TriageBudget float64 // Max spend on LLM triage per scan (default $0.10)
	MaxParallel  int
	RunLimit     int
	DryRun       bool
}

// Engine is the core fixer orchestrator.
type Engine struct {
	ghClient *github.Client
	tracker  *issue.Tracker
	logger   *slog.Logger
	config   Config
}

// New creates a new Engine from the given config.
func New(config Config) (*Engine, error) {
	if config.MaxParallel <= 0 {
		config.MaxParallel = 3
	}
	if config.Branch == "" {
		config.Branch = "main"
	}
	if config.RunLimit <= 0 {
		config.RunLimit = 5
	}
	if config.AgentModel == "" {
		config.AgentModel = "sonnet"
	}
	if config.TriageModel == "" {
		config.TriageModel = "haiku"
	}
	if config.TriageBudget <= 0 {
		config.TriageBudget = 0.50
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}

	tracker, err := issue.NewTracker(config.TrackerPath)
	if err != nil {
		return nil, fmt.Errorf("load tracker: %w", err)
	}

	ghClient := github.NewClient(config.GHRunner, config.RepoDir)

	return &Engine{
		config:   config,
		ghClient: ghClient,
		tracker:  tracker,
		logger:   config.Logger,
	}, nil
}

// ScanResult holds the outcome of a scan.
type ScanResult struct {
	Reconciled    *issue.ReconcileResult
	Runs          []github.WorkflowRun
	Failures      []github.CIFailure
	TotalIssues   int
	ActionableLen int
	TriageCost    float64
}

// Scan fetches CI failures and reconciles them with the tracker.
func (e *Engine) Scan(ctx context.Context) (*ScanResult, error) {
	e.logger.Info("scanning CI failures",
		"branch", e.config.Branch,
		"limit", e.config.RunLimit,
	)

	// Fetch failed runs
	runs, err := e.ghClient.ListFailedRuns(ctx, e.config.Branch, e.config.RunLimit)
	if err != nil {
		return nil, fmt.Errorf("list failed runs: %w", err)
	}

	e.logger.Info("found failed runs", "count", len(runs))

	triageCfg := github.TriageConfig{
		Model: e.config.TriageModel,
		Query: e.config.TriageQuery,
	}

	var (
		allFailures     []github.CIFailure
		totalTriageCost float64
	)

	for i := range runs {
		run := runs[i]

		// Check budget before calling LLM.
		if totalTriageCost >= e.config.TriageBudget {
			e.logger.Warn("triage budget exhausted",
				"spent", fmt.Sprintf("$%.4f", totalTriageCost),
				"budget", fmt.Sprintf("$%.4f", e.config.TriageBudget),
			)
			break
		}

		// Fetch annotations (best-effort, may be empty/useless).
		annotations, _ := e.ghClient.GetAnnotations(ctx, run.ID)

		// Fetch raw log.
		rawLog, err := e.ghClient.GetJobLog(ctx, run.ID)
		if err != nil {
			e.logger.Warn("failed to get job log",
				"runID", run.ID,
				"error", err,
			)
			continue
		}
		cleanedLog := github.CleanLog(rawLog)

		// Get failed jobs for context.
		jobs, err := e.ghClient.GetJobsForRun(ctx, run.ID)
		if err != nil {
			e.logger.Warn("failed to get jobs",
				"runID", run.ID,
				"error", err,
			)
			continue
		}

		var failedJobs []github.JobResult
		for _, job := range jobs {
			if job.Conclusion == "failure" {
				failedJobs = append(failedJobs, job)
			}
		}
		if len(failedJobs) == 0 {
			continue
		}

		e.logger.Debug("triaging run",
			"runID", run.ID,
			"workflow", run.Name,
			"failedJobs", len(failedJobs),
		)

		// Single LLM call per run with all failed jobs + log.
		failures, cost, triageErr := github.TriageRun(ctx, run, failedJobs, annotations, cleanedLog, triageCfg)
		totalTriageCost += cost

		if triageErr != nil {
			e.logger.Warn("triage failed",
				"runID", run.ID,
				"error", triageErr,
			)
			continue
		}

		e.logger.Debug("triage results",
			"runID", run.ID,
			"failures", len(failures),
			"cost", fmt.Sprintf("$%.4f", cost),
		)

		allFailures = append(allFailures, failures...)
	}

	e.logger.Info("triaged CI failures",
		"count", len(allFailures),
		"triageCost", fmt.Sprintf("$%.4f", totalTriageCost),
	)

	// Reconcile with known issues
	reconciled := e.tracker.Reconcile(allFailures)

	if err := e.tracker.Save(); err != nil {
		return nil, fmt.Errorf("save tracker: %w", err)
	}

	actionable := e.tracker.GetActionable()

	return &ScanResult{
		Runs:          runs,
		Failures:      allFailures,
		Reconciled:    reconciled,
		TotalIssues:   len(e.tracker.All()),
		ActionableLen: len(actionable),
		TriageCost:    totalTriageCost,
	}, nil
}

// FixResult holds the outcome of a fix run.
type FixResult struct {
	ScanResult *ScanResult
	Results    []*FixAgentResult
	TotalCost  float64
}

// Fix runs the full workflow: scan + launch fix agents for actionable issues.
func (e *Engine) Fix(ctx context.Context) (*FixResult, error) {
	scanResult, err := e.Scan(ctx)
	if err != nil {
		return nil, err
	}

	fixResult := &FixResult{ScanResult: scanResult}

	actionable := e.tracker.GetActionable()
	if len(actionable) == 0 {
		e.logger.Info("no actionable issues found")
		return fixResult, nil
	}

	e.logger.Info("launching fix agents",
		"count", len(actionable),
		"maxParallel", e.config.MaxParallel,
		"dryRun", e.config.DryRun,
	)

	if e.config.DryRun {
		for _, iss := range actionable {
			fixResult.Results = append(fixResult.Results, &FixAgentResult{
				Issue: iss,
			})
		}
		return fixResult, nil
	}

	// Launch fix agents with bounded parallelism
	sem := make(chan struct{}, e.config.MaxParallel)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, iss := range actionable {
		// Mark as in_progress
		e.tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)

		wg.Add(1)
		go func(iss *issue.Issue) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			result := RunFixAgent(ctx, FixAgentConfig{
				Issue:      iss,
				WTManager:  e.config.WTManager,
				GHRunner:   e.config.GHRunner,
				Model:      e.config.AgentModel,
				BudgetUSD:  e.config.AgentBudget,
				BaseBranch: e.config.Branch,
				SessionDir: e.config.SessionDir,
				Logger:     e.logger.With("issue", iss.ID),
			})

			recordFixAttempt(e.tracker, result)

			mu.Lock()
			fixResult.Results = append(fixResult.Results, result)
			fixResult.TotalCost += result.AgentCost
			mu.Unlock()
		}(iss)
	}

	wg.Wait()

	if err := e.tracker.Save(); err != nil {
		return fixResult, fmt.Errorf("save tracker: %w", err)
	}

	return fixResult, nil
}

// MergeResult holds the outcome of merging a single PR.
type MergeResult struct {
	Issue    *issue.Issue
	Error    error
	PRNumber int
}

// MergeApproved merges PRs that have been approved.
func (e *Engine) MergeApproved(ctx context.Context) ([]MergeResult, error) {
	// First refresh PR status
	if err := e.RefreshPRStatus(ctx); err != nil {
		return nil, fmt.Errorf("refresh PR status: %w", err)
	}

	pending := e.tracker.GetPendingMerge()
	if len(pending) == 0 {
		e.logger.Info("no approved PRs to merge")
		return nil, nil
	}

	e.logger.Info("merging approved PRs", "count", len(pending))

	var results []MergeResult
	for _, iss := range pending {
		if len(iss.FixAttempts) == 0 {
			continue
		}
		lastAttempt := iss.FixAttempts[len(iss.FixAttempts)-1]
		if lastAttempt.Branch == "" {
			continue
		}

		mr := MergeResult{Issue: iss, PRNumber: lastAttempt.PRNumber}

		if e.config.DryRun {
			e.logger.Info("would merge PR (dry-run)",
				"pr", lastAttempt.PRNumber,
				"branch", lastAttempt.Branch,
			)
			results = append(results, mr)
			continue
		}

		_, err := e.config.WTManager.MergePRForBranch(ctx, lastAttempt.Branch, wt.MergeOptions{
			MergeMethod: "squash",
			Keep:        false,
		})
		if err != nil {
			mr.Error = err
			e.logger.Warn("merge failed",
				"pr", lastAttempt.PRNumber,
				"error", err,
			)
		} else {
			e.tracker.UpdateStatus(iss.Signature, issue.StatusFixMerged)

			// Cleanup worktree
			if removeErr := e.config.WTManager.Remove(ctx, lastAttempt.Branch, true); removeErr != nil {
				e.logger.Warn("worktree cleanup failed",
					"branch", lastAttempt.Branch,
					"error", removeErr,
				)
			}
		}

		results = append(results, mr)
	}

	if err := e.tracker.Save(); err != nil {
		return results, fmt.Errorf("save tracker: %w", err)
	}

	return results, nil
}

// RefreshPRStatus polls GitHub for updated PR review status and updates the tracker.
func (e *Engine) RefreshPRStatus(ctx context.Context) error {
	allIssues := e.tracker.All()

	for _, iss := range allIssues {
		if iss.Status != issue.StatusFixPending && iss.Status != issue.StatusFixApproved {
			continue
		}
		if len(iss.FixAttempts) == 0 {
			continue
		}

		lastAttempt := &iss.FixAttempts[len(iss.FixAttempts)-1]
		if lastAttempt.Branch == "" {
			continue
		}

		prInfo, err := wt.GetPRByBranch(ctx, e.config.GHRunner, lastAttempt.Branch, e.config.RepoDir)
		if err != nil {
			e.logger.Warn("failed to get PR status",
				"branch", lastAttempt.Branch,
				"error", err,
			)
			continue
		}

		lastAttempt.PRState = prInfo.State
		lastAttempt.PRReview = prInfo.ReviewDecision

		switch {
		case prInfo.State == "MERGED":
			e.tracker.UpdateStatus(iss.Signature, issue.StatusFixMerged)
		case prInfo.State == "CLOSED":
			e.tracker.UpdateStatus(iss.Signature, issue.StatusNew)
		case prInfo.ReviewDecision == "APPROVED":
			e.tracker.UpdateStatus(iss.Signature, issue.StatusFixApproved)
		}
	}

	return e.tracker.Save()
}

// Tracker returns the engine's issue tracker (for status commands).
func (e *Engine) Tracker() *issue.Tracker {
	return e.tracker
}
