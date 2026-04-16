package claude

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// toolUseErrorMarker is the sentinel the Claude CLI wraps around tool
// invocations it refused, blocked, or cancelled (disable-model-invocation,
// sleep block, parallel-cancelled siblings). It is NOT emitted for
// nonzero-exit Bash the agent ran to inspect output — those carry a raw
// "Exit code N\n<output>" string with is_error=true but no wrapper. The
// retry detector requires both IsError and this marker.
const toolUseErrorMarker = "<tool_use_error>"

const toolErrorExcerptMaxRunes = 200

func stringifyToolResult(result interface{}) string {
	if result == nil {
		return ""
	}
	switch v := result.(type) {
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, entry := range v {
			m, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
		return fmt.Sprint(v)
	case []map[string]interface{}:
		var b strings.Builder
		for _, m := range v {
			if text, ok := m["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
		return fmt.Sprint(v)
	default:
		return fmt.Sprint(v)
	}
}

// excerptRunes returns the first max runes of s. Iterates by rune via
// `range string` to avoid allocating a full []rune for large tool outputs.
func excerptRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i]
		}
		count++
	}
	return s
}

// FinalTurnToolError reports the first CLI-reported tool_use_error in
// blocks, returning the failing tool's name and a bounded excerpt. A
// block qualifies iff IsError is set AND the content carries the
// toolUseErrorMarker wrapper — the marker is the stable signal the CLI
// uses for tool invocations it refused, blocked, or cancelled. Nonzero-
// exit Bash (e.g. `gh pr checks` exit 8) sets IsError but lacks the
// wrapper and must not trigger retry.
//
// On parallel batches, first qualifying block wins (first with both
// IsError and the marker).
func FinalTurnToolError(blocks []ContentBlock) (toolName, excerpt string, ok bool) {
	toolNames := make(map[string]string)
	for _, block := range blocks {
		if block.Type == ContentBlockTypeToolUse && block.ToolUseID != "" {
			toolNames[block.ToolUseID] = block.ToolName
		}
	}
	for _, block := range blocks {
		if block.Type != ContentBlockTypeToolResult || !block.IsError {
			continue
		}
		content := stringifyToolResult(block.ToolResult)
		if !strings.Contains(content, toolUseErrorMarker) {
			continue
		}
		name := toolNames[block.ToolUseID]
		if name == "" {
			name = "unknown"
		}
		return name, excerptRunes(content, toolErrorExcerptMaxRunes), true
	}
	return "", "", false
}

// TurnUsage contains token usage for a turn.
type TurnUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	CostUSD             float64
	ContextWindow       int // total context window size for the model
}

// Add accumulates other's counts into u. ContextWindow is not summed —
// callers set it explicitly from per-model usage metadata.
func (u *TurnUsage) Add(other TurnUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheCreationTokens += other.CacheCreationTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CostUSD += other.CostUSD
}

// TotalInputTokens returns the total input context size: fresh input tokens +
// cache creation tokens + cache read tokens. This represents the full context
// window utilization for the turn.
func (u TurnUsage) TotalInputTokens() int {
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens
}

// TurnResult contains the result of a completed turn.
type TurnResult struct {
	Error         error
	Text          string
	Thinking      string
	ContentBlocks []ContentBlock
	Usage         TurnUsage
	TurnNumber    int
	DurationMs    int64
	Success       bool
	// HasLiveBackgroundWork is a snapshot at finalize time: true when the
	// turn ended with registered live Monitor tasks or uncancelled bg-Bash
	// tools the agent parked on. Callers that would otherwise restart the
	// session (e.g. the jiradozer retry-on-tool-error loop) must skip that
	// action when this is true — re-Ask would interrupt the park and the
	// ephemeral session's defer Stop() would orphan the bg work.
	HasLiveBackgroundWork bool
}

// backgroundTools lists tools that the CLI treats as background work even
// though their tool_use.input does not carry `run_in_background: true`.
// They register with the CLI's task registry and emit
// task_started/task_updated/task_notification frames just like
// Bash(run_in_background:true), but the CLI does NOT auto-start a
// continuation assistant turn when they complete — wrappers must observe
// task_updated terminal status (or task_notification) to release suppression.
var backgroundTools = map[string]bool{
	"Monitor": true,
}

// scheduleWakeupToolName is the tool the Claude CLI uses for /loop dynamic
// pacing. When the agent calls this tool, the CLI schedules a future
// continuation turn after a delay. The wrapper must suppress turn completion
// and wait for the continuation, otherwise the session appears "done" and
// callers exit prematurely.
const scheduleWakeupToolName = "ScheduleWakeup"

// hasScheduleWakeup reports whether the turn's ContentBlocks contain a
// ScheduleWakeup tool_use. When present, the CLI will auto-inject a user
// message after the specified delay to start a continuation turn.
func (turn *turnState) hasScheduleWakeup() bool {
	if turn == nil {
		return false
	}
	for _, block := range turn.ContentBlocks {
		if block.Type == ContentBlockTypeToolUse && block.ToolName == scheduleWakeupToolName {
			return true
		}
	}
	return false
}

// scheduleWakeupDelaySeconds extracts the delaySeconds value from the first
// ScheduleWakeup tool_use in the turn. Returns 0 if not found or malformed.
// Accepts float64 (JSON default), int, and int64 to match the decoder
// tolerance used for other tool-input numerics in this package.
func (turn *turnState) scheduleWakeupDelaySeconds() float64 {
	if turn == nil {
		return 0
	}
	for _, block := range turn.ContentBlocks {
		if block.Type == ContentBlockTypeToolUse && block.ToolName == scheduleWakeupToolName {
			switch v := block.ToolInput["delaySeconds"].(type) {
			case float64:
				return v
			case int:
				return float64(v)
			case int64:
				return float64(v)
			}
		}
	}
	return 0
}

// turnState tracks the state of a single turn.
type turnState struct {
	StartTime              time.Time
	UserMessage            interface{}
	Tools                  map[string]*toolState
	liveTasks              map[string]struct{} // task IDs registered via task_started that have not reached a terminal state
	FullText               string
	FullThinking           string
	ContentBlocks          []ContentBlock
	tasksEverTracked       int  // counts task IDs ever added via TrackTask; never decremented
	hasContinuationBgTools bool // true if turn has any bg-Bash (run_in_background) tools that use continuation-ResultMessage release path
	Number                 int
}

// isBackgroundToolUse reports whether a tool_use block represents background
// work. A block is "background" if its input carries run_in_background:true
// (e.g. Bash(run_in_background)) OR its tool name is in backgroundTools
// (e.g. Monitor, which registers a task without the explicit flag).
func isBackgroundToolUse(block ContentBlock) bool {
	if block.Type != ContentBlockTypeToolUse {
		return false
	}
	if isBg, _ := block.ToolInput["run_in_background"].(bool); isBg {
		return true
	}
	return backgroundTools[block.ToolName]
}

// cancelledToolIDs returns the set of tool_use IDs whose tool_result was an
// error, meaning the tool invocation was cancelled before it ran.
func (turn *turnState) cancelledToolIDs() map[string]bool {
	cancelled := make(map[string]bool)
	for _, block := range turn.ContentBlocks {
		if block.Type == ContentBlockTypeToolResult && block.IsError {
			cancelled[block.ToolUseID] = true
		}
	}
	return cancelled
}

// shouldSuppressForBgTasks returns true when the turn has live background
// work and must not be finalized on the intermediate ResultMessage.
//
// The turn is "live" if:
//   - every non-cancelled tool_use is a background tool (run_in_background or
//     in backgroundTools), AND
//   - at least one such non-cancelled bg tool exists, OR the turn has
//     registered task IDs via task_started that are still running.
//
// When any non-bg tool is present, the ResultMessage represents completion
// of synchronous work and must not be suppressed.
func (turn *turnState) shouldSuppressForBgTasks() bool {
	if turn == nil {
		return false
	}

	cancelled := turn.cancelledToolIDs()

	// Non-bg tools always prevent suppression (even if they errored), because
	// their presence means the ResultMessage represents completion of synchronous
	// work. Cancelled bg tools are skipped — they never launched a background task.
	hasBgTool := false
	for _, block := range turn.ContentBlocks {
		if block.Type != ContentBlockTypeToolUse {
			continue
		}
		if !isBackgroundToolUse(block) {
			return false // non-bg tool → ResultMessage is real completion, never suppress
		}
		if !cancelled[block.ToolUseID] {
			hasBgTool = true
		}
	}
	// Either an uncancelled bg tool is present, or task_started has already
	// registered a live task for this turn (e.g. task_started arrived before
	// the bg tool's tool_use_id reached ContentBlocks). Both signal live work.
	return hasBgTool || len(turn.liveTasks) > 0
}

// longestBackgroundToolTimeoutMs returns the largest timeout_ms value across
// all non-cancelled background tool_use blocks in the turn. Returns 0 when
// no bg tool carries an explicit timeout. Used to size the suppression
// safety timer so it never releases before the agent's own deadline.
func (turn *turnState) longestBackgroundToolTimeoutMs() int64 {
	if turn == nil {
		return 0
	}
	cancelled := turn.cancelledToolIDs()
	var maxMs int64
	for _, block := range turn.ContentBlocks {
		if !isBackgroundToolUse(block) || cancelled[block.ToolUseID] {
			continue
		}
		raw, ok := block.ToolInput["timeout_ms"]
		if !ok {
			continue
		}
		var ms int64
		switch v := raw.(type) {
		case float64:
			ms = int64(v)
		case int:
			ms = int64(v)
		case int64:
			ms = v
		default:
			continue
		}
		if ms > maxMs {
			maxMs = ms
		}
	}
	return maxMs
}

// toolState tracks the state of a tool within a turn.
type toolState struct {
	StartTime    time.Time
	Input        map[string]interface{}
	ID           string
	Name         string
	PartialInput string
}

// turnManager manages turn state and completion tracking.
type turnManager struct {
	currentTurn       *turnState
	completionWaiters map[int][]chan *TurnResult
	completedResults  map[int]*TurnResult
	turns             []*turnState
	currentTurnNumber int
	mu                sync.RWMutex
}

// newTurnManager creates a new turn manager.
func newTurnManager() *turnManager {
	return &turnManager{
		turns:             make([]*turnState, 0),
		completionWaiters: make(map[int][]chan *TurnResult),
		completedResults:  make(map[int]*TurnResult),
	}
}

// StartTurn starts a new turn with the given user message.
func (tm *turnManager) StartTurn(userMessage interface{}) *turnState {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.currentTurnNumber++
	turn := &turnState{
		Number:      tm.currentTurnNumber,
		UserMessage: userMessage,
		StartTime:   time.Now(),
		Tools:       make(map[string]*toolState),
	}
	tm.currentTurn = turn
	tm.turns = append(tm.turns, turn)
	return turn
}

// CurrentTurnNumber returns the current turn number.
func (tm *turnManager) CurrentTurnNumber() int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.currentTurnNumber
}

// CurrentTurn returns the current turn state.
func (tm *turnManager) CurrentTurn() *turnState {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.currentTurn
}

// AppendText appends text to the current turn.
func (tm *turnManager) AppendText(text string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.currentTurn != nil {
		tm.currentTurn.FullText += text
		return tm.currentTurn.FullText
	}
	return ""
}

// AppendThinking appends thinking to the current turn.
func (tm *turnManager) AppendThinking(thinking string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.currentTurn != nil {
		tm.currentTurn.FullThinking += thinking
		return tm.currentTurn.FullThinking
	}
	return ""
}

// GetTool returns a tool state by ID, creating it if it doesn't exist.
func (tm *turnManager) GetOrCreateTool(id, name string) *toolState {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.currentTurn == nil {
		return nil
	}
	tool, exists := tm.currentTurn.Tools[id]
	if !exists {
		tool = &toolState{
			ID:        id,
			Name:      name,
			StartTime: time.Now(),
		}
		tm.currentTurn.Tools[id] = tool
	}
	return tool
}

// GetTool returns a tool state by ID.
func (tm *turnManager) GetTool(id string) *toolState {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.currentTurn == nil {
		return nil
	}
	return tm.currentTurn.Tools[id]
}

// FindToolByID searches all turns for a tool by ID.
func (tm *turnManager) FindToolByID(id string) *toolState {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	// Search from most recent to oldest
	for i := len(tm.turns) - 1; i >= 0; i-- {
		if tool, exists := tm.turns[i].Tools[id]; exists {
			return tool
		}
	}
	return nil
}

// WaitForTurn waits for a specific turn to complete.
// Safe to call even after the turn has already completed.
func (tm *turnManager) WaitForTurn(ctx context.Context, turnNumber int) (*TurnResult, error) {
	tm.mu.Lock()
	// Check if the turn already completed before registering a waiter.
	if result, ok := tm.completedResults[turnNumber]; ok {
		tm.mu.Unlock()
		if result.Error != nil {
			return nil, result.Error
		}
		return result, nil
	}
	// Create a channel to receive the result
	resultChan := make(chan *TurnResult, 1)
	tm.completionWaiters[turnNumber] = append(tm.completionWaiters[turnNumber], resultChan)
	tm.mu.Unlock()

	select {
	case result := <-resultChan:
		if result.Error != nil {
			return nil, result.Error
		}
		return result, nil
	case <-ctx.Done():
		// Remove the waiter on cancellation
		tm.mu.Lock()
		waiters := tm.completionWaiters[turnNumber]
		for i, ch := range waiters {
			if ch == resultChan {
				tm.completionWaiters[turnNumber] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
		tm.mu.Unlock()
		return nil, ctx.Err()
	}
}

// CompleteTurn marks a turn as complete and notifies waiters.
func (tm *turnManager) CompleteTurn(result TurnResult) {
	tm.mu.Lock()
	// Cache result so late callers to WaitForTurn see it immediately.
	tm.completedResults[result.TurnNumber] = &result
	waiters := tm.completionWaiters[result.TurnNumber]
	delete(tm.completionWaiters, result.TurnNumber)
	tm.mu.Unlock()

	// Notify all waiters
	for _, ch := range waiters {
		ch <- &result
		close(ch)
	}
}

// GetTurnHistory returns all turns.
func (tm *turnManager) GetTurnHistory() []*turnState {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	// Return a copy to prevent races
	result := make([]*turnState, len(tm.turns))
	copy(result, tm.turns)
	return result
}

// AppendContentBlock appends a content block to the current turn.
// When a bg tool_use that is not in backgroundTools (e.g. Bash with
// run_in_background) is appended, hasContinuationBgTools is set to true so
// AllTasksCompleted skips the fast path for mixed Monitor+bg-Bash turns.
func (tm *turnManager) AppendContentBlock(block ContentBlock) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.currentTurn == nil {
		return
	}
	tm.currentTurn.ContentBlocks = append(tm.currentTurn.ContentBlocks, block)
	if isBackgroundToolUse(block) && !backgroundTools[block.ToolName] {
		tm.currentTurn.hasContinuationBgTools = true
	}
}

// TrackTask records a task ID as live on the current turn. Called from
// task_started handling. No-op if there is no current turn.
func (tm *turnManager) TrackTask(taskID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.currentTurn == nil || taskID == "" {
		return
	}
	if tm.currentTurn.liveTasks == nil {
		tm.currentTurn.liveTasks = make(map[string]struct{})
	}
	tm.currentTurn.liveTasks[taskID] = struct{}{}
	tm.currentTurn.tasksEverTracked++
}

// UntrackTask removes a task ID from the current turn's live set.
// No-op if the task is not tracked or there is no current turn.
func (tm *turnManager) UntrackTask(taskID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.currentTurn == nil || taskID == "" {
		return
	}
	delete(tm.currentTurn.liveTasks, taskID)
}

// HasLiveTasks reports whether the current turn has any registered live
// tasks that have not yet reached a terminal state.
func (tm *turnManager) HasLiveTasks() bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.currentTurn == nil {
		return false
	}
	return len(tm.currentTurn.liveTasks) > 0
}

// AllTasksCompleted reports whether the current turn had tasks registered via
// task_started, all of them have since reached a terminal state, and the turn
// contains no uncancelled bg-Bash (continuation) tools.
//
// Background: Monitor tasks release suppression via terminal task events;
// bg-Bash (run_in_background) releases via a continuation ResultMessage. A
// turn that mixes both must NOT short-circuit on task completion alone —
// the Bash continuation must still arrive. This method returns false for
// mixed turns, deferring release to the continuation-ResultMessage path.
//
// Cancelled bg-Bash tools (IsError on their tool_result) never produce a
// continuation, so they are excluded from the check.
func (tm *turnManager) AllTasksCompleted() bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.currentTurn == nil {
		return false
	}
	if tm.currentTurn.tasksEverTracked == 0 || len(tm.currentTurn.liveTasks) != 0 {
		return false
	}
	if !tm.currentTurn.hasContinuationBgTools {
		return true
	}
	// hasContinuationBgTools is set conservatively on tool_use append. Check
	// whether any such tool is actually uncancelled (i.e. has a non-error
	// tool_result) — cancelled bg-Bash tools never produce a continuation.
	cancelled := tm.currentTurn.cancelledToolIDs()
	for _, block := range tm.currentTurn.ContentBlocks {
		if block.Type == ContentBlockTypeToolUse &&
			isBackgroundToolUse(block) &&
			!backgroundTools[block.ToolName] &&
			!cancelled[block.ToolUseID] {
			return false // uncancelled bg-Bash present, wait for continuation
		}
	}
	return true
}

// GetTurnByNumber returns a turn by its number.
func (tm *turnManager) GetTurnByNumber(n int) *turnState {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	for _, turn := range tm.turns {
		if turn.Number == n {
			return turn
		}
	}
	return nil
}

// GetCompletedResult returns the recorded TurnResult for a completed turn,
// or nil if the turn has not completed (or never existed). The result is
// what CompleteTurn stored, so Text/Thinking/ContentBlocks reflect the
// final turn state — including wakeup-suppression chains where the
// completion was emitted under the original suppressed turn number but
// populated from the last continuation's assistant response.
func (tm *turnManager) GetCompletedResult(n int) *TurnResult {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.completedResults[n]
}
