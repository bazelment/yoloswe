package protocol

import (
	"encoding/json"
	"testing"
)

func TestParseContentBlockDelta_TextDelta(t *testing.T) {
	raw := json.RawMessage(`{"type":"text_delta","text":"hello"}`)
	d, err := ParseContentBlockDelta(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	td, ok := d.(TextDelta)
	if !ok {
		t.Fatalf("expected TextDelta, got %T", d)
	}
	if td.Text != "hello" {
		t.Errorf("expected text 'hello', got %q", td.Text)
	}
	if td.DeltaType() != "text_delta" {
		t.Errorf("expected DeltaType 'text_delta', got %q", td.DeltaType())
	}
}

func TestParseContentBlockDelta_ThinkingDelta(t *testing.T) {
	raw := json.RawMessage(`{"type":"thinking_delta","thinking":"hmm"}`)
	d, err := ParseContentBlockDelta(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	td, ok := d.(ThinkingDelta)
	if !ok {
		t.Fatalf("expected ThinkingDelta, got %T", d)
	}
	if td.Thinking != "hmm" {
		t.Errorf("expected thinking 'hmm', got %q", td.Thinking)
	}
	if td.DeltaType() != "thinking_delta" {
		t.Errorf("expected DeltaType 'thinking_delta', got %q", td.DeltaType())
	}
}

func TestParseContentBlockDelta_InputJSONDelta(t *testing.T) {
	raw := json.RawMessage(`{"type":"input_json_delta","partial_json":"{\"q\":\""}`)
	d, err := ParseContentBlockDelta(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	jd, ok := d.(InputJSONDelta)
	if !ok {
		t.Fatalf("expected InputJSONDelta, got %T", d)
	}
	if jd.PartialJSON != `{"q":"` {
		t.Errorf("unexpected PartialJSON: %q", jd.PartialJSON)
	}
	if jd.DeltaType() != "input_json_delta" {
		t.Errorf("expected DeltaType 'input_json_delta', got %q", jd.DeltaType())
	}
}

func TestParseContentBlockDelta_Unknown(t *testing.T) {
	raw := json.RawMessage(`{"type":"future_delta","data":"x"}`)
	d, err := ParseContentBlockDelta(raw)
	if err != nil {
		t.Fatalf("unexpected error for unknown delta type: %v", err)
	}
	if d != nil {
		t.Errorf("expected nil for unknown delta type, got %T", d)
	}
}

func TestParseContentBlockDelta_InvalidJSON(t *testing.T) {
	_, err := ParseContentBlockDelta(json.RawMessage(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// TestContentBlockStartEvent_ParsedBlock uses the real CLI trace fixture from parse_test.go.
func TestContentBlockStartEvent_ParsedBlock_Text(t *testing.T) {
	msg, err := ParseMessage([]byte(streamContentBlockStart))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("ParseStreamEvent failed: %v", err)
	}
	blockStart := eventData.(ContentBlockStartEvent)

	block, err := blockStart.ParsedBlock()
	if err != nil {
		t.Fatalf("ParsedBlock failed: %v", err)
	}
	if block == nil {
		t.Fatal("expected non-nil block")
	}
	tb, ok := block.(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", block)
	}
	if tb.BlockType() != ContentBlockTypeText {
		t.Errorf("expected block type 'text', got %q", tb.BlockType())
	}
}

func TestContentBlockStartEvent_ParsedBlock_ToolUse(t *testing.T) {
	msg, err := ParseMessage([]byte(streamToolUseStart))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	streamEvent := msg.(StreamEvent)
	eventData, _ := ParseStreamEvent(streamEvent.Event)
	blockStart := eventData.(ContentBlockStartEvent)

	block, err := blockStart.ParsedBlock()
	if err != nil {
		t.Fatalf("ParsedBlock failed: %v", err)
	}
	tb, ok := block.(ToolUseBlock)
	if !ok {
		t.Fatalf("expected ToolUseBlock, got %T", block)
	}
	if tb.Name != "WebSearch" {
		t.Errorf("expected name 'WebSearch', got %q", tb.Name)
	}
}

func TestContentBlockDeltaEvent_ParsedDelta_Text(t *testing.T) {
	msg, err := ParseMessage([]byte(streamTextDelta))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	streamEvent := msg.(StreamEvent)
	eventData, _ := ParseStreamEvent(streamEvent.Event)
	deltaEvent := eventData.(ContentBlockDeltaEvent)

	d, err := deltaEvent.ParsedDelta()
	if err != nil {
		t.Fatalf("ParsedDelta failed: %v", err)
	}
	td, ok := d.(TextDelta)
	if !ok {
		t.Fatalf("expected TextDelta, got %T", d)
	}
	if td.Text != "I'll search for the latest news about" {
		t.Errorf("unexpected text: %q", td.Text)
	}
}

func TestContentBlockDeltaEvent_ParsedDelta_InputJSON(t *testing.T) {
	msg, err := ParseMessage([]byte(streamInputJSONDelta))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	streamEvent := msg.(StreamEvent)
	eventData, _ := ParseStreamEvent(streamEvent.Event)
	deltaEvent := eventData.(ContentBlockDeltaEvent)

	d, err := deltaEvent.ParsedDelta()
	if err != nil {
		t.Fatalf("ParsedDelta failed: %v", err)
	}
	jd, ok := d.(InputJSONDelta)
	if !ok {
		t.Fatalf("expected InputJSONDelta, got %T", d)
	}
	if jd.PartialJSON != `{"query": "US ` {
		t.Errorf("unexpected partial_json: %q", jd.PartialJSON)
	}
}

// TestControlRequest_ParsedRequest verifies the accessor mirrors ParseControlRequest.
func TestControlRequest_ParsedRequest_CanUseTool(t *testing.T) {
	req := ControlRequest{
		Type:      MessageTypeControlRequest,
		RequestID: "req_x",
		Request:   json.RawMessage(`{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}`),
	}

	data, err := req.ParsedRequest()
	if err != nil {
		t.Fatalf("ParsedRequest failed: %v", err)
	}
	canUse, ok := data.(CanUseToolRequest)
	if !ok {
		t.Fatalf("expected CanUseToolRequest, got %T", data)
	}
	if canUse.ToolName != "Bash" {
		t.Errorf("expected tool name 'Bash', got %q", canUse.ToolName)
	}
}

func TestControlRequest_ParsedRequest_Interrupt(t *testing.T) {
	req := ControlRequest{
		Type:      MessageTypeControlRequest,
		RequestID: "req_y",
		Request:   json.RawMessage(`{"subtype":"interrupt"}`),
	}

	data, err := req.ParsedRequest()
	if err != nil {
		t.Fatalf("ParsedRequest failed: %v", err)
	}
	_, ok := data.(InterruptRequest)
	if !ok {
		t.Fatalf("expected InterruptRequest, got %T", data)
	}
}
