package jiradozer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// WorktreeManager is the interface for creating and removing git worktrees.
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
	RoundIndex   int // 0-based current round during multi-round steps (0 when not in rounds)
	RoundTotal   int // total rounds in current step (0 = single-round step)
}

// IsDone returns true if the workflow has completed (successfully or with failure).
func (s IssueStatus) IsDone() bool {
	return s.Step == StepDone || s.Step == StepFailed
}

// Orchestrator manages concurrent issue workflows, each in its own worktree.
type Orchestrator struct {
	tracker    tracker.IssueTracker
	config     *Config
	logger     *slog.Logger
	wtManager  WorktreeManager
	out        io.Writer                   // dry-run output sink (defaults to os.Stdout)
	active     map[string]*managedWorkflow // issueID -> workflow
	statusChan chan IssueStatus
	slotFreed  chan struct{} // non-blocking signal: a workflow slot was freed
	done       chan struct{} // closed by Shutdown to unblock emitStatus
	repoName   string        // repo name used when printing dry-run bramble commands
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
	roundIndex   int // current round in multi-round step
	roundTotal   int // total rounds (0 = single-round)
}

// NewOrchestrator creates a new multi-issue orchestrator.
// repoName is used when formatting dry-run bramble new-session commands;
// pass an empty string in non-dry-run paths and it will be ignored.
func NewOrchestrator(t tracker.IssueTracker, cfg *Config, wtMgr WorktreeManager, repoName string, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		tracker:    t,
		config:     cfg,
		logger:     logger,
		wtManager:  wtMgr,
		active:     make(map[string]*managedWorkflow),
		statusChan: make(chan IssueStatus, 64),
		slotFreed:  make(chan struct{}, 1),
		done:       make(chan struct{}),
		repoName:   repoName,
		out:        os.Stdout,
	}
}

// SetDryRunOutput overrides the writer that dry-run commands are printed to.
// Primarily useful for tests.
func (o *Orchestrator) SetDryRunOutput(w io.Writer) {
	o.out = w
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
		step := StepInit
		if mw.workflow != nil {
			step = mw.workflow.state.Current()
		}
		statuses = append(statuses, IssueStatus{
			Issue:        mw.issue,
			Step:         step,
			StartedAt:    mw.startedAt,
			WorktreePath: mw.worktreePath,
			RoundIndex:   mw.roundIndex,
			RoundTotal:   mw.roundTotal,
		})
	}
	return statuses
}

// ActiveCount returns the number of currently running workflows.
func (o *Orchestrator) ActiveCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.activeCountLocked()
}

// Start launches a workflow for the given issue in a new worktree.
// Returns an error if the concurrency limit is reached or worktree creation fails.
func (o *Orchestrator) Start(ctx context.Context, issue *tracker.Issue) error {
	// Dry-run: print the equivalent bramble new-session command and return
	// success without reserving a slot or touching the worktree manager.
	// RunWithDiscovery only clears the seen set on error, so returning nil
	// keeps the issue out of subsequent polls.
	if o.config.Source.DryRun {
		o.printDryRunCommand(issue)
		return nil
	}

	// Hold the lock for the entire check-and-reserve sequence to prevent
	// TOCTOU races on both the concurrency limit and the duplicate check.
	o.mu.Lock()
	if o.activeCountLocked() >= o.config.Source.MaxConcurrent {
		o.mu.Unlock()
		return fmt.Errorf("concurrency limit reached (%d)", o.config.Source.MaxConcurrent)
	}
	if _, exists := o.active[issue.ID]; exists {
		o.mu.Unlock()
		return fmt.Errorf("issue %s already has an active workflow", issue.Identifier)
	}
	// Reserve the slot with a placeholder so concurrent calls see it.
	placeholder := &managedWorkflow{issue: issue}
	o.active[issue.ID] = placeholder
	o.mu.Unlock()

	branch := fmt.Sprintf("%s/%s", o.config.Source.BranchPrefix, issue.Identifier)
	worktreePath, err := o.wtManager.NewWorktree(ctx, branch, o.config.BaseBranch, issue.Title)
	if err != nil {
		o.mu.Lock()
		delete(o.active, issue.ID)
		o.mu.Unlock()
		return fmt.Errorf("create worktree for %s: %w", issue.Identifier, err)
	}

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

	wf.OnTransition = func(step WorkflowStep) {
		// Skip terminal steps here — they are emitted by the goroutine
		// below with the proper error attached.
		if step != StepDone && step != StepFailed {
			o.mu.Lock()
			mw.roundIndex = 0
			mw.roundTotal = 0
			o.mu.Unlock()
			o.emitStatus(mw, step, nil)
		}
	}
	wf.OnRoundProgress = func(roundIndex, roundTotal int) {
		o.mu.Lock()
		mw.roundIndex = roundIndex
		mw.roundTotal = roundTotal
		o.mu.Unlock()
		o.emitStatus(mw, wf.state.Current(), nil)
	}

	o.mu.Lock()
	o.active[issue.ID] = mw
	o.mu.Unlock()

	o.emitStatus(mw, StepInit, nil)

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		err := wf.Run(wfCtx)
		if err != nil {
			o.logger.Error("workflow failed", "issue", issue.Identifier, "error", err)
			o.emitStatus(mw, StepFailed, err)
		} else {
			o.logger.Info("workflow completed", "issue", issue.Identifier)
			o.emitStatus(mw, StepDone, nil)
		}
		// Use background context for cleanup so worktree removal
		// succeeds even when the parent context is cancelled (e.g. TUI exit).
		o.cleanup(context.Background(), mw)
	}()

	return nil
}

// activeCountLocked returns the number of active (non-terminal) workflows.
// Caller must hold o.mu.
func (o *Orchestrator) activeCountLocked() int {
	count := 0
	for _, mw := range o.active {
		if mw.workflow == nil {
			// Placeholder reservation, counts as active.
			count++
			continue
		}
		step := mw.workflow.state.Current()
		if step != StepDone && step != StepFailed {
			count++
		}
	}
	return count
}

// Cancel stops the workflow for the given issue ID.
func (o *Orchestrator) Cancel(issueID string) {
	o.mu.RLock()
	mw, ok := o.active[issueID]
	o.mu.RUnlock()
	if ok && mw.cancel != nil {
		mw.cancel()
	}
}

// Shutdown signals that no consumer is reading StatusUpdates anymore,
// then waits for all active workflows to complete. This prevents
// blocking sends from hanging when the TUI has exited. Closing
// statusChan after all workflows finish unblocks any goroutine still
// listening on StatusUpdates (e.g. the TUI's listenForStatus goroutine).
func (o *Orchestrator) Shutdown() {
	close(o.done)
	o.wg.Wait()
	close(o.statusChan)
}

// Wait blocks until all active workflows have completed.
func (o *Orchestrator) Wait() {
	o.wg.Wait()
}

// RunWithDiscovery consumes issues from the discovery channel and starts
// workflows for them, respecting the concurrency limit.
func (o *Orchestrator) RunWithDiscovery(ctx context.Context, discovery *Discovery) error {
	issues := discovery.Run(ctx)
	var pending []*tracker.Issue

	startOrRetry := func(issue *tracker.Issue) {
		if err := o.Start(ctx, issue); err != nil {
			o.logger.Warn("failed to start workflow", "issue", issue.Identifier, "error", err)
			// Clear from discovery's seen set so the issue is re-emitted on next poll.
			discovery.ClearSeen(issue.ID)
		}
	}

	for {
		// Drain pending queue while under the concurrency limit.
		active := o.ActiveCount()
		remaining := pending[:0]
		for _, issue := range pending {
			if active >= o.config.Source.MaxConcurrent {
				remaining = append(remaining, issue)
				continue
			}
			startOrRetry(issue)
			active = o.ActiveCount()
		}
		pending = remaining

		select {
		case <-ctx.Done():
			o.Wait()
			return ctx.Err()
		case <-o.slotFreed:
			// A workflow completed; loop back to drain the pending queue.
		case issue, ok := <-issues:
			if !ok {
				o.Wait()
				return nil
			}
			if o.ActiveCount() < o.config.Source.MaxConcurrent {
				startOrRetry(issue)
			} else {
				pending = append(pending, issue)
			}
		}
	}
}

func (o *Orchestrator) emitStatus(mw *managedWorkflow, step WorkflowStep, err error) {
	status := IssueStatus{
		Issue:        mw.issue,
		Step:         step,
		StartedAt:    mw.startedAt,
		WorktreePath: mw.worktreePath,
		Error:        err,
		RoundIndex:   mw.roundIndex,
		RoundTotal:   mw.roundTotal,
	}
	if status.IsDone() {
		status.CompletedAt = time.Now()
		// Terminal updates should not be dropped. Block unless shutdown
		// has been called (done closed), which means no consumer remains.
		select {
		case o.statusChan <- status:
		case <-o.done:
		}
	} else {
		select {
		case o.statusChan <- status:
		default:
			o.logger.Warn("status channel full, dropping update", "issue", mw.issue.Identifier)
		}
	}
}

// printDryRunCommand prints a `bramble new-session` invocation that would
// start an equivalent planner session for the given issue. The live path
// does not actually shell out to `bramble new-session` — it drives
// `wt.Manager` and the workflow/agent code directly — so the printed
// `--prompt` is a hand-authored starter, not a rendered plan/build prompt.
// Branch, base branch, model, repo, and goal do match the live path.
func (o *Orchestrator) printDryRunCommand(issue *tracker.Issue) {
	branch := fmt.Sprintf("%s/%s", o.config.Source.BranchPrefix, issue.Identifier)
	prompt := fmt.Sprintf("Work on %s: %s", issue.Identifier, issue.Title)
	if issue.URL != nil && *issue.URL != "" {
		prompt = fmt.Sprintf("%s\n\n%s", prompt, *issue.URL)
	}

	sq := shellQuote
	args := []string{
		"bramble new-session",
		"  --type planner",
		"  --create-worktree",
		"  --branch " + sq(branch),
		"  --from " + sq(o.config.BaseBranch),
		"  --model " + sq(o.config.Agent.Model),
	}
	if o.repoName != "" {
		args = append(args, "  --repo "+sq(o.repoName))
	}
	args = append(args,
		"  --goal "+sq(issue.Title),
		"  --prompt "+sq(prompt),
	)

	fmt.Fprintf(o.out, "\n# [dry-run] %s — %s\n", issue.Identifier, issue.Title)
	fmt.Fprintln(o.out, strings.Join(args, " \\\n"))
	fmt.Fprintln(o.out)

	o.logger.Info("dry-run: printed bramble new-session command",
		"identifier", issue.Identifier,
		"branch", branch,
	)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (o *Orchestrator) cleanup(ctx context.Context, mw *managedWorkflow) {
	if err := o.wtManager.RemoveWorktree(ctx, mw.branch, true); err != nil {
		o.logger.Warn("failed to remove worktree", "branch", mw.branch, "error", err)
	}
	o.mu.Lock()
	delete(o.active, mw.issue.ID)
	o.mu.Unlock()
	// Non-blocking signal to RunWithDiscovery so it can drain the pending queue
	// without waiting for the next discovery poll.
	select {
	case o.slotFreed <- struct{}{}:
	default:
	}
}
