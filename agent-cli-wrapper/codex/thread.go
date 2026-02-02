package codex

import (
	"context"
	"sync"
	"time"
)

// Thread represents an active conversation thread.
type Thread struct {
	client        *Client
	info          *ThreadInfo
	state         *threadStateManager
	accumulator   *threadAccumulator
	turnWaiters   map[string][]chan *TurnResult
	lastUsage     *TokenUsage // Token usage from last token_count event
	config        ThreadConfig
	turnStartTime time.Time
	id            string
	currentTurnID string
	mu            sync.RWMutex
}

func newThread(client *Client, id string, config ThreadConfig) *Thread {
	return &Thread{
		client:      client,
		id:          id,
		config:      config,
		state:       newThreadStateManager(),
		accumulator: &threadAccumulator{items: make(map[string]*itemAccumulator)},
		turnWaiters: make(map[string][]chan *TurnResult),
	}
}

// ID returns the thread ID.
func (t *Thread) ID() string {
	return t.id
}

// Info returns thread information.
func (t *Thread) Info() *ThreadInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.info
}

// State returns the current thread state.
func (t *Thread) State() ThreadState {
	return t.state.Current()
}

// WaitReady blocks until the thread is ready to receive messages.
// Returns nil if ready, error if closed or context cancelled.
func (t *Thread) WaitReady(ctx context.Context) error {
	return t.state.WaitForReady(ctx)
}

// SendMessage sends a user message and starts a new turn.
// Returns the turn ID.
func (t *Thread) SendMessage(ctx context.Context, content string, opts ...TurnOption) (string, error) {
	return t.SendInput(ctx, []UserInput{{Type: "text", Text: content}}, opts...)
}

// SendInput sends structured user input and starts a new turn.
// Returns the turn ID.
func (t *Thread) SendInput(ctx context.Context, input []UserInput, opts ...TurnOption) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Check state
	if t.state.IsClosed() {
		return "", ErrClientClosed
	}

	if !t.state.IsReady() {
		return "", ErrThreadNotReady
	}

	// Build turn config
	cfg := defaultTurnConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	// Build params
	params := TurnStartParams{
		ThreadID: t.id,
		Input:    input,
	}

	if cfg.ApprovalPolicy != "" {
		params.ApprovalPolicy = string(cfg.ApprovalPolicy)
	}
	if cfg.Model != "" {
		params.Model = cfg.Model
	}
	if cfg.Effort != "" {
		params.Effort = cfg.Effort
	}
	if cfg.Summary != "" {
		params.Summary = cfg.Summary
	}
	if cfg.OutputSchema != nil {
		params.OutputSchema = cfg.OutputSchema
	}
	if cfg.SandboxPolicy != nil {
		params.SandboxPolicy = cfg.SandboxPolicy
	}

	// Transition to processing
	if err := t.state.SetProcessing(); err != nil {
		return "", err
	}

	// Reset accumulator for new turn
	t.accumulator.reset()
	t.turnStartTime = time.Now()

	// Send request
	resp, err := t.client.sendRequestAndWait(ctx, "turn/start", params)
	if err != nil {
		_ = t.state.SetReady() // Revert state
		return "", err
	}

	// Parse response to get turn ID
	var turnResp TurnStartResponse
	if err := unmarshalRaw(resp.Result, &turnResp); err != nil {
		_ = t.state.SetReady()
		return "", &ProtocolError{Message: "failed to parse turn/start response", Cause: err}
	}

	t.currentTurnID = turnResp.Turn.ID
	return turnResp.Turn.ID, nil
}

// WaitForTurn blocks until the current turn completes.
func (t *Thread) WaitForTurn(ctx context.Context) (*TurnResult, error) {
	t.mu.Lock()
	turnID := t.currentTurnID
	if turnID == "" {
		t.mu.Unlock()
		return nil, ErrNoTurnInProgress
	}

	// Create waiter channel
	ch := make(chan *TurnResult, 1)
	t.turnWaiters[turnID] = append(t.turnWaiters[turnID], ch)
	t.mu.Unlock()

	// Wait for completion or context cancellation
	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		// Remove waiter
		t.mu.Lock()
		waiters := t.turnWaiters[turnID]
		for i, w := range waiters {
			if w == ch {
				t.turnWaiters[turnID] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
		t.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Ask sends a message and waits for turn completion (blocking).
// Returns the TurnResult containing the full response.
func (t *Thread) Ask(ctx context.Context, content string, opts ...TurnOption) (*TurnResult, error) {
	_, err := t.SendMessage(ctx, content, opts...)
	if err != nil {
		return nil, err
	}
	return t.WaitForTurn(ctx)
}

// AskWithTimeout is a convenience wrapper with timeout context.
func (t *Thread) AskWithTimeout(content string, timeout time.Duration, opts ...TurnOption) (*TurnResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return t.Ask(ctx, content, opts...)
}

// Interrupt sends an interrupt request for the current turn.
func (t *Thread) Interrupt(ctx context.Context) error {
	t.mu.RLock()
	if !t.state.IsProcessing() {
		t.mu.RUnlock()
		return ErrNoTurnInProgress
	}
	t.mu.RUnlock()

	params := TurnInterruptParams{
		ThreadID: t.id,
	}

	_, err := t.client.sendRequestAndWait(ctx, "turn/interrupt", params)
	return err
}

// CurrentTurnID returns the current turn ID.
func (t *Thread) CurrentTurnID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.currentTurnID
}

// GetFullText returns the accumulated response text for the current turn.
func (t *Thread) GetFullText() string {
	return t.accumulator.getFullText()
}

// Close closes this thread.
func (t *Thread) Close() error {
	t.state.SetClosed()
	return nil
}

// Internal methods called by Client

func (t *Thread) setInfo(info *ThreadInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.info = info
}

func (t *Thread) handleTurnStarted(turnID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentTurnID = turnID
	t.turnStartTime = time.Now()
	t.accumulator.setTurnID(turnID)
}

func (t *Thread) handleTextDelta(turnID, itemID, delta string) string {
	return t.accumulator.handleDelta(turnID, itemID, delta)
}

func (t *Thread) handleTurnCompleted(turnID string, success bool, errMsg string) {
	t.mu.Lock()

	// Calculate duration
	durationMs := time.Since(t.turnStartTime).Milliseconds()

	// Build result
	result := &TurnResult{
		TurnID:     turnID,
		Success:    success,
		FullText:   t.accumulator.getFullText(),
		DurationMs: durationMs,
	}

	if errMsg != "" {
		result.Error = &TurnError{
			ThreadID: t.id,
			TurnID:   turnID,
			Message:  errMsg,
		}
	}

	// Transition state back to ready
	_ = t.state.SetReady()

	// Notify all waiters
	if waiters, ok := t.turnWaiters[turnID]; ok {
		for _, ch := range waiters {
			select {
			case ch <- result:
			default:
			}
			close(ch)
		}
		delete(t.turnWaiters, turnID)
	}

	t.mu.Unlock()
}

func (t *Thread) setReady() {
	_ = t.state.SetReady()
}

func (t *Thread) setError(err error) {
	t.state.SetError(err)
}

// setLastUsage updates the last token usage for this thread.
func (t *Thread) setLastUsage(usage *TokenUsage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastUsage = usage
}

// getAndClearLastUsage returns the last token usage and clears it.
func (t *Thread) getAndClearLastUsage() *TokenUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	usage := t.lastUsage
	t.lastUsage = nil
	return usage
}

// TurnResult contains the result of a completed turn.
type TurnResult struct {
	Error      error
	TurnID     string
	FullText   string
	Usage      TurnUsage
	DurationMs int64
	Success    bool
}
