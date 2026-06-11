package jiradozer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// errConcurrencyLimit is returned by Start when all concurrency slots are full.
// RunWithDiscovery uses errors.Is to distinguish transient (keep pending) from
// permanent failures (retry with ClearSeen or give up).
var errConcurrencyLimit = errors.New("concurrency limit reached")

// LockLabel is the label attached to an issue when jiradozer claims it.
// This provides defense-in-depth alongside the state transition: even if
// another process polls a different state set, the label signals that the
// issue is already being handled.
const LockLabel = "jiradozer-active"

// OrchestratedEnvVar is set in each child subprocess's environment so the
// child knows it runs under an orchestrator parent and must not post its own
// failure report — the orchestrator is the single reporter for per-issue
// failures. Read by the child CLI (cmd/jiradozer) to suppress its own alert.
const OrchestratedEnvVar = "JIRADOZER_ORCHESTRATED"

// tailRingMax bounds the per-issue tail ring buffer: the last N subprocess
// log lines retained for surfacing on failure. Small on purpose — enough
// context for a human to triage, not a full transcript.
const tailRingMax = 20

// tailLineMax caps the length of any single retained tail line so a child
// printing a huge line (e.g. a base64 blob) can't blow up the ring buffer.
const tailLineMax = 2000

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
	return s.Step.IsTerminal()
}

// PreservedWorktree records a worktree that was not cleaned up after its
// workflow ended.
type PreservedWorktree struct {
	Branch       string
	WorktreePath string
	Issue        string       // issue identifier
	Step         WorkflowStep // step at which the workflow ended
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
	// notifier delivers the external failure alert (e.g. Slack) for per-issue
	// subprocess failures. Nil disables the external sink; the tracker-comment
	// sink (o.tracker) is always available. buildRevision is stamped into the
	// failure report so a stale-deploy failure is obvious from the alert.
	notifier      Notifier
	buildRevision string
	mu            sync.RWMutex
	wg            sync.WaitGroup
	forceCleanup  bool
	preserved     []PreservedWorktree
}

//nolint:govet // fieldalignment: grouping by purpose (lifecycle vs watchdog) is more readable than tighter packing
type managedWorkflow struct {
	startedAt    time.Time
	issue        *tracker.Issue
	cmd          *exec.Cmd
	logFile      *os.File
	logPath      string
	cancel       context.CancelFunc
	worktreePath string
	branch       string
	pid          int
	cancelled    bool
	currentStep  string
	stepMu       sync.Mutex
	// tailRing is a bounded ring buffer of the most recent subprocess log
	// lines, appended by tailSubprocessLog and read by the failure path so
	// an on-call human sees what the child printed before it died without
	// opening the per-issue log file. Guarded by tailMu. tailLines() returns
	// a snapshot in chronological order.
	tailMu   sync.Mutex
	tailRing []string
	// lastOutputAt is unix-nanos of the most recent log line from the
	// subprocess. Written by tailSubprocessLog, read by runWatchdog to
	// detect idle gaps.
	lastOutputAt atomic.Int64
	// tailerAlive is 1 while tailSubprocessLog is reading the log file,
	// 0 once it has exited (EOF on stop, or a non-EOF read error after
	// reopen attempts are exhausted). runWatchdog skips its idle check
	// when the tailer is gone — without fresh updates to lastOutputAt
	// the gap would grow unboundedly and kill a still-healthy subprocess.
	tailerAlive atomic.Bool
	// inReview is 1 between "waiting for approval" and the next "step:"
	// or "feedback:" log line. The workflow legitimately blocks here on
	// human input via PollForFeedback, so the watchdog must skip its
	// idle check during this window — otherwise a long human review
	// would be killed by the prior step's timeout.
	inReview atomic.Bool
}

// appendTail records one subprocess log line in the bounded ring buffer.
// The trailing newline is stripped and over-long lines are truncated so a
// runaway child can't grow memory. Safe for concurrent use.
func (mw *managedWorkflow) appendTail(line string) {
	line = strings.TrimRight(line, "\n")
	if line == "" {
		return
	}
	if len(line) > tailLineMax {
		// Truncate on a rune boundary so the retained line stays valid UTF-8
		// (a tracker comment with a split multibyte rune renders as garbage).
		// Back up from the byte cap to the start of the rune it lands in.
		cut := tailLineMax
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	mw.tailMu.Lock()
	defer mw.tailMu.Unlock()
	mw.tailRing = append(mw.tailRing, line)
	if len(mw.tailRing) > tailRingMax {
		// Drop the oldest lines, keeping the last tailRingMax. Re-slice into
		// a fresh backing array so the dropped lines can be garbage-collected
		// instead of being pinned by a growing underlying array.
		mw.tailRing = append([]string(nil), mw.tailRing[len(mw.tailRing)-tailRingMax:]...)
	}
}

// tailLines returns a chronological snapshot of the retained tail lines.
func (mw *managedWorkflow) tailLines() []string {
	mw.tailMu.Lock()
	defer mw.tailMu.Unlock()
	if len(mw.tailRing) == 0 {
		return nil
	}
	return append([]string(nil), mw.tailRing...)
}

// NewOrchestrator creates a new multi-issue orchestrator.
// repoName is used when formatting dry-run bramble new-session commands;
// pass an empty string in non-dry-run paths and it will be ignored.
func NewOrchestrator(t tracker.IssueTracker, cfg *Config, wtMgr WorktreeManager, repoName string, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		tracker:    t,
		config:     cloneConfig(cfg),
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
	o.mu.Lock()
	defer o.mu.Unlock()
	o.selfPath = selfPath
	o.childArgs = append([]string(nil), childArgs...)
	o.logDir = logDir
}

// SetFailureReporting configures per-issue subprocess-failure alerting. The
// tracker comment sink always uses o.tracker; notifier is the optional
// external sink (e.g. Slack), and buildRevision is stamped into each report.
// Both arguments are optional: a nil notifier and empty revision degrade
// gracefully (ReportFailure is nil-safe; FailureReport omits empty fields).
func (o *Orchestrator) SetFailureReporting(notifier Notifier, buildRevision string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.notifier = notifier
	o.buildRevision = buildRevision
}

// SetForceCleanup controls whether worktrees are deleted when workflows are
// cancelled by the user (Ctrl+C). By default, cancelled worktrees are
// preserved so in-progress work is not lost. When force is true, cancelled
// worktrees are removed on cancellation.
//
// Note: worktrees for successfully completed workflows (StepDone) are always
// preserved regardless of this flag — the ship step opens a PR but does not
// merge it, so the branch must remain until the PR is merged.
func (o *Orchestrator) SetForceCleanup(force bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.forceCleanup = force
}

// UpdateConfig replaces the config used for future discovery decisions and
// child launches. Already-running children keep their original argv/config.
func (o *Orchestrator) UpdateConfig(cfg *Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.config = cloneConfig(cfg)
}

// ConfigSnapshot returns a defensive copy of the current config.
func (o *Orchestrator) ConfigSnapshot() *Config {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return cloneConfig(o.config)
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

// ActiveWorkflowSnapshots returns enough metadata to restore child tracking
// after an exec restart.
func (o *Orchestrator) ActiveWorkflowSnapshots() []ManagedWorkflowSnapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]ManagedWorkflowSnapshot, 0, len(o.active))
	for _, mw := range o.active {
		out = append(out, ManagedWorkflowSnapshot{
			Issue:        cloneIssue(mw.issue),
			PID:          mw.pid,
			Branch:       mw.branch,
			WorktreePath: mw.worktreePath,
			LogPath:      mw.logPath,
			StartedAt:    mw.startedAt,
		})
	}
	return out
}

// ActiveCount returns the number of currently running workflows.
func (o *Orchestrator) ActiveCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.activeCountLocked()
}

func (o *Orchestrator) maxConcurrent() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.config.Source.MaxConcurrent
}

// addLockLabel attaches the LockLabel to the issue. The label is the
// quick part of the distributed claim — it signals intent immediately
// even if the state transition (transitionToInProgress) is deferred or
// fails. Best-effort: errors are logged as warnings and do not abort
// the workflow start.
func (o *Orchestrator) addLockLabel(ctx context.Context, issue *tracker.Issue) {
	if err := o.tracker.AddLabel(ctx, issue.ID, LockLabel); err != nil {
		o.logger.Warn("failed to add lock label to issue",
			"issue", issue.Identifier, "label", LockLabel, "error", err)
		return
	}
	o.logger.Info("claimed issue: added lock label",
		"issue", issue.Identifier, "label", LockLabel)
}

// transitionToInProgress moves the issue into the configured InProgress
// state. Called only after the subprocess has been successfully started
// — earlier callers (between worktree creation and cmd.Start) would
// strand the issue in In Progress on log-open / cmd.Start failures
// because discovery's state-filter would no longer surface it for
// retry, even after addLockLabel cleanup ran. Best-effort: errors are
// logged as warnings.
func (o *Orchestrator) transitionToInProgress(ctx context.Context, issue *tracker.Issue) {
	if issue.TeamID == "" {
		o.logger.Warn("issue has no team ID, skipping in_progress state transition",
			"issue", issue.Identifier)
		return
	}

	cfg := o.ConfigSnapshot()
	states, err := o.tracker.FetchWorkflowStates(ctx, issue.TeamID)
	if err != nil {
		o.logger.Warn("failed to fetch workflow states for in_progress claim",
			"issue", issue.Identifier, "error", err)
		return
	}

	for _, s := range states {
		if s.Name == cfg.States.InProgress {
			if err := o.tracker.UpdateIssueState(ctx, issue.ID, s.ID); err != nil {
				o.logger.Warn("failed to transition issue to in_progress",
					"issue", issue.Identifier, "state", s.Name, "error", err)
			} else {
				o.logger.Info("claimed issue: transitioned to in_progress",
					"issue", issue.Identifier, "state", s.Name)
			}
			return
		}
	}

	o.logger.Warn("in_progress state not found in workflow states",
		"issue", issue.Identifier, "expected_name", cfg.States.InProgress)
}

// releaseLockLabelByID removes LockLabel using only an issue ID/identifier.
// Used by Start() error paths that fail after addLockLabel but before
// the managedWorkflow record exists. Uses a fresh background context
// so the caller's possibly-cancelled context does not strand cleanup.
func (o *Orchestrator) releaseLockLabelByID(issueID, identifier string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := o.tracker.RemoveLabel(ctx, issueID, LockLabel); err != nil {
		o.logger.Warn("failed to remove lock label after start failure",
			"issue", identifier, "label", LockLabel, "error", err)
		return
	}
	o.logger.Info("released issue: removed lock label after start failure",
		"issue", identifier, "label", LockLabel)
}

// Start launches a workflow for the given issue in a new worktree.
// Returns an error if the concurrency limit is reached or worktree creation fails.
func (o *Orchestrator) Start(ctx context.Context, issue *tracker.Issue) error {
	cfg := o.ConfigSnapshot()
	// Dry-run: print the equivalent bramble new-session command and return
	// success without reserving a slot or touching the worktree manager.
	// RunWithDiscovery only clears the seen set on error, so returning nil
	// keeps the issue out of subsequent polls.
	if cfg.Source.DryRun {
		o.printDryRunCommand(issue, cfg)
		return nil
	}

	// Hold the lock for the entire check-and-reserve sequence to prevent
	// TOCTOU races on both the concurrency limit and the duplicate check.
	o.mu.Lock()
	if o.activeCountLocked() >= cfg.Source.MaxConcurrent {
		o.mu.Unlock()
		return fmt.Errorf("%w (%d)", errConcurrencyLimit, cfg.Source.MaxConcurrent)
	}
	if _, exists := o.active[issue.ID]; exists {
		o.mu.Unlock()
		return fmt.Errorf("issue %s already has an active workflow", issue.Identifier)
	}
	// Reserve the slot with a placeholder so concurrent calls see it.
	placeholder := &managedWorkflow{issue: issue}
	o.active[issue.ID] = placeholder
	o.mu.Unlock()

	branch := fmt.Sprintf("%s/%s", cfg.Source.BranchPrefix, issue.Identifier)
	worktreePath, err := o.wtManager.NewWorktree(ctx, branch, cfg.BaseBranch, issue.Title)
	if err != nil {
		o.unreserveSlot(issue.ID)
		return fmt.Errorf("create worktree for %s: %w", issue.Identifier, err)
	}

	// Add the lock label as soon as the worktree exists. The label is the
	// quick part of the distributed claim — it signals intent across
	// processes immediately. The state transition (transitionToInProgress)
	// is intentionally deferred until after cmd.Start succeeds: a failure
	// between here and cmd.Start would otherwise strand the issue in
	// In Progress, which discovery's state filter would never re-surface.
	o.addLockLabel(ctx, issue)

	wfCtx, cancel := context.WithCancel(ctx)

	// Build child process arguments: --issue <ID> --work-dir <path> + propagated flags.
	o.mu.RLock()
	selfPath := o.selfPath
	childArgs := append([]string(nil), o.childArgs...)
	logDir := o.logDir
	o.mu.RUnlock()
	args := make([]string, 0, len(childArgs)+4)
	args = append(args, "--issue", issue.Identifier, "--work-dir", worktreePath)
	args = append(args, childArgs...)

	cmd := exec.CommandContext(wfCtx, selfPath, args...)
	cmd.Dir = worktreePath
	// Mark the child as orchestrated so it suppresses its own failure
	// report: the orchestrator is the single source of truth for per-issue
	// failures (it always observes the death, including watchdog/SIGINT
	// kills the child can't self-report), and it carries the log path + tail
	// the child's self-report lacks. Inherit the rest of the environment.
	cmd.Env = append(os.Environ(), OrchestratedEnvVar+"=1")
	// Graceful shutdown: send SIGINT so the child can clean up, then
	// force-kill after WaitDelay if it hasn't exited.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second

	// Per-issue log file for subprocess stdout/stderr.
	// Sanitize identifier for use as a filename: GitHub identifiers like
	// "acme/app#42" contain "/" and "#" which are problematic in file paths.
	// Suffix with UnixNano + pid so two restarts of the same issue inside
	// a single second still get distinct files — otherwise the tailer
	// would replay the prior run's lines and mis-set currentStep/inReview
	// for the new run.
	safeID := sanitizeForFilename(issue.Identifier)
	now := time.Now()
	logPath := filepath.Join(logDir, fmt.Sprintf("%s-%s-%d-%d.log",
		safeID, now.Format("20060102-150405"), now.UnixNano(), os.Getpid()))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		cancel()
		if removeErr := o.wtManager.RemoveWorktree(context.Background(), branch, true); removeErr != nil {
			o.logger.Warn("failed to remove worktree after log open failure", "branch", branch, "error", removeErr)
		}
		// addLockLabel already attached LockLabel above. The state transition
		// has not yet happened (it's deferred until after cmd.Start), so we
		// only need to remove the label to fully roll back the claim and
		// keep the issue rediscoverable.
		o.releaseLockLabelByID(issue.ID, issue.Identifier)
		o.unreserveSlot(issue.ID)
		return fmt.Errorf("open log file for %s: %w", issue.Identifier, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		cancel()
		// Clean up the worktree that was already created before we failed.
		if removeErr := o.wtManager.RemoveWorktree(context.Background(), branch, true); removeErr != nil {
			o.logger.Warn("failed to remove worktree after start failure", "branch", branch, "error", removeErr)
		}
		// State transition has not yet happened — only the label needs
		// rolling back here. transitionToInProgress runs only after we
		// know the subprocess actually started.
		o.releaseLockLabelByID(issue.ID, issue.Identifier)
		o.unreserveSlot(issue.ID)
		return fmt.Errorf("start subprocess for %s: %w", issue.Identifier, err)
	}

	// Subprocess is now running. Commit to the In Progress state so other
	// jiradozer processes polling state-filtered queries stop discovering
	// the issue. Done here (not earlier) so that any failure on the path
	// from worktree creation to here leaves the issue rediscoverable —
	// only the easily-removable label is added before this point.
	o.transitionToInProgress(ctx, issue)

	mw := &managedWorkflow{
		issue:        issue,
		cancel:       cancel,
		worktreePath: worktreePath,
		branch:       branch,
		startedAt:    time.Now(),
		cmd:          cmd,
		logFile:      logFile,
		logPath:      logPath,
		pid:          cmd.Process.Pid,
	}

	o.mu.Lock()
	o.active[issue.ID] = mw
	o.mu.Unlock()

	// Seed lastOutputAt to startup time so the watchdog has a sensible
	// baseline before any log lines are tailed.
	mw.lastOutputAt.Store(time.Now().UnixNano())
	// Mark tailer as alive before launching; tailSubprocessLog clears this
	// flag on exit so runWatchdog can skip stale idle gaps.
	mw.tailerAlive.Store(true)

	o.emitStatus(mw, StepInit, nil)
	o.logger.Info("subprocess started",
		"issue", issue.Identifier,
		"pid", cmd.Process.Pid,
		"log", logPath,
	)

	// Tailer + watchdog share a stop channel; both exit when cmd.Wait
	// returns. The tailer re-emits step transitions on the parent log and
	// updates mw.lastOutputAt; the watchdog cancels the workflow context
	// when the gap exceeds the current step's IdleTimeout.
	stop := make(chan struct{})
	go o.tailSubprocessLog(mw, logPath, stop)
	go o.runWatchdog(mw, watchdogTickInterval, stop)

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer logFile.Close()
		defer close(stop)
		err := cmd.Wait()
		switch {
		case wfCtx.Err() != nil:
			// Context was cancelled (user pressed Ctrl+C). Check this first:
			// a child that traps SIGINT and exits 0 would otherwise be
			// misclassified as StepDone.
			o.logger.Warn("subprocess cancelled", "issue", issue.Identifier, "error", err)
			o.emitStatus(mw, StepCancelled, wfCtx.Err())
			o.cleanup(context.Background(), mw, StepCancelled)
		case err == nil:
			o.logger.Info("subprocess completed", "issue", issue.Identifier)
			o.emitStatus(mw, StepDone, nil)
			o.cleanup(context.Background(), mw, StepDone)
		default:
			o.logger.Error("subprocess failed",
				"issue", issue.Identifier,
				"error", err,
				"log", mw.logPath,
				"tail", strings.Join(mw.tailLines(), " ⏎ "),
			)
			o.reportSubprocessFailure(mw, err)
			o.emitStatus(mw, StepFailed, err)
			o.cleanup(context.Background(), mw, StepFailed)
		}
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
	if !ok {
		return
	}
	if mw.cancel != nil {
		mw.cancel()
		return
	}
	if mw.pid > 0 {
		o.mu.Lock()
		mw.cancelled = true
		o.mu.Unlock()
		if proc, err := os.FindProcess(mw.pid); err == nil {
			_ = proc.Signal(os.Interrupt)
		}
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

// PreservedWorktrees returns the list of worktrees that were preserved after
// their workflows ended. This includes both successful completions (StepDone —
// waiting for the PR to be merged) and cancellations (StepCancelled — preserving
// in-progress work). Only meaningful after Wait or Shutdown has returned.
func (o *Orchestrator) PreservedWorktrees() []PreservedWorktree {
	o.mu.RLock()
	defer o.mu.RUnlock()
	cp := make([]PreservedWorktree, len(o.preserved))
	copy(cp, o.preserved)
	return cp
}

// RestorePreservedWorktrees restores preserved-worktree reporting state after
// an exec restart.
func (o *Orchestrator) RestorePreservedWorktrees(preserved []PreservedWorktree) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.preserved = append(o.preserved, preserved...)
}

// maxStartFailures is the number of consecutive non-transient Start failures
// allowed for a single issue before RunWithDiscovery gives up and stops
// clearing it from the seen set (preventing an infinite retry storm).
const maxStartFailures = 3

// RunWithDiscovery consumes issues from the discovery channel and starts
// workflows for them, respecting the concurrency limit.
func (o *Orchestrator) RunWithDiscovery(ctx context.Context, discovery *Discovery) error {
	issues := discovery.Run(ctx)
	var pending []*tracker.Issue
	// failCounts tracks consecutive permanent Start failures per issue ID.
	// Transient failures (concurrency limit) are not counted here; they are
	// handled by re-queuing into pending instead of clearing the seen set.
	failCounts := make(map[string]int)

	// tryStart attempts to start the issue workflow. Returns true if the issue
	// should be removed from the pending queue (either started successfully or
	// permanently failed). Returns false if it should stay pending (transient).
	tryStart := func(issue *tracker.Issue) bool {
		err := o.Start(ctx, issue)
		if err == nil {
			delete(failCounts, issue.ID)
			return true
		}
		if errors.Is(err, errConcurrencyLimit) {
			// Transient: slots are full. Keep in pending; do not clear seen.
			return false
		}
		// "worktree already exists" means a prior run already claimed this issue.
		// Clearing the seen set would cause an infinite rediscovery storm —
		// treat it as a terminal signal that the issue is already handled.
		if strings.Contains(err.Error(), "worktree already exists") {
			o.logger.Warn("worktree already exists, issue is already being handled — suppressing",
				"issue", issue.Identifier,
				"error", err,
			)
			return true
		}
		// Other permanent errors: retry up to maxStartFailures, then give up.
		failCounts[issue.ID]++
		if failCounts[issue.ID] < maxStartFailures {
			o.logger.Warn("failed to start workflow, will retry",
				"issue", issue.Identifier,
				"error", err,
				"attempt", failCounts[issue.ID],
				"max", maxStartFailures,
			)
			discovery.ClearSeen(issue.ID)
		} else {
			o.logger.Error("failed to start workflow, giving up after repeated failures",
				"issue", issue.Identifier,
				"error", err,
				"failures", failCounts[issue.ID],
			)
			// Do NOT clear seen — leave the issue suppressed to stop the storm.
		}
		return true
	}

	for {
		// Drain pending queue while under the concurrency limit.
		maxConcurrent := o.maxConcurrent()
		active := o.ActiveCount()
		remaining := pending[:0]
		for _, issue := range pending {
			if active >= maxConcurrent {
				remaining = append(remaining, issue)
				continue
			}
			if !tryStart(issue) {
				remaining = append(remaining, issue)
				continue
			}
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
			if o.ActiveCount() < maxConcurrent {
				if !tryStart(issue) {
					pending = append(pending, issue)
				}
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
	}
	// Status updates are part of the observable workflow state. Do not drop
	// non-terminal updates either; restored workflows rely on StepInit reaching
	// the supervisor after an exec restart.
	select {
	case o.statusChan <- status:
	case <-o.done:
	}
}

// printDryRunCommand prints a `bramble new-session` invocation that would
// start an equivalent planner session for the given issue. The live path
// does not actually shell out to `bramble new-session` — it drives
// `wt.Manager` and the workflow/agent code directly — so the printed
// `--prompt` is a hand-authored starter, not a rendered plan/build prompt.
// Branch, base branch, model, repo, and goal do match the live path.
func (o *Orchestrator) printDryRunCommand(issue *tracker.Issue, cfg *Config) {
	branch := fmt.Sprintf("%s/%s", cfg.Source.BranchPrefix, issue.Identifier)
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
		"  --from " + sq(cfg.BaseBranch),
		"  --model " + sq(cfg.Agent.Model),
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

// cleanup removes the issue from the active map and signals a free slot.
// Worktree removal depends on how the workflow ended:
//   - StepDone: worktree is preserved — the ship step opens a PR but does not
//     merge it, so the branch must remain until the PR is merged.
//   - StepCancelled: worktree is preserved by default so in-progress work is
//     not lost; set forceCleanup to remove it unconditionally.
//   - StepFailed: worktree is preserved by default so the operator can
//     inspect the failure and keep any pushed branch / open PR created by
//     earlier steps; set forceCleanup to wipe it.
func (o *Orchestrator) cleanup(ctx context.Context, mw *managedWorkflow, step WorkflowStep) {
	// Always release the lock label, regardless of how the workflow ended.
	// The label is added in addLockLabel before subprocess start and was
	// previously never removed on completion — every successful run
	// leaked it, blocking re-discovery.
	o.releaseLockLabel(mw)

	o.mu.RLock()
	forceCleanup := o.forceCleanup
	o.mu.RUnlock()
	preserve := step == StepDone || ((step == StepCancelled || step == StepFailed) && !forceCleanup)
	if preserve {
		o.logger.Info("preserving worktree",
			"step", step,
			"branch", mw.branch,
			"worktree", mw.worktreePath,
			"issue", mw.issue.Identifier,
		)
		o.mu.Lock()
		o.preserved = append(o.preserved, PreservedWorktree{
			Branch:       mw.branch,
			WorktreePath: mw.worktreePath,
			Issue:        mw.issue.Identifier,
			Step:         step,
		})
		o.mu.Unlock()
	} else {
		if err := o.wtManager.RemoveWorktree(ctx, mw.branch, true); err != nil {
			o.logger.Warn("failed to remove worktree", "branch", mw.branch, "error", err)
		}
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

// RestoreActive starts wait goroutines for active child processes restored
// after syscall.Exec. The restored children must still be children of this
// process; that is true for an in-place exec restart.
func (o *Orchestrator) RestoreActive(snapshots []ManagedWorkflowSnapshot) []string {
	restoredIssueIDs := make([]string, 0, len(snapshots))
	for _, snap := range snapshots {
		if snap.PID <= 0 || snap.Issue == nil || snap.Issue.ID == "" {
			continue
		}
		mw := &managedWorkflow{
			issue:        cloneIssue(snap.Issue),
			startedAt:    snap.StartedAt,
			worktreePath: snap.WorktreePath,
			branch:       snap.Branch,
			logPath:      snap.LogPath,
			pid:          snap.PID,
		}
		o.mu.Lock()
		if _, exists := o.active[mw.issue.ID]; exists {
			o.mu.Unlock()
			continue
		}
		o.active[mw.issue.ID] = mw
		o.mu.Unlock()
		restoredIssueIDs = append(restoredIssueIDs, mw.issue.ID)
		o.emitStatus(mw, StepInit, nil)
		o.wg.Add(1)
		go o.waitRestored(mw)
	}
	return restoredIssueIDs
}

func (o *Orchestrator) waitRestored(mw *managedWorkflow) {
	defer o.wg.Done()
	var status syscall.WaitStatus
	var usage syscall.Rusage
	_, err := syscall.Wait4(mw.pid, &status, 0, &usage)
	switch {
	case err != nil:
		o.logger.Error("restored subprocess wait failed", "issue", mw.issue.Identifier, "pid", mw.pid, "error", err, "log", mw.logPath)
		o.reportSubprocessFailure(mw, err)
		o.emitStatus(mw, StepFailed, err)
		o.cleanup(context.Background(), mw, StepFailed)
	case status.Exited() && status.ExitStatus() == 0:
		o.logger.Info("restored subprocess completed", "issue", mw.issue.Identifier, "pid", mw.pid)
		o.emitStatus(mw, StepDone, nil)
		o.cleanup(context.Background(), mw, StepDone)
	case o.wasCancelled(mw) || (status.Signaled() && status.Signal() == syscall.SIGINT):
		o.logger.Info("restored subprocess cancelled", "issue", mw.issue.Identifier, "pid", mw.pid)
		o.emitStatus(mw, StepCancelled, nil)
		o.cleanup(context.Background(), mw, StepCancelled)
	default:
		err := fmt.Errorf("subprocess pid %d exited with wait status %d", mw.pid, status)
		o.logger.Error("restored subprocess failed", "issue", mw.issue.Identifier, "pid", mw.pid, "status", int(status), "log", mw.logPath)
		o.reportSubprocessFailure(mw, err)
		o.emitStatus(mw, StepFailed, err)
		o.cleanup(context.Background(), mw, StepFailed)
	}
}

// reportSubprocessFailure fans a per-issue subprocess failure out to the
// configured sinks (tracker comment + optional notifier), carrying the child
// log path and a tail of its final output so an on-call human can triage
// without grepping the log directory. Best-effort: ReportFailure never errors
// and a nil notifier / unavailable tracker simply skips that sink.
//
// The orchestrator is the single reporter for per-issue failures — children
// launched under it suppress their own report (see OrchestratedEnvVar) — so a
// cleanly-failing child does not double-comment.
func (o *Orchestrator) reportSubprocessFailure(mw *managedWorkflow, err error) {
	o.mu.RLock()
	notifier := o.notifier
	buildRevision := o.buildRevision
	o.mu.RUnlock()

	step := FailingStepFromError(err)
	if step == "" {
		// The child error from the orchestrator's vantage point is a bare
		// "exit status N" with no step prefix, so fall back to the last step
		// the tailer observed.
		mw.stepMu.Lock()
		step = mw.currentStep
		mw.stepMu.Unlock()
	}

	report := FailureReport{
		Tool:          "jiradozer",
		Target:        mw.issue.Identifier,
		Step:          step,
		Err:           err,
		BuildRevision: buildRevision,
		LogPath:       mw.logPath,
		LogTail:       mw.tailLines(),
	}
	ReportFailure(context.Background(), o.logger, o.tracker, mw.issue.ID, notifier, report)
}

func (o *Orchestrator) wasCancelled(mw *managedWorkflow) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return mw.cancelled
}

// sanitizeForFilename replaces characters that are problematic in file paths
// (path separators, shell metacharacters) with underscores.
func sanitizeForFilename(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', '#', ' ', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		default:
			return r
		}
	}, s)
}
