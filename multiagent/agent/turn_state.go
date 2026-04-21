package agent

import (
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// logicalTurnState accumulates the raw event stream from a claude.Session and
// decides when the "logical turn" (the unit of work a user-visible prompt
// kicked off) is done.
//
// The Python claude-agent-sdk streams ResultMessage at every CLI turn
// boundary. Pure background turns (e.g. a turn whose only tool_use is a
// Monitor) cause the CLI to emit ResultMessage ~immediately and then
// auto-continue with a new turn when the bg task finishes. For a consumer
// that wants to wait for "the work is really done", observing a single
// ResultMessage is not enough — they must also know no bg tasks are still
// live and no uncancelled bg tool_use is parked.
//
// logicalTurnState encapsulates that policy in one place so every consumer
// (claude_provider.Execute, long-running bramble orchestrators, tests) uses
// the same definition.
type logicalTurnState struct {
	// lastResult carries the most recent ResultMessageEvent. When present,
	// LogicalTurnDone consults it together with the bg-work predicate to
	// decide done-ness.
	lastResult *claude.ResultMessageEvent

	// lastTurnComplete is the most recent TurnCompleteEvent observed. Used
	// as the terminal signal: the wrapper emits ResultMessage first, then
	// runs finalization logic (usage accounting, state transitions) and
	// emits TurnComplete. Gating LogicalTurnDone on TurnComplete ensures
	// downstream dispatch sees both events before the consumer loop exits.
	lastTurnComplete *claude.TurnCompleteEvent

	// err is populated when the final result message carries an error and
	// no subsequent non-error continuation turn arrived.
	err error

	// liveTaskIDs is the set of task IDs that have fired TaskStartedEvent
	// but not yet fired a terminal TaskNotificationEvent.
	liveTaskIDs map[string]struct{}

	// bgToolUseIDs is the set of tool_use IDs that represent background work
	// (run_in_background:true Bash or Monitor).
	bgToolUseIDs map[string]struct{}

	// cancelledToolUseIDs tracks tool_use IDs whose tool_result carried
	// IsError — a bg tool that was cancelled never produces a continuation
	// ResultMessage, so it should not hold up logical-turn completion.
	cancelledToolUseIDs map[string]struct{}

	// completedBgToolUseIDs tracks bg tool_use IDs whose task reached a
	// terminal state (via TaskNotificationEvent).
	completedBgToolUseIDs map[string]struct{}

	// taskToToolUse maps task_id → tool_use_id for bg tasks whose
	// TaskStartedEvent carried a tool_use_id. Lets a later
	// TaskNotificationEvent (which may or may not carry tool_use_id) mark
	// the corresponding bg tool_use complete.
	taskToToolUse map[string]string

	// blocks accumulates ContentBlocks across AssistantMessageEvents and
	// UserMessageEvents. Used both to surface final Text/ContentBlocks on
	// the AgentResult and as the source of truth for "are there uncancelled
	// bg tool_use blocks left?"
	blocks []claude.ContentBlock

	// sessionID captures the CLI session ID from the ReadyEvent.
	sessionID string

	// text is the concatenated assistant text across all streamed messages.
	text strings.Builder

	// thinking is the concatenated assistant thinking.
	thinking strings.Builder

	// usage accumulates token/cost totals across all ResultMessageEvents in
	// the logical turn. Pure-bg turns fire multiple result messages — the
	// full logical-turn cost is the sum.
	usage claude.TurnUsage

	// turnNumber is the latest turn number seen.
	turnNumber int

	haveResult bool
}

// newLogicalTurnState returns a fresh state machine ready to consume events.
func newLogicalTurnState() *logicalTurnState {
	return &logicalTurnState{
		liveTaskIDs:           make(map[string]struct{}),
		bgToolUseIDs:          make(map[string]struct{}),
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
			case claude.ContentBlockTypeToolUse:
				if isBackgroundToolUse(block) {
					s.bgToolUseIDs[block.ToolUseID] = struct{}{}
				}
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
		s.liveTaskIDs[e.TaskID] = struct{}{}
		if e.ToolUseID != nil && *e.ToolUseID != "" {
			s.taskToToolUse[e.TaskID] = *e.ToolUseID
		}

	case claude.TaskNotificationEvent:
		// Any terminal TaskNotificationEvent drains the task from the live
		// set — completed, failed, killed, timeout all count as "done".
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
				delete(s.liveTaskIDs, e.TaskID)
				if tuid := s.taskToToolUse[e.TaskID]; tuid != "" {
					s.completedBgToolUseIDs[tuid] = struct{}{}
				}
			}
		}

	case claude.ResultMessageEvent:
		result := e
		s.lastResult = &result
		s.haveResult = true
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

// LogicalTurnDone reports whether the logical turn has finished. Requires:
//   - at least one ResultMessageEvent AND its paired TurnCompleteEvent have
//     arrived, AND
//   - the live task set is empty, AND
//   - no uncancelled bg tool_use block is still awaiting its continuation.
//
// Gating on TurnCompleteEvent (not just ResultMessageEvent) guarantees the
// consumer loop continues pumping dispatchClaudeEvent through the final
// turn-complete notification — otherwise EventHandler.OnTurnComplete would
// race with loop exit and silently drop.
//
// Pure-bg turns return false after the first TurnCompleteEvent (bg task
// still live) and flip to true after the TaskNotificationEvent + the
// auto-continuation ResultMessageEvent+TurnCompleteEvent arrive. Mixed
// sync+bg turns behave the same — the sync tools fire their ResultMessage,
// but liveTaskIDs is non-empty, so the state waits for the bg path to drain.
func (s *logicalTurnState) LogicalTurnDone() bool {
	if !s.haveResult {
		return false
	}
	if s.lastTurnComplete == nil {
		return false
	}
	if len(s.liveTaskIDs) > 0 {
		return false
	}
	// Walk accumulated blocks for uncancelled, uncompleted bg tool_use
	// entries. The Python SDK does not deduplicate tool_use IDs across
	// auto-continuation turns, so the same ID may appear once — that's fine,
	// we only care about presence.
	for _, block := range s.blocks {
		if block.Type != claude.ContentBlockTypeToolUse {
			continue
		}
		if !isBackgroundToolUse(block) {
			continue
		}
		if _, cancelled := s.cancelledToolUseIDs[block.ToolUseID]; cancelled {
			continue
		}
		if _, done := s.completedBgToolUseIDs[block.ToolUseID]; done {
			continue
		}
		// Bg tool still live — wait.
		return false
	}
	return true
}

// HasLiveTasks reports whether any bg task is still registered as live.
// Consumers use this to gate retry decisions: retrying a parked session
// would interrupt the bg work.
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

// Text returns the concatenated assistant text across the logical turn.
func (s *logicalTurnState) Text() string { return s.text.String() }

// Thinking returns the concatenated assistant thinking.
func (s *logicalTurnState) Thinking() string { return s.thinking.String() }

// ContentBlocks returns the accumulated content blocks in arrival order.
func (s *logicalTurnState) ContentBlocks() []claude.ContentBlock { return s.blocks }

// Usage returns the accumulated token/cost totals.
func (s *logicalTurnState) Usage() claude.TurnUsage { return s.usage }

// SessionID returns the CLI session ID captured from ReadyEvent (or empty).
func (s *logicalTurnState) SessionID() string { return s.sessionID }

// Err returns any error surfaced by the latest result message.
func (s *logicalTurnState) Err() error { return s.err }

// TurnNumber returns the latest turn number observed.
func (s *logicalTurnState) TurnNumber() int { return s.turnNumber }

// Success returns true when the latest result message was non-error.
func (s *logicalTurnState) Success() bool {
	if !s.haveResult {
		return false
	}
	return !s.lastResult.IsError
}

// DurationMs returns the last result message's reported duration.
func (s *logicalTurnState) DurationMs() int64 {
	if !s.haveResult {
		return 0
	}
	return s.lastResult.DurationMs
}

// ToTurnResult builds a claude.TurnResult carrying the accumulated state.
// Used by claude_provider.Execute to keep the existing AgentResult-building
// code unchanged — the provider does not need to learn the new event shape
// just to convert a final result.
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

// backgroundToolNames lists tool names that the CLI registers as background
// tasks even when the tool_use input does not carry run_in_background:true.
// Must stay in sync with the wrapper-internal backgroundTools map.
var backgroundToolNames = map[string]bool{
	"Monitor": true,
}

// isBackgroundToolUse mirrors the wrapper-internal classification: a
// tool_use block is "background" if its input carries run_in_background:true
// OR its name is in the known background tool set.
func isBackgroundToolUse(block claude.ContentBlock) bool {
	if block.Type != claude.ContentBlockTypeToolUse {
		return false
	}
	if isBg, _ := block.ToolInput["run_in_background"].(bool); isBg {
		return true
	}
	return backgroundToolNames[block.ToolName]
}
