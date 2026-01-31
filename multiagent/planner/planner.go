// Package planner implements the Planner agent that coordinates sub-agents.
package planner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
	"github.com/bazelment/yoloswe/multiagent/progress"
	"github.com/bazelment/yoloswe/multiagent/protocol"
	"github.com/bazelment/yoloswe/multiagent/subagents/builder"
	"github.com/bazelment/yoloswe/multiagent/subagents/designer"
	"github.com/bazelment/yoloswe/multiagent/subagents/reviewer"
)

// ErrMaxIterationsExceeded is returned when the iteration count exceeds the limit.
var ErrMaxIterationsExceeded = errors.New("max iterations exceeded")

// Planner is a long-running agent that coordinates Designer, Builder, and Reviewer.
type Planner struct {
	progress            progress.Reporter
	session             *agent.LongRunningSession
	iterConfig          *IterationConfig
	stateMachine        *StateMachine
	checkpointMgr       *checkpoint.Manager
	mcpServer           *MCPServer
	swarmSessionID      string
	filesModified       []string
	filesCreated        []string
	reviewerConfig      agent.AgentConfig
	config              agent.AgentConfig
	designerConfig      agent.AgentConfig
	builderConfig       agent.AgentConfig
	phaseStats          PhaseStats
	maxIterations       int
	totalCost           float64
	iterationCount      int
	mu                  sync.Mutex
	checkpointEnabled   bool
	waitingForUserInput bool
	inBuildPhase        bool
	pendingBuildStart   bool
}

// Config holds configuration for the Planner and its sub-agents.
type Config struct {
	Progress            progress.Reporter
	SessionDir          string
	PlannerConfig       agent.AgentConfig
	DesignerConfig      agent.AgentConfig
	BuilderConfig       agent.AgentConfig
	ReviewerConfig      agent.AgentConfig
	MaxIterations       int
	EnableCheckpointing bool
}

// New creates a new Planner agent.
func New(cfg Config, swarmSessionID string) *Planner {
	cfg.PlannerConfig.Role = agent.RolePlanner
	if cfg.PlannerConfig.SystemPrompt == "" {
		cfg.PlannerConfig.SystemPrompt = SystemPrompt
	}

	p := &Planner{
		session:           agent.NewLongRunningSession(cfg.PlannerConfig, swarmSessionID),
		config:            cfg.PlannerConfig,
		swarmSessionID:    swarmSessionID,
		designerConfig:    cfg.DesignerConfig,
		builderConfig:     cfg.BuilderConfig,
		reviewerConfig:    cfg.ReviewerConfig,
		maxIterations:     cfg.MaxIterations,
		filesCreated:      make([]string, 0),
		filesModified:     make([]string, 0),
		checkpointEnabled: cfg.EnableCheckpointing,
		stateMachine:      NewStateMachine(),
	}

	// Initialize checkpoint manager if enabled
	if cfg.EnableCheckpointing && cfg.SessionDir != "" {
		p.checkpointMgr = checkpoint.NewManager(cfg.SessionDir, swarmSessionID)
	}

	// Initialize progress reporter
	if cfg.Progress != nil {
		p.progress = cfg.Progress
	}

	// Create MCP server (will be started in Start())
	p.mcpServer = NewMCPServer(p)

	return p
}

// Role returns the agent's role.
func (p *Planner) Role() agent.AgentRole {
	return agent.RolePlanner
}

// SessionDir returns the session recording directory.
func (p *Planner) SessionDir() string {
	return p.session.SessionDir()
}

// TotalCost returns the accumulated cost including sub-agents.
func (p *Planner) TotalCost() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.totalCost + p.session.TotalCost()
}

// TurnCount returns the number of turns completed.
func (p *Planner) TurnCount() int {
	return p.session.TurnCount()
}

// IterationCount returns the number of sub-agent iterations executed.
func (p *Planner) IterationCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.iterationCount
}

// checkIterations verifies that the iteration count is within the limit.
// Returns ErrMaxIterationsExceeded if the limit is exceeded.
// A limit of 0 means unlimited.
func (p *Planner) checkIterations() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.maxIterations <= 0 {
		return nil // No limit
	}

	if p.iterationCount >= p.maxIterations {
		return fmt.Errorf("%w: current iterations %d >= max %d", ErrMaxIterationsExceeded, p.iterationCount, p.maxIterations)
	}
	return nil
}

// incrementIterations increments the iteration counter.
func (p *Planner) incrementIterations() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.iterationCount++
}

// Start initializes the Planner's session and MCP server.
func (p *Planner) Start(ctx context.Context) error {
	// Start the MCP server first
	mcpURL, err := p.mcpServer.Start()
	if err != nil {
		return fmt.Errorf("failed to start MCP server: %w", err)
	}

	// Configure the Claude session to use the MCP server
	mcpConfig := claude.NewMCPConfig().AddHTTPServer("planner-tools", mcpURL)
	p.session.SetSessionOptions(
		claude.WithMCPConfig(mcpConfig),
		claude.WithSystemPrompt(p.config.SystemPrompt),
	)

	return p.session.Start(ctx)
}

// Stop gracefully shuts down the Planner.
func (p *Planner) Stop() error {
	// Stop the session first
	err := p.session.Stop()

	// Then stop the MCP server
	if p.mcpServer != nil {
		p.mcpServer.Stop()
	}

	return err
}

// SendMessage sends a message to the Planner and waits for completion.
func (p *Planner) SendMessage(ctx context.Context, message string) (*claude.TurnResult, error) {
	return p.session.SendMessage(ctx, message)
}

// ExecuteMission runs a complete mission, coordinating sub-agents as needed.
// This is a higher-level method that manages the full workflow.
func (p *Planner) ExecuteMission(ctx context.Context, mission string) (*protocol.PlannerResult, error) {
	// Progress: Planner is thinking
	if p.progress != nil {
		p.progress.Event(progress.NewAgentThinkingEvent(agent.RolePlanner, "Analyzing mission and planning tasks"))
	}

	// Send the mission to the Planner's Claude session
	// The Planner will use its tools (designer, builder, reviewer) as needed
	result, err := p.session.SendMessage(ctx, formatMissionMessage(mission))
	if err != nil {
		return nil, fmt.Errorf("mission execution failed: %w", err)
	}

	// Build the result
	p.mu.Lock()
	plannerResult := &protocol.PlannerResult{
		Success:       result.Success,
		FilesCreated:  p.filesCreated,
		FilesModified: p.filesModified,
		TotalCost:     p.totalCost + p.session.TotalCost(),
	}
	p.mu.Unlock()

	return plannerResult, nil
}

// CallDesigner invokes the Designer sub-agent.
func (p *Planner) CallDesigner(ctx context.Context, req *protocol.DesignRequest) (*protocol.DesignResponse, error) {
	startTime := time.Now()

	// Check iteration limit before proceeding
	if err := p.checkIterations(); err != nil {
		return nil, err
	}

	// Get current iteration for progress reporting
	p.mu.Lock()
	iteration := p.iterationCount + 1
	p.mu.Unlock()

	// Progress: phase change to designing
	if p.progress != nil {
		p.progress.Event(progress.NewPhaseChangeEvent(checkpoint.PhaseNotStarted, checkpoint.PhaseDesigning, iteration))
		p.progress.Event(progress.NewAgentStartEvent(agent.RoleDesigner, "", "Analyzing requirements and creating design"))
	}

	// Checkpoint: starting design phase
	if p.checkpointMgr != nil {
		if err := p.checkpointMgr.StartDesign(); err != nil {
			// Log but don't fail on checkpoint errors
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	d := designer.New(p.designerConfig, p.swarmSessionID)

	prompt := formatDesignPrompt(req)
	result, execResult, taskID, err := d.ExecuteWithFiles(ctx, prompt)

	// Increment iteration count after the call
	p.incrementIterations()

	duration := time.Since(startTime)
	cost := d.TotalCost()

	if err != nil {
		// Progress: agent failed
		if p.progress != nil {
			p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleDesigner, taskID, false, cost, duration, err))
		}
		// Checkpoint: record failure
		if p.checkpointMgr != nil {
			_ = p.checkpointMgr.Fail(err)
		}
		return nil, fmt.Errorf("designer failed (task %s): %w", taskID, err)
	}

	// Update cost
	p.mu.Lock()
	p.totalCost += cost
	p.mu.Unlock()

	if !result.Success {
		failErr := fmt.Errorf("designer task failed: %v", result.Error)
		// Progress: agent failed
		if p.progress != nil {
			p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleDesigner, taskID, false, cost, duration, failErr))
		}
		if p.checkpointMgr != nil {
			_ = p.checkpointMgr.Fail(failErr)
		}
		return nil, failErr
	}

	// Progress: agent completed successfully
	if p.progress != nil {
		p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleDesigner, taskID, true, cost, duration, nil))
	}

	// Parse the design response from the result text
	// Note: Sub-agents often respond conversationally while using tools to create files.
	// JSON parsing may fail, which is fine - we track files via tool events instead.
	var response *protocol.DesignResponse
	if result.Text != "" {
		response, _ = designer.ParseDesignResponse(result.Text)
	}
	if response == nil {
		response = &protocol.DesignResponse{}
	}

	// Track files from execution (Designer might create files directly)
	if execResult != nil {
		p.mu.Lock()
		p.filesCreated = append(p.filesCreated, execResult.FilesCreated...)
		p.filesModified = append(p.filesModified, execResult.FilesModified...)
		p.mu.Unlock()
	}

	// Checkpoint: design completed successfully
	if p.checkpointMgr != nil {
		if err := p.checkpointMgr.CompleteDesign(response, cost); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	return response, nil
}

// CallBuilder invokes the Builder sub-agent.
func (p *Planner) CallBuilder(ctx context.Context, req *protocol.BuildRequest) (*protocol.BuildResponse, error) {
	startTime := time.Now()

	// Check iteration limit before proceeding
	if err := p.checkIterations(); err != nil {
		return nil, err
	}

	// Get current iteration for progress reporting
	p.mu.Lock()
	iteration := p.iterationCount + 1
	p.mu.Unlock()

	// Progress: phase change to building
	if p.progress != nil {
		p.progress.Event(progress.NewPhaseChangeEvent(checkpoint.PhaseDesigning, checkpoint.PhaseBuilding, iteration))
		p.progress.Event(progress.NewAgentStartEvent(agent.RoleBuilder, "", "Implementing code changes"))
	}

	// Checkpoint: starting build phase
	if p.checkpointMgr != nil {
		if err := p.checkpointMgr.StartBuild(); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	b := builder.New(p.builderConfig, p.swarmSessionID)

	prompt := formatBuildPrompt(req)
	result, execResult, taskID, err := b.ExecuteWithFiles(ctx, prompt)

	// Increment iteration count after the call
	p.incrementIterations()

	duration := time.Since(startTime)
	cost := b.TotalCost()

	if err != nil {
		// Progress: agent failed
		if p.progress != nil {
			p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleBuilder, taskID, false, cost, duration, err))
		}
		if p.checkpointMgr != nil {
			_ = p.checkpointMgr.Fail(err)
		}
		return nil, fmt.Errorf("builder failed (task %s): %w", taskID, err)
	}

	// Update cost
	p.mu.Lock()
	p.totalCost += cost
	p.mu.Unlock()

	if !result.Success {
		failErr := fmt.Errorf("builder task failed: %v", result.Error)
		// Progress: agent failed
		if p.progress != nil {
			p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleBuilder, taskID, false, cost, duration, failErr))
		}
		if p.checkpointMgr != nil {
			_ = p.checkpointMgr.Fail(failErr)
		}
		return nil, failErr
	}

	// Progress: agent completed successfully
	if p.progress != nil {
		p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleBuilder, taskID, true, cost, duration, nil))
	}

	// Build response from file tracking (more reliable than JSON parsing)
	var response *protocol.BuildResponse
	if execResult != nil {
		response = &protocol.BuildResponse{
			FilesCreated:  execResult.FilesCreated,
			FilesModified: execResult.FilesModified,
		}
		// Also update planner's file tracking
		p.mu.Lock()
		p.filesCreated = append(p.filesCreated, execResult.FilesCreated...)
		p.filesModified = append(p.filesModified, execResult.FilesModified...)
		p.mu.Unlock()
	} else {
		response = &protocol.BuildResponse{}
	}

	// Checkpoint: build completed successfully
	if p.checkpointMgr != nil {
		if err := p.checkpointMgr.CompleteBuild(response, cost); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	return response, nil
}

// CallReviewer invokes the Reviewer sub-agent.
func (p *Planner) CallReviewer(ctx context.Context, req *protocol.ReviewRequest) (*protocol.ReviewResponse, error) {
	startTime := time.Now()

	// Check iteration limit before proceeding
	if err := p.checkIterations(); err != nil {
		return nil, err
	}

	// Get current iteration for progress reporting
	p.mu.Lock()
	iteration := p.iterationCount + 1
	p.mu.Unlock()

	// Progress: phase change to reviewing
	if p.progress != nil {
		p.progress.Event(progress.NewPhaseChangeEvent(checkpoint.PhaseBuilding, checkpoint.PhaseReviewing, iteration))
		p.progress.Event(progress.NewAgentStartEvent(agent.RoleReviewer, "", "Reviewing implementation"))
	}

	// Checkpoint: starting review phase
	if p.checkpointMgr != nil {
		if err := p.checkpointMgr.StartReview(); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	r := reviewer.New(p.reviewerConfig, p.swarmSessionID)

	prompt := formatReviewPrompt(req)
	result, execResult, taskID, err := r.ExecuteWithFiles(ctx, prompt)

	// Increment iteration count after the call
	p.incrementIterations()

	duration := time.Since(startTime)
	cost := r.TotalCost()

	if err != nil {
		// Progress: agent failed
		if p.progress != nil {
			p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleReviewer, taskID, false, cost, duration, err))
		}
		if p.checkpointMgr != nil {
			_ = p.checkpointMgr.Fail(err)
		}
		return nil, fmt.Errorf("reviewer failed (task %s): %w", taskID, err)
	}

	// Update cost
	p.mu.Lock()
	p.totalCost += cost
	p.mu.Unlock()

	if !result.Success {
		failErr := fmt.Errorf("reviewer task failed: %v", result.Error)
		// Progress: agent failed
		if p.progress != nil {
			p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleReviewer, taskID, false, cost, duration, failErr))
		}
		if p.checkpointMgr != nil {
			_ = p.checkpointMgr.Fail(failErr)
		}
		return nil, failErr
	}

	// Progress: agent completed successfully
	if p.progress != nil {
		p.progress.Event(progress.NewAgentCompleteEvent(agent.RoleReviewer, taskID, true, cost, duration, nil))
	}

	// Track files from execution result (reviewers typically don't create files, but track anyway)
	if execResult != nil {
		p.mu.Lock()
		p.filesCreated = append(p.filesCreated, execResult.FilesCreated...)
		p.filesModified = append(p.filesModified, execResult.FilesModified...)
		p.mu.Unlock()
	}

	// Parse the review response from the result text
	// Note: Sub-agents often respond conversationally. JSON parsing may fail,
	// which is fine - the review feedback goes to Planner's context anyway.
	var response *protocol.ReviewResponse
	if result.Text != "" {
		response, _ = reviewer.ParseReviewResponse(result.Text)
	}
	if response == nil {
		response = &protocol.ReviewResponse{}
	}

	// Checkpoint: review completed successfully
	if p.checkpointMgr != nil {
		if err := p.checkpointMgr.CompleteReview(response, cost); err != nil {
			fmt.Printf("Warning: failed to save checkpoint: %v\n", err)
		}
	}

	return response, nil
}

// Recording returns the Planner's session recording.
func (p *Planner) Recording() *claude.SessionRecording {
	return p.session.Recording()
}

// SetMission sets the mission for checkpointing.
func (p *Planner) SetMission(mission string) {
	if p.checkpointMgr != nil {
		p.checkpointMgr.SetMission(mission)
	}
}

// MarkComplete marks the checkpoint as completed.
func (p *Planner) MarkComplete() error {
	if p.checkpointMgr != nil {
		return p.checkpointMgr.Complete()
	}
	return nil
}

// GetCheckpoint returns the current checkpoint state, or nil if not enabled.
func (p *Planner) GetCheckpoint() *checkpoint.Checkpoint {
	if p.checkpointMgr != nil {
		return p.checkpointMgr.Current()
	}
	return nil
}

// RestoreFromCheckpoint loads state from a checkpoint.
func (p *Planner) RestoreFromCheckpoint(cp *checkpoint.Checkpoint) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.iterationCount = cp.IterationCount
	p.filesCreated = cp.FilesCreated
	p.filesModified = cp.FilesModified
	p.totalCost = cp.TotalCost
}

// NewFromCheckpoint creates a Planner and restores its state from a checkpoint.
func NewFromCheckpoint(cfg Config, swarmSessionID string, cp *checkpoint.Checkpoint) *Planner {
	p := New(cfg, swarmSessionID)
	p.RestoreFromCheckpoint(cp)
	return p
}

// LoadCheckpoint loads a checkpoint from the session directory.
func LoadCheckpoint(sessionDir, sessionID string) (*checkpoint.Checkpoint, error) {
	return checkpoint.Load(sessionDir, sessionID)
}

// CheckpointExists checks if a checkpoint exists for the given session.
func CheckpointExists(sessionDir, sessionID string) bool {
	return checkpoint.Exists(sessionDir, sessionID)
}

// formatMissionMessage formats a mission into a message for the Planner.
func formatMissionMessage(mission string) string {
	return fmt.Sprintf(`# New Mission

%s

Please analyze this mission, break it into tasks, and execute them using your sub-agents (designer, builder, reviewer).

When complete, provide a summary of what was accomplished.`, mission)
}

// formatDesignPrompt formats a design request for the Designer.
func formatDesignPrompt(req *protocol.DesignRequest) string {
	prompt := fmt.Sprintf("Task: %s\n", req.Task)
	if req.Context != "" {
		prompt += fmt.Sprintf("\nContext:\n%s\n", req.Context)
	}
	if len(req.Constraints) > 0 {
		prompt += "\nConstraints:\n"
		for _, c := range req.Constraints {
			prompt += fmt.Sprintf("- %s\n", c)
		}
	}
	return prompt
}

// formatBuildPrompt formats a build request for the Builder.
func formatBuildPrompt(req *protocol.BuildRequest) string {
	prompt := fmt.Sprintf("Task: %s\nWorking Directory: %s\n", req.Task, req.WorkDir)
	if req.Design != nil {
		prompt += fmt.Sprintf("\nDesign:\n%s\n", req.Design.Architecture)
	}
	if req.Feedback != nil && len(req.Feedback.Issues) > 0 {
		prompt += "\nFeedback to address:\n"
		for _, issue := range req.Feedback.Issues {
			prompt += fmt.Sprintf("- [%s] %s: %s\n", issue.Severity, issue.File, issue.Message)
		}
	}
	return prompt
}

// formatReviewPrompt formats a review request for the Reviewer.
func formatReviewPrompt(req *protocol.ReviewRequest) string {
	prompt := fmt.Sprintf("Task: %s\n\nFiles to review:\n", req.Task)
	for _, f := range req.FilesChanged {
		prompt += fmt.Sprintf("- %s\n", f)
	}
	if req.OriginalDesign != nil {
		prompt += fmt.Sprintf("\nOriginal Design:\n%s\n", req.OriginalDesign.Architecture)
	}
	return prompt
}

// State returns the current Planner state.
func (p *Planner) State() PlannerState {
	if p.stateMachine == nil {
		return StateIdle
	}
	return p.stateMachine.State()
}

// StateHistory returns the state transition history.
func (p *Planner) StateHistory() []StateTransition {
	if p.stateMachine == nil {
		return nil
	}
	return p.stateMachine.History()
}

// PhaseStats returns the phase-aware statistics.
func (p *Planner) PhaseStats() PhaseStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.phaseStats
}

// ExecuteMissionStreaming runs a mission with streaming event support.
// Unlike ExecuteMission, this method returns a channel of events for real-time progress.
func (p *Planner) ExecuteMissionStreaming(ctx context.Context, mission string) (<-chan MissionEvent, error) {
	events := make(chan MissionEvent, 100)

	go func() {
		defer close(events)

		// Track success/failure state
		missionSuccess := true
		var missionError error

		// Emit start event
		events <- NewMissionStartEvent(mission)

		// Transition to planning
		if p.stateMachine != nil {
			if err := p.stateMachine.Transition(StatePlanning, "mission_received"); err == nil {
				events <- NewStateChangeEvent(StateIdle, StatePlanning, "mission_received")
			}
		}

		// Progress: Planner is thinking
		if p.progress != nil {
			p.progress.Event(progress.NewAgentThinkingEvent(agent.RolePlanner, "Analyzing mission and planning tasks"))
		}

		// Send message asynchronously
		_, err := p.session.SendMessageAsync(ctx, formatMissionMessage(mission))
		if err != nil {
			events <- NewMissionErrorEvent(fmt.Errorf("failed to send mission: %w", err))
			if p.stateMachine != nil {
				_ = p.stateMachine.Transition(StateFailed, "send_failed")
			}
			return
		}

		// Process events from the session
		sessionEvents := p.session.Events()
		if sessionEvents == nil {
			// No events channel - fall back to waiting for turn
			result, waitErr := p.session.WaitForTurn(ctx)
			if waitErr != nil {
				events <- NewMissionErrorEvent(waitErr)
				if p.stateMachine != nil {
					_ = p.stateMachine.Transition(StateFailed, "wait_failed")
				}
				return
			}

			// Build result
			p.mu.Lock()
			plannerResult := &protocol.PlannerResult{
				Success:       result.Success,
				FilesCreated:  p.filesCreated,
				FilesModified: p.filesModified,
				TotalCost:     p.totalCost + p.session.TotalCost(),
			}
			p.mu.Unlock()

			events <- NewMissionCompleteEvent(plannerResult)
			if p.stateMachine != nil {
				if result.Success {
					_ = p.stateMachine.Transition(StateCompleted, "mission_complete")
				} else {
					_ = p.stateMachine.Transition(StateFailed, "mission_failed")
				}
			}
			return
		}

		// Process streaming events
		for {
			select {
			case <-ctx.Done():
				events <- NewMissionErrorEvent(ctx.Err())
				if p.stateMachine != nil {
					_ = p.stateMachine.Transition(StateFailed, "context_cancelled")
				}
				return

			case event, ok := <-sessionEvents:
				if !ok {
					// Session ended - build result with tracked success state
					p.mu.Lock()
					plannerResult := &protocol.PlannerResult{
						Success:       missionSuccess,
						FilesCreated:  p.filesCreated,
						FilesModified: p.filesModified,
						TotalCost:     p.totalCost + p.session.TotalCost(),
					}
					p.mu.Unlock()

					if missionSuccess {
						events <- NewMissionCompleteEvent(plannerResult)
						if p.stateMachine != nil {
							_ = p.stateMachine.Transition(StateCompleted, "mission_complete")
						}
					} else {
						if missionError != nil {
							events <- NewMissionErrorEvent(missionError)
						}
						if p.stateMachine != nil {
							_ = p.stateMachine.Transition(StateFailed, "mission_failed")
						}
					}
					return
				}

				// Handle the event and emit mission events
				// Track failures from events
				switch e := event.(type) {
				case claude.ErrorEvent:
					// ErrorEvent terminates immediately - no need to track missionSuccess/missionError
					// since we emit the error and return right after
					events <- NewMissionErrorEvent(e.Error)
					if p.stateMachine != nil {
						_ = p.stateMachine.Transition(StateFailed, "error_event")
					}
					return

				case claude.TurnCompleteEvent:
					if !e.Success {
						missionSuccess = false
						if e.Error != nil {
							missionError = e.Error
						}
					}
				}

				p.handleSessionEventStreaming(event, events)
			}
		}
	}()

	return events, nil
}

// handleSessionEventStreaming processes a Claude session event and emits mission events.
func (p *Planner) handleSessionEventStreaming(event claude.Event, missionEvents chan<- MissionEvent) {
	switch e := event.(type) {
	case claude.TextEvent:
		missionEvents <- NewTextStreamEvent(e.Text, e.FullText)

	case claude.ThinkingEvent:
		missionEvents <- NewThinkingStreamEvent(e.Thinking, e.FullThinking)

	case claude.ToolStartEvent:
		missionEvents <- NewToolStartEvent(e.Name, e.ID)

	case claude.ToolCompleteEvent:
		missionEvents <- NewToolCompleteEvent(e.Name, e.ID, e.Input)

		// Track file operations
		if filePath, ok := e.Input["file_path"].(string); ok && filePath != "" {
			action := "modify"
			if e.Name == "Write" {
				action = "create"
			}
			missionEvents <- NewFileChangeEvent(filePath, action, agent.RolePlanner)

			// Update internal file tracking
			p.mu.Lock()
			if e.Name == "Write" {
				p.filesCreated = append(p.filesCreated, filePath)
			} else if e.Name == "Edit" {
				p.filesModified = append(p.filesModified, filePath)
			}
			p.mu.Unlock()
		}

	case claude.TurnCompleteEvent:
		// Record usage in session for cost/turn tracking
		p.session.RecordUsage(e.Usage)

		// Phase-aware stats tracking (from yoloswe/planner pattern)
		p.mu.Lock()
		currentState := StateIdle
		if p.stateMachine != nil {
			currentState = p.stateMachine.State()
		}

		// Handle pending build start transition
		if p.pendingBuildStart {
			p.phaseStats.Planning.Add(e.Usage)
			p.inBuildPhase = true
			p.pendingBuildStart = false
		} else if p.inBuildPhase {
			p.phaseStats.AddForPhase(currentState, e.Usage)
		} else {
			p.phaseStats.Planning.Add(e.Usage)
		}
		p.mu.Unlock()

		var turnErr error
		if e.Error != nil {
			turnErr = e.Error
		}
		missionEvents <- NewTurnCompleteEvent(e.TurnNumber, e.Success, e.Usage.CostUSD, turnErr)

		// Emit cost update with updated totals
		missionEvents <- NewCostUpdateEvent(p.TotalCost(), 0)

	case claude.ErrorEvent:
		missionEvents <- NewMissionErrorEvent(e.Error)
	}
}

// IsWaitingForInput returns true if the Planner is waiting for user input.
func (p *Planner) IsWaitingForInput() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitingForUserInput
}

// SetWaitingForInput sets the waiting for input flag.
func (p *Planner) SetWaitingForInput(waiting bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waitingForUserInput = waiting
}

// IsInBuildPhase returns true if the Planner is in the build phase.
func (p *Planner) IsInBuildPhase() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inBuildPhase
}

// SetInBuildPhase sets the build phase flag.
func (p *Planner) SetInBuildPhase(inBuild bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inBuildPhase = inBuild
}

// SetPendingBuildStart sets the pending build start flag.
// This is used during plan→build transitions to correctly attribute stats.
func (p *Planner) SetPendingBuildStart(pending bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pendingBuildStart = pending
}

// Reset resets the Planner to its initial state for a new mission.
func (p *Planner) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.filesCreated = make([]string, 0)
	p.filesModified = make([]string, 0)
	p.iterationCount = 0
	p.totalCost = 0
	p.phaseStats.Reset()
	p.waitingForUserInput = false
	p.inBuildPhase = false
	p.pendingBuildStart = false

	if p.stateMachine != nil {
		p.stateMachine.Reset()
	}
}
