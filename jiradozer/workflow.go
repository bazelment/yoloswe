package jiradozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// State keys for the stateIDs map, mapping logical workflow states to tracker state IDs.
const (
	stateKeyInProgress = "in_progress"
	stateKeyInReview   = "in_review"
	stateKeyDone       = "done"
)

// Workflow drives the issue through plan → build → create_pr → validate → ship.
type Workflow struct {
	lastCommentAt   time.Time
	tracker         tracker.IssueTracker
	lastError       error
	config          *Config
	state           *StateMachine
	logger          *slog.Logger
	sessionIDs      map[WorkflowStep]string
	stateIDs        map[string]string
	OnTransition    func(step WorkflowStep)
	OnRoundProgress func(roundIndex, roundTotal int)
	runStepAgent    func(ctx context.Context, stepName string, data PromptData, cfg StepConfig, workDir string, feedback string, resumeSessionID string, logger *slog.Logger) (string, string, error)
	issue           *tracker.Issue
	plan            string
	buildOutput     string
	feedback        string
	botCommentIDs   []string
}

// NewWorkflow creates a new workflow for the given issue.
func NewWorkflow(t tracker.IssueTracker, issue *tracker.Issue, cfg *Config, logger *slog.Logger) *Workflow {
	return &Workflow{
		tracker:      t,
		issue:        issue,
		state:        NewStateMachine(),
		config:       cfg,
		logger:       logger,
		stateIDs:     make(map[string]string),
		sessionIDs:   make(map[WorkflowStep]string),
		runStepAgent: RunStepAgent,
	}
}

// Run executes the workflow loop until completion or failure.
func (w *Workflow) Run(ctx context.Context) error {
	w.logger.Info("workflow starting",
		"issue", w.issue.Identifier,
		"title", w.issue.Title,
		"model", w.config.Agent.Model,
		"work_dir", w.config.WorkDir,
		"base_branch", w.config.BaseBranch,
	)
	workflowStart := time.Now()
	defer func() {
		finalStep := w.state.Current().String()
		duration := time.Since(workflowStart)
		if w.lastError != nil {
			w.logger.Error("workflow finished",
				"issue", w.issue.Identifier,
				"final_step", finalStep,
				"error", w.lastError,
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
			w.logger.Info("workflow completed successfully")
			if id, ok := w.stateIDs[stateKeyDone]; ok {
				if err := w.tracker.UpdateIssueState(ctx, w.issue.ID, id); err != nil {
					w.logger.Warn("failed to update issue state to done", "error", err)
				}
			}
			return nil
		case StepFailed:
			w.logger.Error("workflow failed", "error", w.lastError)
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
	stepStart := time.Now()

	data := w.promptData()
	var allOutputs []string
	feedbackInjected := false
	for i, round := range stepCfg.Rounds {
		if ctx.Err() != nil {
			w.fail(ctx, ctx.Err())
			return
		}

		w.logger.Info("round start", "step", stepName, "round", i+1, "total", totalRounds)
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
			output, _, err = w.runStepAgent(ctx, stepName, data, roundCfg, w.config.WorkDir, roundFeedback, "", w.logger)
			if err != nil {
				w.fail(ctx, fmt.Errorf("%s round %d/%d: %w", stepName, i+1, totalRounds, err))
				return
			}
		}
		allOutputs = append(allOutputs, output)

		heading := capitalize(stepName)
		comment := fmt.Sprintf("## %s Round %d/%d\n\n%s", heading, i+1, totalRounds, truncateOutput(output, 2000))
		roundComment, err := w.tracker.PostComment(ctx, w.issue.ID, comment)
		if err != nil {
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

	// Filter empty round outputs before joining so separator-only text
	// (e.g. "\n\n---\n\n") is not mistaken for real plan content.
	var nonEmpty []string
	for _, o := range allOutputs {
		if strings.TrimSpace(o) != "" {
			nonEmpty = append(nonEmpty, o)
		}
	}
	w.logger.Info("step completed", "step", stepName, "issue", w.issue.Identifier, "rounds", totalRounds, "duration", time.Since(stepStart))
	w.captureOutput(stepName, strings.Join(nonEmpty, "\n\n---\n\n"))
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

	stepStart := time.Now()
	cfg := w.config.ResolveStep(stepCfg)
	output, newSessionID, err := w.runStepAgent(ctx, stepName, w.promptData(), cfg, w.config.WorkDir, feedback, sessionID, w.logger)
	if err != nil {
		w.fail(ctx, fmt.Errorf("%s step: %w", stepName, err))
		return
	}

	w.logger.Info("step completed", "step", stepName, "issue", w.issue.Identifier, "session_id", newSessionID, "duration", time.Since(stepStart))
	w.sessionIDs[currentStep] = newSessionID
	w.captureOutput(stepName, output)

	heading := capitalize(stepName)
	comment := fmt.Sprintf("## %s Complete\n\n%s", heading, output)
	resultComment, err := w.tracker.PostComment(ctx, w.issue.ID, comment)
	if err != nil {
		w.logger.Warn("failed to post step comment", "step", stepName, "error", err)
	} else {
		if resultComment.ID != "" {
			w.botCommentIDs = append(w.botCommentIDs, resultComment.ID)
		}
		if !resultComment.CreatedAt.IsZero() {
			w.lastCommentAt = resultComment.CreatedAt
		}
	}

	w.transitionToReview(ctx, reviewStep, trigger)
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

// truncateOutput shortens text that exceeds maxLen runes, appending a truncation notice.
func truncateOutput(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen]) + "\n\n... (truncated)"
	}
	return s
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
		w.logger.Info("auto-approving", "step", w.state.Current())
		w.feedback = ""
		if err := w.transition(approveTarget, "auto_approved"); err != nil {
			w.fail(ctx, err)
		}
		return
	}

	w.logger.Info("waiting for approval", "step", w.state.Current(), "issue", w.issue.Identifier)

	fb, err := PollForFeedback(ctx, w.tracker, w.issue.ID, w.lastCommentAt, w.config.PollInterval, w.logger, w.botCommentIDs)
	if err != nil {
		w.fail(ctx, fmt.Errorf("polling for feedback: %w", err))
		return
	}

	w.lastCommentAt = fb.Comment.CreatedAt

	switch fb.Action {
	case FeedbackApprove:
		w.logger.Info("feedback: approved", "step", w.state.Current())
		w.feedback = ""
		if err := w.transition(approveTarget, "approved"); err != nil {
			w.fail(ctx, err)
		}
	case FeedbackRedo:
		w.logger.Info("feedback: redo", "step", w.state.Current())
		w.feedback = fb.Message
		if err := w.transition(redoTarget, "redo"); err != nil {
			w.fail(ctx, err)
		}
	case FeedbackComment:
		w.logger.Info("feedback: comment", "step", w.state.Current(), "message", fb.Message)
		w.feedback = fb.Message
		if err := w.transition(redoTarget, "feedback"); err != nil {
			w.fail(ctx, err)
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
		w.lastCommentAt = time.Now()
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
