package agent

import (
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// logicalTurnState decides when the "logical turn" (the unit of work a
// user-visible prompt kicked off) is done. Pure-background turns stream a
// ResultMessage immediately, then the CLI auto-continues when the bg task
// finishes — so "done" requires both the terminal events AND no live bg work.
type logicalTurnState struct {
	lastResult       *claude.ResultMessageEvent
	lastTurnComplete *claude.TurnCompleteEvent

	err error

	liveTaskIDs           map[string]struct{}
	terminalTaskIDs       map[string]struct{}
	cancelledToolUseIDs   map[string]struct{}
	completedBgToolUseIDs map[string]struct{}
	taskToToolUse         map[string]string

	blocks []claude.ContentBlock

	sessionID string
	text      strings.Builder
	thinking  strings.Builder

	// usage accumulates across all ResultMessageEvents in the logical turn:
	// pure-bg turns fire multiple result messages and the full cost is the sum.
	usage claude.TurnUsage

	turnNumber int
}

func newLogicalTurnState() *logicalTurnState {
	return &logicalTurnState{
		liveTaskIDs:           make(map[string]struct{}),
		terminalTaskIDs:       make(map[string]struct{}),
		cancelledToolUseIDs:   make(map[string]struct{}),
		completedBgToolUseIDs: make(map[string]struct{}),
		taskToToolUse:         make(map[string]string),
	}
}

// Apply feeds one SDK event into the state machine. Safe to call with any
// event type — unrecognised events are ignored.
func (s *logicalTurnState) Apply(ev claude.Event) {
	switch e := ev.(type) {
	case claude.ReadyEvent:
		s.sessionID = e.Info.SessionID

	case claude.AssistantMessageEvent:
		s.turnNumber = e.TurnNumber
		for _, block := range e.Blocks {
			s.blocks = append(s.blocks, block)
			switch block.Type {
			case claude.ContentBlockTypeText:
				s.text.WriteString(block.Text)
			case claude.ContentBlockTypeThinking:
				s.thinking.WriteString(block.Thinking)
			}
		}

	case claude.UserMessageEvent:
		for _, block := range e.Blocks {
			s.blocks = append(s.blocks, block)
			if block.Type == claude.ContentBlockTypeToolResult && block.IsError {
				s.cancelledToolUseIDs[block.ToolUseID] = struct{}{}
			}
		}

	case claude.TaskStartedEvent:
		if e.ToolUseID != nil && *e.ToolUseID != "" {
			s.taskToToolUse[e.TaskID] = *e.ToolUseID
		}
		// Ignore a TaskStartedEvent if the terminal event already arrived for
		// this task (possible under stream reorder). Re-adding a terminal
		// task to liveTaskIDs would strand LogicalTurnDone forever.
		if _, terminal := s.terminalTaskIDs[e.TaskID]; terminal {
			if tuid := s.taskToToolUse[e.TaskID]; tuid != "" {
				s.completedBgToolUseIDs[tuid] = struct{}{}
			}
			break
		}
		s.liveTaskIDs[e.TaskID] = struct{}{}

	case claude.TaskNotificationEvent:
		// Any terminal TaskNotificationEvent drains the task from the live
		// set — completed, failed, killed, timeout all count as "done".
		s.terminalTaskIDs[e.TaskID] = struct{}{}
		delete(s.liveTaskIDs, e.TaskID)
		toolUseID := ""
		if e.ToolUseID != nil {
			toolUseID = *e.ToolUseID
		}
		if toolUseID == "" {
			toolUseID = s.taskToToolUse[e.TaskID]
		}
		if toolUseID != "" {
			s.completedBgToolUseIDs[toolUseID] = struct{}{}
		}

	case claude.TaskUpdatedEvent:
		if e.Status != nil {
			switch *e.Status {
			case "completed", "failed", "killed", "timeout":
				s.terminalTaskIDs[e.TaskID] = struct{}{}
				delete(s.liveTaskIDs, e.TaskID)
				if tuid := s.taskToToolUse[e.TaskID]; tuid != "" {
					s.completedBgToolUseIDs[tuid] = struct{}{}
				}
			}
		}

	case claude.ResultMessageEvent:
		result := e
		s.lastResult = &result
		// Each ResultMessageEvent starts a new completion wave; drop any
		// stale TurnCompleteEvent from a prior wave so LogicalTurnDone only
		// fires once the TurnCompleteEvent paired with *this* Result arrives.
		// Without this, a continuation Result + an earlier wave's
		// TurnComplete would satisfy the gate and cause streamTurn to return
		// before the matching TurnComplete is drained from the channel.
		s.lastTurnComplete = nil
		s.turnNumber = e.TurnNumber
		s.usage.Add(e.Usage)
		// Track the latest error; a successful continuation clears it.
		if e.IsError {
			s.err = e.Error
		} else {
			s.err = nil
		}

	case claude.TurnCompleteEvent:
		tc := e
		s.lastTurnComplete = &tc
		s.turnNumber = e.TurnNumber

	case claude.ErrorEvent:
		if s.err == nil {
			s.err = e.Error
		}
	}
}

// LogicalTurnDone reports whether the logical turn has finished. Requires a
// ResultMessage + TurnComplete pair AND no live tasks AND no uncancelled bg
// tool_use block awaiting continuation. Gating on TurnComplete (not just
// ResultMessage) ensures downstream handlers see OnTurnComplete before the
// consumer loop exits.
func (s *logicalTurnState) LogicalTurnDone() bool {
	if s.lastResult == nil || s.lastTurnComplete == nil {
		return false
	}
	if len(s.liveTaskIDs) > 0 {
		return false
	}
	return !s.hasUncancelledBgToolUse()
}

// HasLiveTasks reports whether any bg task or uncancelled bg tool_use is
// still outstanding. Consumers gate retry decisions on this: retrying a
// parked session would interrupt the bg work.
func (s *logicalTurnState) HasLiveTasks() bool {
	return len(s.liveTaskIDs) > 0 || s.hasUncancelledBgToolUse()
}

func (s *logicalTurnState) hasUncancelledBgToolUse() bool {
	for _, block := range s.blocks {
		if block.Type != claude.ContentBlockTypeToolUse || !isBackgroundToolUse(block) {
			continue
		}
		if _, cancelled := s.cancelledToolUseIDs[block.ToolUseID]; cancelled {
			continue
		}
		if _, done := s.completedBgToolUseIDs[block.ToolUseID]; done {
			continue
		}
		return true
	}
	return false
}

func (s *logicalTurnState) Text() string                         { return s.text.String() }
func (s *logicalTurnState) Thinking() string                     { return s.thinking.String() }
func (s *logicalTurnState) ContentBlocks() []claude.ContentBlock { return s.blocks }
func (s *logicalTurnState) Usage() claude.TurnUsage              { return s.usage }
func (s *logicalTurnState) SessionID() string                    { return s.sessionID }
func (s *logicalTurnState) Err() error                           { return s.err }
func (s *logicalTurnState) TurnNumber() int                      { return s.turnNumber }

func (s *logicalTurnState) Success() bool {
	return s.lastResult != nil && !s.lastResult.IsError
}

func (s *logicalTurnState) DurationMs() int64 {
	if s.lastResult == nil {
		return 0
	}
	return s.lastResult.DurationMs
}

func (s *logicalTurnState) ToTurnResult() *claude.TurnResult {
	return &claude.TurnResult{
		TurnNumber:    s.turnNumber,
		Success:       s.Success(),
		DurationMs:    s.DurationMs(),
		Usage:         s.usage,
		Text:          s.Text(),
		Thinking:      s.Thinking(),
		ContentBlocks: s.blocks,
		Error:         s.err,
	}
}

// backgroundToolNames lists tool names the CLI treats as background even
// without run_in_background:true in the tool_use input.
var backgroundToolNames = map[string]bool{
	"Monitor": true,
}

func isBackgroundToolUse(block claude.ContentBlock) bool {
	if block.Type != claude.ContentBlockTypeToolUse {
		return false
	}
	if isBg, _ := block.ToolInput["run_in_background"].(bool); isBg {
		return true
	}
	return backgroundToolNames[block.ToolName]
}
