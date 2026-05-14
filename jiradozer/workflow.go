package jiradozer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// CommentData is the template context for rendering tracker comment bodies
// from StepConfig.CommentTemplate / RoundCommentTemplate.
type CommentData struct {
	Step        string // step name, e.g. "plan", "build"
	Heading     string // capitalize(Step), e.g. "Plan", "Build"
	Output      string // agent or command output for this step / round
	Round       int    // 1-based round index; only meaningful for round comments
	TotalRounds int    // total rounds; only meaningful for round comments
}

// CommentPoster is the write-side tracker capability needed for result comments.
type CommentPoster interface {
	PostComment(ctx context.Context, issueID string, body string) (tracker.Comment, error)
}

var errRenderComment = errors.New("render comment")

func renderCommentTemplate(tmplStr string, data CommentData) (string, error) {
	return renderTemplate("comment", tmplStr, data)
}

// RenderStepComment renders the configured result comment for a workflow step.
func RenderStepComment(stepName string, stepCfg StepConfig, output string) (string, error) {
	return renderCommentTemplate(stepCfg.CommentTemplate, CommentData{
		Step:    stepName,
		Heading: capitalize(stepName),
		Output:  output,
	})
}

// RenderRoundComment renders the configured result comment for one workflow round.
func RenderRoundComment(stepName string, stepCfg StepConfig, output string, round, totalRounds int) (string, error) {
	return renderCommentTemplate(stepCfg.RoundCommentTemplate, CommentData{
		Step:        stepName,
		Heading:     capitalize(stepName),
		Output:      output,
		Round:       round,
		TotalRounds: totalRounds,
	})
}

// PostStepComment renders and posts the configured result comment for a workflow step.
func PostStepComment(ctx context.Context, t CommentPoster, issueID string, stepName string, stepCfg StepConfig, output string) (tracker.Comment, error) {
	comment, err := RenderStepComment(stepName, stepCfg, output)
	if err != nil {
		return tracker.Comment{}, fmt.Errorf("%w: %w", errRenderComment, err)
	}
	return t.PostComment(ctx, issueID, comment)
}

// PostRoundComment renders and posts the configured result comment for one workflow round.
func PostRoundComment(ctx context.Context, t CommentPoster, issueID string, stepName string, stepCfg StepConfig, output string, round, totalRounds int) (tracker.Comment, error) {
	comment, err := RenderRoundComment(stepName, stepCfg, output, round, totalRounds)
	if err != nil {
		return tracker.Comment{}, fmt.Errorf("%w: %w", errRenderComment, err)
	}
	return t.PostComment(ctx, issueID, comment)
}

// State keys for the stateIDs map, mapping logical workflow states to tracker state IDs.
const (
	stateKeyInProgress = "in_progress"
	stateKeyInReview   = "in_review"
	stateKeyDone       = "done"
)

// DefaultMaxRedos is the maximum number of times a step can be re-run via
// feedback before the workflow fails. This prevents infinite loops when
// comments are repeatedly parsed as redo requests.
const DefaultMaxRedos = 3

// phaseState tracks per-phase bookkeeping: notStarted → inProgress → done.
type phaseState int

const (
	phaseNotStarted phaseState = iota
	phaseInProgress
	phaseDone
)

type reviewApproval struct {
	logMessage       string
	statusMessage    string
	transitionReason string
	approveAll       bool
}

// Workflow drives the issue through plan → build → create_pr → validate → ship.
type Workflow struct {
	lastCommentAt       time.Time
	tracker             tracker.IssueTracker
	lastError           error
	config              *Config
	state               *StateMachine
	logger              *slog.Logger
	sessionIDs          map[WorkflowStep]string
	stateIDs            map[string]string
	redoCounts          map[WorkflowStep]int
	phases              map[string]phaseState
	skipPhaseSources    map[string]skipPhaseSource
	OnTransition        func(step WorkflowStep)
	OnRoundProgress     func(roundIndex, roundTotal int)
	runStepAgent        func(ctx context.Context, stepName string, data PromptData, cfg StepConfig, workDir string, feedback string, resumeSessionID string, renderer *render.Renderer, logger *slog.Logger) (StepAgentResult, error)
	renderer            *render.Renderer
	issue               *tracker.Issue
	plan                string
	buildOutput         string
	feedback            string
	botCommentIDs       []string
	lastLabels          []string
	maxRedos            int
	approveAllRemaining bool
}

// NewWorkflow creates a new workflow for the given issue. The caller's
// Issue is not mutated: the workflow takes a shallow copy and filters its
// own copy's Labels so the first agent prompt (before any successful
// refreshLabels) doesn't leak bookkeeping labels.
func NewWorkflow(t tracker.IssueTracker, issue *tracker.Issue, cfg *Config, logger *slog.Logger) *Workflow {
	w := &Workflow{
		tracker:          t,
		state:            NewStateMachine(),
		config:           cfg,
		logger:           logger,
		stateIDs:         make(map[string]string),
		sessionIDs:       make(map[WorkflowStep]string),
		redoCounts:       make(map[WorkflowStep]int),
		phases:           make(map[string]phaseState),
		skipPhaseSources: make(map[string]skipPhaseSource),
		maxRedos:         DefaultMaxRedos,
		runStepAgent:     RunStepAgent,
	}
	for _, phase := range cfg.SkipPhases {
		w.addSkipPhase(phase, cfg.skipSourceForPhase(phase))
	}
	if issue != nil {
		// Shallow-copy so Labels mutations on w.issue don't bleed back into
		// the caller's struct. lastLabels keeps the full (unfiltered) set for
		// phase-skip decisions before the first tracker fetch.
		issueCopy := *issue
		w.lastLabels = slices.Clone(issue.Labels)
		w.addSkipPhasesFromLabels(issue.Labels)
		issueCopy.Labels = slices.DeleteFunc(slices.Clone(issue.Labels), isJiradozerLabel)
		w.issue = &issueCopy
	}
	return w
}

func (w *Workflow) addSkipPhase(phase string, source skipPhaseSource) {
	if startStepForPhase(phase) == StepInit || source == "" {
		return
	}
	if prev := w.skipPhaseSources[phase]; prev != "" && skipPhasePriority(prev) > skipPhasePriority(source) {
		return
	}
	w.skipPhaseSources[phase] = source
}

func (w *Workflow) addSkipPhasesFromLabels(labels []string) {
	for _, label := range labels {
		if phase := phaseForSkipLabel(label); phase != "" {
			w.addSkipPhase(phase, skipPhaseSourceLabel)
		}
	}
}

func (w *Workflow) reconcileSkipPhasesFromLabels(labels []string) {
	labelPhases := make(map[string]bool)
	for _, label := range labels {
		if phase := phaseForSkipLabel(label); phase != "" {
			labelPhases[phase] = true
		}
	}
	for phase, source := range w.skipPhaseSources {
		if source != skipPhaseSourceLabel || labelPhases[phase] {
			continue
		}
		if configSource := w.config.skipSourceForPhase(phase); configSource != "" {
			w.skipPhaseSources[phase] = configSource
		} else {
			delete(w.skipPhaseSources, phase)
		}
	}
	for phase := range labelPhases {
		w.addSkipPhase(phase, skipPhaseSourceLabel)
	}
}

func (w *Workflow) isSkipPhase(phase string) bool {
	_, ok := w.skipPhaseSources[phase]
	return ok
}

func skipPhasePriority(source skipPhaseSource) int {
	switch source {
	case skipPhaseSourceCLI:
		return 3
	case skipPhaseSourceLabel:
		return 2
	case skipPhaseSourceConfig:
		return 1
	default:
		return 0
	}
}

// SetRenderer sets the terminal renderer for streaming output.
func (w *Workflow) SetRenderer(r *render.Renderer) {
	w.renderer = r
}

// status emits a renderer status line when a renderer is configured.
func (w *Workflow) status(msg string) {
	if w.renderer != nil {
		w.renderer.Status(msg)
	}
}

// Run executes the workflow loop until completion or failure.
func (w *Workflow) Run(ctx context.Context) (runErr error) {
	w.logger.Info("workflow starting",
		"issue", w.issue.Identifier,
		"title", w.issue.Title,
		"model", w.config.Agent.Model,
		"work_dir", w.config.WorkDir,
		"base_branch", w.config.BaseBranch,
	)
	for _, p := range phaseTable {
		if w.isSkipPhase(p.name) {
			w.logger.Info("phase configured to skip", "phase", p.name, "source", w.skipPhaseSources[p.name])
		}
	}
	workflowStart := time.Now()
	defer func() {
		finalStep := w.state.Current().String()
		duration := time.Since(workflowStart)
		if runErr != nil {
			w.logger.Error("workflow finished",
				"issue", w.issue.Identifier,
				"final_step", finalStep,
				"error", runErr,
				"duration", duration,
				"transitions", len(w.state.History()),
			)
		} else {
			w.logger.Info("workflow finished",
				"issue", w.issue.Identifier,
				"final_step", finalStep,
				"duration", duration,
				"transitions", len(w.state.History()),
			)
		}
	}()

	// Resolve workflow state names to IDs.
	if err := w.resolveStateIDs(ctx); err != nil {
		return fmt.Errorf("resolve workflow states: %w", err)
	}

	if err := w.transition(StepPlanning, "start"); err != nil {
		return err
	}

	if id, ok := w.stateIDs[stateKeyInProgress]; ok {
		if err := w.tracker.UpdateIssueState(ctx, w.issue.ID, id); err != nil {
			w.logger.Warn("failed to update issue state to in_progress", "error", err)
		}
	}

	// Honor pre-existing -done labels by skipping completed phases. A fresh
	// workflow run never re-does plan/build/validate/ship that the user has
	// already marked done. Users clear the label manually to re-run a phase.
	w.skipCompletedOrConfiguredPhases(ctx)
	if phase := phaseForStep(w.state.Current()); phase != "" {
		w.enterPhase(ctx, phase)
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		switch w.state.Current() {
		case StepPlanning:
			w.runStepOrRounds(ctx, "plan", w.config.Plan, StepPlanReview, "plan_complete")
		case StepPlanReview:
			w.runReview(ctx, StepBuilding, StepPlanning)
		case StepBuilding:
			delete(w.sessionIDs, StepCreatingPR) // Fresh build cycle invalidates old create_pr session.
			if w.plan == "" {
				w.logger.Warn("NO PLAN AVAILABLE — build step is running without a plan")
			}
			w.runStepOrRounds(ctx, "build", w.config.Build, StepCreatingPR, "build_complete")
		case StepCreatingPR:
			w.runStep(ctx, "create_pr", w.config.CreatePR, StepBuildReview, "pr_created")
		case StepBuildReview:
			w.runReview(ctx, StepValidating, StepBuilding)
		case StepValidating:
			w.runStepOrRounds(ctx, "validate", w.config.Validate, StepValidateReview, "validation_complete")
		case StepValidateReview:
			w.runReview(ctx, StepShipping, StepValidating)
		case StepShipping:
			w.runStepOrRounds(ctx, "ship", w.config.Ship, StepShipReview, "ship_complete")
		case StepShipReview:
			w.runReview(ctx, StepDone, StepShipping)
		case StepDone:
			if id, ok := w.stateIDs[stateKeyDone]; ok {
				if err := w.tracker.UpdateIssueState(ctx, w.issue.ID, id); err != nil {
					w.logger.Warn("failed to update issue state to done", "error", err)
				}
			}
			return nil
		case StepFailed:
			return w.lastError
		default:
			return fmt.Errorf("workflow reached unexpected state: %s", w.state.Current())
		}
	}
}

// runStepOrRounds dispatches to multi-round execution when rounds are configured,
// otherwise runs the single-session step.
func (w *Workflow) runStepOrRounds(ctx context.Context, stepName string, stepCfg StepConfig, reviewStep WorkflowStep, trigger string) {
	if len(stepCfg.Rounds) > 0 {
		w.runStepRounds(ctx, stepName, stepCfg, reviewStep, trigger)
		return
	}
	w.runStep(ctx, stepName, stepCfg, reviewStep, trigger)
}

// runStepRounds runs rounds sequentially for a multi-round step. Each round is
// either a shell command or an agent session (no resume). On redo, all rounds
// re-run from the start; feedback is injected into the first agent round only.
func (w *Workflow) runStepRounds(ctx context.Context, stepName string, stepCfg StepConfig, reviewStep WorkflowStep, trigger string) {
	resolved := w.config.ResolveStep(stepCfg)
	totalRounds := len(stepCfg.Rounds)

	// Reset lastCommentAt and botCommentIDs so transitionToReview uses
	// fresh values from this step cycle rather than stale ones from a prior cycle.
	w.lastCommentAt = time.Time{}
	w.botCommentIDs = nil

	feedback := w.feedback
	w.feedback = ""

	w.logger.Info("step: "+stepName, "issue", w.issue.Identifier, "rounds", totalRounds, "feedback", feedback != "")
	w.status(fmt.Sprintf("Step: %s (%s) %d rounds", stepName, w.issue.Identifier, totalRounds))
	stepStart := time.Now()

	data := w.promptData()
	var allOutputs []string
	var roundSessionIDs []string
	feedbackInjected := false
	for i, round := range stepCfg.Rounds {
		if ctx.Err() != nil {
			w.fail(ctx, ctx.Err())
			return
		}

		w.logger.Info("round start", "step", stepName, "round", i+1, "total", totalRounds)
		w.status(fmt.Sprintf("Round %d/%d", i+1, totalRounds))
		if w.OnRoundProgress != nil {
			w.OnRoundProgress(i, totalRounds)
		}

		var output string
		var err error
		if round.IsCommand() {
			output, err = RunCommand(ctx, stepName, data, round.Command, w.config.WorkDir, w.logger)
			if err != nil {
				w.fail(ctx, fmt.Errorf("%s round %d/%d: %w", stepName, i+1, totalRounds, err))
				return
			}
		} else {
			roundCfg := ResolveRound(round, resolved)
			roundFeedback := ""
			if !feedbackInjected {
				roundFeedback = feedback
				feedbackInjected = true
			}
			var res StepAgentResult
			res, err = w.runStepAgent(ctx, stepName, data, roundCfg, w.config.WorkDir, roundFeedback, "", w.renderer, w.logger)
			if res.SessionID != "" {
				roundSessionIDs = append(roundSessionIDs, res.SessionID)
			}
			if err != nil {
				w.fail(ctx, fmt.Errorf("%s round %d/%d: %w", stepName, i+1, totalRounds, err))
				return
			}
			output = res.Output
		}
		allOutputs = append(allOutputs, output)

		roundComment, err := PostRoundComment(ctx, w.tracker, w.issue.ID, stepName, stepCfg, output, i+1, totalRounds)
		if err != nil {
			if errors.Is(err, errRenderComment) {
				w.fail(ctx, fmt.Errorf("%s round %d/%d: render round_comment_template: %w", stepName, i+1, totalRounds, err))
				return
			}
			w.logger.Warn("failed to post round comment", "step", stepName, "round", i+1, "error", err)
		} else {
			if roundComment.ID != "" {
				w.botCommentIDs = append(w.botCommentIDs, roundComment.ID)
			}
			if i == totalRounds-1 && !roundComment.CreatedAt.IsZero() {
				w.lastCommentAt = roundComment.CreatedAt
			}
		}
	}

	// Warn if feedback was provided but could not be injected (all rounds are
	// command rounds). Feedback cannot be passed to shell commands; the redo
	// cycle will re-run commands identically.
	if feedback != "" && !feedbackInjected {
		w.logger.Warn("feedback not injected: all rounds are command rounds; redo will re-run commands unchanged",
			"step", stepName, "feedback", truncate(feedback, 200))
	}

	stepDuration := time.Since(stepStart)
	w.logger.Info("step completed", "step", stepName, "issue", w.issue.Identifier, "rounds", totalRounds, "session_ids", roundSessionIDs, "duration", stepDuration)
	w.status(fmt.Sprintf("Step %s complete (%s)", stepName, stepDuration.Truncate(time.Second)))
	w.captureOutput(stepName, JoinRoundOutputs(allOutputs))
	if w.maybeSkipAfterNoChangeBuild(ctx, stepName) {
		return
	}
	w.transitionToReview(ctx, reviewStep, trigger)
}

// runStep runs an agent session for the current workflow step.
func (w *Workflow) runStep(ctx context.Context, stepName string, stepCfg StepConfig, reviewStep WorkflowStep, trigger string) {
	currentStep := w.state.Current()
	sessionID := w.sessionIDs[currentStep]

	// Reset lastCommentAt and botCommentIDs so transitionToReview uses
	// fresh values from this step cycle rather than stale ones from a prior cycle.
	w.lastCommentAt = time.Time{}
	w.botCommentIDs = nil

	feedback := w.feedback
	w.feedback = ""

	w.logger.Info("step: "+stepName, "issue", w.issue.Identifier, "feedback", feedback != "", "resume", sessionID != "")
	w.status(fmt.Sprintf("Step: %s (%s)", stepName, w.issue.Identifier))

	stepStart := time.Now()
	cfg := w.config.ResolveStep(stepCfg)
	res, err := w.runStepAgent(ctx, stepName, w.promptData(), cfg, w.config.WorkDir, feedback, sessionID, w.renderer, w.logger)
	if err != nil {
		w.fail(ctx, fmt.Errorf("%s step: %w", stepName, err))
		return
	}
	output := res.Output
	newSessionID := res.SessionID

	stepDuration := time.Since(stepStart)
	w.logger.Info("step completed", "step", stepName, "issue", w.issue.Identifier, "session_id", newSessionID, "duration", stepDuration)
	w.status(fmt.Sprintf("Step %s complete (%s)", stepName, stepDuration.Truncate(time.Second)))
	w.sessionIDs[currentStep] = newSessionID
	w.captureOutput(stepName, output)

	resultComment, err := PostStepComment(ctx, w.tracker, w.issue.ID, stepName, stepCfg, output)
	if err != nil {
		if errors.Is(err, errRenderComment) {
			w.fail(ctx, fmt.Errorf("%s: render comment_template: %w", stepName, err))
			return
		}
		w.logger.Warn("failed to post step comment", "step", stepName, "error", err)
	} else {
		if resultComment.ID != "" {
			w.botCommentIDs = append(w.botCommentIDs, resultComment.ID)
		}
		if !resultComment.CreatedAt.IsZero() {
			w.lastCommentAt = resultComment.CreatedAt
		}
	}

	if w.maybeSkipAfterNoChangeBuild(ctx, stepName) {
		return
	}
	w.transitionToReview(ctx, reviewStep, trigger)
}

func (w *Workflow) maybeSkipAfterNoChangeBuild(ctx context.Context, stepName string) bool {
	if stepName != "build" {
		return false
	}
	if !w.buildProducedNoChanges(ctx) {
		return false
	}
	w.logger.Info("build produced no changes; skipping create_pr and remaining phases", "issue", w.issue.Identifier)
	w.status("Build produced no changes; skipping to done")
	if _, err := w.tracker.PostComment(ctx, w.issue.ID, "Build produced no changes; nothing to ship. Marking issue done."); err != nil {
		w.logger.Warn("failed to post no-change build comment", "error", err)
	}
	w.forceTransition(StepDone)
	w.handlePhaseBoundary(ctx)
	return true
}

// buildProducedNoChanges returns true when the build left a clean working tree
// and HEAD has no diff against origin/<base>. Git errors fail open so the
// workflow proceeds to create_pr rather than incorrectly skipping work.
func (w *Workflow) buildProducedNoChanges(ctx context.Context) bool {
	status, err := runGit(ctx, w.config.WorkDir, "status", "--porcelain")
	if err != nil {
		w.logger.Warn("could not check git status; assuming changes exist", "error", err)
		return false
	}
	if strings.TrimSpace(status) != "" {
		return false
	}

	base := "origin/" + w.config.BaseBranch
	err = runGitQuiet(ctx, w.config.WorkDir, "diff", "--quiet", base+"...HEAD")
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	w.logger.Warn("could not diff against base; assuming changes exist", "base", base, "error", err)
	return false
}

// promptData builds the template context for agent prompts.
func (w *Workflow) promptData() PromptData {
	data := NewPromptData(w.issue, w.config.BaseBranch)
	data.Plan = w.plan
	data.BuildOutput = w.buildOutput
	return data
}

// captureOutput stores step output for use by downstream steps.
// Plan output is also persisted to disk so the build step can load it
// when run as a separate process invocation (--run-step=build).
func (w *Workflow) captureOutput(stepName, output string) {
	switch stepName {
	case "plan":
		w.plan = output
		PersistPlan(w.config.WorkDir, output, w.logger)
	case "build":
		w.buildOutput = output
	}
}

// capitalize returns s with the first letter uppercased.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// runReview waits for human feedback and transitions accordingly.
func (w *Workflow) runReview(ctx context.Context, approveTarget, redoTarget WorkflowStep) {
	if w.shouldAutoApprove(w.state.Current()) {
		if w.lastCommentAt.IsZero() {
			w.lastCommentAt = time.Now()
		}
		fb, err := w.fetchImmediateFeedback(ctx)
		if err != nil {
			w.logger.Warn("failed to check for feedback before auto-approval", "step", w.state.Current(), "error", err)
		} else if fb != nil {
			w.handleReviewFeedback(ctx, fb, approveTarget, redoTarget)
			return
		}
		w.logger.Info("auto-approving", "step", w.state.Current())
		w.status(fmt.Sprintf("Auto-approved %s", w.state.Current()))
		w.feedback = ""
		if err := w.approveTransition(ctx, approveTarget, "auto_approved"); err != nil {
			w.fail(ctx, err)
		}
		return
	}

	w.logger.Info("waiting for approval", "step", w.state.Current(), "issue", w.issue.Identifier)
	w.status(fmt.Sprintf("Waiting for approval on %s...", w.issue.Identifier))

	fb, err := PollForFeedback(ctx, w.tracker, w.issue.ID, w.lastCommentAt, w.config.PollInterval, w.logger, w.botCommentIDs)
	if err != nil {
		w.fail(ctx, fmt.Errorf("polling for feedback: %w", err))
		return
	}

	w.lastCommentAt = fb.Comment.CreatedAt
	w.handleReviewFeedback(ctx, fb, approveTarget, redoTarget)
}

func (w *Workflow) handleReviewFeedback(ctx context.Context, fb *FeedbackResult, approveTarget, redoTarget WorkflowStep) {
	switch fb.Action {
	case FeedbackApprove:
		w.applyReviewApproval(ctx, approveTarget, reviewApproval{
			logMessage:       "feedback: approved",
			statusMessage:    "Approved",
			transitionReason: "approved",
		})
	case FeedbackApproveAll:
		w.applyReviewApproval(ctx, approveTarget, reviewApproval{
			logMessage:       "feedback: approve all",
			statusMessage:    "Approve-all enabled",
			transitionReason: "approve_all",
			approveAll:       true,
		})
	case FeedbackRedo:
		w.applyReviewRedo(ctx, redoTarget, fb.Message, reviewRedo{
			logMessage:    "feedback: redo",
			statusMessage: "Redo requested",
		})
	case FeedbackComment:
		w.applyReviewRedo(ctx, redoTarget, fb.Message, reviewRedo{
			logMessage:    "feedback: comment",
			statusMessage: "Feedback received",
			logMessageKey: "message",
		})
	}
}

type reviewRedo struct {
	logMessage    string
	statusMessage string
	logMessageKey string
}

func (w *Workflow) applyReviewRedo(ctx context.Context, redoTarget WorkflowStep, feedback string, redo reviewRedo) {
	logArgs := []any{"step", w.state.Current()}
	if redo.logMessageKey != "" {
		logArgs = append(logArgs, redo.logMessageKey, feedback)
	}
	w.logger.Info(redo.logMessage, logArgs...)
	w.status(redo.statusMessage)
	w.feedback = feedback
	if err := w.tryRedo(ctx, redoTarget); err != nil {
		w.fail(ctx, err)
	}
}

func (w *Workflow) applyReviewApproval(ctx context.Context, approveTarget WorkflowStep, approval reviewApproval) {
	w.logger.Info(approval.logMessage, "step", w.state.Current())
	if err := w.approveTransition(ctx, approveTarget, approval.transitionReason); err != nil {
		w.fail(ctx, err)
		return
	}
	w.status(approval.statusMessage)
	w.feedback = ""
	if approval.approveAll {
		w.approveAllRemaining = true
	}
}

func (w *Workflow) fetchImmediateFeedback(ctx context.Context) (*FeedbackResult, error) {
	comments, err := w.tracker.FetchComments(ctx, w.issue.ID, w.lastCommentAt)
	if err != nil {
		return nil, err
	}

	exclude := make(map[string]bool, len(w.botCommentIDs))
	for _, id := range w.botCommentIDs {
		exclude[id] = true
	}
	latest := latestFeedbackComment(comments, exclude)
	if latest != nil {
		return &FeedbackResult{
			Action:  ParseCommentAction(latest.Body),
			Message: latest.Body,
			Comment: *latest,
		}, nil
	}
	return nil, nil
}

// approveTransition advances to approveTarget, then closes out the prior
// phase and skips ahead over any phases whose -done label was added mid-run.
func (w *Workflow) approveTransition(ctx context.Context, approveTarget WorkflowStep, trigger string) error {
	if err := w.transition(approveTarget, trigger); err != nil {
		return err
	}
	w.handlePhaseBoundary(ctx)
	return nil
}

func (w *Workflow) handlePhaseBoundary(ctx context.Context) {
	cur := w.state.Current()

	if cur == StepDone {
		// Reaching StepDone is authoritative evidence that every prior phase
		// ran to completion. Retry any -done writes that were left unfinished
		// by transient tracker failures at earlier boundaries (including ship
		// itself), so the final issue reflects the real workflow state.
		w.completePriorPhases(ctx, PhaseShip)
		if w.phases[PhaseShip] != phaseDone {
			w.completePhase(ctx, PhaseShip)
		}
		// Final sweep: completePhase's -inprogress removal is best-effort, so
		// a transient RemoveLabel failure can leave the issue with both
		// -inprogress and -done for a completed phase. At StepDone there's no
		// subsequent handlePhaseBoundary to reconcile via skipCompletedOrConfiguredPhases, so
		// do the cleanup here.
		w.cleanupStalePhaseLabels(ctx)
		return
	}

	if curPhase := phaseForStep(cur); curPhase != "" {
		w.completePriorPhases(ctx, curPhase)
	}

	w.skipCompletedOrConfiguredPhases(ctx)

	if phase := phaseForStep(w.state.Current()); phase != "" {
		w.enterPhase(ctx, phase)
	}
}

// cleanupStalePhaseLabels removes any -inprogress label still present on
// the issue for a phase that bookkeeping considers phaseDone. This is the
// terminal counterpart to skipCompletedOrConfiguredPhases' stale-label reconciliation —
// without it, a transient RemoveLabel failure during completePhase would
// leave the issue permanently tagged with both -inprogress and -done.
func (w *Workflow) cleanupStalePhaseLabels(ctx context.Context) {
	labels := w.refreshLabels(ctx)
	for _, p := range phaseTable {
		if w.phases[p.name] != phaseDone {
			continue
		}
		if !slices.Contains(labels, inProgressLabel(p.name)) {
			continue
		}
		if err := w.tracker.RemoveLabel(ctx, w.issue.ID, inProgressLabel(p.name)); err != nil {
			w.logger.Warn("failed to clear stale in-progress label at terminal sweep", "phase", p.name, "error", err)
		}
	}
}

// completePriorPhases walks phaseTable up to currentPhase and attempts to
// mark each earlier phase as done. Phases already phaseDone are skipped by
// completePhase itself. Phases still phaseNotStarted can happen when the
// initial enterPhase AddLabel failed transiently; since the state machine
// has since advanced past them, retry the -done write here so the issue
// reflects the workflow's actual progress.
func (w *Workflow) completePriorPhases(ctx context.Context, currentPhase string) {
	for _, p := range phaseTable {
		if p.name == currentPhase {
			return
		}
		if w.phases[p.name] != phaseDone {
			w.completePhase(ctx, p.name)
		}
	}
}

// skipCompletedOrConfiguredPhases refreshes labels from the tracker, then walks
// phases forward from the current step, jumping past any phase marked -done or
// configured to skip. If all remaining phases are complete or skipped, jumps to
// StepDone.
func (w *Workflow) skipCompletedOrConfiguredPhases(ctx context.Context) {
	labels := w.refreshLabels(ctx)
	w.reconcileSkipPhasesFromLabels(labels)

	for {
		cur := w.state.Current()
		phase := phaseForStep(cur)
		if phase == "" {
			return
		}
		hasDoneLabel := slices.Contains(labels, doneLabel(phase))
		if !hasDoneLabel && !w.isSkipPhase(phase) {
			return
		}
		// The -done label or explicit skip is authoritative, so clear any stale
		// -inprogress label left over from a prior interrupted run or a user
		// toggling phase state manually. Without this, the issue can show a
		// phase as both current and already done/skipped.
		if slices.Contains(labels, inProgressLabel(phase)) || w.phases[phase] == phaseInProgress {
			if err := w.tracker.RemoveLabel(ctx, w.issue.ID, inProgressLabel(phase)); err != nil {
				w.logger.Warn("failed to clear stale in-progress label while skipping phase", "phase", phase, "error", err)
			}
		}
		w.phases[phase] = phaseDone
		nextPhase := phaseAfter(phase)
		if nextPhase == "" {
			w.logger.Info("all phases already marked done or skipped; skipping to StepDone")
			w.forceTransition(StepDone)
			return
		}
		next := startStepForPhase(nextPhase)
		source := skipPhaseSourceDone
		if !hasDoneLabel {
			source = w.skipPhaseSources[phase]
		}
		w.logger.Info("skipping phase", "phase", phase, "source", source, "next", next)
		w.forceTransition(next)
		if phase == PhasePlan && w.plan == "" {
			if content, err := LoadPersistedPlan(w.config.WorkDir); err != nil {
				w.logger.Warn("failed to load persisted plan after skipping plan phase", "error", err)
			} else if content == "" {
				w.logger.Warn("skipping plan but no persisted plan found; build will run blind", "path", PlanFilePath(w.config.WorkDir))
			} else {
				w.plan = content
				w.logger.Info("loaded persisted plan from disk after skipping plan phase", "path", PlanFilePath(w.config.WorkDir))
			}
		}
	}
}

// refreshLabels fetches the latest label set from the tracker and returns it
// for phase decisions (including jiradozer-* bookkeeping labels). The
// user-facing subset is mirrored onto w.issue.Labels so agent prompts don't
// see bookkeeping noise via promptData. Labels and LabelIDs are filtered in
// lockstep so they continue to describe the same attachment set — Linear's
// nodeToIssue populates them in parallel, and any future caller using them
// together would otherwise get a mismatch. On fetch failure, returns the
// last known full set from w.lastLabels.
func (w *Workflow) refreshLabels(ctx context.Context) []string {
	fresh, err := w.tracker.FetchIssue(ctx, w.issue.Identifier)
	if err != nil || fresh == nil {
		if err != nil {
			w.logger.Warn("failed to refresh issue labels", "error", err)
		} else {
			w.logger.Warn("refresh issue returned nil without error; falling back to cached labels")
		}
		return w.lastLabels
	}
	w.lastLabels = fresh.Labels
	filteredLabels := make([]string, 0, len(fresh.Labels))
	filteredIDs := make([]string, 0, len(fresh.LabelIDs))
	for i, name := range fresh.Labels {
		if isJiradozerLabel(name) {
			continue
		}
		filteredLabels = append(filteredLabels, name)
		if i < len(fresh.LabelIDs) {
			filteredIDs = append(filteredIDs, fresh.LabelIDs[i])
		}
	}
	w.issue.Labels = filteredLabels
	w.issue.LabelIDs = filteredIDs
	return fresh.Labels
}

// enterPhase flips internal bookkeeping to inProgress only after the
// tracker confirms the -inprogress label write. On tracker failure the
// phase stays in phaseNotStarted so a retry (or the next event) will
// attempt the write again — keeping the in-memory state and the tracker
// from silently diverging.
func (w *Workflow) enterPhase(ctx context.Context, phase string) {
	if w.phases[phase] != phaseNotStarted {
		return
	}
	if err := w.tracker.AddLabel(ctx, w.issue.ID, inProgressLabel(phase)); err != nil {
		w.logger.Warn("failed to add phase in-progress label", "phase", phase, "error", err)
		return
	}
	w.phases[phase] = phaseInProgress
}

// completePhase flips internal bookkeeping to phaseDone only after the
// tracker confirms the -done label write. Order matters: add -done first,
// then remove -inprogress. A crash between these two calls leaves the
// issue with both labels, which skipCompletedOrConfiguredPhases reconciles on the next run.
// The reverse order would leave the issue with NEITHER label on crash,
// losing durable evidence that the phase completed and causing a resumed
// run to redo an already-finished phase.
//
// The -inprogress removal is best-effort (a stale -inprogress will be
// cleared on the next skipCompletedOrConfiguredPhases), but the -done write is
// authoritative: without it the phase stays "in progress" in memory and
// the workflow will retry the transition rather than advance past a phase
// the tracker didn't record.
func (w *Workflow) completePhase(ctx context.Context, phase string) {
	if w.phases[phase] == phaseDone {
		return
	}
	if err := w.tracker.AddLabel(ctx, w.issue.ID, doneLabel(phase)); err != nil {
		w.logger.Warn("failed to add phase done label", "phase", phase, "error", err)
		return
	}
	if err := w.tracker.RemoveLabel(ctx, w.issue.ID, inProgressLabel(phase)); err != nil {
		w.logger.Warn("failed to remove phase in-progress label", "phase", phase, "error", err)
	}
	w.phases[phase] = phaseDone
}

// tryRedo transitions to the redo target if the circuit breaker hasn't tripped.
// Returns an error (and transitions to StepFailed) if the step has been re-run
// maxRedos times.
func (w *Workflow) tryRedo(ctx context.Context, redoTarget WorkflowStep) error {
	w.approveAllRemaining = false
	w.redoCounts[redoTarget]++
	count := w.redoCounts[redoTarget]
	if count > w.maxRedos {
		return fmt.Errorf("step %s has been re-run %d times (max %d), giving up", redoTarget, count-1, w.maxRedos)
	}
	w.logger.Info("redo attempt", "target", redoTarget, "attempt", count, "max", w.maxRedos)
	if err := w.transition(redoTarget, "redo"); err != nil {
		return err
	}
	w.reopenPhaseOnRedo(ctx, redoTarget)
	w.skipCompletedOrConfiguredPhases(ctx)
	if w.state.Current() != redoTarget {
		w.feedback = ""
	}
	return nil
}

// reopenPhaseOnRedo keeps phase labels honest across backward (redo)
// transitions. The state machine allows transitions like
// ValidateReview → Building, which rewinds the agent into an already-
// completed phase. Without this reconciliation, the issue would keep
// jiradozer-build-done while the build agent is actively re-running, and
// observers would see stale labels until the next forward phase boundary.
//
// Semantics: when redoing into a phase earlier than (or the same as) the
// current in-progress phase, mark the target phase and every later phase
// as not-started again, replace any -done labels with -inprogress, and
// clear stray -inprogress labels on phases we're rewinding past.
func (w *Workflow) reopenPhaseOnRedo(ctx context.Context, redoTarget WorkflowStep) {
	targetPhase := phaseForStep(redoTarget)
	if targetPhase == "" {
		// Redoing into a review gate or terminal step has no phase of its
		// own; nothing to reconcile here.
		return
	}
	// Find targetPhase's index in phaseTable; every phase at or after that
	// index needs to be rewound.
	targetIdx := -1
	for i, p := range phaseTable {
		if p.name == targetPhase {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return
	}
	for i := targetIdx; i < len(phaseTable); i++ {
		phase := phaseTable[i].name
		prior := w.phases[phase]
		// Target phase: re-enter as in-progress. Later phases: reset to
		// not-started (they haven't happened in this rewound run).
		if i == targetIdx {
			if prior == phaseDone {
				if err := w.tracker.RemoveLabel(ctx, w.issue.ID, doneLabel(phase)); err != nil {
					w.logger.Warn("failed to remove -done label on redo", "phase", phase, "error", err)
				}
			}
			// Force a fresh -inprogress write even if prior was phaseInProgress,
			// in case the previous -inprogress was removed by skipCompletedOrConfiguredPhases or
			// a prior boundary.
			w.phases[phase] = phaseNotStarted
			w.enterPhase(ctx, phase)
		} else {
			// Phases "ahead" of the redo target shouldn't carry state from
			// the forward run we're rewinding. Clear any labels and reset
			// bookkeeping so the next forward pass starts clean.
			if prior == phaseInProgress {
				if err := w.tracker.RemoveLabel(ctx, w.issue.ID, inProgressLabel(phase)); err != nil {
					w.logger.Warn("failed to remove -inprogress on redo", "phase", phase, "error", err)
				}
			}
			if prior == phaseDone {
				if err := w.tracker.RemoveLabel(ctx, w.issue.ID, doneLabel(phase)); err != nil {
					w.logger.Warn("failed to remove -done on redo", "phase", phase, "error", err)
				}
			}
			w.phases[phase] = phaseNotStarted
		}
	}
}

func (w *Workflow) transitionToReview(ctx context.Context, reviewStep WorkflowStep, trigger string) {
	if err := w.transition(reviewStep, trigger); err != nil {
		w.fail(ctx, err)
		return
	}

	// Skip review machinery (issue state update, waiting comment) for non-review steps.
	if !reviewStep.IsReview() {
		return
	}

	if id, ok := w.stateIDs[stateKeyInReview]; ok {
		if err := w.tracker.UpdateIssueState(ctx, w.issue.ID, id); err != nil {
			w.logger.Warn("failed to update issue state to in_review", "error", err)
		}
	}

	if w.shouldAutoApprove(reviewStep) {
		return
	}

	waitingComment, err := PostWaitingComment(ctx, w.tracker, w.issue.ID, w.state.Current())
	if err != nil {
		w.logger.Warn("failed to post waiting comment", "error", err)
	} else if waitingComment.ID != "" {
		w.botCommentIDs = append(w.botCommentIDs, waitingComment.ID)
	}
	// lastCommentAt was set by the step result comment in runStep/runStepRounds.
	// Do not overwrite it here — doing so would skip user comments posted between
	// the result comment and the waiting comment.
	// If it is zero (step comment failed or returned no server timestamp), prefer
	// the waiting comment's server timestamp before falling back to local time.
	if w.lastCommentAt.IsZero() {
		if err == nil && !waitingComment.CreatedAt.IsZero() {
			w.lastCommentAt = waitingComment.CreatedAt
		} else {
			w.lastCommentAt = time.Now()
		}
	}
}

// shouldAutoApprove returns true if the given review step should be
// auto-approved (skipping human feedback polling).
func (w *Workflow) shouldAutoApprove(reviewStep WorkflowStep) bool {
	if !reviewStep.IsReview() {
		return false
	}
	if w.approveAllRemaining {
		return true
	}
	return w.shouldConfigAutoApprove(reviewStep)
}

func (w *Workflow) shouldConfigAutoApprove(reviewStep WorkflowStep) bool {
	switch reviewStep {
	case StepPlanReview:
		return w.config.Plan.AutoApprove
	case StepBuildReview:
		return w.config.Build.AutoApprove
	case StepValidateReview:
		return w.config.Validate.AutoApprove
	case StepShipReview:
		return w.config.Ship.AutoApprove
	}
	return false
}

// transition wraps state.Transition and fires the OnTransition callback on success.
func (w *Workflow) transition(target WorkflowStep, trigger string) error {
	if err := w.state.Transition(target, trigger); err != nil {
		return err
	}
	if w.OnTransition != nil {
		w.OnTransition(target)
	}
	return nil
}

// forceTransition bypasses state machine validation (used when jumping over
// already-done phases) but still fires OnTransition so subscribers see the
// jump.
func (w *Workflow) forceTransition(target WorkflowStep) {
	w.state.ForceState(target)
	if w.OnTransition != nil {
		w.OnTransition(target)
	}
}

func (w *Workflow) fail(ctx context.Context, err error) {
	w.lastError = err
	if tErr := w.transition(StepFailed, "error: "+err.Error()); tErr != nil {
		w.logger.Error("failed to transition to StepFailed, forcing state", "from", w.state.Current(), "error", tErr)
		w.state.ForceState(StepFailed)
	}
}

func (w *Workflow) resolveStateIDs(ctx context.Context) error {
	if w.issue.TeamID == "" {
		w.logger.Warn("issue has no team ID, skipping state resolution")
		return nil
	}

	states, err := w.tracker.FetchWorkflowStates(ctx, w.issue.TeamID)
	if err != nil {
		return err
	}

	nameMap := map[string]string{
		w.config.States.InProgress: stateKeyInProgress,
		w.config.States.InReview:   stateKeyInReview,
		w.config.States.Done:       stateKeyDone,
	}

	for _, s := range states {
		if logicalName, ok := nameMap[s.Name]; ok {
			w.stateIDs[logicalName] = s.ID
		}
	}

	return nil
}
