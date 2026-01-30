package claude

import (
	"context"
	"sync"
	"time"
)

// TurnUsage contains token usage for a turn.
type TurnUsage struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	CostUSD         float64
}

// TurnResult contains the result of a completed turn.
type TurnResult struct {
	Error      error
	Text       string
	Thinking   string
	Usage      TurnUsage
	TurnNumber int
	DurationMs int64
	Success    bool
}

// turnState tracks the state of a single turn.
type turnState struct {
	StartTime    time.Time
	UserMessage  interface{}
	Tools        map[string]*toolState
	FullText     string
	FullThinking string
	Number       int
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
	turns             []*turnState
	currentTurnNumber int
	mu                sync.RWMutex
}

// newTurnManager creates a new turn manager.
func newTurnManager() *turnManager {
	return &turnManager{
		turns:             make([]*turnState, 0),
		completionWaiters: make(map[int][]chan *TurnResult),
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
func (tm *turnManager) WaitForTurn(ctx context.Context, turnNumber int) (*TurnResult, error) {
	// Create a channel to receive the result
	resultChan := make(chan *TurnResult, 1)

	tm.mu.Lock()
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
