// Package orchestrator implements the user-facing Orchestrator agent.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
	"github.com/bazelment/yoloswe/multiagent/planner"
	"github.com/bazelment/yoloswe/multiagent/progress"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// ErrBudgetExceeded is returned when the total cost exceeds the configured budget.
var ErrBudgetExceeded = errors.New("budget exceeded")

// Orchestrator is the user-facing agent that triages requests and delegates to Planner.
type Orchestrator struct {
	swarmConfig    agent.SwarmConfig
	session        *agent.LongRunningSession
	planner        *planner.Planner
	swarmSessionID string
	config         agent.AgentConfig
	totalCost      float64
	mu             sync.Mutex
	started        bool
}

// New creates a new Orchestrator agent.
func New(swarmConfig agent.SwarmConfig) (*Orchestrator, error) {
	// Generate session ID if not provided
	sessionID := swarmConfig.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("swarm-%d", time.Now().UnixNano())
	}

	// Create session directory
	sessionDir := filepath.Join(swarmConfig.SessionDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create session directory: %w", err)
	}

	// Create orchestrator config
	orchConfig := agent.AgentConfig{
		Role:         agent.RoleOrchestrator,
		Model:        swarmConfig.OrchestratorModel,
		SystemPrompt: SystemPrompt,
		WorkDir:      swarmConfig.WorkDir,
		SessionDir:   swarmConfig.SessionDir,
	}

	// Create planner config
	plannerCfg := planner.Config{
		PlannerConfig: agent.AgentConfig{
			Role:       agent.RolePlanner,
			Model:      swarmConfig.PlannerModel,
			WorkDir:    swarmConfig.WorkDir,
			SessionDir: swarmConfig.SessionDir,
		},
		DesignerConfig: agent.AgentConfig{
			Role:       agent.RoleDesigner,
			Model:      swarmConfig.DesignerModel,
			WorkDir:    swarmConfig.WorkDir,
			SessionDir: swarmConfig.SessionDir,
		},
		BuilderConfig: agent.AgentConfig{
			Role:       agent.RoleBuilder,
			Model:      swarmConfig.BuilderModel,
			WorkDir:    swarmConfig.WorkDir,
			SessionDir: swarmConfig.SessionDir,
		},
		ReviewerConfig: agent.AgentConfig{
			Role:       agent.RoleReviewer,
			Model:      swarmConfig.ReviewerModel,
			WorkDir:    swarmConfig.WorkDir,
			SessionDir: swarmConfig.SessionDir,
		},
		MaxIterations:       swarmConfig.MaxIterations,
		EnableCheckpointing: swarmConfig.EnableCheckpointing,
		SessionDir:          swarmConfig.SessionDir,
		Progress:            convertProgressReporter(swarmConfig.Progress),
	}

	return &Orchestrator{
		session:        agent.NewLongRunningSession(orchConfig, sessionID),
		config:         orchConfig,
		swarmConfig:    swarmConfig,
		swarmSessionID: sessionID,
		planner:        planner.New(plannerCfg, sessionID),
	}, nil
}

// SessionID returns the swarm session ID.
func (o *Orchestrator) SessionID() string {
	return o.swarmSessionID
}

// Role returns the agent's role.
func (o *Orchestrator) Role() agent.AgentRole {
	return agent.RoleOrchestrator
}

// SessionDir returns the session recording directory.
func (o *Orchestrator) SessionDir() string {
	return o.session.SessionDir()
}

// TotalCost returns the accumulated cost including Planner and sub-agents.
func (o *Orchestrator) TotalCost() float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.totalCost + o.session.TotalCost() + o.planner.TotalCost()
}

// checkBudget verifies that the total cost is within the configured budget.
// Returns ErrBudgetExceeded if the budget is exceeded.
// A budget of 0 means unlimited.
func (o *Orchestrator) checkBudget() error {
	budget := o.swarmConfig.TotalBudgetUSD
	if budget <= 0 {
		return nil // No budget limit
	}

	currentCost := o.TotalCost()
	if currentCost > budget {
		return fmt.Errorf("%w: current cost $%.4f exceeds budget $%.4f", ErrBudgetExceeded, currentCost, budget)
	}
	return nil
}

// TurnCount returns the Orchestrator's turn count.
func (o *Orchestrator) TurnCount() int {
	return o.session.TurnCount()
}

// Start initializes the Orchestrator and Planner sessions.
func (o *Orchestrator) Start(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.started {
		return fmt.Errorf("orchestrator already started")
	}

	// Start orchestrator session
	if err := o.session.Start(ctx); err != nil {
		return fmt.Errorf("failed to start orchestrator session: %w", err)
	}

	// Start planner session
	if err := o.planner.Start(ctx); err != nil {
		o.session.Stop()
		return fmt.Errorf("failed to start planner session: %w", err)
	}

	o.started = true
	return nil
}

// Stop gracefully shuts down the Orchestrator and Planner.
func (o *Orchestrator) Stop() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.started {
		return nil
	}

	var errs []error

	if err := o.planner.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("planner stop: %w", err))
	}

	if err := o.session.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("orchestrator stop: %w", err))
	}

	o.started = false

	if len(errs) > 0 {
		return fmt.Errorf("stop errors: %v", errs)
	}
	return nil
}

// SendMessage sends a user message to the Orchestrator.
// The Orchestrator will decide whether to respond directly or delegate to Planner.
func (o *Orchestrator) SendMessage(ctx context.Context, message string) (*claude.TurnResult, error) {
	o.mu.Lock()
	if !o.started {
		o.mu.Unlock()
		return nil, fmt.Errorf("orchestrator not started")
	}
	o.mu.Unlock()

	// Check budget before proceeding
	if err := o.checkBudget(); err != nil {
		return nil, err
	}

	return o.session.SendMessage(ctx, message)
}

// DelegateToPlanner sends a mission directly to the Planner.
// This is called when the Orchestrator decides to delegate.
func (o *Orchestrator) DelegateToPlanner(ctx context.Context, mission string) (*protocol.PlannerResult, error) {
	o.mu.Lock()
	if !o.started {
		o.mu.Unlock()
		return nil, fmt.Errorf("orchestrator not started")
	}
	o.mu.Unlock()

	// Check budget before proceeding
	if err := o.checkBudget(); err != nil {
		return nil, err
	}

	return o.planner.ExecuteMission(ctx, mission)
}

// ExecuteMission is a convenience method that sends a mission through the Orchestrator.
// The Orchestrator will triage and potentially delegate to Planner.
func (o *Orchestrator) ExecuteMission(ctx context.Context, mission string) (*protocol.PlannerResult, error) {
	// Set mission for checkpointing
	o.planner.SetMission(mission)

	// For now, delegate directly to planner
	// In a more sophisticated implementation, the Orchestrator would use its
	// Claude session to decide whether to handle directly or delegate
	return o.DelegateToPlanner(ctx, mission)
}

// ResumeMission resumes a mission from a checkpoint.
func (o *Orchestrator) ResumeMission(ctx context.Context, cp *checkpoint.Checkpoint) (*protocol.PlannerResult, error) {
	o.mu.Lock()
	if !o.started {
		o.mu.Unlock()
		return nil, fmt.Errorf("orchestrator not started")
	}
	o.mu.Unlock()

	// Check budget before proceeding
	if err := o.checkBudget(); err != nil {
		return nil, err
	}

	// Restore planner state from checkpoint
	o.planner.RestoreFromCheckpoint(cp)

	// Build a resume message based on the checkpoint phase
	resumePhase := cp.ResumePhase()
	var resumeMessage string

	switch resumePhase {
	case checkpoint.PhaseDesigning:
		resumeMessage = fmt.Sprintf(`Resume mission from design phase.

Original Mission: %s

Please start the design phase.`, cp.Mission)

	case checkpoint.PhaseBuilding:
		resumeMessage = fmt.Sprintf(`Resume mission from build phase.

Original Mission: %s

The design phase has been completed. Please proceed with the build phase.`, cp.Mission)
		if cp.DesignResponse != nil {
			resumeMessage += fmt.Sprintf("\n\nPrevious Design:\n%s", cp.DesignResponse.Architecture)
		}

	case checkpoint.PhaseReviewing:
		resumeMessage = fmt.Sprintf(`Resume mission from review phase.

Original Mission: %s

The build phase has been completed. Please proceed with the review phase.`, cp.Mission)

	default:
		resumeMessage = fmt.Sprintf(`Resume mission.

Original Mission: %s

Please analyze this mission and continue from where we left off.`, cp.Mission)
	}

	// Send the resume message to the planner
	result, err := o.planner.SendMessage(ctx, resumeMessage)
	if err != nil {
		return nil, fmt.Errorf("resume failed: %w", err)
	}

	// Mark checkpoint as complete if successful
	if result.Success {
		if err := o.planner.MarkComplete(); err != nil {
			fmt.Printf("Warning: failed to mark checkpoint complete: %v\n", err)
		}
	}

	// Build the result
	plannerCp := o.planner.GetCheckpoint()
	plannerResult := &protocol.PlannerResult{
		Success: result.Success,
	}

	if plannerCp != nil {
		plannerResult.FilesCreated = plannerCp.FilesCreated
		plannerResult.FilesModified = plannerCp.FilesModified
		plannerResult.TotalCost = plannerCp.TotalCost
	}

	return plannerResult, nil
}

// Recording returns the Orchestrator's session recording.
func (o *Orchestrator) Recording() *claude.SessionRecording {
	return o.session.Recording()
}

// PlannerRecording returns the Planner's session recording.
func (o *Orchestrator) PlannerRecording() *claude.SessionRecording {
	return o.planner.Recording()
}

// Summary generates a summary of the swarm session.
type Summary struct {
	AgentCosts        map[string]float64 `json:"agent_costs"`
	SessionID         string             `json:"session_id"`
	TotalCost         float64            `json:"total_cost"`
	OrchestratorTurns int                `json:"orchestrator_turns"`
	PlannerTurns      int                `json:"planner_turns"`
}

// GetSummary returns a summary of the session.
func (o *Orchestrator) GetSummary() *Summary {
	return &Summary{
		SessionID:         o.swarmSessionID,
		TotalCost:         o.TotalCost(),
		OrchestratorTurns: o.session.TurnCount(),
		PlannerTurns:      o.planner.TurnCount(),
		AgentCosts: map[string]float64{
			"orchestrator": o.session.TotalCost(),
			"planner":      o.planner.TotalCost(),
		},
	}
}

// WriteSummary writes the session summary to a JSON file in the session directory.
func (o *Orchestrator) WriteSummary() error {
	summary := o.GetSummary()

	// Create the summary file path
	summaryPath := filepath.Join(o.swarmConfig.SessionDir, o.swarmSessionID, "summary.json")

	// Marshal to JSON with indentation
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal summary: %w", err)
	}

	// Write to file
	if err := os.WriteFile(summaryPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write summary file: %w", err)
	}

	return nil
}

// progressAdapter wraps agent.ProgressReporter to implement progress.Reporter.
type progressAdapter struct {
	pr agent.ProgressReporter
}

func (a *progressAdapter) Event(event progress.Event) {
	a.pr.Event(event)
}

func (a *progressAdapter) Close() {
	a.pr.Close()
}

// convertProgressReporter converts agent.ProgressReporter to progress.Reporter.
func convertProgressReporter(pr agent.ProgressReporter) progress.Reporter {
	if pr == nil {
		return nil
	}
	return &progressAdapter{pr: pr}
}
