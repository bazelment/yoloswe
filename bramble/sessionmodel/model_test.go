package sessionmodel

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- OutputBuffer tests ------------------------------------------------------

func TestOutputBuffer_Eviction(t *testing.T) {
	buf := NewOutputBuffer(3)
	for i := 0; i < 5; i++ {
		buf.Append(OutputLine{Content: string(rune('a' + i))})
	}
	snap := buf.Snapshot()
	require.Len(t, snap, 3)
	assert.Equal(t, "c", snap[0].Content)
	assert.Equal(t, "d", snap[1].Content)
	assert.Equal(t, "e", snap[2].Content)
}

func TestOutputBuffer_StreamingTextMerge(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.AppendStreamingText("Hello")
	buf.AppendStreamingText(" world")
	snap := buf.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "Hello world", snap[0].Content)
	assert.Equal(t, OutputTypeText, snap[0].Type)
}

func TestOutputBuffer_StreamingTextNewLineAfterOtherType(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.Append(OutputLine{Type: OutputTypeToolStart, Content: "Read"})
	buf.AppendStreamingText("Some text")
	snap := buf.Snapshot()
	require.Len(t, snap, 2)
	assert.Equal(t, OutputTypeToolStart, snap[0].Type)
	assert.Equal(t, OutputTypeText, snap[1].Type)
	assert.Equal(t, "Some text", snap[1].Content)
}

func TestOutputBuffer_StreamingThinkingMerge(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.AppendStreamingThinking("Let me")
	buf.AppendStreamingThinking(" think...")
	snap := buf.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "Let me think...", snap[0].Content)
	assert.Equal(t, OutputTypeThinking, snap[0].Type)
}

func TestOutputBuffer_StreamingThinkingSkipsWhitespace(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.AppendStreamingThinking("   ")
	buf.AppendStreamingThinking("\t\n")
	assert.Equal(t, 0, buf.Len(), "whitespace-only thinking should be ignored")
}

func TestOutputBuffer_UpdateToolByID(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.Append(OutputLine{Type: OutputTypeToolStart, ToolID: "t1", ToolName: "Read", ToolState: ToolStateRunning})
	buf.Append(OutputLine{Type: OutputTypeText, Content: "some text"})

	found := buf.UpdateToolByID("t1", func(line *OutputLine) {
		line.ToolState = ToolStateComplete
		line.DurationMs = 500
	})
	assert.True(t, found)

	snap := buf.Snapshot()
	assert.Equal(t, ToolStateComplete, snap[0].ToolState)
	assert.Equal(t, int64(500), snap[0].DurationMs)
}

func TestOutputBuffer_UpdateToolByID_NotFound(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.Append(OutputLine{Type: OutputTypeToolStart, ToolID: "t1", ToolName: "Read"})

	found := buf.UpdateToolByID("nonexistent", func(line *OutputLine) {
		line.ToolState = ToolStateComplete
	})
	assert.False(t, found)
}

func TestOutputBuffer_SnapshotIsDeepCopy(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.Append(OutputLine{
		Type:      OutputTypeToolStart,
		ToolID:    "t1",
		ToolInput: map[string]interface{}{"file": "/foo.go"},
	})

	snap := buf.Snapshot()
	// Mutate the snapshot's map.
	snap[0].ToolInput["file"] = "/mutated.go"

	// Original buffer should be unaffected.
	original := buf.Snapshot()
	assert.Equal(t, "/foo.go", original[0].ToolInput["file"])
}

func TestOutputBuffer_SnapshotDeepCopiesToolResult(t *testing.T) {
	buf := NewOutputBuffer(10)
	buf.Append(OutputLine{
		Type:       OutputTypeToolStart,
		ToolID:     "t1",
		ToolResult: map[string]interface{}{"stdout": "hello", "nested": []interface{}{"a", "b"}},
	})

	snap := buf.Snapshot()
	// Mutate the snapshot's ToolResult.
	result := snap[0].ToolResult.(map[string]interface{})
	result["stdout"] = "mutated"
	nested := result["nested"].([]interface{})
	nested[0] = "mutated"

	// Original buffer should be unaffected.
	original := buf.Snapshot()
	origResult := original[0].ToolResult.(map[string]interface{})
	assert.Equal(t, "hello", origResult["stdout"])
	origNested := origResult["nested"].([]interface{})
	assert.Equal(t, "a", origNested[0])
}

// --- SessionModel tests ------------------------------------------------------

func TestSessionModel_StatusRejectsFromTerminal(t *testing.T) {
	terminalStatuses := []SessionStatus{StatusCompleted, StatusFailed, StatusStopped}
	for _, terminal := range terminalStatuses {
		t.Run(string(terminal), func(t *testing.T) {
			m := NewSessionModel(0)
			m.SetMeta(SessionMeta{Status: terminal})

			err := m.UpdateStatus(StatusRunning)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "terminal")

			// Status should remain unchanged.
			assert.Equal(t, terminal, m.Meta().Status)
		})
	}
}

func TestSessionModel_StatusAllowsNonTerminal(t *testing.T) {
	m := NewSessionModel(0)
	m.SetMeta(SessionMeta{Status: StatusRunning})

	err := m.UpdateStatus(StatusCompleted)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, m.Meta().Status)
}

func TestSessionModel_ConcurrentAccess(t *testing.T) {
	m := NewSessionModel(100)
	m.SetMeta(SessionMeta{Status: StatusRunning})

	var wg sync.WaitGroup
	const goroutines = 10
	const iterations = 100

	// Concurrent writers.
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				m.AppendOutput(OutputLine{Type: OutputTypeText, Content: "hello"})
				m.AppendStreamingText("delta")
				m.AppendStreamingThinking("think")
				m.UpdateProgress(func(p *ProgressSnapshot) {
					p.TurnCount++
				})
			}
		}()
	}

	// Concurrent readers.
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = m.Meta()
				_ = m.Output()
				_ = m.Progress()
			}
		}()
	}

	wg.Wait()
	// If we get here without a race detector panic, the test passes.
	assert.Greater(t, len(m.Output()), 0)
}

// recordingObserver records events for test verification.
type recordingObserver struct { //nolint:govet // fieldalignment: test fixture readability
	mu     sync.Mutex
	events []ModelEvent
}

func (r *recordingObserver) OnModelEvent(event ModelEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingObserver) Events() []ModelEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelEvent, len(r.events))
	copy(out, r.events)
	return out
}

func TestSessionModel_ObserverNotified(t *testing.T) {
	m := NewSessionModel(0)
	obs := &recordingObserver{}
	m.AddObserver(obs)

	// Trigger each mutation type.
	m.SetMeta(SessionMeta{Status: StatusRunning})
	_ = m.UpdateStatus(StatusCompleted)
	m.AppendOutput(OutputLine{Type: OutputTypeText, Content: "hi", Timestamp: time.Now()})
	m.AppendStreamingText("delta")
	m.AppendStreamingThinking("think")
	m.UpdateProgress(func(p *ProgressSnapshot) { p.TurnCount = 1 })

	events := obs.Events()
	require.GreaterOrEqual(t, len(events), 6, "should have at least 6 events")

	// Verify event types are present.
	var hasMeta, hasStatus, hasOutput, hasProgress bool
	for _, e := range events {
		switch e.(type) {
		case MetaUpdated:
			hasMeta = true
		case StatusChanged:
			hasStatus = true
		case OutputAppended:
			hasOutput = true
		case ProgressUpdated:
			hasProgress = true
		}
	}
	assert.True(t, hasMeta, "should have MetaUpdated event")
	assert.True(t, hasStatus, "should have StatusChanged event")
	assert.True(t, hasOutput, "should have OutputAppended event")
	assert.True(t, hasProgress, "should have ProgressUpdated event")
}
