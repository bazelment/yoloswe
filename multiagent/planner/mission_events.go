package planner

import (
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// MissionEvent is the interface for all mission execution events.
// These events enable streaming visibility into mission progress.
type MissionEvent interface {
	// missionEvent is a marker method to prevent external implementations.
	missionEvent()
	// Timestamp returns when the event occurred.
	Timestamp() time.Time
}

// baseMissionEvent provides common fields for all mission events.
type baseMissionEvent struct {
	ts time.Time
}

func (e baseMissionEvent) Timestamp() time.Time { return e.ts }

// MissionStartEvent fires when mission execution begins.
type MissionStartEvent struct {
	baseMissionEvent
	Mission string
}

func (MissionStartEvent) missionEvent() {}

// NewMissionStartEvent creates a new MissionStartEvent.
func NewMissionStartEvent(mission string) MissionStartEvent {
	return MissionStartEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Mission:          mission,
	}
}

// MissionCompleteEvent fires when mission completes successfully.
type MissionCompleteEvent struct {
	baseMissionEvent
	Result *protocol.PlannerResult
}

func (MissionCompleteEvent) missionEvent() {}

// NewMissionCompleteEvent creates a new MissionCompleteEvent.
func NewMissionCompleteEvent(result *protocol.PlannerResult) MissionCompleteEvent {
	return MissionCompleteEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Result:           result,
	}
}

// MissionErrorEvent fires when mission fails.
type MissionErrorEvent struct {
	baseMissionEvent
	Error error
}

func (MissionErrorEvent) missionEvent() {}

// NewMissionErrorEvent creates a new MissionErrorEvent.
func NewMissionErrorEvent(err error) MissionErrorEvent {
	return MissionErrorEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Error:            err,
	}
}

// StateChangeEvent fires when Planner state changes.
type StateChangeEvent struct {
	baseMissionEvent
	Trigger string
	From    PlannerState
	To      PlannerState
}

func (StateChangeEvent) missionEvent() {}

// NewStateChangeEvent creates a new StateChangeEvent.
func NewStateChangeEvent(from, to PlannerState, trigger string) StateChangeEvent {
	return StateChangeEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Trigger:          trigger,
		From:             from,
		To:               to,
	}
}

// TextStreamEvent contains streaming text from the Planner.
type TextStreamEvent struct {
	baseMissionEvent
	Text     string // New text chunk
	FullText string // Accumulated full text
}

func (TextStreamEvent) missionEvent() {}

// NewTextStreamEvent creates a new TextStreamEvent.
func NewTextStreamEvent(text, fullText string) TextStreamEvent {
	return TextStreamEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Text:             text,
		FullText:         fullText,
	}
}

// ThinkingStreamEvent contains streaming thinking from the Planner.
type ThinkingStreamEvent struct {
	baseMissionEvent
	Thinking     string // New thinking chunk
	FullThinking string // Accumulated full thinking
}

func (ThinkingStreamEvent) missionEvent() {}

// NewThinkingStreamEvent creates a new ThinkingStreamEvent.
func NewThinkingStreamEvent(thinking, fullThinking string) ThinkingStreamEvent {
	return ThinkingStreamEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Thinking:         thinking,
		FullThinking:     fullThinking,
	}
}

// SubAgentStartEvent fires when a sub-agent begins work.
type SubAgentStartEvent struct {
	baseMissionEvent
	Role        agent.AgentRole
	TaskID      string
	Description string
}

func (SubAgentStartEvent) missionEvent() {}

// NewSubAgentStartEvent creates a new SubAgentStartEvent.
func NewSubAgentStartEvent(role agent.AgentRole, taskID, description string) SubAgentStartEvent {
	return SubAgentStartEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Role:             role,
		TaskID:           taskID,
		Description:      description,
	}
}

// SubAgentCompleteEvent fires when a sub-agent finishes.
type SubAgentCompleteEvent struct {
	baseMissionEvent
	Error    error
	TaskID   string
	Role     agent.AgentRole
	Duration time.Duration
	CostUSD  float64
	Success  bool
}

func (SubAgentCompleteEvent) missionEvent() {}

// NewSubAgentCompleteEvent creates a new SubAgentCompleteEvent.
func NewSubAgentCompleteEvent(role agent.AgentRole, taskID string, success bool, costUSD float64, duration time.Duration, err error) SubAgentCompleteEvent {
	return SubAgentCompleteEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Error:            err,
		TaskID:           taskID,
		Role:             role,
		Duration:         duration,
		CostUSD:          costUSD,
		Success:          success,
	}
}

// ToolStartEvent fires when a tool begins execution.
type ToolStartEvent struct {
	baseMissionEvent
	ToolName string
	ToolID   string
}

func (ToolStartEvent) missionEvent() {}

// NewToolStartEvent creates a new ToolStartEvent.
func NewToolStartEvent(toolName, toolID string) ToolStartEvent {
	return ToolStartEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		ToolName:         toolName,
		ToolID:           toolID,
	}
}

// ToolCompleteEvent fires when a tool completes.
type ToolCompleteEvent struct {
	baseMissionEvent
	Input    map[string]interface{}
	ToolName string
	ToolID   string
}

func (ToolCompleteEvent) missionEvent() {}

// NewToolCompleteEvent creates a new ToolCompleteEvent.
func NewToolCompleteEvent(toolName, toolID string, input map[string]interface{}) ToolCompleteEvent {
	return ToolCompleteEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Input:            input,
		ToolName:         toolName,
		ToolID:           toolID,
	}
}

// FileChangeEvent fires when a file is created or modified.
type FileChangeEvent struct {
	baseMissionEvent
	Path   string
	Action string // "create" or "modify"
	Agent  agent.AgentRole
}

func (FileChangeEvent) missionEvent() {}

// NewFileChangeEvent creates a new FileChangeEvent.
func NewFileChangeEvent(path, action string, agentRole agent.AgentRole) FileChangeEvent {
	return FileChangeEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Path:             path,
		Action:           action,
		Agent:            agentRole,
	}
}

// CostUpdateEvent fires with accumulated cost information.
type CostUpdateEvent struct {
	baseMissionEvent
	TotalCostUSD float64
	BudgetUSD    float64 // 0 means no budget limit
}

func (CostUpdateEvent) missionEvent() {}

// NewCostUpdateEvent creates a new CostUpdateEvent.
func NewCostUpdateEvent(totalCost, budget float64) CostUpdateEvent {
	return CostUpdateEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		TotalCostUSD:     totalCost,
		BudgetUSD:        budget,
	}
}

// TurnCompleteEvent fires when a conversation turn completes.
type TurnCompleteEvent struct {
	baseMissionEvent
	Error      error
	CostUSD    float64
	TurnNumber int
	Success    bool
}

func (TurnCompleteEvent) missionEvent() {}

// NewTurnCompleteEvent creates a new TurnCompleteEvent.
func NewTurnCompleteEvent(turnNumber int, success bool, costUSD float64, err error) TurnCompleteEvent {
	return TurnCompleteEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Error:            err,
		CostUSD:          costUSD,
		TurnNumber:       turnNumber,
		Success:          success,
	}
}

// IterationStartEvent fires when a builder-reviewer iteration begins.
type IterationStartEvent struct {
	baseMissionEvent
	Iteration int
}

func (IterationStartEvent) missionEvent() {}

// NewIterationStartEvent creates a new IterationStartEvent.
func NewIterationStartEvent(iteration int) IterationStartEvent {
	return IterationStartEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		Iteration:        iteration,
	}
}

// IterationCompleteEvent fires when a builder-reviewer iteration completes.
type IterationCompleteEvent struct {
	baseMissionEvent
	ExitReason ExitReason
	Iteration  int
	Accepted   bool
}

func (IterationCompleteEvent) missionEvent() {}

// NewIterationCompleteEvent creates a new IterationCompleteEvent.
func NewIterationCompleteEvent(iteration int, accepted bool, exitReason ExitReason) IterationCompleteEvent {
	return IterationCompleteEvent{
		baseMissionEvent: baseMissionEvent{ts: time.Now()},
		ExitReason:       exitReason,
		Iteration:        iteration,
		Accepted:         accepted,
	}
}
