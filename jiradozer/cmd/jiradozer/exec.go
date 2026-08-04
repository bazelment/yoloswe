package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/github"
	"github.com/bazelment/yoloswe/jiradozer/tracker/local"
	"github.com/bazelment/yoloswe/wt"
)

// execArgs are the flags for a single self-contained task execution.
type execArgs struct {
	issueID      string
	description  string
	taskID       string
	repo         string
	configPath   string
	branchPrefix string
	modelID      string
	skipPhases   string
	autoApprove  string
	tmuxSession  string
	maxBudget    float64
	force        bool
}

// newExecCmd builds the worker that a dispatcher places on a box.
//
// It is deliberately self-contained: everything the team-mode orchestrator does
// around a child subprocess — claim the issue, create the worktree, record what
// happened, report a failure — happens inside this one process instead. That is
// what lets it be dropped on any host with no supervisor watching it.
func newExecCmd(args *execArgs) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Run one task end-to-end on this box",
		Long: "Claim an issue, create its worktree, run the workflow, and record the outcome.\n\n" +
			"Unlike `run`, this owns the whole lifecycle, so it needs no parent process.\n" +
			"Progress is written to a run-log (see `jiradozer runs`) and mirrored to the\n" +
			"issue as a start and an end comment.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app := cliapp.FromContext(cmd.Context())
			return runExec(cmd.Context(), app, *args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&args.issueID, "issue", "", "Issue identifier to work (e.g. INF-1234)")
	f.StringVar(&args.description, "description", "", "Ad-hoc task description; uses the local tracker instead of an issue")
	f.StringVar(&args.taskID, "task-id", "", "Correlation id from the dispatcher's task list")
	f.StringVar(&args.repo, "repo", "", "wt-managed repository name (required)")
	f.StringVar(&args.branchPrefix, "branch-prefix", "", "Override source.branch_prefix")
	f.StringVar(&args.modelID, "model", "", "Override agent.model")
	f.StringVar(&args.skipPhases, "skip-phases", "", "Comma-separated phases to skip")
	f.StringVar(&args.autoApprove, "auto-approve", "", "Auto-approve review gates")
	f.StringVar(&args.tmuxSession, "tmux-session", "", "Record the tmux session this run was dispatched into")
	f.Float64Var(&args.maxBudget, "max-budget", 0, "Override max_budget_usd")
	f.BoolVar(&args.force, "force", false, "Proceed even when the issue is already claimed by another host")
	// There is deliberately no --keep-worktree here. exec ALWAYS keeps its
	// worktree — its terminal state is "a PR is open", which authorises nothing
	// — and only `jiradozer gc` ever reclaims one, by asking whether the PR
	// landed. A flag that cannot change either answer would only promise a
	// control that does not exist.
	return cmd
}

// loadExecConfig reuses run's loader so exec and run cannot drift in how they
// interpret a config file, then applies exec's own overrides.
//
// --description forces the local tracker and auto-approves every gate, matching
// run's behaviour: an ad-hoc task has no human watching a review gate, and a
// dispatched one has no terminal to watch it from either.
func loadExecConfig(args execArgs) (*jiradozer.Config, error) {
	cfg, err := loadRunConfig(runArgs{
		configPath:  args.configPath,
		description: args.description,
		issueID:     args.issueID,
		modelID:     args.modelID,
		maxBudget:   args.maxBudget,
		skipPhases:  args.skipPhases,
		autoApprove: args.autoApprove,
	})
	if err != nil {
		return nil, err
	}
	if args.branchPrefix != "" {
		cfg.Source.BranchPrefix = args.branchPrefix
	}
	return cfg, nil
}

// resolveExecWTManager builds a wt.Manager for an explicitly named repo.
//
// The repo cannot be inferred here the way `run` infers it. wt.GetCurrentRepoName
// resolves from os.Getwd() and falls back to `git remote get-url origin` in the
// cwd — but a dispatched worker starts under `tmux new-session -d`, whose cwd is
// $HOME: not a wt worktree, not a git repo. Team mode never hit this because its
// supervisor always ran from inside the repo. So --repo is required, and saying
// so plainly beats failing later with "not in a wt-managed repository".
func resolveExecWTManager(repo string) (*wt.Manager, error) {
	if repo == "" {
		return nil, errors.New("--repo is required: a dispatched worker starts in $HOME, so the repository cannot be inferred from the working directory")
	}
	root, err := resolveWTRoot()
	if err != nil {
		return nil, err
	}
	return wt.NewManager(root, repo), nil
}

func runExec(ctx context.Context, app *cliapp.App, args execArgs) (runErr error) {
	logger := app.Logger

	// Under an orchestrator, children suppress their own failure report because
	// the parent is the single reporter. exec has no parent, so inheriting that
	// variable would silence every failure on the fleet with nothing left to
	// speak for it. Refuse rather than run un-alerted.
	if os.Getenv(jiradozer.OrchestratedEnvVar) != "" {
		return fmt.Errorf("%s is set: exec owns its own lifecycle and must not run under an orchestrator", jiradozer.OrchestratedEnvVar)
	}
	if (args.issueID == "") == (args.description == "") {
		return errors.New("exactly one of --issue or --description is required")
	}

	wtMgr, err := resolveExecWTManager(args.repo)
	if err != nil {
		return err
	}

	cfg, err := loadExecConfig(args)
	if err != nil {
		return err
	}

	runID, err := jiradozer.NewRunID()
	if err != nil {
		return err
	}

	// The lease target is whatever names this task on this box. It is taken
	// FIRST: it is kernel-backed, so it is the one claim that is correct even
	// when this process is SIGKILLed.
	lease, err := jiradozer.AcquireLease(leaseTarget(args))
	if err != nil {
		return err
	}
	// Released last, after the run-log is final, so a reader that observes "no
	// lease held" also observes a settled meta.json rather than a torn view.
	defer func() {
		if err := lease.Release(); err != nil {
			logger.Warn("failed to release lease", "path", lease.Path(), "error", err)
		}
	}()

	x := &execRun{
		app:      app,
		logger:   logger,
		cfg:      cfg,
		args:     args,
		wtMgr:    wtMgr,
		runID:    runID,
		lookupPR: defaultPRLookup,
	}

	// exec owns its own failure reporting, for the same reason it refuses to run
	// under an orchestrator: nothing else is watching. The lease release
	// deferred above still runs after this one.
	defer x.reportExit(ctx, &runErr)

	return x.run(ctx)
}

// reportExit is runExec's exit hook: it must be deferred, never called, because
// its recover only fires from a deferred frame.
//
// The recover lives HERE, in the same defer that reports the ordinary error,
// and that is the point: a panic leaves runErr nil, so the report is skipped on
// the one failure least likely to be noticed without an alert. Splitting "name
// the panic" and "send the report" across two defers would make the alert
// depend on their relative order — a constraint nothing in the file states and
// any later edit could silently swap. Here both branches call the same
// reporter, so there is no order to get wrong.
//
// runErr is taken by pointer because a deferred call's arguments are evaluated
// at defer time: by value it would always report the nil the named return
// started as, never the error the function is exiting with.
//
// The panic is re-raised, matching run()'s own recover: reporting a bug is not
// the same as surviving it.
//
//nolint:gocritic // ptrToRefParam: see above, the pointer is load-bearing.
func (x *execRun) reportExit(ctx context.Context, runErr *error) {
	if p := recover(); p != nil {
		x.reportFailure(ctx, fmt.Errorf("panic: %v", p))
		panic(p)
	}
	x.reportFailure(ctx, *runErr)
}

// reportFailure sends this run's EXTERNAL failure alert, or nothing.
//
// The external sink is the one that matters: finish() already writes the error
// to the run-log and posts it as the end comment, so passing a tracker poster
// here as well would duplicate that comment on every failure. A cancellation is
// a stop rather than a failure and is filtered out by shouldReportFailure —
// which also makes nil the no-op, so the caller needs no branch of its own.
func (x *execRun) reportFailure(ctx context.Context, runErr error) {
	if !shouldReportFailure(runErr) {
		return
	}
	var notifier jiradozer.Notifier
	if x.cfg.Notify.SlackWebhook != "" {
		notifier = jiradozer.SlackWebhookNotifier{WebhookURL: x.cfg.Notify.SlackWebhook}
	}
	jiradozer.ReportFailure(ctx, x.logger, nil, "", notifier, jiradozer.FailureReport{
		Tool:          "jiradozer",
		Target:        x.reportTarget(),
		Step:          jiradozer.FailingStepFromError(runErr),
		Err:           runErr,
		BuildRevision: x.app.Build.ShortRevision(),
		LogPath:       x.app.LogPath,
	})
}

// execRun holds one task execution's resolved dependencies.
type execRun struct {
	app     *cliapp.App
	logger  *slog.Logger
	cfg     *jiradozer.Config
	wtMgr   *wt.Manager
	rl      *jiradozer.RunLog
	tracker tracker.IssueTracker
	issue   *tracker.Issue
	// lookupPR resolves the PR opened for a branch. Injected so the recording
	// path can be tested without a GitHub round trip.
	lookupPR func(ctx context.Context, branch, dir string) (*wt.PRInfo, error)
	// newTracker builds the issue tracker client. Injected so the claim
	// ORDERING — run-log written first, label attached next, worktree last —
	// can be driven through run() itself rather than asserted on the pieces,
	// which is the only way the ordering stays enforced.
	newTracker func(cfg *jiradozer.Config, issueID string) (tracker.IssueTracker, error)
	// worktreePath is set only once wt has actually produced a checkout. It is
	// the sentinel for "a directory exists that someone could be sent to":
	// cfg.WorkDir cannot serve that purpose, because it starts life as the
	// configured base directory (LoadConfig defaults it to ".") and is only
	// overwritten with the real path on success.
	worktreePath string
	runID        string
	branch       string
	args         execArgs
	// claimed records that THIS run attached the lock label. It is the only
	// thing that authorises removing it again: see claim() and finish().
	claimed bool
}

// defaultPRLookup asks gh which PR exists for a branch, from inside the
// worktree so the repository is unambiguous.
func defaultPRLookup(ctx context.Context, branch, dir string) (*wt.PRInfo, error) {
	return wt.GetPRByBranch(ctx, &wt.DefaultGHRunner{}, branch, dir)
}

// reportTarget names this run in a failure alert. It prefers the resolved issue
// identifier, then whatever the caller named, so an alert is never anonymous
// even when the failure happened before the issue was fetched.
func (x *execRun) reportTarget() string {
	if x.issue != nil && x.issue.Identifier != "" {
		return x.issue.Identifier
	}
	if x.args.issueID != "" {
		return x.args.issueID
	}
	if x.args.taskID != "" {
		return x.args.taskID
	}
	return describeTarget(x.args.description)
}

func (x *execRun) run(ctx context.Context) (runErr error) {
	// For a tracker-backed run the client is needed before the worktree, to
	// fetch the issue and read its claim. For --description the local tracker
	// is rooted inside the run directory, which does not exist yet, so it is
	// built later. The two orders are why exec cannot reuse run()'s flow.
	if x.args.issueID != "" {
		newTracker := x.newTracker
		if newTracker == nil {
			newTracker = createTracker
		}
		t, err := newTracker(x.cfg, x.args.issueID)
		if err != nil {
			return err
		}
		x.tracker = t

		issue, err := t.FetchIssue(ctx, x.args.issueID)
		if err != nil {
			return fmt.Errorf("fetch issue: %w", err)
		}
		x.issue = issue
		if err := x.checkNotClaimed(issue); err != nil {
			return err
		}
	}

	// Name the branch before recording the run: the branch is what makes the
	// checkout's path knowable in advance, and the path is what gc needs.
	x.deriveBranch()

	// The run-log goes in BEFORE the checkout, never after it. The run-log IS
	// gc's ownership namespace — these are ordinary wt worktrees sitting beside
	// human-owned ones, so gc's only discovery path is walking run-logs, and it
	// skips any run directory without a readable meta.json. A checkout created
	// before that first write is therefore a directory nothing can ever name:
	// no amount of later cleanup finds it, because nothing records that it is
	// ours. Writing the run-log first makes the window unreachable rather than
	// merely narrow — a SIGKILL, a lost power rail or a wedged post-create hook
	// inside `wt.New` all leave a record gc can act on.
	if err := x.startRunLog(); err != nil {
		return err
	}

	// From here the run is recorded, so every exit path settles the run-log —
	// including the one that returns nothing. A panic unwinding through here
	// leaves the named return nil, so a plain finish(runErr) settles meta.json
	// as `done` and writes a `run_finished` event whose detail says the run
	// succeeded, while the process is in the middle of crashing. A dispatcher
	// on another box reads only those two records, and cannot tell that run
	// apart from one that actually shipped.
	defer func() {
		p := recover()
		if p == nil {
			x.finish(runErr)
			return
		}
		// The stack goes to the log, not into the error: the error text also
		// becomes meta.json's failure reason and the issue's end comment, and
		// neither is improved by a hundred frames. Captured inside the deferred
		// call, where the panicking frames are still on the stack.
		x.logger.Error("run panicked", "run_id", x.runID, "panic", p, "stack", string(debug.Stack()))
		x.finish(fmt.Errorf("panic: %v", p))
		// Re-raised, never swallowed. A panic is a bug, and a run that quietly
		// downgraded one to a failed status would hide it from everything above
		// — the point of recovering here is only to settle the record first, so
		// the crash stops being silent, not to survive it.
		panic(p)
	}()

	// Claim after the run-log and before the worktree.
	//
	// Before the worktree, because the label is the only claim another box can
	// see — the flock lease is per-host — and every instant between the check
	// above and the label being attached is a window in which a second dispatch
	// reads an unclaimed issue and starts a duplicate run. Claiming after
	// createWorktree stretched that window across a full checkout (clone,
	// hooks, minutes); here it is one local file write plus the round trip
	// below. It does not CLOSE the window: no tracker offers a compare-and-set,
	// so two workers can still both read "unclaimed" and both label.
	//
	// After the run-log, because the two orders fail differently under a hard
	// kill, and only one of them is recoverable. Claim first and a SIGKILL in
	// the gap leaves a label on the issue with no record anywhere saying who
	// took it or why — every later dispatch refuses the issue until a human
	// finds it and passes --force. Run-log first and the same kill leaves a
	// record with a stale heartbeat and no label: `runs` and `gc` can both see
	// it, and nothing is blocked meanwhile.
	//
	// FATAL, unlike every other tracker write in this file. Proceeding past a
	// failed AddLabel would build the worktree with no fleet-visible claim at
	// all, which is the exact state this ordering exists to prevent — worse
	// than the old ordering, because it looks claimed and is not. Nothing is
	// abandoned on this path: the label was never attached, and removing one on
	// a failed add could strip a claim another host owns. The run-log that now
	// exists is settled by the deferred finish() above.
	if x.issue != nil && x.args.issueID != "" {
		if err := x.claim(ctx); err != nil {
			return fmt.Errorf("claim %s: %w", x.issue.Identifier, err)
		}
	}

	heartbeatCtx, stopBeating := context.WithCancel(ctx)
	stopHeartbeat := x.rl.StartHeartbeat(heartbeatCtx, jiradozer.HeartbeatInterval, func(err error) {
		x.logger.Warn("heartbeat write failed", "error", err)
	})
	defer func() {
		stopBeating()
		// Wait for the beat goroutine before finish() writes the terminal meta,
		// or a late beat could resurrect the heartbeat of a finished run.
		stopHeartbeat()
	}()

	// Under the heartbeat, not before it: a clone with post-create hooks runs
	// for minutes, and a run that is not beating during them reads as dead to
	// anything asking `jiradozer runs` whether this box is still working.
	if err := x.createWorktree(ctx); err != nil {
		return err
	}

	if x.args.description != "" {
		if err := x.createLocalIssue(); err != nil {
			return err
		}
		// The local tracker lives inside this run's directory, so its issue
		// cannot exist any earlier and its label is a per-run record rather
		// than a cross-host claim. For a --description run the fleet-wide
		// exclusion is `dispatch`'s lease probe on the derived adhoc- target
		// and nothing else.
		//
		// Best-effort here precisely because it excludes nothing: the run-log
		// and the worktree already exist, and killing a live run over a label
		// that no other host can even read would cost more than it protects.
		if err := x.claim(ctx); err != nil {
			x.logger.Warn("failed to label the local issue", "issue", x.issue.Identifier, "error", err)
		}
	}

	x.postStartComment(ctx)

	wf := jiradozer.NewWorkflow(x.tracker, x.issue, x.cfg, x.logger)
	wf.SetRenderer(x.app.Renderer)
	wf.OnTransition = x.recordPhase
	return wf.Run(ctx)
}

// recordPhase mirrors a workflow step into the run-log.
//
// Phase is the only thing that makes a remote worker's progress readable from
// another box: `jiradozer fleet runs` reads meta.json over ssh and can see
// neither this host's log nor its terminal. Best-effort — a run must not die
// because a status write failed.
//
// It writes both the snapshot and the trail, because meta.Phase keeps only the
// last step; see jiradozer.Event for why the sequence needs its own record.
//
// The step's NAME, not the WorkflowStep itself: it is an int, and marshalling
// it raw would put a number in the trail that means nothing to a reader and
// silently shifts if the enum is ever reordered.
func (x *execRun) recordPhase(step jiradozer.WorkflowStep) {
	if x.rl == nil {
		return
	}
	if err := x.rl.UpdateMeta(func(m *jiradozer.RunMeta) { m.Phase = step.String() }); err != nil {
		x.logger.Warn("failed to record workflow phase", "step", step, "error", err)
	}
	x.event(jiradozer.EventPhase, step.String(), nil)
}

// checkNotClaimed turns the lock label into an actual check.
//
// The label has always been written and removed but never READ: discovery
// suppresses by an in-memory set, and the tracker's filter has OR semantics
// with no exclusion key, so nothing could act on it. That was harmless while a
// single supervisor owned every pickup. Across a fleet it is the only
// cross-host claim there is — the flock lease cannot see another machine.
func (x *execRun) checkNotClaimed(issue *tracker.Issue) error {
	if x.args.force || !slices.Contains(issue.Labels, jiradozer.LockLabel) {
		return nil
	}
	return fmt.Errorf("%s is already claimed (label %q). Another host may be working it; check `jiradozer runs --json` across the fleet, then re-run with --force if that claim is stale",
		issue.Identifier, jiradozer.LockLabel)
}

// deriveBranch names this run's branch. Split out of createWorktree because
// the name is needed one step earlier than the checkout: the run-log records
// where the checkout WILL be, and that path is derived from the branch.
//
// Pure — it touches neither disk nor tracker — so it cannot fail, and a run
// that never gets a worktree still has a branch name to be recorded under.
func (x *execRun) deriveBranch() {
	prefix := x.args.branchPrefix
	if prefix == "" {
		prefix = x.cfg.Source.BranchPrefix
	}
	if prefix == "" {
		prefix = "jiradozer"
	}
	leaf := x.args.issueID
	if leaf == "" {
		leaf = x.args.taskID
	}
	if leaf == "" {
		leaf = x.runID
	}
	x.branch = prefix + "/" + sanitizeBranchLeaf(leaf)
}

// plannedWorktreePath is where createWorktree is going to put the checkout.
//
// Knowable in advance because wt derives it purely from the branch name
// (wt.Manager.New joins RepoDir() with the branch), and it has to be knowable:
// the run-log records it before `wt.New` is called, so that a death anywhere
// inside the checkout still leaves gc a path to reclaim. If wt ever changes
// that derivation this prediction goes stale silently, which is why
// TestThePlannedWorktreePathIsWhereWtActuallyPutsIt drives the real manager.
func (x *execRun) plannedWorktreePath() string {
	if x.wtMgr == nil || x.branch == "" {
		return x.cfg.WorkDir
	}
	return filepath.Join(x.wtMgr.RepoDir(), x.branch)
}

func (x *execRun) createWorktree(ctx context.Context) error {
	goal := x.args.description
	if x.issue != nil {
		goal = x.issue.Title
	}
	path, err := x.wtMgr.New(ctx, x.branch, x.cfg.BaseBranch, goal)
	if err != nil {
		return fmt.Errorf("create worktree for %s: %w", x.branch, err)
	}
	// Everything downstream — the agent, the local tracker, the workflow — runs
	// against this directory.
	x.cfg.WorkDir = path
	// And this is the only place that may say a checkout exists.
	x.worktreePath = path
	// Reconcile the prediction with what wt actually did. Normally identical;
	// when it is not, the run-log is pointing gc at a directory that does not
	// exist while the real checkout leaks, so correcting it matters more than
	// the write costs.
	if x.rl != nil && path != x.rl.Meta().WorktreePath {
		if err := x.rl.UpdateMeta(func(m *jiradozer.RunMeta) { m.WorktreePath = path }); err != nil {
			x.logger.Warn("failed to record the worktree path", "path", path, "error", err)
		}
	}
	x.logger.Info("worktree created", "branch", x.branch, "path", path)
	x.event(jiradozer.EventWorktreeCreated, path, map[string]any{"branch": x.branch})
	return nil
}

// startRunLog records the run BEFORE the checkout it describes, so that no
// worktree can exist without a run-log naming it. If it were written after —
// or at the end, as an outcome record — a run that died mid-checkout would
// leave a directory gc has no way to discover, since walking run-logs is its
// only discovery path.
func (x *execRun) startRunLog() error {
	meta := jiradozer.RunMeta{
		RunID:        x.runID,
		TaskID:       x.args.taskID,
		Description:  x.args.description,
		LeaseTarget:  leaseTarget(x.args),
		Repo:         x.args.repo,
		Branch:       x.branch,
		BaseBranch:   x.cfg.BaseBranch,
		WorktreePath: x.plannedWorktreePath(),
		TmuxSession:  x.args.tmuxSession,
		LogPath:      x.app.LogPath,
		State:        jiradozer.RunStateRunning,
	}
	if wtRoot, err := resolveWTRoot(); err == nil {
		meta.WTRoot = wtRoot
	}
	if x.issue != nil {
		meta.IssueIdentifier = x.issue.Identifier
		meta.IssueID = x.issue.ID
		if x.issue.URL != nil {
			meta.IssueURL = *x.issue.URL
		}
	} else {
		meta.IssueIdentifier = x.args.issueID
	}

	rl, err := jiradozer.NewRunLog(meta)
	if err != nil {
		return err
	}
	x.rl = rl
	x.logger.Info("run started", "run_id", x.runID, "run_dir", rl.Dir(), "target", meta.Target())
	x.event(jiradozer.EventRunStarted, meta.Target(), map[string]any{
		"run_id": x.runID,
		"branch": x.branch,
		"repo":   x.args.repo,
	})
	return nil
}

// event appends one line to the run's audit trail.
//
// Best-effort, for the same reason recordPhase is: the trail is a record of a
// run, never a dependency of one, and a run that died because a log write
// failed would be strictly worse than a run with a gap in its log.
//
// The nil check guards helpers invoked on a run that has not reached
// startRunLog. run() reaches none of them in that state, but these are methods
// on a half-built struct and Append on a nil RunLog panics — taking the run
// down over a log line.
func (x *execRun) event(kind, detail string, fields map[string]any) {
	if x.rl == nil {
		return
	}
	if err := x.rl.Append(kind, detail, fields); err != nil {
		x.logger.Warn("failed to append run event", "kind", kind, "error", err)
	}
}

// createLocalIssue builds the --description mode issue.
//
// The local tracker is rooted in the RUN directory, not the worktree. Rooted in
// the worktree, the issue JSON and every step comment, phase label and failure
// report would live inside a directory that gets reclaimed — and would be
// invisible to anything reading this box remotely. Same reasoning that puts the
// run-log outside the worktree.
func (x *execRun) createLocalIssue() error {
	dir := filepath.Join(x.rl.Dir(), "issues")
	lt, err := local.NewTracker(dir)
	if err != nil {
		return fmt.Errorf("create local tracker: %w", err)
	}
	x.tracker = lt

	title := jiradozer.GenerateTitle(x.args.description)
	issue, err := lt.CreateIssue(title, x.args.description)
	if err != nil {
		return fmt.Errorf("create local issue: %w", err)
	}
	x.issue = issue
	return x.rl.UpdateMeta(func(m *jiradozer.RunMeta) {
		m.IssueID = issue.ID
		if m.IssueIdentifier == "" {
			m.IssueIdentifier = issue.Identifier
		}
	})
}

// claim attaches the lock label and reports whether it landed.
//
// Whether a failure is fatal is the CALLER's decision, and the two callers
// answer differently: for a tracker-backed issue the label is the fleet's only
// cross-host claim, so failing to attach it must stop the run before a worktree
// exists; for a --description run the label sits on a per-run local tracker
// nothing else can read, so it is a record and losing it costs nothing. Which
// is why this reports the error rather than swallowing it.
func (x *execRun) claim(ctx context.Context) error {
	if err := x.tracker.AddLabel(ctx, x.issue.ID, jiradozer.LockLabel); err != nil {
		return err
	}
	// Only a label this run actually attached may be removed by finish(). A
	// failed AddLabel is indistinguishable from "someone else got there first",
	// and releasing on that path would strip a claim another host owns.
	x.claimed = true
	x.logger.Info("claimed issue", "issue", x.issue.Identifier, "label", jiradozer.LockLabel)
	return nil
}

// postStartComment records where this run is happening.
//
// It doubles as a recoverable index. Run-logs are per-host local files, so when
// a box is stopped — which happens routinely — its runs vanish from view and an
// issue would otherwise sit labelled with nothing saying who took it. This
// comment survives on the tracker.
func (x *execRun) postStartComment(ctx context.Context) {
	host, _ := os.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, "**jiradozer** started on `%s`\n\n", host)
	fmt.Fprintf(&b, "- run: `%s`\n", x.runID)
	fmt.Fprintf(&b, "- branch: `%s`\n", x.branch)
	// worktreePath, not cfg.WorkDir: this comment only runs after the checkout,
	// where the two agree — but only one of them is a directory that is
	// guaranteed to exist, and a path printed to an operator has to be one they
	// can cd into.
	fmt.Fprintf(&b, "- worktree: `%s`\n", x.worktreePath)
	fmt.Fprintf(&b, "- run log: `%s`\n", x.rl.Dir())
	if x.app.LogPath != "" {
		fmt.Fprintf(&b, "- log: `%s`\n", x.app.LogPath)
	}
	if x.args.tmuxSession != "" {
		fmt.Fprintf(&b, "- tmux: `%s`\n", x.args.tmuxSession)
	}
	if x.args.taskID != "" {
		fmt.Fprintf(&b, "- task: `%s`\n", x.args.taskID)
	}
	fmt.Fprintf(&b, "\nStatus: `jiradozer runs --issue %s --json` on that host.", x.rl.Meta().Target())

	if _, err := x.tracker.PostComment(ctx, x.issue.ID, b.String()); err != nil {
		x.logger.Warn("failed to post start comment", "issue", x.issue.Identifier, "error", err)
	}
}

// finish settles the run-log, releases the claim and posts the end comment.
// It runs on every exit path including a failure, because a run that stops
// without recording why is the case a dispatcher cannot act on.
func (x *execRun) finish(runErr error) {
	// The run context is likely already cancelled by now, so use a fresh one:
	// the final tracker writes matter most exactly when the run is ending.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
	defer cancel()

	state := jiradozer.RunStateDone
	switch {
	case runErr == nil:
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		state = jiradozer.RunStateCancelled
	default:
		state = jiradozer.RunStateFailed
	}

	// Record the PR before settling the meta. This is not decoration: gc keys
	// reclamation solely on PRURL, so a run that finishes without one keeps its
	// worktree FOREVER — and exec always keeps its worktree. Resolving it here,
	// from the branch, rather than threading it out of the workflow means it is
	// recorded however the PR came to exist (create_pr, a resumed run, or a
	// human opening it mid-flight).
	pr := x.resolvePR(ctx)

	// The worktree is deliberately kept. A terminal state here means "a PR is
	// open", never "it merged", so nothing at this point authorises deleting
	// the branch's only checkout. `jiradozer gc` reclaims it later by asking
	// whether the PR actually landed.
	//
	// Conditional because the run-log now outlives failures that happen before
	// the checkout: claiming to have KEPT a worktree that was never created
	// would send a human looking for a directory that does not exist. The test
	// is worktreePath, not cfg.WorkDir — the latter is non-empty from config
	// load onward and so is true even on a run that never reached wt.New.
	kept := x.worktreePath != ""
	if err := x.rl.UpdateMeta(func(m *jiradozer.RunMeta) {
		if kept {
			m.WorktreeKept = true
			m.WorktreeKeptReason = "terminal state is an open PR, not a merge; reclaimed by `jiradozer gc`"
		}
		if pr != nil {
			m.PRURL = pr.URL
			m.PRNumber = pr.Number
		}
	}); err != nil {
		x.logger.Warn("failed to record worktree retention", "error", err)
	}
	if err := x.rl.Finish(state, "", runErr); err != nil {
		x.logger.Warn("failed to write terminal run meta", "error", err)
	}

	// Closes the trail, and does it here rather than at the end of finish():
	// everything below is a tracker round trip that can hang or fail, and a
	// trail whose last line is missing cannot be told apart from a run that was
	// killed mid-flight. The error goes in the fields so a reader replaying the
	// trail sees why a run ended without opening meta.json.
	finishFields := map[string]any{}
	if runErr != nil {
		finishFields["error"] = jiradozer.Truncate(runErr.Error(), 2000)
	}
	if pr != nil {
		finishFields["pr_url"] = pr.URL
	}
	x.event(jiradozer.EventRunFinished, string(state), finishFields)

	if x.tracker != nil && x.issue != nil {
		x.postEndComment(ctx, state, runErr)
		// Gated on claimed, not on the tracker existing. finish() now runs on
		// paths where the claim itself failed, and a failed AddLabel cannot be
		// told apart from another host having claimed first — so releasing
		// unconditionally would let a run that never held the lock strip the
		// lock of the run that does.
		if x.claimed {
			if err := x.tracker.RemoveLabel(ctx, x.issue.ID, jiradozer.LockLabel); err != nil {
				// Loud: a claim left on an issue nobody is working blocks the
				// fleet from picking it up again, and only the log says why.
				x.logger.Error("could not release the lock label; the issue stays claimed until this label is removed or --force is passed",
					"issue", x.issue.Identifier, "label", jiradozer.LockLabel, "error", err)
			}
		}
	}
	x.logger.Info("run finished", "run_id", x.runID, "state", state, "run_dir", x.rl.Dir())
}

// resolvePR asks GitHub which PR this run's branch opened, or nil.
//
// A missing PR is a NORMAL outcome, not an error: a run that failed before
// create_pr, or one whose build produced no changes, legitimately has none. So
// this logs at debug and returns nil rather than warning — but a PR that exists
// and is not recorded leaks a worktree permanently, which is why the lookup
// happens on every exit path instead of only the successful one.
func (x *execRun) resolvePR(ctx context.Context) *wt.PRInfo {
	// The lookup has to run inside this run's checkout: `gh` resolves the repo
	// from the directory it runs in, so asking from a base directory that is
	// merely where worktrees live — or from "." — either fails or answers about
	// somebody else's repository.
	if x.lookupPR == nil || x.branch == "" || x.worktreePath == "" {
		return nil
	}
	pr, err := x.lookupPR(ctx, x.branch, x.worktreePath)
	if err != nil {
		x.logger.Debug("no PR resolved for branch", "branch", x.branch, "error", err)
		return nil
	}
	if pr == nil || pr.URL == "" {
		return nil
	}
	x.logger.Info("recorded PR for run", "branch", x.branch, "pr", pr.URL)
	return pr
}

func (x *execRun) postEndComment(ctx context.Context, state jiradozer.RunState, runErr error) {
	host, _ := os.Hostname()
	m := x.rl.Meta()
	var b strings.Builder
	fmt.Fprintf(&b, "**jiradozer** finished on `%s` — `%s`\n\n", host, state)
	fmt.Fprintf(&b, "- run: `%s`\n", x.runID)
	fmt.Fprintf(&b, "- branch: `%s`\n", x.branch)
	fmt.Fprintf(&b, "- duration: %s\n", time.Since(m.StartedAt).Truncate(time.Second))
	if m.PRURL != "" {
		fmt.Fprintf(&b, "- PR: %s\n", m.PRURL)
	}
	// Only when one was actually kept, and named by the path wt produced. The
	// configured work_dir is the wrong answer twice over: it is the base
	// directory rather than the checkout, and it is set even on a run that
	// failed before the checkout existed.
	if m.WorktreeKept {
		fmt.Fprintf(&b, "- worktree kept at `%s` (reclaimed by `jiradozer gc` once the PR lands)\n", x.worktreePath)
	}
	if runErr != nil {
		fmt.Fprintf(&b, "\nError:\n```\n%s\n```\n", jiradozer.Truncate(runErr.Error(), 2000))
	}
	if _, err := x.tracker.PostComment(ctx, x.issue.ID, b.String()); err != nil {
		x.logger.Warn("failed to post end comment", "issue", x.issue.Identifier, "error", err)
	}
}

// leaseTarget names this task for the flock lease.
//
// It must be DERIVED, never random, and `dispatch` must compute the same value
// this worker will. The lease is what lets a dispatcher refuse a second run for
// work some box is already doing, and that check happens before the worker
// exists — so a per-run identifier would make every --description dispatch
// unique and two concurrent dispatches of the same task would both proceed,
// producing duplicate worktrees and duplicate PRs.
//
// A description is hashed rather than used verbatim because it is free-form
// multi-line text and this becomes a lock FILENAME.
//
// The description hash is scoped by --repo; an issue or task id is not. Those
// two are already unique across the fleet, and folding the repo in would WEAKEN
// them — the same issue dispatched at two repos would stop excluding itself,
// which is wrong, because an issue is claimed once. A description is the only
// identifier here that is free-form, so it is the only one where two unrelated
// tasks ("update the deps") collide, and a collision makes dispatch refuse a
// perfectly valid run as already-running elsewhere.
func leaseTarget(x execArgs) string {
	if x.issueID != "" {
		return canonicalIssueTarget(x.issueID)
	}
	if x.taskID != "" {
		return strings.ToLower(strings.TrimSpace(x.taskID))
	}
	if x.description == "" {
		return ""
	}
	// The separator keeps the repo boundary unambiguous: without it "ab"+"cd"
	// and "a"+"bcd" would hash alike, and a repo name can contain anything.
	sum := sha256.Sum256([]byte(x.repo + "\x00" + strings.TrimSpace(x.description)))
	return "adhoc-" + hex.EncodeToString(sum[:6])
}

// canonicalIssueTarget folds every spelling of one issue onto one lock name.
//
// The lease has to be derivable without contacting a tracker — that is the
// whole reason `dispatch` can use it as a cheap pre-flight check — so this
// canonicalizes syntactically, mirroring what each tracker already does with
// the identifier it is handed:
//
//   - GitHub accepts "owner/repo#42" and the issue URL for the same issue, and
//     reports "owner/repo#42" as its identity (tracker/github/client.go:386).
//     Both spellings fold to that.
//   - The local tracker resolves LOCAL-1 and local-1 to one file by lowercasing
//     (tracker/local/local.go:130). So case folds here too, which also covers
//     Linear-style "INF-1234" being typed either way.
//
// The exact output does not matter; agreeing does. Two spellings that reach two
// lock names silently disable the guard that refuses a second run for a task
// already in flight — and unlike the tracker's lock label, which is applied
// against the canonical issue after a fetch, nothing downstream catches it
// before both runs have built worktrees.
func canonicalIssueTarget(issueID string) string {
	issueID = strings.TrimSpace(issueID)
	if owner, repo, number, err := github.ParseIdentifier(issueID); err == nil {
		return fmt.Sprintf("%s/%s#%d", owner, repo, number)
	}
	return strings.ToLower(issueID)
}

// sanitizeBranchLeaf keeps an identifier usable as one branch path segment.
// GitHub identifiers like "acme/app#42" would otherwise nest the branch.
func sanitizeBranchLeaf(s string) string {
	return strings.NewReplacer("/", "-", "#", "-", " ", "-", "~", "-", "^", "-", ":", "-").Replace(s)
}
