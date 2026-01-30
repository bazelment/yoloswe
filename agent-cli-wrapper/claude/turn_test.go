package claude

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTurnManager_StartTurn(t *testing.T) {
	tm := newTurnManager()

	// Start first turn
	turn1 := tm.StartTurn("Hello")
	if turn1.Number != 1 {
		t.Errorf("expected turn number 1, got %d", turn1.Number)
	}
	if turn1.UserMessage != "Hello" {
		t.Errorf("expected user message 'Hello', got %v", turn1.UserMessage)
	}

	// Start second turn
	turn2 := tm.StartTurn("World")
	if turn2.Number != 2 {
		t.Errorf("expected turn number 2, got %d", turn2.Number)
	}

	// Current turn should be turn2
	if tm.CurrentTurnNumber() != 2 {
		t.Errorf("expected current turn number 2, got %d", tm.CurrentTurnNumber())
	}
}

func TestTurnManager_AppendText(t *testing.T) {
	tm := newTurnManager()
	tm.StartTurn("Test")

	// Append text
	result := tm.AppendText("Hello ")
	if result != "Hello " {
		t.Errorf("expected 'Hello ', got %q", result)
	}

	result = tm.AppendText("World")
	if result != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", result)
	}

	// Verify turn state
	turn := tm.CurrentTurn()
	if turn.FullText != "Hello World" {
		t.Errorf("expected full text 'Hello World', got %q", turn.FullText)
	}
}

func TestTurnManager_AppendThinking(t *testing.T) {
	tm := newTurnManager()
	tm.StartTurn("Test")

	// Append thinking
	result := tm.AppendThinking("Thinking ")
	if result != "Thinking " {
		t.Errorf("expected 'Thinking ', got %q", result)
	}

	result = tm.AppendThinking("more...")
	if result != "Thinking more..." {
		t.Errorf("expected 'Thinking more...', got %q", result)
	}

	// Verify turn state
	turn := tm.CurrentTurn()
	if turn.FullThinking != "Thinking more..." {
		t.Errorf("expected full thinking 'Thinking more...', got %q", turn.FullThinking)
	}
}

func TestTurnManager_NoTurn(t *testing.T) {
	tm := newTurnManager()

	// AppendText with no turn should return empty
	result := tm.AppendText("Hello")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}

	// AppendThinking with no turn should return empty
	result = tm.AppendThinking("Thinking")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}

	// GetOrCreateTool with no turn should return nil
	tool := tm.GetOrCreateTool("tool-1", "TestTool")
	if tool != nil {
		t.Errorf("expected nil tool, got %v", tool)
	}

	// GetTool with no turn should return nil
	tool = tm.GetTool("tool-1")
	if tool != nil {
		t.Errorf("expected nil tool, got %v", tool)
	}
}

func TestTurnManager_Tools(t *testing.T) {
	tm := newTurnManager()
	tm.StartTurn("Test")

	// Create a tool
	tool := tm.GetOrCreateTool("tool-1", "WebSearch")
	if tool == nil {
		t.Fatal("expected tool to be created")
	}
	if tool.ID != "tool-1" {
		t.Errorf("expected tool ID 'tool-1', got %q", tool.ID)
	}
	if tool.Name != "WebSearch" {
		t.Errorf("expected tool name 'WebSearch', got %q", tool.Name)
	}

	// Get the same tool again should return the same instance
	tool2 := tm.GetOrCreateTool("tool-1", "WebSearch")
	if tool != tool2 {
		t.Error("expected same tool instance")
	}

	// Modify tool state
	tool.PartialInput = `{"query": "test"`
	tool.Input = map[string]interface{}{"query": "test"}

	// GetTool should return the tool with state
	tool3 := tm.GetTool("tool-1")
	if tool3 == nil {
		t.Fatal("expected tool to be found")
	}
	if tool3.PartialInput != `{"query": "test"` {
		t.Errorf("expected partial input, got %q", tool3.PartialInput)
	}

	// GetTool for unknown ID should return nil
	unknown := tm.GetTool("unknown-id")
	if unknown != nil {
		t.Errorf("expected nil for unknown tool, got %v", unknown)
	}
}

func TestTurnManager_FindToolByID(t *testing.T) {
	tm := newTurnManager()

	// Create tools across multiple turns
	tm.StartTurn("Turn 1")
	tool1 := tm.GetOrCreateTool("tool-1", "Read")

	tm.StartTurn("Turn 2")
	tool2 := tm.GetOrCreateTool("tool-2", "Write")

	// FindToolByID should find tools from any turn
	found1 := tm.FindToolByID("tool-1")
	if found1 == nil || found1 != tool1 {
		t.Error("expected to find tool-1")
	}

	found2 := tm.FindToolByID("tool-2")
	if found2 == nil || found2 != tool2 {
		t.Error("expected to find tool-2")
	}

	// Unknown tool should return nil
	unknown := tm.FindToolByID("unknown")
	if unknown != nil {
		t.Error("expected nil for unknown tool")
	}
}

func TestTurnManager_WaitForTurn(t *testing.T) {
	tm := newTurnManager()
	tm.StartTurn("Test")

	// Start waiting in goroutine
	var wg sync.WaitGroup
	var result *TurnResult
	var err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err = tm.WaitForTurn(context.Background(), 1)
	}()

	// Give goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Complete the turn
	tm.CompleteTurn(TurnResult{
		TurnNumber: 1,
		Success:    true,
		DurationMs: 100,
		Usage: TurnUsage{
			InputTokens:  10,
			OutputTokens: 20,
			CostUSD:      0.001,
		},
	})

	// Wait for result
	wg.Wait()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if !result.Success {
		t.Error("expected success")
	}
	if result.TurnNumber != 1 {
		t.Errorf("expected turn number 1, got %d", result.TurnNumber)
	}
	if result.Usage.CostUSD != 0.001 {
		t.Errorf("expected cost 0.001, got %f", result.Usage.CostUSD)
	}
}

func TestTurnManager_WaitForTurn_Cancelled(t *testing.T) {
	tm := newTurnManager()
	tm.StartTurn("Test")

	ctx, cancel := context.WithCancel(context.Background())

	// Start waiting in goroutine
	var wg sync.WaitGroup
	var result *TurnResult
	var err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err = tm.WaitForTurn(ctx, 1)
	}()

	// Give goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Cancel context
	cancel()

	// Wait for result
	wg.Wait()

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if result != nil {
		t.Error("expected nil result on cancellation")
	}
}

func TestTurnManager_WaitForTurn_WithError(t *testing.T) {
	tm := newTurnManager()
	tm.StartTurn("Test")

	// Start waiting in goroutine
	var wg sync.WaitGroup
	var result *TurnResult
	var err error

	wg.Add(1)
	go func() {
		defer wg.Done()
		result, err = tm.WaitForTurn(context.Background(), 1)
	}()

	// Give goroutine time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Complete the turn with an error
	turnErr := &ProtocolError{Message: "test error"}
	tm.CompleteTurn(TurnResult{
		TurnNumber: 1,
		Success:    false,
		Error:      turnErr,
	})

	// Wait for result
	wg.Wait()

	if err == nil {
		t.Fatal("expected error")
	}
	if result != nil {
		t.Error("expected nil result on error")
	}
}

func TestTurnManager_MultipleWaiters(t *testing.T) {
	tm := newTurnManager()
	tm.StartTurn("Test")

	// Start multiple waiters
	var wg sync.WaitGroup
	results := make([]*TurnResult, 3)
	errors := make([]error, 3)

	for i := 0; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errors[i] = tm.WaitForTurn(context.Background(), 1)
		}()
	}

	// Give goroutines time to start waiting
	time.Sleep(10 * time.Millisecond)

	// Complete the turn
	tm.CompleteTurn(TurnResult{
		TurnNumber: 1,
		Success:    true,
		DurationMs: 100,
	})

	// Wait for all
	wg.Wait()

	// All should receive the result
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

func TestTurnManager_GetTurnHistory(t *testing.T) {
	tm := newTurnManager()

	// Create multiple turns
	tm.StartTurn("Turn 1")
	tm.StartTurn("Turn 2")
	tm.StartTurn("Turn 3")

	history := tm.GetTurnHistory()
	if len(history) != 3 {
		t.Errorf("expected 3 turns in history, got %d", len(history))
	}

	for i, turn := range history {
		expectedNumber := i + 1
		if turn.Number != expectedNumber {
			t.Errorf("turn %d: expected number %d, got %d", i, expectedNumber, turn.Number)
		}
	}

	// Verify the returned slice is a copy (doesn't affect internal state)
	history[0] = nil
	internalHistory := tm.GetTurnHistory()
	if internalHistory[0] == nil {
		t.Error("GetTurnHistory should return a copy")
	}
}

func TestTurnManager_Concurrent(t *testing.T) {
	tm := newTurnManager()

	// Test concurrent access
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tm.StartTurn("concurrent turn")
			tm.AppendText("text")
			tm.AppendThinking("thinking")
			tm.GetOrCreateTool("tool", "Test")
			_ = tm.CurrentTurnNumber()
			_ = tm.CurrentTurn()
			_ = tm.GetTurnHistory()
		}(i)
	}
	wg.Wait()

	// Should complete without race conditions
	// (run with -race flag to verify)
}
