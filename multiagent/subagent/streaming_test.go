package subagent

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

func TestNewStreamingSubAgent(t *testing.T) {
	config := agent.AgentConfig{
		Model:   "sonnet",
		WorkDir: "/tmp/test",
	}

	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeDesigner)

	if sub.requestID != "req-1" {
		t.Errorf("expected requestID 'req-1', got %q", sub.requestID)
	}
	if sub.agentType != AgentTypeDesigner {
		t.Errorf("expected agentType %q, got %q", AgentTypeDesigner, sub.agentType)
	}
	if sub.sessionID != "session-1" {
		t.Errorf("expected sessionID 'session-1', got %q", sub.sessionID)
	}
}

func TestStreamingSubAgent_Cancel(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	// Initially not cancelled
	if sub.IsCancelled() {
		t.Error("expected sub-agent to not be cancelled initially")
	}

	// Cancel
	sub.Cancel("user requested")

	if !sub.IsCancelled() {
		t.Error("expected sub-agent to be cancelled after Cancel()")
	}

	// Verify error event was emitted
	select {
	case event := <-sub.Events():
		errEvent, ok := event.(*Error)
		if !ok {
			t.Fatalf("expected Error event, got %T", event)
		}
		if errEvent.Code != "cancelled" {
			t.Errorf("expected code 'cancelled', got %q", errEvent.Code)
		}
		if errEvent.Message != "user requested" {
			t.Errorf("expected message 'user requested', got %q", errEvent.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected error event after cancel")
	}
}

func TestStreamingSubAgent_CancelIdempotent(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeReviewer)

	// Cancel multiple times - should not panic
	sub.Cancel("first cancel")
	sub.Cancel("second cancel")
	sub.Cancel("third cancel")

	// Only one error event should be emitted
	eventCount := 0
	for {
		select {
		case <-sub.Events():
			eventCount++
		case <-time.After(50 * time.Millisecond):
			goto done
		}
	}
done:
	if eventCount != 1 {
		t.Errorf("expected 1 error event, got %d", eventCount)
	}
}

func TestStreamingSubAgent_Accessors(t *testing.T) {
	config := agent.AgentConfig{Model: "haiku"}
	sub := NewStreamingSubAgent(config, "session-123", "req-456", AgentTypeDesigner)

	if sub.RequestID() != "req-456" {
		t.Errorf("expected RequestID 'req-456', got %q", sub.RequestID())
	}
	if sub.AgentType() != AgentTypeDesigner {
		t.Errorf("expected AgentType %q, got %q", AgentTypeDesigner, sub.AgentType())
	}
	if sub.TotalCost() != 0 {
		t.Errorf("expected initial TotalCost 0, got %f", sub.TotalCost())
	}
	if sub.FullText() != "" {
		t.Errorf("expected initial FullText empty, got %q", sub.FullText())
	}
	if len(sub.FilesCreated()) != 0 {
		t.Errorf("expected initial FilesCreated empty, got %v", sub.FilesCreated())
	}
	if len(sub.FilesModified()) != 0 {
		t.Errorf("expected initial FilesModified empty, got %v", sub.FilesModified())
	}
}

func TestStreamingSubAgent_TrackFileOperation(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	tests := []struct {
		name         string
		toolName     string
		input        map[string]interface{}
		wantAction   FileAction
		wantCreated  int
		wantModified int
	}{
		{
			name:         "Write creates file",
			toolName:     "Write",
			input:        map[string]interface{}{"file_path": "/tmp/new.go"},
			wantAction:   FileActionCreate,
			wantCreated:  1,
			wantModified: 0,
		},
		{
			name:         "Edit modifies file",
			toolName:     "Edit",
			input:        map[string]interface{}{"file_path": "/tmp/existing.go"},
			wantAction:   FileActionModify,
			wantCreated:  1, // from previous test
			wantModified: 1,
		},
		{
			name:         "Read doesn't track",
			toolName:     "Read",
			input:        map[string]interface{}{"file_path": "/tmp/read.go"},
			wantAction:   FileActionRead,
			wantCreated:  1,
			wantModified: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub.trackFileOperation(tt.toolName, tt.input)

			// Drain the file event
			select {
			case event := <-sub.Events():
				fileEvent, ok := event.(*FileEvent)
				if !ok {
					t.Fatalf("expected FileEvent, got %T", event)
				}
				if fileEvent.Action != tt.wantAction {
					t.Errorf("expected action %q, got %q", tt.wantAction, fileEvent.Action)
				}
			case <-time.After(50 * time.Millisecond):
				if tt.wantAction != FileActionRead { // Read events are emitted but don't track
					t.Error("expected file event")
				}
			}

			if len(sub.FilesCreated()) != tt.wantCreated {
				t.Errorf("expected %d files created, got %d", tt.wantCreated, len(sub.FilesCreated()))
			}
			if len(sub.FilesModified()) != tt.wantModified {
				t.Errorf("expected %d files modified, got %d", tt.wantModified, len(sub.FilesModified()))
			}
		})
	}
}

func TestStreamingSubAgent_TrackFileOperation_EmptyPath(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	// Empty path should not track
	sub.trackFileOperation("Write", map[string]interface{}{"file_path": ""})

	// No event should be emitted
	select {
	case <-sub.Events():
		t.Error("unexpected event for empty path")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}

	if len(sub.FilesCreated()) != 0 {
		t.Errorf("expected no files created for empty path")
	}
}

func TestStreamingSubAgent_TrackFileOperation_UnknownTool(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	// Unknown tool should not track
	sub.trackFileOperation("Bash", map[string]interface{}{"command": "ls"})
	sub.trackFileOperation("Grep", map[string]interface{}{"pattern": "test"})

	// No events should be emitted
	select {
	case <-sub.Events():
		t.Error("unexpected event for unknown tool")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestStreamingSubAgent_EmitAfterClose(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	// Close eventsDone (simulating goroutine finished) then close events
	close(sub.eventsDone)
	sub.closeEventsDirect()

	// emitEvent after close should not panic
	sub.emitEvent(NewProgress("req-1", AgentTypeBuilder, PhaseStreaming))
	sub.emitError("test", "test error", false)

	// Multiple close calls should not panic
	sub.closeEventsDirect()
	sub.closeEventsDirect()
}

func TestStreamingSubAgent_CancelAfterClose(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	// Close eventsDone and events first
	close(sub.eventsDone)
	sub.closeEventsDirect()

	// Cancel after close should not panic (event won't be sent)
	sub.Cancel("late cancel")

	// Should still be marked as cancelled
	if !sub.IsCancelled() {
		t.Error("expected sub-agent to be cancelled")
	}
}

func TestStreamingSubAgent_ConcurrentEmit(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	// Start consumer
	done := make(chan struct{})
	eventCount := 0
	go func() {
		defer close(done)
		for range sub.Events() {
			eventCount++
		}
	}()

	// Concurrent emits from multiple goroutines
	const numGoroutines = 10
	const eventsPerGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < eventsPerGoroutine; j++ {
				sub.emitEvent(NewProgress("req-1", AgentTypeBuilder, PhaseStreaming))
			}
		}()
	}
	wg.Wait()

	// Close events and wait for consumer
	close(sub.eventsDone)
	sub.closeEventsDirect()
	<-done

	// Some events may be dropped if channel is full, but no panics
	if eventCount == 0 {
		t.Error("expected at least some events to be received")
	}
}

func TestStreamingSubAgent_ConcurrentFileTracking(t *testing.T) {
	config := agent.AgentConfig{Model: "sonnet"}
	sub := NewStreamingSubAgent(config, "session-1", "req-1", AgentTypeBuilder)

	// Start consumer
	go func() {
		for range sub.Events() {
		}
	}()

	// Concurrent file tracking from multiple goroutines
	const numGoroutines = 10
	const filesPerGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		goroutineID := i
		go func() {
			defer wg.Done()
			for j := 0; j < filesPerGoroutine; j++ {
				path := fmt.Sprintf("/tmp/file_%d_%d.go", goroutineID, j)
				sub.trackFileOperation("Write", map[string]interface{}{"file_path": path})
			}
		}()
	}
	wg.Wait()

	// Close events
	close(sub.eventsDone)
	sub.closeEventsDirect()

	// Verify all files were tracked (may have some duplicates if paths collide)
	created := sub.FilesCreated()
	if len(created) != numGoroutines*filesPerGoroutine {
		t.Errorf("expected %d files created, got %d", numGoroutines*filesPerGoroutine, len(created))
	}
}
