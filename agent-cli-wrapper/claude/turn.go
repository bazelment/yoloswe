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

// FinalTurnToolError reports a CLI-reported tool_use_error only when it
// is the LAST tool_result in the turn. A block qualifies iff IsError is
// set AND the content carries the toolUseErrorMarker wrapper — the marker
// is the stable signal the CLI uses for tool invocations it refused,
// blocked, or cancelled. Nonzero-exit Bash (e.g. `gh pr checks` exit 8)
// sets IsError but lacks the wrapper and must not trigger retry.
//
// Walking in reverse catches the last tool_result first. If the agent
// recovered from a transient error (e.g. "File has not been read yet"
// followed by a successful Read + Edit), the last tool_result is a
// success, so ok=false — the turn is not retried.
//
// On parallel batches where the errored group is the final group, the
// cancelled sibling (carrying the marker) is still the last tool_result
// in forward order, so it remains detectable.
func FinalTurnToolError(blocks []ContentBlock) (toolName, excerpt string, ok bool) {
	for i := len(blocks) - 1; i >= 0; i-- {
		block := blocks[i]
		if block.Type != ContentBlockTypeToolResult {
			continue
		}
		if !block.IsError {
			return "", "", false
		}
		content := stringifyToolResult(block.ToolResult)
		if !strings.Contains(content, toolUseErrorMarker) {
			return "", "", false
		}
		// Build tool name map only on the error path (uncommon case).
		toolNames := make(map[string]string)
		for _, b := range blocks {
			if b.Type == ContentBlockTypeToolUse && b.ToolUseID != "" {
				toolNames[b.ToolUseID] = b.ToolName
			}
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
}

// scheduleWakeupToolName is the tool the Claude CLI uses for /loop dynamic
// pacing. When the agent calls this tool, the CLI schedules a future
// continuation turn after a delay. The wrapper must suppress turn completion
// and wait for the continuation, otherwise the session appears "done" and
// callers exit prematurely.
const scheduleWakeupToolName = "ScheduleWakeup"

// latestScheduleWakeup returns the last ScheduleWakeup tool_use block in the
// turn (in arrival order), or nil if none. For chained wakeups a continuation
// appends a new block; the safety timer should read from the newest.
func (turn *turnState) latestScheduleWakeup() *ContentBlock {
	if turn == nil {
		return nil
	}
	for i := len(turn.ContentBlocks) - 1; i >= 0; i-- {
		block := &turn.ContentBlocks[i]
		if block.Type == ContentBlockTypeToolUse && block.ToolName == scheduleWakeupToolName {
			return block
		}
	}
	return nil
}

func (turn *turnState) latestScheduleWakeupToolID() string {
	if b := turn.latestScheduleWakeup(); b != nil {
		return b.ToolUseID
	}
	return ""
}

func (turn *turnState) latestScheduleWakeupDelaySeconds() float64 {
	b := turn.latestScheduleWakeup()
	if b == nil {
		return 0
	}
	switch v := b.ToolInput["delaySeconds"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// turnState tracks the state of a single turn.
type turnState struct {
	StartTime     time.Time
	UserMessage   interface{}
	Tools         map[string]*toolState
	FullText      string
	FullThinking  string
	ContentBlocks []ContentBlock
	Number        int
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
func (tm *turnManager) AppendContentBlock(block ContentBlock) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.currentTurn == nil {
		return
	}
	tm.currentTurn.ContentBlocks = append(tm.currentTurn.ContentBlocks, block)
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
