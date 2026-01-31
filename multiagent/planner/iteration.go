package planner

import (
	"context"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/multiagent/progress"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// ExitReason indicates why the iteration loop exited.
// This pattern is adopted from yoloswe/swe.go for consistent exit handling.
type ExitReason string

const (
	// ExitReasonAccepted indicates the reviewer approved the changes.
	ExitReasonAccepted ExitReason = "accepted"
	// ExitReasonBudgetExceeded indicates the budget limit was reached.
	ExitReasonBudgetExceeded ExitReason = "budget"
	// ExitReasonTimeExceeded indicates the time limit was reached.
	ExitReasonTimeExceeded ExitReason = "timeout"
	// ExitReasonMaxIterations indicates the maximum iteration count was reached.
	ExitReasonMaxIterations ExitReason = "max_iterations"
	// ExitReasonError indicates an unrecoverable error occurred.
	ExitReasonError ExitReason = "error"
	// ExitReasonInterrupt indicates the user interrupted (Ctrl+C).
	ExitReasonInterrupt ExitReason = "interrupt"
)

// IsSuccess returns true if the exit reason indicates successful completion.
func (r ExitReason) IsSuccess() bool {
	return r == ExitReasonAccepted
}

// IterationConfig controls the iteration loop behavior.
type IterationConfig struct {
	// MaxIterations is the maximum number of builder-reviewer cycles.
	// 0 means unlimited (use with caution).
	MaxIterations int

	// MaxBudgetUSD is the budget limit for the iteration loop.
	// 0 means no budget limit.
	MaxBudgetUSD float64

	// MaxDuration is the time limit for the iteration loop.
	// 0 means no time limit.
	MaxDuration time.Duration

	// AutoApprove skips review approval prompts when true.
	AutoApprove bool
}

// DefaultIterationConfig returns sensible defaults for iteration configuration.
func DefaultIterationConfig() IterationConfig {
	return IterationConfig{
		MaxIterations: 10,
		MaxBudgetUSD:  5.0,
		MaxDuration:   30 * time.Minute,
		AutoApprove:   true,
	}
}

// IterationResult contains the outcome of an iteration loop.
type IterationResult struct {
	FinalError     error
	LastReview     *protocol.ReviewResponse
	ExitReason     ExitReason
	FilesCreated   []string
	FilesModified  []string
	TotalDuration  time.Duration
	TotalCostUSD   float64
	IterationCount int
}

// RunIterationLoop executes the builder-reviewer cycle until an exit condition is met.
// This implements the pattern from yoloswe/swe.go with budget/time/iteration checks.
//
// The loop follows this pattern:
//  1. Check limits (time, budget, iteration count)
//  2. Run builder with current request
//  3. Run reviewer on builder output
//  4. If approved, exit with success
//  5. If rejected, update build request with feedback and continue
func (p *Planner) RunIterationLoop(ctx context.Context, design *protocol.DesignResponse, buildReq *protocol.BuildRequest) (*IterationResult, error) {
	startTime := time.Now()
	result := &IterationResult{
		FilesCreated:  make([]string, 0),
		FilesModified: make([]string, 0),
	}

	// Get iteration config
	iterConfig := p.GetIterationConfig()

	for iteration := 1; ; iteration++ {
		result.IterationCount = iteration

		// Emit iteration start event
		if p.progress != nil {
			p.progress.Event(progress.NewIterationEvent(iteration, p.GetIterationConfig().MaxIterations, "iteration_start"))
		}

		// === Check Limits ===

		// Check context cancellation
		if ctx.Err() != nil {
			result.ExitReason = ExitReasonInterrupt
			result.TotalDuration = time.Since(startTime)
			result.TotalCostUSD = p.TotalCost()
			return result, nil
		}

		// Check time limit
		elapsed := time.Since(startTime)
		if iterConfig.MaxDuration > 0 && elapsed >= iterConfig.MaxDuration {
			result.ExitReason = ExitReasonTimeExceeded
			result.TotalDuration = elapsed
			result.TotalCostUSD = p.TotalCost()
			return result, nil
		}

		// Check budget limit
		currentCost := p.TotalCost()
		if iterConfig.MaxBudgetUSD > 0 && currentCost >= iterConfig.MaxBudgetUSD {
			result.ExitReason = ExitReasonBudgetExceeded
			result.TotalDuration = elapsed
			result.TotalCostUSD = currentCost
			return result, nil
		}

		// Check iteration limit
		if iterConfig.MaxIterations > 0 && iteration > iterConfig.MaxIterations {
			result.ExitReason = ExitReasonMaxIterations
			result.TotalDuration = elapsed
			result.TotalCostUSD = currentCost
			return result, nil
		}

		// === Builder Phase ===
		if p.stateMachine != nil {
			_ = p.stateMachine.Transition(StateBuilding, "iteration_build")
		}

		buildResp, err := p.CallBuilder(ctx, buildReq)
		if err != nil {
			if ctx.Err() != nil {
				result.ExitReason = ExitReasonInterrupt
				result.TotalDuration = time.Since(startTime)
				result.TotalCostUSD = p.TotalCost()
				return result, nil
			}
			result.ExitReason = ExitReasonError
			result.FinalError = fmt.Errorf("builder failed on iteration %d: %w", iteration, err)
			result.TotalDuration = time.Since(startTime)
			result.TotalCostUSD = p.TotalCost()
			return result, result.FinalError
		}

		// Track files from this iteration
		result.FilesCreated = appendUnique(result.FilesCreated, buildResp.FilesCreated...)
		result.FilesModified = appendUnique(result.FilesModified, buildResp.FilesModified...)

		// === Reviewer Phase ===
		if p.stateMachine != nil {
			_ = p.stateMachine.Transition(StateReviewing, "iteration_review")
		}

		reviewReq := &protocol.ReviewRequest{
			Task:           buildReq.Task,
			FilesChanged:   append(buildResp.FilesCreated, buildResp.FilesModified...),
			OriginalDesign: design,
		}

		reviewResp, err := p.CallReviewer(ctx, reviewReq)
		if err != nil {
			if ctx.Err() != nil {
				result.ExitReason = ExitReasonInterrupt
				result.TotalDuration = time.Since(startTime)
				result.TotalCostUSD = p.TotalCost()
				return result, nil
			}
			result.ExitReason = ExitReasonError
			result.FinalError = fmt.Errorf("reviewer failed on iteration %d: %w", iteration, err)
			result.TotalDuration = time.Since(startTime)
			result.TotalCostUSD = p.TotalCost()
			return result, result.FinalError
		}

		result.LastReview = reviewResp

		// Check for acceptance
		if !reviewResp.HasCriticalIssues() {
			result.ExitReason = ExitReasonAccepted
			result.TotalDuration = time.Since(startTime)
			result.TotalCostUSD = p.TotalCost()

			// Emit iteration complete event
			if p.progress != nil {
				p.progress.Event(progress.NewIterationEvent(iteration, p.GetIterationConfig().MaxIterations, "iteration_accepted"))
			}

			return result, nil
		}

		// === Prepare Next Iteration ===

		// Update build request with reviewer feedback
		buildReq.Feedback = reviewResp

		// Emit iteration complete event (continuing)
		if p.progress != nil {
			// Indicate we're continuing to next iteration
			p.progress.Event(progress.NewIterationEvent(iteration, p.GetIterationConfig().MaxIterations, "iteration_rejected_continuing"))
		}

		// Transition back to planning state for next iteration decision
		if p.stateMachine != nil {
			_ = p.stateMachine.Transition(StatePlanning, "iteration_continue")
		}
	}
}

// appendUnique appends items to a slice, avoiding duplicates.
func appendUnique(slice []string, items ...string) []string {
	seen := make(map[string]bool, len(slice))
	for _, s := range slice {
		seen[s] = true
	}

	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			slice = append(slice, item)
		}
	}

	return slice
}

// GetIterationConfig returns the current iteration configuration.
// If not set, returns default configuration.
func (p *Planner) GetIterationConfig() IterationConfig {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.iterConfig == nil {
		cfg := DefaultIterationConfig()
		return cfg
	}
	return *p.iterConfig
}

// SetIterationConfig sets the iteration configuration.
func (p *Planner) SetIterationConfig(cfg IterationConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.iterConfig = &cfg
}

// RunDesignBuildReviewLoop runs a complete design-build-review workflow with iteration.
// This is a convenience method that:
//  1. Calls the designer to create a design
//  2. Runs the iteration loop with the design
//  3. Returns the combined result
func (p *Planner) RunDesignBuildReviewLoop(ctx context.Context, task string, workDir string) (*IterationResult, error) {
	// === Design Phase ===
	if p.stateMachine != nil {
		_ = p.stateMachine.Transition(StateDesigning, "design_start")
	}

	designReq := &protocol.DesignRequest{
		Task: task,
	}

	designResp, err := p.CallDesigner(ctx, designReq)
	if err != nil {
		return nil, fmt.Errorf("design phase failed: %w", err)
	}

	// Transition to planning before iteration
	if p.stateMachine != nil {
		_ = p.stateMachine.Transition(StatePlanning, "design_complete")
	}

	// === Build-Review Iteration ===
	buildReq := &protocol.BuildRequest{
		Task:    task,
		WorkDir: workDir,
		Design:  designResp,
	}

	result, err := p.RunIterationLoop(ctx, designResp, buildReq)
	if err != nil {
		return result, err
	}

	// Mark complete if successful
	if result.ExitReason.IsSuccess() && p.stateMachine != nil {
		_ = p.stateMachine.Transition(StateCompleted, "iteration_accepted")
	}

	return result, nil
}

// FormatFeedbackForBuilder formats reviewer feedback into a string for the builder.
// This follows the pattern from yoloswe/swe.go formatFeedback.
func FormatFeedbackForBuilder(review *protocol.ReviewResponse) string {
	if review == nil {
		return ""
	}
	if len(review.Issues) == 0 {
		return review.Summary
	}

	feedback := review.Summary + "\n\nIssues to address:\n"

	for i, issue := range review.Issues {
		feedback += fmt.Sprintf("\n%d. [%s] %s", i+1, issue.Severity, issue.Message)
		if issue.File != "" {
			feedback += fmt.Sprintf("\n   File: %s", issue.File)
			if issue.Line > 0 {
				feedback += fmt.Sprintf(":%d", issue.Line)
			}
		}
		if issue.Suggestion != "" {
			feedback += fmt.Sprintf("\n   Suggestion: %s", issue.Suggestion)
		}
	}

	return feedback
}
