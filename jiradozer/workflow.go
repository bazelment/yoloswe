package jiradozer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/multiagent/agent"
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
	stateIDs      map[string]string
	agentModel    agent.AgentModel
	plan          string
	feedback      string
	skipTo        string
}

// NewWorkflow creates a new workflow for the given issue.
// skipTo optionally skips to a specific step (e.g. "build", "validate", "ship").
func NewWorkflow(t tracker.IssueTracker, issue *tracker.Issue, model agent.AgentModel, cfg *Config, logger *slog.Logger, skipTo string) *Workflow {
	return &Workflow{
		tracker:    t,
		issue:      issue,
		state:      NewStateMachine(),
		agentModel: model,
		config:     cfg,
		logger:     logger,
		stateIDs:   make(map[string]string),
		skipTo:     skipTo,
	}
}

// Run executes the workflow loop until completion or failure.
func (w *Workflow) Run(ctx context.Context) error {
	// Resolve workflow state names to IDs.
	if err := w.resolveStateIDs(ctx); err != nil {
		return fmt.Errorf("resolve workflow states: %w", err)
	}

	// Transition to first step (or skip-to target).
	firstStep, trigger := StepPlanning, "start"
	switch w.skipTo {
	case "build":
		firstStep, trigger = StepBuilding, "skip_to_build"
	case "validate":
		firstStep, trigger = StepValidating, "skip_to_validate"
	case "ship":
		firstStep, trigger = StepShipping, "skip_to_ship"
	case "", "plan":
		// default: start at planning
	default:
		return fmt.Errorf("unknown --skip-to value: %q (valid: plan, build, validate, ship)", w.skipTo)
	}
	if err := w.state.Transition(firstStep, trigger); err != nil {
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
			w.runPlanning(ctx)
		case StepPlanReview:
			w.runReview(ctx, StepBuilding, StepPlanning)
		case StepBuilding:
			w.runBuilding(ctx)
		case StepBuildReview:
			w.runReview(ctx, StepValidating, StepBuilding)
		case StepValidating:
			w.runValidating(ctx)
		case StepValidateReview:
			w.runReview(ctx, StepShipping, StepValidating)
		case StepShipping:
			w.runShipping(ctx)
		case StepShipReview:
			w.runReview(ctx, StepDone, StepBuilding)
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

func (w *Workflow) runPlanning(ctx context.Context) {
	w.logger.Info("step: planning", "feedback", w.feedback != "")

	plan, err := RunPlanAgent(ctx, w.agentModel, w.issue, w.config.Plan, w.config.WorkDir, w.config.MaxBudgetUSD, w.feedback, w.logger)
	if err != nil {
		w.fail(ctx, fmt.Errorf("plan step: %w", err))
		return
	}

	w.plan = plan

	// Post plan as comment.
	comment := fmt.Sprintf("## Implementation Plan\n\n%s", plan)
	if err := w.tracker.PostComment(ctx, w.issue.ID, comment); err != nil {
		w.logger.Warn("failed to post plan comment", "error", err)
	}

	w.transitionToReview(ctx, StepPlanReview, "plan_complete")
}

func (w *Workflow) runBuilding(ctx context.Context) {
	w.logger.Info("step: building", "feedback", w.feedback != "")

	if w.plan == "" {
		w.logger.Warn("no plan available (likely --skip-to build); agent will work from issue description only")
	}

	output, err := RunBuildAgent(ctx, w.agentModel, w.issue, w.plan, w.config.Build, w.config.WorkDir, w.config.MaxBudgetUSD, w.feedback, w.logger)
	if err != nil {
		w.fail(ctx, fmt.Errorf("build step: %w", err))
		return
	}

	// Post build summary as comment.
	summary := output
	if len(summary) > 3000 {
		summary = summary[:3000] + "\n\n... (truncated)"
	}
	comment := fmt.Sprintf("## Build Complete\n\n%s", summary)
	if err := w.tracker.PostComment(ctx, w.issue.ID, comment); err != nil {
		w.logger.Warn("failed to post build comment", "error", err)
	}

	w.transitionToReview(ctx, StepBuildReview, "build_complete")
}

func (w *Workflow) runValidating(ctx context.Context) {
	w.logger.Info("step: validating")

	commands := w.config.Validation.Commands
	if len(commands) == 0 {
		w.logger.Info("no validation commands configured, skipping")
		if err := w.state.Transition(StepShipping, "no_validation"); err != nil {
			w.fail(ctx, err)
		}
		return
	}

	timeout := time.Duration(w.config.Validation.TimeoutSeconds) * time.Second
	results, err := RunValidation(ctx, w.config.WorkDir, commands, timeout)
	if err != nil {
		w.fail(ctx, fmt.Errorf("validation step: %w", err))
		return
	}

	// Post results as comment.
	comment := FormatValidationResults(results)
	if err := w.tracker.PostComment(ctx, w.issue.ID, comment); err != nil {
		w.logger.Warn("failed to post validation comment", "error", err)
	}

	w.transitionToReview(ctx, StepValidateReview, "validation_complete")
}

func (w *Workflow) runShipping(ctx context.Context) {
	w.logger.Info("step: shipping")

	pr, err := CreatePR(ctx, w.issue, w.config.BaseBranch, w.config.WorkDir, w.logger)
	if err != nil {
		w.fail(ctx, fmt.Errorf("ship step: %w", err))
		return
	}

	comment := fmt.Sprintf("## Pull Request Created\n\n[PR #%d](%s)", pr.Number, pr.URL)
	if err := w.tracker.PostComment(ctx, w.issue.ID, comment); err != nil {
		w.logger.Warn("failed to post PR comment", "error", err)
	}

	w.transitionToReview(ctx, StepShipReview, "pr_created")
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
