package jiradozer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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

// IssueStatus represents the current state of a tracked issue's subprocess.
type IssueStatus struct {
	Issue        *tracker.Issue
	Error        error
	StartedAt    time.Time
	CompletedAt  time.Time
	WorktreePath string
	Step         WorkflowStep
}

// IsDone returns true if the workflow has completed (successfully or with failure).
func (s IssueStatus) IsDone() bool {
	return s.Step == StepDone || s.Step == StepFailed
}

// Orchestrator manages concurrent issue workflows, each in its own worktree.
//
//nolint:govet // fieldalignment: sync types at the end require padding
type Orchestrator struct {
	tracker    tracker.IssueTracker
	wtManager  WorktreeManager
	out        io.Writer
	config     *Config
	logger     *slog.Logger
	active     map[string]*managedWorkflow
	statusChan chan IssueStatus
	slotFreed  chan struct{}
	done       chan struct{}
	childArgs  []string
	repoName   string
	selfPath   string
	logDir     string
	mu         sync.RWMutex
	wg         sync.WaitGroup
}

type managedWorkflow struct {
	issue        *tracker.Issue
	cmd          *exec.Cmd
	logFile      *os.File
	cancel       context.CancelFunc
	startedAt    time.Time
	worktreePath string
	branch       string
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

// SetSubprocessMode configures the orchestrator to spawn child jiradozer
// processes instead of running workflows in-process. selfPath is the path
// to the jiradozer binary, childArgs are flags propagated to each child
// (e.g. --config, --model), and logDir is the directory for per-issue logs.
func (o *Orchestrator) SetSubprocessMode(selfPath string, childArgs []string, logDir string) {
	o.selfPath = selfPath
	o.childArgs = childArgs
	o.logDir = logDir
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
			Step:         StepInit,
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
		o.unreserveSlot(issue.ID)
		return fmt.Errorf("create worktree for %s: %w", issue.Identifier, err)
	}

	wfCtx, cancel := context.WithCancel(ctx)

	// Build child process arguments: --issue <ID> --work-dir <path> + propagated flags.
	args := make([]string, 0, len(o.childArgs)+4)
	args = append(args, "--issue", issue.Identifier, "--work-dir", worktreePath)
	args = append(args, o.childArgs...)

	cmd := exec.CommandContext(wfCtx, o.selfPath, args...)
	cmd.Dir = worktreePath
	// Graceful shutdown: send SIGINT so the child can clean up, then
	// force-kill after WaitDelay if it hasn't exited.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second

	// Per-issue log file for subprocess stdout/stderr.
	logPath := filepath.Join(o.logDir, fmt.Sprintf("%s-%s.log",
		issue.Identifier, time.Now().Format("20060102-150405")))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		cancel()
		o.unreserveSlot(issue.ID)
		return fmt.Errorf("open log file for %s: %w", issue.Identifier, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		cancel()
		o.unreserveSlot(issue.ID)
		return fmt.Errorf("start subprocess for %s: %w", issue.Identifier, err)
	}

	mw := &managedWorkflow{
		issue:        issue,
		cancel:       cancel,
		worktreePath: worktreePath,
		branch:       branch,
		startedAt:    time.Now(),
		cmd:          cmd,
		logFile:      logFile,
	}

	o.mu.Lock()
	o.active[issue.ID] = mw
	o.mu.Unlock()

	o.emitStatus(mw, StepInit, nil)
	o.logger.Info("subprocess started",
		"issue", issue.Identifier,
		"pid", cmd.Process.Pid,
		"log", logPath,
	)

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer logFile.Close()
		err := cmd.Wait()
		if err != nil {
			o.logger.Error("subprocess failed", "issue", issue.Identifier, "error", err)
			o.emitStatus(mw, StepFailed, err)
		} else {
			o.logger.Info("subprocess completed", "issue", issue.Identifier)
			o.emitStatus(mw, StepDone, nil)
		}
		o.cleanup(context.Background(), mw)
	}()

	return nil
}

// activeCountLocked returns the number of active subprocesses.
// Caller must hold o.mu.
func (o *Orchestrator) activeCountLocked() int {
	return len(o.active)
}

// unreserveSlot removes a placeholder reservation and signals that
// a slot is free so RunWithDiscovery can drain pending issues.
func (o *Orchestrator) unreserveSlot(issueID string) {
	o.mu.Lock()
	delete(o.active, issueID)
	o.mu.Unlock()
	select {
	case o.slotFreed <- struct{}{}:
	default:
	}
}

// Cancel stops the subprocess for the given issue ID.
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
