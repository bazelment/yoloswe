package codex

import (
	"sync"
	"testing"
)

func TestStreamAccumulator_HandleDelta(t *testing.T) {
	sa := newStreamAccumulator()

	// First delta
	full1 := sa.HandleDelta("thread1", "turn1", "item1", "Hello ")
	if full1 != "Hello " {
		t.Errorf("expected 'Hello ', got %q", full1)
	}

	// Second delta
	full2 := sa.HandleDelta("thread1", "turn1", "item1", "World!")
	if full2 != "Hello World!" {
		t.Errorf("expected 'Hello World!', got %q", full2)
	}

	// Get full text
	fullText := sa.GetFullText("thread1")
	if fullText != "Hello World!" {
		t.Errorf("expected 'Hello World!', got %q", fullText)
	}
}

func TestStreamAccumulator_MultipleThreads(t *testing.T) {
	sa := newStreamAccumulator()

	// Thread 1
	sa.HandleDelta("thread1", "turn1", "item1", "Thread 1 text")

	// Thread 2
	sa.HandleDelta("thread2", "turn1", "item1", "Thread 2 text")

	// Verify isolation
	if sa.GetFullText("thread1") != "Thread 1 text" {
		t.Errorf("thread1 text mismatch")
	}
	if sa.GetFullText("thread2") != "Thread 2 text" {
		t.Errorf("thread2 text mismatch")
	}
}

func TestStreamAccumulator_NewTurnResets(t *testing.T) {
	sa := newStreamAccumulator()

	// Turn 1
	sa.HandleDelta("thread1", "turn1", "item1", "Turn 1 text")
	if sa.GetFullText("thread1") != "Turn 1 text" {
		t.Errorf("turn 1 text mismatch")
	}

	// Turn 2 - should reset
	sa.HandleDelta("thread1", "turn2", "item1", "Turn 2 text")
	if sa.GetFullText("thread1") != "Turn 2 text" {
		t.Errorf("expected text to reset for new turn, got %q", sa.GetFullText("thread1"))
	}
}

func TestStreamAccumulator_Reset(t *testing.T) {
	sa := newStreamAccumulator()

	sa.HandleDelta("thread1", "turn1", "item1", "Some text")
	if sa.GetFullText("thread1") != "Some text" {
		t.Errorf("text should be accumulated")
	}

	sa.Reset("thread1")
	if sa.GetFullText("thread1") != "" {
		t.Errorf("expected empty text after reset, got %q", sa.GetFullText("thread1"))
	}
}

func TestStreamAccumulator_SetTurnID(t *testing.T) {
	sa := newStreamAccumulator()

	// Accumulate for turn1
	sa.HandleDelta("thread1", "turn1", "item1", "Turn 1 text")

	// Set turn ID to turn2 - should reset
	sa.SetTurnID("thread1", "turn2")
	if sa.GetFullText("thread1") != "" {
		t.Errorf("expected empty text after SetTurnID, got %q", sa.GetFullText("thread1"))
	}
}

func TestStreamAccumulator_RemoveThread(t *testing.T) {
	sa := newStreamAccumulator()

	sa.HandleDelta("thread1", "turn1", "item1", "Some text")
	sa.RemoveThread("thread1")

	// Should get empty text for removed thread
	if sa.GetFullText("thread1") != "" {
		t.Errorf("expected empty text for removed thread")
	}
}

func TestStreamAccumulator_EmptyThread(t *testing.T) {
	sa := newStreamAccumulator()

	// Get full text for non-existent thread
	fullText := sa.GetFullText("nonexistent")
	if fullText != "" {
		t.Errorf("expected empty text for non-existent thread, got %q", fullText)
	}
}

func TestStreamAccumulator_MultipleItems(t *testing.T) {
	sa := newStreamAccumulator()

	// Multiple items in same turn - all should accumulate to full text
	sa.HandleDelta("thread1", "turn1", "item1", "Item 1 ")
	sa.HandleDelta("thread1", "turn1", "item2", "Item 2")

	// Full text should contain both
	fullText := sa.GetFullText("thread1")
	if fullText != "Item 1 Item 2" {
		t.Errorf("expected 'Item 1 Item 2', got %q", fullText)
	}
}

func TestStreamAccumulator_Concurrent(t *testing.T) {
	sa := newStreamAccumulator()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			threadID := "thread1"
			if n%2 == 0 {
				threadID = "thread2"
			}
			sa.HandleDelta(threadID, "turn1", "item1", "x")
			_ = sa.GetFullText(threadID)
		}(i)
	}
	wg.Wait()

	// Both threads should have accumulated text
	if sa.GetFullText("thread1") == "" {
		t.Error("thread1 should have text")
	}
	if sa.GetFullText("thread2") == "" {
		t.Error("thread2 should have text")
	}
}

func TestThreadAccumulator_HandleDelta(t *testing.T) {
	ta := &threadAccumulator{
		items: make(map[string]*itemAccumulator),
	}

	// First delta
	full1 := ta.handleDelta("turn1", "item1", "Hello ")
	if full1 != "Hello " {
		t.Errorf("expected 'Hello ', got %q", full1)
	}

	// Second delta
	full2 := ta.handleDelta("turn1", "item1", "World!")
	if full2 != "Hello World!" {
		t.Errorf("expected 'Hello World!', got %q", full2)
	}
}

func TestThreadAccumulator_Reset(t *testing.T) {
	ta := &threadAccumulator{
		currentTurnID: "turn1",
		fullText:      "some text",
		items:         map[string]*itemAccumulator{"item1": {text: "item text"}},
	}

	ta.reset()

	if ta.fullText != "" {
		t.Errorf("expected empty fullText after reset")
	}
	if len(ta.items) != 0 {
		t.Errorf("expected empty items after reset")
	}
}
