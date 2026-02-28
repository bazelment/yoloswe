package sessionmodel

import (
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// FromClaudeEvent maps a claude.Event to SessionModel mutations.
// This bridges the existing claude.Session event path into the new model
// during the transition period.
func FromClaudeEvent(model *SessionModel, event claude.Event) {
	switch e := event.(type) {
	case claude.ReadyEvent:
		model.SetMeta(SessionMeta{
			SessionID:      e.Info.SessionID,
			Model:          e.Info.Model,
			CWD:            e.Info.WorkDir,
			PermissionMode: string(e.Info.PermissionMode),
			Tools:          e.Info.Tools,
			Status:         StatusRunning,
		})

	case claude.TextEvent:
		model.AppendStreamingText(e.Text)

	case claude.ThinkingEvent:
		model.AppendStreamingThinking(e.Thinking)

	case claude.ToolStartEvent:
		model.AppendOutput(OutputLine{
			Timestamp: e.Timestamp,
			Type:      OutputTypeToolStart,
			Content:   e.Name, // input not yet available at start
			ToolName:  e.Name,
			ToolID:    e.ID,
			ToolState: ToolStateRunning,
			StartTime: e.Timestamp,
		})
		model.UpdateProgress(func(p *ProgressSnapshot) {
			p.CurrentTool = e.Name
			p.CurrentPhase = "tool_execution"
			p.LastActivity = time.Now()
		})

	case claude.ToolCompleteEvent:
		model.UpdateTool(e.ID, func(line *OutputLine) {
			if e.Input != nil {
				line.ToolInput = e.Input
				line.Content = FormatToolContent(e.Name, e.Input)
			}
		})

	case claude.CLIToolResultEvent:
		now := time.Now()
		model.UpdateTool(e.ToolUseID, func(line *OutputLine) {
			line.ToolResult = e.Content
			line.IsError = e.IsError
			if e.IsError {
				line.ToolState = ToolStateError
			} else {
				line.ToolState = ToolStateComplete
			}
			if !line.StartTime.IsZero() {
				line.DurationMs = now.Sub(line.StartTime).Milliseconds()
			}
		})
		model.UpdateProgress(func(p *ProgressSnapshot) {
			p.CurrentTool = ""
			p.CurrentPhase = ""
			p.LastActivity = time.Now()
		})

	case claude.TurnCompleteEvent:
		model.UpdateProgress(func(p *ProgressSnapshot) {
			p.TurnCount = e.TurnNumber
			p.TotalCostUSD = e.Usage.CostUSD
			p.InputTokens = e.Usage.InputTokens
			p.OutputTokens = e.Usage.OutputTokens
			p.LastActivity = time.Now()
		})
		model.AppendOutput(OutputLine{
			Timestamp:  time.Now(),
			Type:       OutputTypeTurnEnd,
			Content:    fmt.Sprintf("Turn %d complete", e.TurnNumber),
			TurnNumber: e.TurnNumber,
			CostUSD:    e.Usage.CostUSD,
			DurationMs: e.DurationMs,
			IsError:    !e.Success,
		})

	case claude.ErrorEvent:
		model.AppendOutput(OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			Content:   fmt.Sprintf("%s: %v", e.Context, e.Error),
			IsError:   true,
		})
	}
}

// FromAgentEvent maps an agent.AgentEvent to SessionModel mutations.
// This is used by providerRunner for non-Claude providers (Codex/Gemini).
func FromAgentEvent(model *SessionModel, event agent.AgentEvent) {
	switch e := event.(type) {
	case agent.TextAgentEvent:
		model.AppendStreamingText(e.Text)

	case agent.ThinkingAgentEvent:
		model.AppendStreamingThinking(e.Thinking)

	case agent.ToolStartAgentEvent:
		now := time.Now()
		model.AppendOutput(OutputLine{
			Timestamp: now,
			Type:      OutputTypeToolStart,
			Content:   FormatToolContent(e.Name, e.Input),
			ToolName:  e.Name,
			ToolID:    e.ID,
			ToolInput: e.Input,
			ToolState: ToolStateRunning,
			StartTime: now,
		})
		model.UpdateProgress(func(p *ProgressSnapshot) {
			p.CurrentTool = e.Name
			p.CurrentPhase = "tool_execution"
			p.LastActivity = time.Now()
		})

	case agent.ToolCompleteAgentEvent:
		now := time.Now()
		model.UpdateTool(e.ID, func(line *OutputLine) {
			if e.Input != nil {
				line.ToolInput = e.Input
				line.Content = FormatToolContent(e.Name, e.Input)
			}
			line.ToolResult = e.Result
			line.IsError = e.IsError
			if e.IsError {
				line.ToolState = ToolStateError
			} else {
				line.ToolState = ToolStateComplete
			}
			if !line.StartTime.IsZero() {
				line.DurationMs = now.Sub(line.StartTime).Milliseconds()
			}
		})
		model.UpdateProgress(func(p *ProgressSnapshot) {
			p.CurrentTool = ""
			p.CurrentPhase = ""
			p.LastActivity = time.Now()
		})

	case agent.TurnCompleteAgentEvent:
		model.UpdateProgress(func(p *ProgressSnapshot) {
			p.TurnCount = e.TurnNumber
			p.TotalCostUSD = e.CostUSD
			p.LastActivity = time.Now()
		})
		model.AppendOutput(OutputLine{
			Timestamp:  time.Now(),
			Type:       OutputTypeTurnEnd,
			Content:    fmt.Sprintf("Turn %d complete", e.TurnNumber),
			TurnNumber: e.TurnNumber,
			CostUSD:    e.CostUSD,
			DurationMs: e.DurationMs,
			IsError:    !e.Success,
		})

	case agent.ErrorAgentEvent:
		model.AppendOutput(OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			Content:   fmt.Sprintf("%s: %v", e.Context, e.Err),
			IsError:   true,
		})
	}
}
