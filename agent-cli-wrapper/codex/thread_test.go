package codex

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNewThread(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{
		Model:   "gpt-4o",
		WorkDir: "/home/user",
	})

	if thread == nil {
		t.Fatal("newThread should return a thread")
	}
	if thread.id != "thread-123" {
		t.Errorf("unexpected id: %q", thread.id)
	}
	if thread.client != client {
		t.Error("client should be set")
	}
	if thread.state == nil {
		t.Error("state should be initialized")
	}
	if thread.turnWaiters == nil {
		t.Error("turnWaiters should be initialized")
	}
}

func TestThread_ID(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	if thread.ID() != "thread-123" {
		t.Errorf("unexpected ID: %q", thread.ID())
	}
}

func TestThread_State_Initial(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	if thread.State() != ThreadStateCreating {
		t.Errorf("expected creating state, got %v", thread.State())
	}
}

func TestThread_Info_Initial(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	if thread.Info() != nil {
		t.Error("Info should be nil initially")
	}
}

func TestThread_SetInfo(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	info := &ThreadInfo{
		ID:            "thread-123",
		Path:          "/path/to/thread",
		ModelProvider: "openai",
	}
	thread.setInfo(info)

	if thread.Info() != info {
		t.Error("Info should be set")
	}
	if thread.Info().ModelProvider != "openai" {
		t.Errorf("unexpected ModelProvider: %q", thread.Info().ModelProvider)
	}
}

func TestThread_CurrentTurnID_Initial(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	if thread.CurrentTurnID() != "" {
		t.Errorf("CurrentTurnID should be empty initially, got %q", thread.CurrentTurnID())
	}
}

func TestThread_GetFullText_Initial(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	if thread.GetFullText() != "" {
		t.Errorf("GetFullText should be empty initially")
	}
}

func TestThread_SendMessage_NotReady(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})
	ctx := context.Background()

	_, err := thread.SendMessage(ctx, "Hello")
	if err != ErrThreadNotReady {
		t.Errorf("expected ErrThreadNotReady, got %v", err)
	}
}

func TestThread_SendInput_NotReady(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})
	ctx := context.Background()

	input := []UserInput{{Type: "text", Text: "Hello"}}
	_, err := thread.SendInput(ctx, input)
	if err != ErrThreadNotReady {
		t.Errorf("expected ErrThreadNotReady, got %v", err)
	}
}

func TestThread_WaitForTurn_NoTurn(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})
	ctx := context.Background()

	_, err := thread.WaitForTurn(ctx)
	if err != ErrNoTurnInProgress {
		t.Errorf("expected ErrNoTurnInProgress, got %v", err)
	}
}

func TestThread_Interrupt_NoTurn(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})
	ctx := context.Background()

	err := thread.Interrupt(ctx)
	if err != ErrNoTurnInProgress {
		t.Errorf("expected ErrNoTurnInProgress, got %v", err)
	}
}

func TestThread_Close(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	err := thread.Close()
	if err != nil {
		t.Errorf("Close should not error: %v", err)
	}

	if thread.State() != ThreadStateClosed {
		t.Errorf("expected closed state, got %v", thread.State())
	}
}

func TestThread_SendMessage_Closed(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})
	ctx := context.Background()

	// Set ready first
	thread.state.SetReady()
	// Then close
	thread.Close()

	_, err := thread.SendMessage(ctx, "Hello")
	if err != ErrClientClosed {
		t.Errorf("expected ErrClientClosed, got %v", err)
	}
}

func TestThread_HandleTurnStarted(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	thread.handleTurnStarted("turn-456")

	if thread.CurrentTurnID() != "turn-456" {
		t.Errorf("unexpected CurrentTurnID: %q", thread.CurrentTurnID())
	}
}

func TestThread_HandleTextDelta(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	full := thread.handleTextDelta("turn-456", "item-789", "Hello ")
	if full != "Hello " {
		t.Errorf("expected 'Hello ', got %q", full)
	}

	full = thread.handleTextDelta("turn-456", "item-789", "World!")
	if full != "Hello World!" {
		t.Errorf("expected 'Hello World!', got %q", full)
	}

	if thread.GetFullText() != "Hello World!" {
		t.Errorf("expected full text 'Hello World!', got %q", thread.GetFullText())
	}
}

func TestThread_HandleTurnCompleted(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	// Set up turn
	thread.state.SetReady()
	thread.state.SetProcessing()
	thread.handleTurnStarted("turn-456")
	thread.handleTextDelta("turn-456", "item-789", "Response text")

	// Add a waiter
	waiterCh := make(chan *TurnResult, 1)
	thread.turnWaiters["turn-456"] = []chan *TurnResult{waiterCh}

	// Complete the turn
	thread.handleTurnCompleted("turn-456", true, "")

	// Check state transitioned back to ready
	if thread.State() != ThreadStateReady {
		t.Errorf("expected ready state after turn complete, got %v", thread.State())
	}

	// Check waiter received result
	select {
	case result := <-waiterCh:
		if result == nil {
			t.Fatal("expected result")
		}
		if result.TurnID != "turn-456" {
			t.Errorf("unexpected TurnID: %q", result.TurnID)
		}
		if !result.Success {
			t.Error("expected success")
		}
		if result.FullText != "Response text" {
			t.Errorf("unexpected FullText: %q", result.FullText)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("waiter did not receive result")
	}
}

func TestThread_HandleTurnCompleted_WithError(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	thread.state.SetReady()
	thread.state.SetProcessing()
	thread.handleTurnStarted("turn-456")

	waiterCh := make(chan *TurnResult, 1)
	thread.turnWaiters["turn-456"] = []chan *TurnResult{waiterCh}

	thread.handleTurnCompleted("turn-456", false, "Something went wrong")

	select {
	case result := <-waiterCh:
		if result.Success {
			t.Error("expected failure")
		}
		if result.Error == nil {
			t.Error("expected error")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("waiter did not receive result")
	}
}

func TestThread_SetReady(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	// Start from creating state
	thread.setReady()

	if thread.State() != ThreadStateReady {
		t.Errorf("expected ready state, got %v", thread.State())
	}
}

func TestThread_WaitForTurn_ContextCancellation(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	// Set up a turn in progress
	thread.currentTurnID = "turn-456"

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err = thread.WaitForTurn(ctx)
	}()

	// Give goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Cancel context
	cancel()

	wg.Wait()

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestThread_MultipleWaiters(t *testing.T) {
	client := NewClient()
	thread := newThread(client, "thread-123", ThreadConfig{})

	thread.state.SetReady()
	thread.state.SetProcessing()
	thread.handleTurnStarted("turn-456")

	// Add multiple waiters
	var wg sync.WaitGroup
	results := make([]*TurnResult, 3)
	errors := make([]error, 3)

	for i := 0; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Directly add waiter channel
			ch := make(chan *TurnResult, 1)
			thread.mu.Lock()
			thread.turnWaiters["turn-456"] = append(thread.turnWaiters["turn-456"], ch)
			thread.mu.Unlock()

			select {
			case r := <-ch:
				results[i] = r
			case <-time.After(time.Second):
				errors[i] = ErrTimeout
			}
		}()
	}

	// Give waiters time to register
	time.Sleep(20 * time.Millisecond)

	// Complete the turn
	thread.handleTurnCompleted("turn-456", true, "")

	wg.Wait()

	for i := 0; i < 3; i++ {
		if errors[i] != nil {
			t.Errorf("waiter %d got error: %v", i, errors[i])
		}
		if results[i] == nil {
			t.Errorf("waiter %d got nil result", i)
		} else if !results[i].Success {
			t.Errorf("waiter %d got unsuccessful result", i)
		}
	}
}

func TestTurnResult_Fields(t *testing.T) {
	result := &TurnResult{
		TurnID:     "turn-456",
		Success:    true,
		FullText:   "Response text",
		DurationMs: 1234,
		Usage: TurnUsage{
			InputTokens:  100,
			OutputTokens: 50,
		},
	}

	if result.TurnID != "turn-456" {
		t.Errorf("unexpected TurnID: %q", result.TurnID)
	}
	if !result.Success {
		t.Error("expected Success")
	}
	if result.FullText != "Response text" {
		t.Errorf("unexpected FullText: %q", result.FullText)
	}
	if result.DurationMs != 1234 {
		t.Errorf("unexpected DurationMs: %d", result.DurationMs)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("unexpected InputTokens: %d", result.Usage.InputTokens)
	}
}

func TestTurnResult_WithError(t *testing.T) {
	result := &TurnResult{
		TurnID:  "turn-456",
		Success: false,
		Error: &TurnError{
			ThreadID: "thread-123",
			TurnID:   "turn-456",
			Message:  "something went wrong",
		},
	}

	if result.Success {
		t.Error("expected failure")
	}
	if result.Error == nil {
		t.Error("expected error")
	}
}

func TestThread_LastUsage(t *testing.T) {
	thread := newThread(nil, "thread-123", ThreadConfig{})

	// Initially nil
	usage := thread.getAndClearLastUsage()
	if usage != nil {
		t.Error("expected nil usage initially")
	}

	// Set usage
	thread.setLastUsage(&TokenUsage{
		InputTokens:           100,
		OutputTokens:          50,
		CachedInputTokens:     80,
		ReasoningOutputTokens: 10,
		TotalTokens:           150,
	})

	// Get and clear returns the usage
	usage = thread.getAndClearLastUsage()
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if usage.InputTokens != 100 {
		t.Errorf("unexpected InputTokens: %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("unexpected OutputTokens: %d", usage.OutputTokens)
	}
	if usage.CachedInputTokens != 80 {
		t.Errorf("unexpected CachedInputTokens: %d", usage.CachedInputTokens)
	}
	if usage.ReasoningOutputTokens != 10 {
		t.Errorf("unexpected ReasoningOutputTokens: %d", usage.ReasoningOutputTokens)
	}
	if usage.TotalTokens != 150 {
		t.Errorf("unexpected TotalTokens: %d", usage.TotalTokens)
	}

	// After get and clear, should be nil again
	usage = thread.getAndClearLastUsage()
	if usage != nil {
		t.Error("expected nil usage after clear")
	}
}

func TestThread_LastUsage_Overwrite(t *testing.T) {
	thread := newThread(nil, "thread-123", ThreadConfig{})

	// Set first usage
	thread.setLastUsage(&TokenUsage{
		InputTokens:  100,
		OutputTokens: 50,
	})

	// Overwrite with second usage
	thread.setLastUsage(&TokenUsage{
		InputTokens:  200,
		OutputTokens: 100,
	})

	// Should get the second usage
	usage := thread.getAndClearLastUsage()
	if usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if usage.InputTokens != 200 {
		t.Errorf("expected InputTokens=200, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 100 {
		t.Errorf("expected OutputTokens=100, got %d", usage.OutputTokens)
	}
}
