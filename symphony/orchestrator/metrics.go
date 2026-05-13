package orchestrator

import (
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/symphony/agent"
	"github.com/bazelment/yoloswe/symphony/model"
)

// recordLiveSessionEvent folds one agent event into the live session state used
// by snapshots and aggregate accounting.
func recordLiveSessionEvent(session *model.LiveSession, event agent.Event, observedAt time.Time) {
	session.LastAgentTimestamp = &observedAt
	if event.Type != "" {
		eventStr := string(event.Type)
		session.LastAgentEvent = &eventStr
	}
	if event.Message != "" {
		session.LastAgentMessage = event.Message
	}

	if event.Type == agent.EventSessionStarted {
		recordSessionIdentity(session, event)
	}

	recordTokenUsage(session, event)
}

func recordSessionIdentity(session *model.LiveSession, event agent.Event) {
	if event.SessionID != "" {
		session.SessionID = event.SessionID
	}
	if event.ThreadID != "" {
		session.ThreadID = event.ThreadID
	}
	if event.TurnID != "" {
		session.TurnID = event.TurnID
	}
	if event.PID != nil {
		pidStr := fmt.Sprintf("%d", *event.PID)
		session.AgentPID = &pidStr
	}
	session.TurnCount++
}

func recordTokenUsage(session *model.LiveSession, event agent.Event) {
	if event.TotalTokens <= 0 {
		return
	}

	inputDelta := event.InputTokens - session.LastReportedInputToks
	outputDelta := event.OutputTokens - session.LastReportedOutputToks

	if inputDelta > 0 {
		session.InputTokens += inputDelta
		session.LastReportedInputToks = event.InputTokens
	}
	if outputDelta > 0 {
		session.OutputTokens += outputDelta
		session.LastReportedOutputToks = event.OutputTokens
	}
	session.TotalTokens = session.InputTokens + session.OutputTokens
	session.LastReportedTotalToks = event.TotalTokens
}
