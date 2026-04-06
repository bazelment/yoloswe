package jiradozer

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// WorktreeManager is the interface for creating and removing git worktrees.
// Use WTManagerAdapter to adapt wt.Manager to this interface.
type WorktreeManager interface {
	NewWorktree(ctx context.Context, branch, baseBranch, goal string) (worktreePath string, err error)
	RemoveWorktree(ctx context.Context, nameOrBranch string, deleteBranch bool) error
}

// IssueStatus represents the current state of a tracked issue's workflow.
type IssueStatus struct {
	Issue        *tracker.Issue
	Error        error
	StartedAt    time.Time
	CompletedAt  time.Time
	WorktreePath string
	Step         WorkflowStep
	Done         bool
}

// Orchestrator manages concurrent issue workflows, each in its own worktree.
type Orchestrator struct {
	tracker    tracker.IssueTracker
	config     *Config
	logger     *slog.Logger
	wtManager  WorktreeManager
	active     map[string]*managedWorkflow // issueID -> workflow
	statusChan chan IssueStatus
	mu         sync.RWMutex
	wg         sync.WaitGroup
}

type managedWorkflow struct {
	workflow     *Workflow
	issue        *tracker.Issue
	cancel       context.CancelFunc
	startedAt    time.Time
	worktreePath string
	branch       string
}

// NewOrchestrator creates a new multi-issue orchestrator.
func NewOrchestrator(t tracker.IssueTracker, cfg *Config, wtMgr WorktreeManager, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		tracker:    t,
		config:     cfg,
		logger:     logger,
		wtManager:  wtMgr,
		active:     make(map[string]*managedWorkflow),
		statusChan: make(chan IssueStatus, 64),
	}
}

// StatusUpdates returns the channel that receives status updates for all workflows.
func (o *Orchestrator) StatusUpdates() <-chan IssueStatus {
	return o.statusChan
}

// Snapshot returns a point-in-time copy of all tracked issue statuses.
func (o *Orchestrator) Snapshot() []IssueStatus {
	o.mu.RLock()
	defer o.mu.RUnlock()
	statuses := make([]IssueStatus, 0, len(o.active))
	for _, mw := range o.active {
		statuses = append(statuses, IssueStatus{
			Issue:        mw.issue,
			Step:         mw.workflow.state.Current(),
			StartedAt:    mw.startedAt,
			WorktreePath: mw.worktreePath,
		})
	}
	return statuses
}

// ActiveCount returns the number of currently running workflows.
func (o *Orchestrator) ActiveCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	count := 0
	for _, mw := range o.active {
		step := mw.workflow.state.Current()
		if step != StepDone && step != StepFailed {
			count++
		}
	}
	return count
}

// Start launches a workflow for the given issue in a new worktree.
// Returns an error if the concurrency limit is reached or worktree creation fails.
func (o *Orchestrator) Start(ctx context.Context, issue *tracker.Issue) error {
	if o.ActiveCount() >= o.config.Source.MaxConcurrent {
		return fmt.Errorf("concurrency limit reached (%d)", o.config.Source.MaxConcurrent)
	}

	o.mu.Lock()
	if _, exists := o.active[issue.ID]; exists {
		o.mu.Unlock()
		return fmt.Errorf("issue %s already has an active workflow", issue.Identifier)
	}
	o.mu.Unlock()

	branch := fmt.Sprintf("%s/%s", o.config.Source.BranchPrefix, issue.Identifier)
	worktreePath, err := o.wtManager.NewWorktree(ctx, branch, o.config.BaseBranch, issue.Title)
	if err != nil {
		return fmt.Errorf("create worktree for %s: %w", issue.Identifier, err)
	}

	// Create a per-issue config copy with the worktree path as WorkDir.
	issueCfg := *o.config
	issueCfg.WorkDir = worktreePath

	wfCtx, cancel := context.WithCancel(ctx)
	wf := NewWorkflow(o.tracker, issue, &issueCfg, o.logger.With("issue", issue.Identifier))

	mw := &managedWorkflow{
		workflow:     wf,
		issue:        issue,
		cancel:       cancel,
		worktreePath: worktreePath,
		branch:       branch,
		startedAt:    time.Now(),
	}

	// Wire up transition callback to emit status updates.
	wf.OnTransition = func(step WorkflowStep) {
		o.emitStatus(mw, step, nil, false)
	}

	o.mu.Lock()
	o.active[issue.ID] = mw
	o.mu.Unlock()

	o.emitStatus(mw, StepInit, nil, false)

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		err := wf.Run(wfCtx)
		if err != nil {
			o.logger.Error("workflow failed", "issue", issue.Identifier, "error", err)
			o.emitStatus(mw, StepFailed, err, true)
		} else {
			o.logger.Info("workflow completed", "issue", issue.Identifier)
			o.emitStatus(mw, StepDone, nil, true)
		}
		o.cleanup(ctx, mw)
	}()

	return nil
}

// Cancel stops the workflow for the given issue ID.
func (o *Orchestrator) Cancel(issueID string) {
	o.mu.RLock()
	mw, ok := o.active[issueID]
	o.mu.RUnlock()
	if ok {
		mw.cancel()
	}
}

// Wait blocks until all active workflows have completed.
func (o *Orchestrator) Wait() {
	o.wg.Wait()
}

// RunWithDiscovery consumes issues from the discovery channel and starts
// workflows for them, respecting the concurrency limit.
func (o *Orchestrator) RunWithDiscovery(ctx context.Context, discovery *Discovery) error {
	issues := discovery.Run(ctx)
	// Buffer issues that can't be started yet due to concurrency limits.
	var pending []*tracker.Issue

	for {
		// Try to drain pending queue first.
		for len(pending) > 0 && o.ActiveCount() < o.config.Source.MaxConcurrent {
			issue := pending[0]
			pending = pending[1:]
			if err := o.Start(ctx, issue); err != nil {
				o.logger.Warn("failed to start workflow", "issue", issue.Identifier, "error", err)
			}
		}

		select {
		case <-ctx.Done():
			o.Wait()
			return ctx.Err()
		case issue, ok := <-issues:
			if !ok {
				o.Wait()
				return nil
			}
			if o.ActiveCount() < o.config.Source.MaxConcurrent {
				if err := o.Start(ctx, issue); err != nil {
					o.logger.Warn("failed to start workflow", "issue", issue.Identifier, "error", err)
				}
			} else {
				pending = append(pending, issue)
			}
		}
	}
}

func (o *Orchestrator) emitStatus(mw *managedWorkflow, step WorkflowStep, err error, done bool) {
	status := IssueStatus{
		Issue:        mw.issue,
		Step:         step,
		StartedAt:    mw.startedAt,
		WorktreePath: mw.worktreePath,
		Error:        err,
		Done:         done,
	}
	if done {
		status.CompletedAt = time.Now()
	}
	select {
	case o.statusChan <- status:
	default:
		o.logger.Warn("status channel full, dropping update", "issue", mw.issue.Identifier)
	}
}

func (o *Orchestrator) cleanup(ctx context.Context, mw *managedWorkflow) {
	if err := o.wtManager.RemoveWorktree(ctx, mw.branch, true); err != nil {
		o.logger.Warn("failed to remove worktree", "branch", mw.branch, "error", err)
	}
}
