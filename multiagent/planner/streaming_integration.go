package planner

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
	"github.com/bazelment/yoloswe/multiagent/progress"
	"github.com/bazelment/yoloswe/multiagent/protocol"
	"github.com/bazelment/yoloswe/multiagent/subagent"
	"github.com/bazelment/yoloswe/multiagent/subagents/designer"
)

// requestCounter generates unique request IDs
var requestCounter uint64

func generateRequestID() string {
	id := atomic.AddUint64(&requestCounter, 1)
	return fmt.Sprintf("req-%d", id)
}

// StreamingPlanner extends Planner with streaming sub-agent support.
// It wraps the existing Planner and adds active sub-agent tracking.
type StreamingPlanner struct {
	*Planner

	// Active sub-agents for cancellation support
	activeSubAgents map[string]*subagent.StreamingSubAgent
}

// NewStreamingPlanner creates a new StreamingPlanner wrapping the given Planner.
func NewStreamingPlanner(p *Planner) *StreamingPlanner {
	return &StreamingPlanner{
		Planner:         p,
		activeSubAgents: make(map[string]*subagent.StreamingSubAgent),
	}
}

// CallDesignerStreaming invokes the Designer sub-agent with streaming support.
func (sp *StreamingPlanner) CallDesignerStreaming(ctx context.Context, req *protocol.DesignRequest) (*protocol.DesignResponse, error) {
	startTime := time.Now()

	// Check iteration limit before proceeding
	if err := sp.checkIterations(); err != nil {
		return nil, err
	}

	// Get current iteration for progress reporting
	sp.mu.Lock()
	iteration := sp.iterationCount + 1
	sp.mu.Unlock()

	// Create streaming sub-agent (generate requestID first for consistent tracking)
	requestID := generateRequestID()

	// Progress: phase change to designing
	if sp.progress != nil {
		sp.progress.Event(progress.NewPhaseChangeEvent(checkpoint.PhaseNotStarted, checkpoint.PhaseDesigning, iteration))
		sp.progress.Event(progress.NewAgentStartEvent(agent.RoleDesigner, requestID, "Analyzing requirements and creating design"))
	}

	// Checkpoint: starting design phase
	if sp.checkpointMgr != nil {
		if err := sp.checkpointMgr.StartDesign(); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}
	sub := subagent.NewStreamingSubAgent(sp.designerConfig, sp.swarmSessionID, requestID, subagent.AgentTypeDesigner)

	// Store for cancellation
	sp.mu.Lock()
	sp.activeSubAgents[requestID] = sub
	sp.mu.Unlock()
	defer func() {
		sp.mu.Lock()
		delete(sp.activeSubAgents, requestID)
		sp.mu.Unlock()
	}()

	// Forward events to progress reporter in background
	go sp.forwardSubAgentEvents(sub, agent.RoleDesigner)

	// Execute
	prompt := formatDesignPrompt(req)
	result, err := sub.Execute(ctx, prompt)

	// Increment iteration count after the call
	sp.incrementIterations()

	duration := time.Since(startTime)

	if err != nil {
		// Progress: agent failed
		if sp.progress != nil {
			sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleDesigner, requestID, false, sub.TotalCost(), duration, err))
		}
		// Checkpoint: record failure
		if sp.checkpointMgr != nil {
			_ = sp.checkpointMgr.Fail(err)
		}
		return nil, fmt.Errorf("designer failed (request %s): %w", requestID, err)
	}

	// Update cost and file tracking
	sp.mu.Lock()
	sp.totalCost += result.TotalCostUSD
	sp.filesCreated = append(sp.filesCreated, result.FilesCreated...)
	sp.filesModified = append(sp.filesModified, result.FilesModified...)
	sp.mu.Unlock()

	if !result.Success {
		failErr := fmt.Errorf("designer task failed: %s", result.Error)
		// Progress: agent failed
		if sp.progress != nil {
			sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleDesigner, requestID, false, result.TotalCostUSD, duration, failErr))
		}
		if sp.checkpointMgr != nil {
			_ = sp.checkpointMgr.Fail(failErr)
		}
		return nil, failErr
	}

	// Progress: agent completed successfully
	if sp.progress != nil {
		sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleDesigner, requestID, true, result.TotalCostUSD, duration, nil))
	}

	// Parse response if not already parsed
	var response *protocol.DesignResponse
	if result.Design != nil {
		response = result.Design
	} else if result.Text != "" {
		response, err = designer.ParseDesignResponse(result.Text)
		if err != nil {
			// Log but don't fail - return empty response
			fmt.Printf("Warning: failed to parse design response: %v\n", err)
			response = &protocol.DesignResponse{}
		}
	} else {
		response = &protocol.DesignResponse{}
	}

	// Checkpoint: design completed successfully
	if sp.checkpointMgr != nil {
		if err := sp.checkpointMgr.CompleteDesign(response, result.TotalCostUSD); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	return response, nil
}

// CallBuilderStreaming invokes the Builder sub-agent with streaming support.
func (sp *StreamingPlanner) CallBuilderStreaming(ctx context.Context, req *protocol.BuildRequest) (*protocol.BuildResponse, error) {
	startTime := time.Now()

	// Check iteration limit before proceeding
	if err := sp.checkIterations(); err != nil {
		return nil, err
	}

	// Get current iteration for progress reporting
	sp.mu.Lock()
	iteration := sp.iterationCount + 1
	sp.mu.Unlock()

	// Create streaming sub-agent (generate requestID first for consistent tracking)
	requestID := generateRequestID()

	// Progress: phase change to building
	if sp.progress != nil {
		sp.progress.Event(progress.NewPhaseChangeEvent(checkpoint.PhaseDesigning, checkpoint.PhaseBuilding, iteration))
		sp.progress.Event(progress.NewAgentStartEvent(agent.RoleBuilder, requestID, "Implementing code changes"))
	}

	// Checkpoint: starting build phase
	if sp.checkpointMgr != nil {
		if err := sp.checkpointMgr.StartBuild(); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}
	sub := subagent.NewStreamingSubAgent(sp.builderConfig, sp.swarmSessionID, requestID, subagent.AgentTypeBuilder)

	// Store for cancellation
	sp.mu.Lock()
	sp.activeSubAgents[requestID] = sub
	sp.mu.Unlock()
	defer func() {
		sp.mu.Lock()
		delete(sp.activeSubAgents, requestID)
		sp.mu.Unlock()
	}()

	// Forward events to progress reporter in background
	go sp.forwardSubAgentEvents(sub, agent.RoleBuilder)

	// Execute
	prompt := formatBuildPrompt(req)
	result, err := sub.Execute(ctx, prompt)

	// Increment iteration count after the call
	sp.incrementIterations()

	duration := time.Since(startTime)

	if err != nil {
		// Progress: agent failed
		if sp.progress != nil {
			sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleBuilder, requestID, false, sub.TotalCost(), duration, err))
		}
		if sp.checkpointMgr != nil {
			_ = sp.checkpointMgr.Fail(err)
		}
		return nil, fmt.Errorf("builder failed (request %s): %w", requestID, err)
	}

	// Update cost and file tracking
	sp.mu.Lock()
	sp.totalCost += result.TotalCostUSD
	sp.filesCreated = append(sp.filesCreated, result.FilesCreated...)
	sp.filesModified = append(sp.filesModified, result.FilesModified...)
	sp.mu.Unlock()

	if !result.Success {
		failErr := fmt.Errorf("builder task failed: %s", result.Error)
		// Progress: agent failed
		if sp.progress != nil {
			sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleBuilder, requestID, false, result.TotalCostUSD, duration, failErr))
		}
		if sp.checkpointMgr != nil {
			_ = sp.checkpointMgr.Fail(failErr)
		}
		return nil, failErr
	}

	// Progress: agent completed successfully
	if sp.progress != nil {
		sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleBuilder, requestID, true, result.TotalCostUSD, duration, nil))
	}

	// Build response from tracked state
	response := &protocol.BuildResponse{
		FilesCreated:  result.FilesCreated,
		FilesModified: result.FilesModified,
	}

	// Checkpoint: build completed successfully
	if sp.checkpointMgr != nil {
		if err := sp.checkpointMgr.CompleteBuild(response, result.TotalCostUSD); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	return response, nil
}

// CallReviewerStreaming invokes the Reviewer sub-agent with streaming support.
func (sp *StreamingPlanner) CallReviewerStreaming(ctx context.Context, req *protocol.ReviewRequest) (*protocol.ReviewResponse, error) {
	startTime := time.Now()

	// Check iteration limit before proceeding
	if err := sp.checkIterations(); err != nil {
		return nil, err
	}

	// Get current iteration for progress reporting
	sp.mu.Lock()
	iteration := sp.iterationCount + 1
	sp.mu.Unlock()

	// Create streaming sub-agent (generate requestID first for consistent tracking)
	requestID := generateRequestID()

	// Progress: phase change to reviewing
	if sp.progress != nil {
		sp.progress.Event(progress.NewPhaseChangeEvent(checkpoint.PhaseBuilding, checkpoint.PhaseReviewing, iteration))
		sp.progress.Event(progress.NewAgentStartEvent(agent.RoleReviewer, requestID, "Reviewing implementation"))
	}

	// Checkpoint: starting review phase
	if sp.checkpointMgr != nil {
		if err := sp.checkpointMgr.StartReview(); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}
	sub := subagent.NewStreamingSubAgent(sp.reviewerConfig, sp.swarmSessionID, requestID, subagent.AgentTypeReviewer)

	// Store for cancellation
	sp.mu.Lock()
	sp.activeSubAgents[requestID] = sub
	sp.mu.Unlock()
	defer func() {
		sp.mu.Lock()
		delete(sp.activeSubAgents, requestID)
		sp.mu.Unlock()
	}()

	// Forward events to progress reporter in background
	go sp.forwardSubAgentEvents(sub, agent.RoleReviewer)

	// Execute
	prompt := formatReviewPrompt(req)
	result, err := sub.Execute(ctx, prompt)

	// Increment iteration count after the call
	sp.incrementIterations()

	duration := time.Since(startTime)

	if err != nil {
		// Progress: agent failed
		if sp.progress != nil {
			sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleReviewer, requestID, false, sub.TotalCost(), duration, err))
		}
		if sp.checkpointMgr != nil {
			_ = sp.checkpointMgr.Fail(err)
		}
		return nil, fmt.Errorf("reviewer failed (request %s): %w", requestID, err)
	}

	// Update cost
	sp.mu.Lock()
	sp.totalCost += result.TotalCostUSD
	sp.mu.Unlock()

	if !result.Success {
		failErr := fmt.Errorf("reviewer task failed: %s", result.Error)
		// Progress: agent failed
		if sp.progress != nil {
			sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleReviewer, requestID, false, result.TotalCostUSD, duration, failErr))
		}
		if sp.checkpointMgr != nil {
			_ = sp.checkpointMgr.Fail(failErr)
		}
		return nil, failErr
	}

	// Progress: agent completed successfully
	if sp.progress != nil {
		sp.progress.Event(progress.NewAgentCompleteEvent(agent.RoleReviewer, requestID, true, result.TotalCostUSD, duration, nil))
	}

	// Return review from result or empty
	response := result.Review
	if response == nil {
		response = &protocol.ReviewResponse{}
	}

	// Checkpoint: review completed successfully
	if sp.checkpointMgr != nil {
		if err := sp.checkpointMgr.CompleteReview(response, result.TotalCostUSD); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	return response, nil
}

// CancelSubAgent cancels a running sub-agent by request ID.
func (sp *StreamingPlanner) CancelSubAgent(requestID, reason string) error {
	sp.mu.Lock()
	sub, exists := sp.activeSubAgents[requestID]
	sp.mu.Unlock()

	if !exists {
		return fmt.Errorf("no active sub-agent with request ID: %s", requestID)
	}

	sub.Cancel(reason)
	return nil
}

// CancelAllSubAgents cancels all active sub-agents.
func (sp *StreamingPlanner) CancelAllSubAgents(reason string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	for _, sub := range sp.activeSubAgents {
		sub.Cancel(reason)
	}
}

// ActiveSubAgentCount returns the number of active sub-agents.
func (sp *StreamingPlanner) ActiveSubAgentCount() int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return len(sp.activeSubAgents)
}

// forwardSubAgentEvents routes sub-agent events to the progress reporter.
func (sp *StreamingPlanner) forwardSubAgentEvents(sub *subagent.StreamingSubAgent, role agent.AgentRole) {
	for event := range sub.Events() {
		switch e := event.(type) {
		case *subagent.Progress:
			// Forward tool events as tool start/complete
			if sp.progress != nil && e.ToolName != "" {
				if e.ToolStarted {
					sp.progress.Event(progress.NewToolStartEvent(role, e.ToolName, e.ToolID))
				} else {
					sp.progress.Event(progress.NewToolCompleteEvent(role, e.ToolName, e.ToolID, e.ToolInput))
				}
			}

		case *subagent.FileEvent:
			// Files are already tracked in the result, but we could emit events here
			// if needed for real-time UI updates

		case *subagent.CostUpdate:
			// Could emit cost update events for real-time budget tracking
			// For now, costs are aggregated in the result
		}
	}
}
