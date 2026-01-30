package claude

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// testSession creates a minimal session for testing the accumulator.
func testSession() *Session {
	s := &Session{
		events:      make(chan Event, 100),
		turnManager: newTurnManager(),
	}
	return s
}

// collectEvents drains the event channel and returns all events.
func collectEvents(s *Session) []Event {
	var events []Event
	for {
		select {
		case e := <-s.events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestStreamAccumulator_TextDelta(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)

	// Start a turn
	s.turnManager.StartTurn("Hello")

	// Simulate message_start
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"message_start","message":{"model":"haiku","id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}}`),
	})

	// Simulate content_block_start for text
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	})

	// Simulate text delta events
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`),
	})

	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"World!"}}`),
	})

	// Collect events
	events := collectEvents(s)

	// Should have 2 TextEvents
	textEvents := filterEvents[TextEvent](events)
	if len(textEvents) != 2 {
		t.Fatalf("expected 2 TextEvents, got %d", len(textEvents))
	}

	// First text event
	if textEvents[0].Text != "Hello " {
		t.Errorf("expected text 'Hello ', got %q", textEvents[0].Text)
	}
	if textEvents[0].FullText != "Hello " {
		t.Errorf("expected full text 'Hello ', got %q", textEvents[0].FullText)
	}

	// Second text event
	if textEvents[1].Text != "World!" {
		t.Errorf("expected text 'World!', got %q", textEvents[1].Text)
	}
	if textEvents[1].FullText != "Hello World!" {
		t.Errorf("expected full text 'Hello World!', got %q", textEvents[1].FullText)
	}

	// Verify turn state
	turn := s.turnManager.CurrentTurn()
	if turn.FullText != "Hello World!" {
		t.Errorf("expected turn full text 'Hello World!', got %q", turn.FullText)
	}
}

func TestStreamAccumulator_ToolUse(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)

	// Start a turn
	s.turnManager.StartTurn("Search for something")

	// Simulate message_start
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"message_start","message":{"model":"haiku","id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}}`),
	})

	// Simulate content_block_start for tool_use
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_123","name":"WebSearch","input":{}}}`),
	})

	// Collect tool start event
	events := collectEvents(s)
	toolStartEvents := filterEvents[ToolStartEvent](events)
	if len(toolStartEvents) != 1 {
		t.Fatalf("expected 1 ToolStartEvent, got %d", len(toolStartEvents))
	}
	if toolStartEvents[0].ID != "toolu_123" {
		t.Errorf("expected tool ID 'toolu_123', got %q", toolStartEvents[0].ID)
	}
	if toolStartEvents[0].Name != "WebSearch" {
		t.Errorf("expected tool name 'WebSearch', got %q", toolStartEvents[0].Name)
	}

	// Simulate input_json_delta events
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"test"}}`),
	})

	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" search\"}"}}`),
	})

	// Collect progress events
	events = collectEvents(s)
	progressEvents := filterEvents[ToolProgressEvent](events)
	if len(progressEvents) != 2 {
		t.Fatalf("expected 2 ToolProgressEvents, got %d", len(progressEvents))
	}

	// Verify partial input accumulation
	if progressEvents[1].PartialInput != `{"query": "test search"}` {
		t.Errorf("unexpected partial input: %q", progressEvents[1].PartialInput)
	}

	// Simulate content_block_stop
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_stop","index":0}`),
	})

	// Collect complete event
	events = collectEvents(s)
	completeEvents := filterEvents[ToolCompleteEvent](events)
	if len(completeEvents) != 1 {
		t.Fatalf("expected 1 ToolCompleteEvent, got %d", len(completeEvents))
	}
	if completeEvents[0].ID != "toolu_123" {
		t.Errorf("expected tool ID 'toolu_123', got %q", completeEvents[0].ID)
	}
	if completeEvents[0].Input["query"] != "test search" {
		t.Errorf("expected input query 'test search', got %v", completeEvents[0].Input["query"])
	}

	// Verify tool was registered in turn
	tool := s.turnManager.GetTool("toolu_123")
	if tool == nil {
		t.Fatal("expected tool to be registered in turn")
	}
	if tool.Input["query"] != "test search" {
		t.Errorf("expected tool input query 'test search', got %v", tool.Input["query"])
	}
}

func TestStreamAccumulator_ThinkingDelta(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)

	// Start a turn
	s.turnManager.StartTurn("Think about this")

	// Simulate message_start
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"message_start","message":{"model":"haiku","id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}}`),
	})

	// Simulate content_block_start for thinking
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
	})

	// Simulate thinking delta
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}`),
	})

	// Collect events
	events := collectEvents(s)
	thinkingEvents := filterEvents[ThinkingEvent](events)
	if len(thinkingEvents) != 1 {
		t.Fatalf("expected 1 ThinkingEvent, got %d", len(thinkingEvents))
	}

	if thinkingEvents[0].Thinking != "Let me think..." {
		t.Errorf("expected thinking 'Let me think...', got %q", thinkingEvents[0].Thinking)
	}
	if thinkingEvents[0].FullThinking != "Let me think..." {
		t.Errorf("expected full thinking 'Let me think...', got %q", thinkingEvents[0].FullThinking)
	}

	// Verify turn state
	turn := s.turnManager.CurrentTurn()
	if turn.FullThinking != "Let me think..." {
		t.Errorf("expected turn full thinking 'Let me think...', got %q", turn.FullThinking)
	}
}

func TestStreamAccumulator_Reset(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)

	// Add some blocks
	acc.blocks[0] = &blockState{index: 0, blockType: "text"}
	acc.blocks[1] = &blockState{index: 1, blockType: "tool_use"}

	if len(acc.blocks) != 2 {
		t.Errorf("expected 2 blocks, got %d", len(acc.blocks))
	}

	// Reset
	acc.Reset()

	if len(acc.blocks) != 0 {
		t.Errorf("expected 0 blocks after reset, got %d", len(acc.blocks))
	}
}

func TestStreamAccumulator_MessageStop(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)

	// Add some blocks
	acc.blocks[0] = &blockState{index: 0, blockType: "text"}

	// Simulate message_stop
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"message_stop"}`),
	})

	// Blocks should be cleared
	if len(acc.blocks) != 0 {
		t.Errorf("expected 0 blocks after message_stop, got %d", len(acc.blocks))
	}
}

func TestStreamAccumulator_MultipleBlocks(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)

	// Start a turn
	s.turnManager.StartTurn("Mixed content")

	// Simulate message_start
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"message_start","message":{"model":"haiku","id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}}`),
	})

	// Text block at index 0
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	})
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll search for that."}}`),
	})
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_stop","index":0}`),
	})

	// Tool block at index 1
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_456","name":"Read","input":{}}}`),
	})
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file\": \"test.txt\"}"}}`),
	})
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_stop","index":1}`),
	})

	// Collect all events
	events := collectEvents(s)

	// Should have text event
	textEvents := filterEvents[TextEvent](events)
	if len(textEvents) != 1 {
		t.Errorf("expected 1 TextEvent, got %d", len(textEvents))
	}

	// Should have tool events
	toolStartEvents := filterEvents[ToolStartEvent](events)
	if len(toolStartEvents) != 1 {
		t.Errorf("expected 1 ToolStartEvent, got %d", len(toolStartEvents))
	}

	toolCompleteEvents := filterEvents[ToolCompleteEvent](events)
	if len(toolCompleteEvents) != 1 {
		t.Errorf("expected 1 ToolCompleteEvent, got %d", len(toolCompleteEvents))
	}
}

func TestStreamAccumulator_RealTraceData(t *testing.T) {
	// Test with real trace data from integration tests
	s := testSession()
	acc := newStreamAccumulator(s)
	s.turnManager.StartTurn("Search latest news about US tariff rate against China/Japan/EU")

	// Real message_start from trace
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"message_start","message":{"model":"claude-haiku-4-5-20251001","id":"msg_01UEh6PyLVYUH279j3Hj84zz","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":8264,"cache_read_input_tokens":13856,"cache_creation":{"ephemeral_5m_input_tokens":8264,"ephemeral_1h_input_tokens":0},"output_tokens":8,"service_tier":"standard"}}}`),
	})

	// Real content_block_start for text
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	})

	// Real text_delta
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll search for the latest news about"}}`),
	})

	// Real content_block_stop
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"content_block_stop","index":0}`),
	})

	// Real tool_use start
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01W8o9N6gnPxzM5fB2R6g3mo","name":"WebSearch","input":{}}}`),
	})

	// Real input_json_delta
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"US "}}`),
	})
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"tariff rates China Japan EU 2026 latest news\"}"}}`),
	})

	// Real content_block_stop for tool
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "c4e0fdb8-ea8e-4900-a07b-a7977627afb2",
		Event:     json.RawMessage(`{"type":"content_block_stop","index":1}`),
	})

	// Collect events
	events := collectEvents(s)

	// Verify text was accumulated
	textEvents := filterEvents[TextEvent](events)
	if len(textEvents) != 1 {
		t.Fatalf("expected 1 TextEvent, got %d", len(textEvents))
	}
	if textEvents[0].Text != "I'll search for the latest news about" {
		t.Errorf("unexpected text: %q", textEvents[0].Text)
	}

	// Verify tool events
	toolStartEvents := filterEvents[ToolStartEvent](events)
	if len(toolStartEvents) != 1 {
		t.Fatalf("expected 1 ToolStartEvent, got %d", len(toolStartEvents))
	}
	if toolStartEvents[0].Name != "WebSearch" {
		t.Errorf("expected tool name 'WebSearch', got %q", toolStartEvents[0].Name)
	}

	toolCompleteEvents := filterEvents[ToolCompleteEvent](events)
	if len(toolCompleteEvents) != 1 {
		t.Fatalf("expected 1 ToolCompleteEvent, got %d", len(toolCompleteEvents))
	}
	if toolCompleteEvents[0].Input["query"] != "US tariff rates China Japan EU 2026 latest news" {
		t.Errorf("unexpected tool input: %v", toolCompleteEvents[0].Input)
	}
}

// filterEvents extracts events of a specific type from a slice.
func filterEvents[T Event](events []Event) []T {
	var result []T
	for _, e := range events {
		if typed, ok := e.(T); ok {
			result = append(result, typed)
		}
	}
	return result
}

func TestBlockState(t *testing.T) {
	// Test blockState initialization
	bs := &blockState{
		index:     0,
		blockType: "text",
	}

	if bs.index != 0 {
		t.Errorf("expected index 0, got %d", bs.index)
	}
	if bs.blockType != "text" {
		t.Errorf("expected blockType 'text', got %q", bs.blockType)
	}

	// Test tool blockState
	toolBs := &blockState{
		index:     1,
		blockType: "tool_use",
		toolID:    "toolu_123",
		toolName:  "Read",
	}

	if toolBs.toolID != "toolu_123" {
		t.Errorf("expected toolID 'toolu_123', got %q", toolBs.toolID)
	}

	// Test accumulating partial JSON
	toolBs.partialJSON = `{"file": "`
	toolBs.partialJSON += `test.txt"}`

	if toolBs.partialJSON != `{"file": "test.txt"}` {
		t.Errorf("unexpected partial JSON: %q", toolBs.partialJSON)
	}
}

func TestStreamAccumulator_UnknownDeltaType(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)
	s.turnManager.StartTurn("Test")

	// Start a text block
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	})

	// Send unknown delta type - should be ignored without error
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"unknown_delta","data":"test"}}`),
	})

	// Should have no events (unknown delta ignored)
	events := collectEvents(s)
	if len(events) != 0 {
		t.Errorf("expected no events for unknown delta type, got %d", len(events))
	}
}

func TestStreamAccumulator_MissingBlockIndex(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)
	s.turnManager.StartTurn("Test")

	// Send delta for non-existent block - should be ignored
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_delta","index":99,"delta":{"type":"text_delta","text":"orphan"}}`),
	})

	// Should have no events
	events := collectEvents(s)
	if len(events) != 0 {
		t.Errorf("expected no events for missing block, got %d", len(events))
	}

	// Send stop for non-existent block - should be ignored
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_stop","index":99}`),
	})

	events = collectEvents(s)
	if len(events) != 0 {
		t.Errorf("expected no events for missing block stop, got %d", len(events))
	}
}

func TestStreamAccumulator_ToolWithTimestamp(t *testing.T) {
	s := testSession()
	acc := newStreamAccumulator(s)
	s.turnManager.StartTurn("Test")

	before := time.Now()

	// Start tool
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_time","name":"Bash","input":{}}}`),
	})

	after := time.Now()

	events := collectEvents(s)
	toolStartEvents := filterEvents[ToolStartEvent](events)
	if len(toolStartEvents) != 1 {
		t.Fatal("expected ToolStartEvent")
	}

	// Timestamp should be reasonable
	if toolStartEvents[0].Timestamp.Before(before) || toolStartEvents[0].Timestamp.After(after) {
		t.Errorf("timestamp out of expected range: %v", toolStartEvents[0].Timestamp)
	}
}

func TestStreamAccumulator_ToolWithEmptyInput(t *testing.T) {
	// Test that tools with no input parameters still emit ToolCompleteEvent
	// This covers the fix for tools that have no input_json_delta events
	s := testSession()
	acc := newStreamAccumulator(s)
	s.turnManager.StartTurn("Run a simple command")

	// Simulate message_start
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"message_start","message":{"model":"haiku","id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}}`),
	})

	// Start tool_use block - note: no input_json_delta events will follow
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_empty","name":"NoParamTool","input":{}}}`),
	})

	// Collect and verify ToolStartEvent
	events := collectEvents(s)
	toolStartEvents := filterEvents[ToolStartEvent](events)
	if len(toolStartEvents) != 1 {
		t.Fatalf("expected 1 ToolStartEvent, got %d", len(toolStartEvents))
	}
	if toolStartEvents[0].ID != "toolu_empty" {
		t.Errorf("expected tool ID 'toolu_empty', got %q", toolStartEvents[0].ID)
	}
	if toolStartEvents[0].Name != "NoParamTool" {
		t.Errorf("expected tool name 'NoParamTool', got %q", toolStartEvents[0].Name)
	}

	// Directly send content_block_stop WITHOUT any input_json_delta events
	// This simulates a tool that takes no parameters
	acc.HandleEvent(protocol.StreamEvent{
		SessionID: "test",
		Event:     json.RawMessage(`{"type":"content_block_stop","index":0}`),
	})

	// Collect and verify ToolCompleteEvent
	events = collectEvents(s)
	completeEvents := filterEvents[ToolCompleteEvent](events)
	if len(completeEvents) != 1 {
		t.Fatalf("expected 1 ToolCompleteEvent for empty input tool, got %d", len(completeEvents))
	}
	if completeEvents[0].ID != "toolu_empty" {
		t.Errorf("expected tool ID 'toolu_empty', got %q", completeEvents[0].ID)
	}
	if completeEvents[0].Name != "NoParamTool" {
		t.Errorf("expected tool name 'NoParamTool', got %q", completeEvents[0].Name)
	}

	// Input should be an empty map, not nil
	if completeEvents[0].Input == nil {
		t.Error("expected Input to be empty map, got nil")
	}
	if len(completeEvents[0].Input) != 0 {
		t.Errorf("expected empty input map, got %d entries: %v", len(completeEvents[0].Input), completeEvents[0].Input)
	}

	// Verify tool was registered in turn with empty input
	tool := s.turnManager.GetTool("toolu_empty")
	if tool == nil {
		t.Fatal("expected tool to be registered in turn")
	}
	if tool.Input == nil {
		t.Error("expected tool.Input to be empty map, got nil")
	}
	if len(tool.Input) != 0 {
		t.Errorf("expected empty tool input, got %v", tool.Input)
	}
}
