// Package engine orchestrates the medivac workflow: scan, fix, merge, verify.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bazelment/yoloswe/medivac/github"
	"github.com/bazelment/yoloswe/medivac/issue"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
)

// Scanner abstracts CI data gathering operations.
// This interface enables testing and potential support for non-GitHub CI systems.
type Scanner interface {
	ListFailedRuns(ctx context.Context, branch string, limit int) ([]github.WorkflowRun, error)
	GetJobsForRun(ctx context.Context, runID int64) ([]github.JobResult, error)
	GetAnnotations(ctx context.Context, runID int64) ([]github.Annotation, error)
	GetJobLog(ctx context.Context, runID int64) (string, error)
}

// Config configures the medivac engine.
type Config struct {
	WTManager   *wt.Manager
	GHRunner    wt.GHRunner
	Scanner     Scanner        // injectable for testing; nil = create default github.Client
	TriageQuery github.QueryFn // injectable for testing; nil = real claude.Query
	Logger      *slog.Logger
	RepoDir     string
	TrackerPath string
	AgentModel  string
	Branch      string
	SessionDir  string
	LogFile     string // Path to the log file for this run (stored in fix attempts)
	TriageModel string // Claude model for triage (default "haiku")
	AgentBudget float64
	MaxParallel int
	RunLimit    int
	DryRun      bool
}

// Engine is the core medivac orchestrator.
type Engine struct {
	scanner Scanner
	tracker *issue.Tracker
	logger  *slog.Logger
	config  Config
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
	if config.Logger == nil {
		config.Logger = slog.Default()
	}

	// Validate model IDs against the registry.
	if _, ok := agent.ModelByID(config.AgentModel); !ok {
		return nil, fmt.Errorf("unknown agent model %q; see multiagent/agent AllModels for valid IDs", config.AgentModel)
	}
	if _, ok := agent.ModelByID(config.TriageModel); !ok {
		return nil, fmt.Errorf("unknown triage model %q; see multiagent/agent AllModels for valid IDs", config.TriageModel)
	}

	tracker, err := issue.NewTracker(config.TrackerPath)
	if err != nil {
		return nil, fmt.Errorf("load tracker: %w", err)
	}

	// Use provided scanner or create default github.Client
	scanner := config.Scanner
	if scanner == nil {
		scanner = github.NewClient(config.GHRunner, config.RepoDir, config.Logger)
	}

	return &Engine{
		config:  config,
		scanner: scanner,
		tracker: tracker,
		logger:  config.Logger,
	}, nil
}

// ScanResult holds the outcome of a scan.
type ScanResult struct {
	Reconciled    *issue.ReconcileResult
	Runs          []github.WorkflowRun
	Failures      []issue.CIFailure
	TotalIssues   int
	ActionableLen int
	TriageCost    float64
}

// Scan fetches CI failures and reconciles them with the tracker.
// It uses a two-phase approach: Phase 1 extracts errors per-run,
// Phase 2 deduplicates across runs in a single LLM call.
// Runs that have already been triaged (tracked via reviewed-run markers)
// are skipped, making repeated scans free when no new runs appear.
func (e *Engine) Scan(ctx context.Context) (*ScanResult, error) {
	e.logger.Info("scanning CI failures",
		"branch", e.config.Branch,
		"limit", e.config.RunLimit,
	)

	// Fetch failed runs.
	runs, err := e.scanner.ListFailedRuns(ctx, e.config.Branch, e.config.RunLimit)
	if err != nil {
		return nil, fmt.Errorf("list failed runs: %w", err)
	}

	e.logger.Info("found failed runs", "count", len(runs))

	// Build active run ID set (for pruning stale reviewed markers).
	activeRunIDs := make(map[int64]bool, len(runs))
	for i := range runs {
		activeRunIDs[runs[i].ID] = true
	}

	// Filter to unreviewed runs.
	var unreviewedRuns []github.WorkflowRun
	for i := range runs {
		if !e.tracker.IsRunReviewed(runs[i].ID) {
			unreviewedRuns = append(unreviewedRuns, runs[i])
		}
	}

	e.logger.Info("unreviewed runs", "count", len(unreviewedRuns), "reviewed", len(runs)-len(unreviewedRuns))

	if len(unreviewedRuns) == 0 {
		e.logger.Info("all runs already reviewed, nothing to triage")
		// Still reconcile (to detect resolved issues) but with empty failures.
		reconciled := e.tracker.Reconcile(nil)
		e.tracker.PruneReviewedRuns(activeRunIDs)
		if err := e.tracker.Save(); err != nil {
			return nil, fmt.Errorf("save tracker: %w", err)
		}
		return &ScanResult{
			Runs:          runs,
			Reconciled:    reconciled,
			TotalIssues:   len(e.tracker.All()),
			ActionableLen: len(e.tracker.GetActionable()),
		}, nil
	}

	// Gather data from all unreviewed runs (no LLM calls here).
	var runDataSlice []github.RunData
	var gatheredRunIDs []int64

	for i := range unreviewedRuns {
		run := unreviewedRuns[i]

		// Fetch annotations (best-effort).
		annotations, _ := e.scanner.GetAnnotations(ctx, run.ID)

		// Fetch raw log.
		rawLog, err := e.scanner.GetJobLog(ctx, run.ID)
		if err != nil {
			e.logger.Warn("failed to get job log", "runID", run.ID, "error", err)
			continue
		}
		cleanedLog := github.CleanLog(rawLog)
		trimmedLog := github.TrimLog(cleanedLog, 100, 100)

		e.logger.Debug("fetched CI log",
			"runID", run.ID,
			"rawBytes", len(rawLog),
			"cleanedBytes", len(cleanedLog),
			"trimmedBytes", len(trimmedLog),
		)
		e.logger.Log(ctx, LevelDump, "raw CI log", "runID", run.ID, "content", rawLog)
		e.logger.Log(ctx, LevelTrace, "trimmed CI log", "runID", run.ID, "content", trimmedLog)

		// Get failed jobs.
		jobs, err := e.scanner.GetJobsForRun(ctx, run.ID)
		if err != nil {
			e.logger.Warn("failed to get jobs", "runID", run.ID, "error", err)
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

		e.logger.Debug("gathered run data",
			"runID", run.ID,
			"workflow", run.Name,
			"failedJobs", len(failedJobs),
			"annotations", len(annotations),
		)

		runDataSlice = append(runDataSlice, github.RunData{
			Run:         run,
			FailedJobs:  failedJobs,
			Annotations: annotations,
			Log:         trimmedLog,
		})
		gatheredRunIDs = append(gatheredRunIDs, run.ID)
	}

	// Single LLM call to triage all runs at once.
	triageCfg := github.TriageConfig{
		Model:  e.config.TriageModel,
		Query:  e.config.TriageQuery,
		Logger: e.logger,
	}

	batchResult, err := github.TriageBatch(ctx, runDataSlice, triageCfg)
	if err != nil {
		return nil, fmt.Errorf("batch triage: %w", err)
	}

	allFailures := batchResult.Failures

	e.logger.Info("triaged CI failures",
		"runs", len(runDataSlice),
		"failures", len(allFailures),
		"triageCost", fmt.Sprintf("$%.4f", batchResult.Cost),
	)

	// Mark all gathered runs as reviewed.
	e.tracker.MarkRunsReviewed(gatheredRunIDs)
	e.tracker.PruneReviewedRuns(activeRunIDs)

	// Reconcile with known issues.
	reconciled := e.tracker.Reconcile(allFailures)

	e.logger.Debug("reconciled issues",
		"new", len(reconciled.New),
		"updated", len(reconciled.Updated),
		"resolved", len(reconciled.Resolved),
	)

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
		TriageCost:    batchResult.Cost,
	}, nil
}

// FixResult holds the outcome of a fix run.
type FixResult struct {
	ScanResult   *ScanResult
	Results      []*FixAgentResult
	GroupResults []*GroupFixAgentResult
	TotalCost    float64
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

	groups := GroupIssues(actionable)

	e.logger.Info("grouped issues",
		"actionableCount", len(actionable),
		"groupCount", len(groups),
	)

	e.logger.Info("launching fix agents",
		"groupCount", len(groups),
		"maxParallel", e.config.MaxParallel,
		"dryRun", e.config.DryRun,
	)

	e.launchAgents(ctx, groups, fixResult)

	if err := e.tracker.Save(); err != nil {
		return fixResult, fmt.Errorf("save tracker: %w", err)
	}

	return fixResult, nil
}

// FixFromTracker launches fix agents for currently actionable issues in the
// tracker without re-scanning CI. Use this when you have already run a scan
// and want to fix the issues it found.
func (e *Engine) FixFromTracker(ctx context.Context) (*FixResult, error) {
	fixResult := &FixResult{}

	actionable := e.tracker.GetActionable()
	if len(actionable) == 0 {
		e.logger.Info("no actionable issues found in tracker")
		return fixResult, nil
	}

	groups := GroupIssues(actionable)

	e.logger.Info("grouped issues from tracker",
		"actionableCount", len(actionable),
		"groupCount", len(groups),
	)

	e.logger.Info("launching fix agents from tracker",
		"groupCount", len(groups),
		"maxParallel", e.config.MaxParallel,
		"dryRun", e.config.DryRun,
	)

	e.launchAgents(ctx, groups, fixResult)

	if err := e.tracker.Save(); err != nil {
		return fixResult, fmt.Errorf("save tracker: %w", err)
	}

	return fixResult, nil
}

// launchAgents runs fix agents for the given groups, populating fixResult.
// It handles dry-run short-circuit, bounded parallelism, and tracker updates.
func (e *Engine) launchAgents(ctx context.Context, groups []IssueGroup, fixResult *FixResult) {
	if e.config.DryRun {
		for _, group := range groups {
			if len(group.Issues) == 1 {
				fixResult.Results = append(fixResult.Results, &FixAgentResult{
					Issue: group.Issues[0],
				})
			} else {
				fixResult.GroupResults = append(fixResult.GroupResults, &GroupFixAgentResult{
					Group: group,
				})
			}
		}
		return
	}

	// Fetch once before launching parallel agents to avoid concurrent
	// git-fetch conflicts on the same bare repo.
	if err := e.config.WTManager.FetchOrigin(ctx); err != nil {
		e.logger.Error("failed to fetch origin before launching agents", "error", err)
		// Mark all issues as failed and return.
		for _, group := range groups {
			for _, iss := range group.Issues {
				e.tracker.UpdateStatus(iss.Signature, issue.StatusNew)
			}
			if len(group.Issues) == 1 {
				fixResult.Results = append(fixResult.Results, &FixAgentResult{
					Issue: group.Issues[0],
					Error: fmt.Errorf("pre-fetch failed: %w", err),
				})
			} else {
				fixResult.GroupResults = append(fixResult.GroupResults, &GroupFixAgentResult{
					Group: group,
					Error: fmt.Errorf("pre-fetch failed: %w", err),
				})
			}
		}
		return
	}

	// Launch fix agents with bounded parallelism
	sem := make(chan struct{}, e.config.MaxParallel)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, group := range groups {
		for _, iss := range group.Issues {
			e.tracker.UpdateStatus(iss.Signature, issue.StatusInProgress)
		}

		wg.Add(1)
		go func(g IssueGroup) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			if len(g.Issues) == 1 {
				// Singleton group: use existing single-issue path
				result := RunFixAgent(ctx, FixAgentConfig{
					Issue:      g.Issues[0],
					WTManager:  e.config.WTManager,
					GHRunner:   e.config.GHRunner,
					Model:      e.config.AgentModel,
					BudgetUSD:  e.config.AgentBudget,
					BaseBranch: e.config.Branch,
					SessionDir: e.config.SessionDir,
					Logger:     e.logger.With("issue", g.Issues[0].ID),
				})
				recordFixAttempt(e.tracker, g.Issues, result.Branch, result.PRURL, result.PRNumber, result.AgentCost, result.Analysis, result.Success, result.Error, e.config.LogFile)
				mu.Lock()
				fixResult.Results = append(fixResult.Results, result)
				fixResult.TotalCost += result.AgentCost
				mu.Unlock()
			} else {
				// Multi-issue group: use grouped agent
				result := RunGroupFixAgent(ctx, GroupFixAgentConfig{
					Group:      g,
					WTManager:  e.config.WTManager,
					GHRunner:   e.config.GHRunner,
					Model:      e.config.AgentModel,
					BudgetUSD:  e.config.AgentBudget,
					BaseBranch: e.config.Branch,
					SessionDir: e.config.SessionDir,
					Logger:     e.logger.With("group", g.Key),
				})
				recordFixAttempt(e.tracker, g.Issues, result.Branch, result.PRURL, result.PRNumber, result.AgentCost, result.Analysis, result.Success, result.Error, e.config.LogFile)
				mu.Lock()
				fixResult.GroupResults = append(fixResult.GroupResults, result)
				fixResult.TotalCost += result.AgentCost
				mu.Unlock()
			}
		}(group)
	}

	wg.Wait()
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
