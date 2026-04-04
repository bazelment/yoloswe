package jiradozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// Workflow drives the issue through plan → build → validate → ship.
type Workflow struct {
	lastCommentAt time.Time
	tracker       tracker.IssueTracker
	lastError     error
	issue         *tracker.Issue
	state         *StateMachine
	config        *Config
	logger        *slog.Logger
	sessionIDs    map[WorkflowStep]string // per-step session IDs for resume
	stateIDs      map[string]string
	plan          string
	buildOutput   string
	feedback      string
}

// NewWorkflow creates a new workflow for the given issue.
func NewWorkflow(t tracker.IssueTracker, issue *tracker.Issue, cfg *Config, logger *slog.Logger) *Workflow {
	return &Workflow{
		tracker:    t,
		issue:      issue,
		state:      NewStateMachine(),
		config:     cfg,
		logger:     logger,
		stateIDs:   make(map[string]string),
		sessionIDs: make(map[WorkflowStep]string),
	}
}

// Run executes the workflow loop until completion or failure.
func (w *Workflow) Run(ctx context.Context) error {
	// Resolve workflow state names to IDs.
	if err := w.resolveStateIDs(ctx); err != nil {
		return fmt.Errorf("resolve workflow states: %w", err)
	}

	if err := w.state.Transition(StepPlanning, "start"); err != nil {
		return err
	}

	// Move issue to "In Progress".
	if id, ok := w.stateIDs["in_progress"]; ok {
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
			w.runStep(ctx, "plan", w.config.Plan, StepPlanReview, "plan_complete")
		case StepPlanReview:
			w.runReview(ctx, StepBuilding, StepPlanning)
		case StepBuilding:
			w.runStep(ctx, "build", w.config.Build, StepBuildReview, "build_complete")
		case StepBuildReview:
			w.runReview(ctx, StepValidating, StepBuilding)
		case StepValidating:
			w.runStep(ctx, "validate", w.config.Validate, StepValidateReview, "validation_complete")
		case StepValidateReview:
			w.runReview(ctx, StepShipping, StepValidating)
		case StepShipping:
			w.runStep(ctx, "ship", w.config.Ship, StepShipReview, "ship_complete")
		case StepShipReview:
			w.runReview(ctx, StepDone, StepShipping)
		case StepDone:
			w.logger.Info("workflow completed successfully")
			if id, ok := w.stateIDs["done"]; ok {
				if err := w.tracker.UpdateIssueState(ctx, w.issue.ID, id); err != nil {
					w.logger.Warn("failed to update issue state to done", "error", err)
				}
			}
			return nil
		case StepFailed:
			w.logger.Error("workflow failed", "error", w.lastError)
			return w.lastError
		}
	}
}

// runStep runs an agent session for the current workflow step.
func (w *Workflow) runStep(ctx context.Context, stepName string, stepCfg StepConfig, reviewStep WorkflowStep, trigger string) {
	currentStep := w.state.Current()
	sessionID := w.sessionIDs[currentStep]

	w.logger.Info("step: "+stepName, "feedback", w.feedback != "", "resume", sessionID != "")

	cfg := w.config.ResolveStep(stepCfg)
	data := NewPromptData(w.issue, w.config.BaseBranch)
	data.Plan = w.plan
	data.BuildOutput = w.buildOutput

	output, newSessionID, err := RunStepAgent(ctx, stepName, data, cfg, w.config.WorkDir, w.feedback, sessionID, w.logger)
	if err != nil {
		w.fail(ctx, fmt.Errorf("%s step: %w", stepName, err))
		return
	}

	w.sessionIDs[currentStep] = newSessionID

	// Capture outputs for downstream steps.
	switch stepName {
	case "plan":
		w.plan = output
	case "build":
		w.buildOutput = output
	}

	// Post result as comment.
	summary := output
	if len(summary) > 3000 {
		summary = summary[:3000] + "\n\n... (truncated)"
	}
	heading := strings.ToUpper(stepName[:1]) + stepName[1:]
	comment := fmt.Sprintf("## %s Complete\n\n%s", heading, summary)
	if err := w.tracker.PostComment(ctx, w.issue.ID, comment); err != nil {
		w.logger.Warn("failed to post "+stepName+" comment", "error", err)
	}

	w.transitionToReview(ctx, reviewStep, trigger)
}

// runReview waits for human feedback and transitions accordingly.
func (w *Workflow) runReview(ctx context.Context, approveTarget, redoTarget WorkflowStep) {
	fb, err := PollForFeedback(ctx, w.tracker, w.issue.ID, w.lastCommentAt, w.config.PollInterval, w.logger)
	if err != nil {
		w.fail(ctx, fmt.Errorf("polling for feedback: %w", err))
		return
	}

	w.lastCommentAt = fb.Comment.CreatedAt

	switch fb.Action {
	case FeedbackApprove:
		w.logger.Info("feedback: approved", "step", w.state.Current())
		w.feedback = ""
		if err := w.state.Transition(approveTarget, "approved"); err != nil {
			w.fail(ctx, err)
		}
	case FeedbackRedo:
		w.logger.Info("feedback: redo", "step", w.state.Current())
		w.feedback = fb.Message
		if err := w.state.Transition(redoTarget, "redo"); err != nil {
			w.fail(ctx, err)
		}
	case FeedbackComment:
		w.logger.Info("feedback: comment", "step", w.state.Current(), "message", fb.Message)
		w.feedback = fb.Message
		if err := w.state.Transition(redoTarget, "feedback"); err != nil {
			w.fail(ctx, err)
		}
	}
}

func (w *Workflow) transitionToReview(ctx context.Context, reviewStep WorkflowStep, trigger string) {
	if err := w.state.Transition(reviewStep, trigger); err != nil {
		w.fail(ctx, err)
		return
	}

	// Move issue to "In Review".
	if id, ok := w.stateIDs["in_review"]; ok {
		if err := w.tracker.UpdateIssueState(ctx, w.issue.ID, id); err != nil {
			w.logger.Warn("failed to update issue state to in_review", "error", err)
		}
	}

	w.lastCommentAt = time.Now()

	if err := PostWaitingComment(ctx, w.tracker, w.issue.ID, w.state.Current()); err != nil {
		w.logger.Warn("failed to post waiting comment", "error", err)
	}
}

func (w *Workflow) fail(ctx context.Context, err error) {
	w.lastError = err
	_ = w.state.Transition(StepFailed, "error: "+err.Error())
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
		w.config.States.InProgress: "in_progress",
		w.config.States.InReview:   "in_review",
		w.config.States.Done:       "done",
	}

	for _, s := range states {
		if logicalName, ok := nameMap[s.Name]; ok {
			w.stateIDs[logicalName] = s.ID
		}
	}

	return nil
}
