package codex

import "sync"

// streamAccumulator accumulates streaming text deltas into full responses.
type streamAccumulator struct {
	threads map[string]*threadAccumulator
	mu      sync.Mutex
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{
		threads: make(map[string]*threadAccumulator),
	}
}

// getOrCreateThread gets or creates a thread accumulator.
func (sa *streamAccumulator) getOrCreateThread(threadID string) *threadAccumulator {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	ta, ok := sa.threads[threadID]
	if !ok {
		ta = &threadAccumulator{
			items: make(map[string]*itemAccumulator),
		}
		sa.threads[threadID] = ta
	}
	return ta
}

// HandleDelta handles a text delta and returns the accumulated full text.
func (sa *streamAccumulator) HandleDelta(threadID, turnID, itemID, delta string) string {
	ta := sa.getOrCreateThread(threadID)
	return ta.handleDelta(turnID, itemID, delta)
}

// GetFullText returns the accumulated full text for a thread.
func (sa *streamAccumulator) GetFullText(threadID string) string {
	ta := sa.getOrCreateThread(threadID)
	return ta.getFullText()
}

// Reset resets the accumulator for a thread (typically at turn start).
func (sa *streamAccumulator) Reset(threadID string) {
	sa.mu.Lock()
	defer sa.mu.Unlock()

	if ta, ok := sa.threads[threadID]; ok {
		ta.reset()
	}
}

// SetTurnID sets the current turn ID for a thread.
func (sa *streamAccumulator) SetTurnID(threadID, turnID string) {
	ta := sa.getOrCreateThread(threadID)
	ta.setTurnID(turnID)
}

// RemoveThread removes a thread accumulator.
func (sa *streamAccumulator) RemoveThread(threadID string) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	delete(sa.threads, threadID)
}

// threadAccumulator accumulates data for a single thread.
type threadAccumulator struct {
	items         map[string]*itemAccumulator
	currentTurnID string
	fullText      string
	mu            sync.Mutex
}

func (ta *threadAccumulator) handleDelta(turnID, itemID, delta string) string {
	ta.mu.Lock()
	defer ta.mu.Unlock()

	// If this is a new turn, reset
	if turnID != ta.currentTurnID {
		ta.currentTurnID = turnID
		ta.fullText = ""
		ta.items = make(map[string]*itemAccumulator)
	}

	// Accumulate into full text
	ta.fullText += delta

	// Also track per-item
	ia, ok := ta.items[itemID]
	if !ok {
		ia = &itemAccumulator{}
		ta.items[itemID] = ia
	}
	ia.text += delta

	return ta.fullText
}

func (ta *threadAccumulator) getFullText() string {
	ta.mu.Lock()
	defer ta.mu.Unlock()
	return ta.fullText
}

func (ta *threadAccumulator) reset() {
	ta.mu.Lock()
	defer ta.mu.Unlock()
	ta.fullText = ""
	ta.items = make(map[string]*itemAccumulator)
}

func (ta *threadAccumulator) setTurnID(turnID string) {
	ta.mu.Lock()
	defer ta.mu.Unlock()
	if turnID != ta.currentTurnID {
		ta.currentTurnID = turnID
		ta.fullText = ""
		ta.items = make(map[string]*itemAccumulator)
	}
}

// itemAccumulator accumulates data for a single item.
type itemAccumulator struct {
	text string
}
