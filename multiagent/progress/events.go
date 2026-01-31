// Package progress provides progress reporting for swarm execution.
package progress

import (
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
)

// EventType identifies the kind of progress event.
type EventType int

const (
	EventPhaseChange EventType = iota
	EventAgentStart
	EventAgentComplete
	EventAgentThinking
	EventToolStart
	EventToolComplete
	EventIteration
	EventCostUpdate
	EventFileChange
	EventError
)

// Event is the interface for all progress events.
type Event interface {
	Type() EventType
	Timestamp() time.Time
}

// PhaseChangeEvent fires when the mission phase changes.
type PhaseChangeEvent struct {
	ts        time.Time
	From      checkpoint.Phase
	To        checkpoint.Phase
	Iteration int
}

// Type returns the event type.
func (e PhaseChangeEvent) Type() EventType { return EventPhaseChange }

// Timestamp returns when the event occurred.
func (e PhaseChangeEvent) Timestamp() time.Time { return e.ts }

// NewPhaseChangeEvent creates a new phase change event.
func NewPhaseChangeEvent(from, to checkpoint.Phase, iteration int) PhaseChangeEvent {
	return PhaseChangeEvent{
		ts:        time.Now(),
		From:      from,
		To:        to,
		Iteration: iteration,
	}
}

// AgentStartEvent fires when an agent begins work.
type AgentStartEvent struct {
	ts       time.Time
	Role     agent.AgentRole
	TaskID   string
	TaskDesc string
}

// Type returns the event type.
func (e AgentStartEvent) Type() EventType { return EventAgentStart }

// Timestamp returns when the event occurred.
func (e AgentStartEvent) Timestamp() time.Time { return e.ts }

// NewAgentStartEvent creates a new agent start event.
func NewAgentStartEvent(role agent.AgentRole, taskID, taskDesc string) AgentStartEvent {
	return AgentStartEvent{
		ts:       time.Now(),
		Role:     role,
		TaskID:   taskID,
		TaskDesc: taskDesc,
	}
}

// AgentCompleteEvent fires when an agent finishes.
type AgentCompleteEvent struct {
	ts       time.Time
	Error    error
	Role     agent.AgentRole
	TaskID   string
	CostUSD  float64
	Duration time.Duration
	Success  bool
}

// Type returns the event type.
func (e AgentCompleteEvent) Type() EventType { return EventAgentComplete }

// Timestamp returns when the event occurred.
func (e AgentCompleteEvent) Timestamp() time.Time { return e.ts }

// NewAgentCompleteEvent creates a new agent complete event.
func NewAgentCompleteEvent(role agent.AgentRole, taskID string, success bool, cost float64, duration time.Duration, err error) AgentCompleteEvent {
	return AgentCompleteEvent{
		ts:       time.Now(),
		Role:     role,
		TaskID:   taskID,
		Success:  success,
		CostUSD:  cost,
		Duration: duration,
		Error:    err,
	}
}

// AgentThinkingEvent fires when an agent is processing (waiting for LLM response).
type AgentThinkingEvent struct {
	ts      time.Time
	Role    agent.AgentRole
	Message string
}

// Type returns the event type.
func (e AgentThinkingEvent) Type() EventType { return EventAgentThinking }

// Timestamp returns when the event occurred.
func (e AgentThinkingEvent) Timestamp() time.Time { return e.ts }

// NewAgentThinkingEvent creates a new agent thinking event.
func NewAgentThinkingEvent(role agent.AgentRole, message string) AgentThinkingEvent {
	return AgentThinkingEvent{
		ts:      time.Now(),
		Role:    role,
		Message: message,
	}
}

// ToolActivityEvent fires for tool usage.
type ToolActivityEvent struct {
	ts        time.Time
	Input     map[string]interface{}
	AgentRole agent.AgentRole
	ToolName  string
	ToolID    string
	Started   bool
}

// Type returns the event type.
func (e ToolActivityEvent) Type() EventType {
	if e.Started {
		return EventToolStart
	}
	return EventToolComplete
}

// Timestamp returns when the event occurred.
func (e ToolActivityEvent) Timestamp() time.Time { return e.ts }

// NewToolStartEvent creates a new tool start event.
func NewToolStartEvent(role agent.AgentRole, toolName, toolID string) ToolActivityEvent {
	return ToolActivityEvent{
		ts:        time.Now(),
		AgentRole: role,
		ToolName:  toolName,
		ToolID:    toolID,
		Started:   true,
	}
}

// NewToolCompleteEvent creates a new tool complete event.
func NewToolCompleteEvent(role agent.AgentRole, toolName, toolID string, input map[string]interface{}) ToolActivityEvent {
	return ToolActivityEvent{
		ts:        time.Now(),
		AgentRole: role,
		ToolName:  toolName,
		ToolID:    toolID,
		Input:     input,
		Started:   false,
	}
}

// IterationEvent fires when a design-build-review cycle occurs.
type IterationEvent struct {
	ts            time.Time
	Reason        string
	Number        int
	MaxIterations int
}

// Type returns the event type.
func (e IterationEvent) Type() EventType { return EventIteration }

// Timestamp returns when the event occurred.
func (e IterationEvent) Timestamp() time.Time { return e.ts }

// NewIterationEvent creates a new iteration event.
func NewIterationEvent(number, max int, reason string) IterationEvent {
	return IterationEvent{
		ts:            time.Now(),
		Number:        number,
		MaxIterations: max,
		Reason:        reason,
	}
}

// CostUpdateEvent fires periodically with accumulated cost.
type CostUpdateEvent struct {
	ts           time.Time
	AgentCosts   map[agent.AgentRole]float64
	TotalCostUSD float64
	BudgetUSD    float64
}

// Type returns the event type.
func (e CostUpdateEvent) Type() EventType { return EventCostUpdate }

// Timestamp returns when the event occurred.
func (e CostUpdateEvent) Timestamp() time.Time { return e.ts }

// NewCostUpdateEvent creates a new cost update event.
func NewCostUpdateEvent(total, budget float64, agentCosts map[agent.AgentRole]float64) CostUpdateEvent {
	return CostUpdateEvent{
		ts:           time.Now(),
		TotalCostUSD: total,
		BudgetUSD:    budget,
		AgentCosts:   agentCosts,
	}
}

// ErrorEvent fires when an error occurs.
type ErrorEvent struct {
	ts      time.Time
	Err     error
	Context string
}

// Type returns the event type.
func (e ErrorEvent) Type() EventType { return EventError }

// Timestamp returns when the event occurred.
func (e ErrorEvent) Timestamp() time.Time { return e.ts }

// NewErrorEvent creates a new error event.
func NewErrorEvent(err error, context string) ErrorEvent {
	return ErrorEvent{
		ts:      time.Now(),
		Err:     err,
		Context: context,
	}
}

// FileChangeEvent fires when a file is created or modified.
type FileChangeEvent struct {
	ts        time.Time
	AgentRole agent.AgentRole
	Path      string
	Action    string // "create" or "modify"
}

// Type returns the event type.
func (e FileChangeEvent) Type() EventType { return EventFileChange }

// Timestamp returns when the event occurred.
func (e FileChangeEvent) Timestamp() time.Time { return e.ts }

// NewFileChangeEvent creates a new file change event.
func NewFileChangeEvent(role agent.AgentRole, path, action string) FileChangeEvent {
	return FileChangeEvent{
		ts:        time.Now(),
		AgentRole: role,
		Path:      path,
		Action:    action,
	}
}
