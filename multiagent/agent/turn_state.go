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

	// lastSuccessfulResult records the most recent non-error ResultMessageEvent
	// of the logical turn. Unlike lastResult, it is NOT cleared by
	// invalidateForContinuation, so it survives a continuation wave that was
	// armed but never re-resulted before the stream closed. This is the only
	// evidence that the turn ever succeeded once invalidation has nilled
	// lastResult; the terminal-EOF resolution path consults it so a clean CLI
	// exit after a successful wave is reported as success, not a silent
	// Success=false. See claude_provider.go's closed-stream branches.
	lastSuccessfulResult *claude.ResultMessageEvent

	err error

	liveTaskIDs           map[string]struct{}
	terminalTaskIDs       map[string]struct{}
	cancelledToolUseIDs   map[string]struct{}
	completedBgToolUseIDs map[string]struct{}
	taskToToolUse         map[string]string

	blocks claude.ContentBlocks

	sessionID string
	text      strings.Builder
	thinking  strings.Builder

	// usage accumulates across all ResultMessageEvents in the logical turn:
	// pure-bg turns fire multiple result messages and the full cost is the sum.
	usage claude.TurnUsage

	turnNumber int

	// forcedDone latches a terminal TurnCompleteEvent with WakeupTimedOut set;
	// LogicalTurnDone short-circuits on it.
	forcedDone bool

	// sawFailedBgTask latches when any background task reached a non-success
	// terminal status (failed/killed/timeout). The terminal-EOF resolution
	// must NOT report success when a bg task failed: a turn can emit a
	// successful end-of-turn result while a Monitor is still live, then have
	// that Monitor fail and the CLI exit without a continuation result. Without
	// this gate, lastSuccessfulResult alone would mask the failed bg work as
	// success. Consulted only by TurnSucceededTerminally.
	sawFailedBgTask bool
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
			switch b := block.(type) {
			case claude.TextBlock:
				s.text.WriteString(b.Text)
			case claude.ThinkingBlock:
				s.thinking.WriteString(b.Thinking)
			}
		}

	case claude.UserMessageEvent:
		for _, block := range e.Blocks {
			s.blocks = append(s.blocks, block)
			result, ok := block.(claude.ToolResultBlock)
			if ok && result.IsError != nil && *result.IsError {
				s.cancelledToolUseIDs[result.ToolUseID] = struct{}{}
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
		if isFailedTaskStatus(e.Status) {
			s.sawFailedBgTask = true
		}
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
		s.invalidateForContinuation()

	case claude.TaskUpdatedEvent:
		if e.Status != nil {
			switch *e.Status {
			case "completed", "failed", "killed", "timeout":
				s.terminalTaskIDs[e.TaskID] = struct{}{}
				delete(s.liveTaskIDs, e.TaskID)
				if tuid := s.taskToToolUse[e.TaskID]; tuid != "" {
					s.completedBgToolUseIDs[tuid] = struct{}{}
				}
				if isFailedTaskStatus(*e.Status) {
					s.sawFailedBgTask = true
				}
				s.invalidateForContinuation()
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
			// Record the last non-error completion. This sticks across
			// invalidateForContinuation so a clean stream close after a
			// successful wave can still be resolved as success.
			s.lastSuccessfulResult = &result
		}

	case claude.TurnCompleteEvent:
		tc := e
		s.lastTurnComplete = &tc
		s.turnNumber = e.TurnNumber
		if e.WakeupTimedOut {
			s.forcedDone = true
		}

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
	// A safety-timer completion is terminal — honour it regardless of live
	// bg work or a missing ResultMessage, else a backgrounded infinite loop
	// strands streamTurn forever.
	if s.forcedDone {
		return true
	}
	if s.lastResult == nil || s.lastTurnComplete == nil {
		return false
	}
	if len(s.liveTaskIDs) > 0 {
		return false
	}
	return !s.hasUncancelledBgToolUse()
}

// SawTurnComplete reports whether a TurnCompleteEvent for the current
// completion wave is currently outstanding. streamTurn uses this to arm a
// grace timer: when a turn-complete has arrived but LogicalTurnDone() is
// still false (gated on a background tool_use), the loop must not block
// forever. Note lastTurnComplete is cleared by invalidateForContinuation
// and on each new ResultMessageEvent, so this only reports the live wave.
func (s *logicalTurnState) SawTurnComplete() bool {
	return s.lastTurnComplete != nil
}

// HasLiveTasks reports whether any bg task or uncancelled bg tool_use is
// still outstanding. Consumers gate retry decisions on this: retrying a
// parked session would interrupt the bg work.
func (s *logicalTurnState) HasLiveTasks() bool {
	return len(s.liveTaskIDs) > 0 || s.hasUncancelledBgToolUse()
}

// invalidateForContinuation drops the currently-observed Result+TurnComplete
// pair when a terminal bg-task event lands after the wave's ResultMessage
// already arrived. The CLI auto-continues after any terminal bg event, so the
// logical turn must wait for the continuation wave's Result+TurnComplete pair
// before it can be considered done. Without this, a wave-1 (Result, TurnComplete)
// pair followed by a terminal bg event would satisfy LogicalTurnDone
// immediately — before the continuation wave drains — causing streamTurn to
// return prematurely.
func (s *logicalTurnState) invalidateForContinuation() {
	if s.lastResult == nil {
		return
	}
	s.lastResult = nil
	s.lastTurnComplete = nil
}

func (s *logicalTurnState) hasUncancelledBgToolUse() bool {
	for _, block := range s.blocks {
		toolUse, ok := block.(claude.ToolUseBlock)
		if !ok || !isBackgroundToolUse(toolUse) {
			continue
		}
		if _, cancelled := s.cancelledToolUseIDs[toolUse.ID]; cancelled {
			continue
		}
		if _, done := s.completedBgToolUseIDs[toolUse.ID]; done {
			continue
		}
		return true
	}
	return false
}

func (s *logicalTurnState) Text() string                        { return s.text.String() }
func (s *logicalTurnState) Thinking() string                    { return s.thinking.String() }
func (s *logicalTurnState) ContentBlocks() claude.ContentBlocks { return s.blocks }
func (s *logicalTurnState) Usage() claude.TurnUsage             { return s.usage }
func (s *logicalTurnState) SessionID() string                   { return s.sessionID }
func (s *logicalTurnState) Err() error                          { return s.err }
func (s *logicalTurnState) TurnNumber() int                     { return s.turnNumber }

func (s *logicalTurnState) Success() bool {
	return s.lastResult != nil && !s.lastResult.IsError
}

// TurnSucceededTerminally reports whether the logical turn ever produced a
// successful completion that should hold at a clean stream close, even if the
// last wave was invalidated for a continuation that never re-resulted. It is
// only meaningful when no error is outstanding — a real error always wins —
// and never holds when a background task reached a failed/killed/timeout
// terminal status, which the absent continuation result would otherwise mask.
func (s *logicalTurnState) TurnSucceededTerminally() bool {
	return s.err == nil && !s.sawFailedBgTask && s.lastSuccessfulResult != nil
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

// ToTerminalTurnResult resolves the turn at a clean stream close (EOF with no
// error). It differs from ToTurnResult only in the success determination: when
// the live wave was invalidated for a continuation that never arrived
// (lastResult == nil) but an earlier wave succeeded (TurnSucceededTerminally),
// the turn is reported as success. A real error or a turn that closed before
// any successful result still reports Success=false via the normal path.
func (s *logicalTurnState) ToTerminalTurnResult() *claude.TurnResult {
	result := s.ToTurnResult()
	if !result.Success && s.TurnSucceededTerminally() {
		result.Success = true
		// Surface the last successful wave's duration (ToTurnResult derived 0
		// from the nilled lastResult); usage/text/blocks already accumulate
		// across waves and survive invalidation. TurnSucceededTerminally
		// guarantees lastSuccessfulResult is non-nil here.
		result.DurationMs = s.lastSuccessfulResult.DurationMs
	}
	// A failed/killed/timeout background task never sets s.err, so a non-success
	// close caused by one would otherwise carry no error. Attach a classifiable
	// one; a genuine ResultMessage error (result.Error != nil) still wins.
	if !result.Success && result.Error == nil && s.sawFailedBgTask {
		result.Error = claude.ErrBackgroundTaskFailed
	}
	return result
}

// isFailedTaskStatus reports whether a terminal background-task status is a
// failure rather than a clean completion. "completed" is the only success;
// failed/killed/timeout all indicate the bg work did not succeed.
func isFailedTaskStatus(status string) bool {
	switch status {
	case "failed", "killed", "timeout":
		return true
	default:
		return false
	}
}

// backgroundToolNames lists tool names the CLI treats as background even
// without run_in_background:true in the tool_use input.
//
// Monitor streams a blocking command. Task/Agent spawn a sub-agent that the CLI
// dispatches asynchronously and reports on via TaskStarted/TaskNotification
// events — the tool_use input carries no run_in_background flag, so it must be
// recognized by name to keep hasUncancelledBgToolUse (hence LogicalTurnDone)
// gated until the sub-agent's terminal notification lands.
var backgroundToolNames = map[string]bool{
	"Monitor": true,
	"Task":    true,
	"Agent":   true,
}

func isBackgroundToolUse(block claude.ToolUseBlock) bool {
	if isBg, _ := block.Input["run_in_background"].(bool); isBg {
		return true
	}
	return backgroundToolNames[block.Name]
}
