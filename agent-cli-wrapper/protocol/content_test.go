package protocol

import (
	"encoding/json"
	"testing"
)

func TestUnmarshalContentBlock_UnknownType(t *testing.T) {
	raw := json.RawMessage(`{"type":"server_tool_use","id":"srv_123","name":"some_tool"}`)

	block, err := UnmarshalContentBlock(raw)
	if err != nil {
		t.Fatalf("expected no error for unknown type, got: %v", err)
	}
	unknown, ok := block.(UnknownContentBlock)
	if !ok {
		t.Fatalf("expected UnknownContentBlock for unknown type, got: %T", block)
	}
	if unknown.BlockType() != ContentBlockType("server_tool_use") {
		t.Fatalf("type: %q", unknown.BlockType())
	}
	if string(unknown.Raw) != string(raw) {
		t.Fatalf("raw: %s", unknown.Raw)
	}
}

func TestContentBlocks_PreservesUnknownTypes(t *testing.T) {
	// Mix of known and unknown block types
	raw := `[
		{"type":"text","text":"hello"},
		{"type":"server_tool_use","id":"srv_123","name":"some_tool"},
		{"type":"tool_use","id":"toolu_abc","name":"Bash","input":{"command":"ls"}},
		{"type":"image","source":{"type":"base64","data":"..."}}
	]`

	var blocks ContentBlocks
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(blocks) != 4 {
		t.Fatalf("expected 4 blocks, got %d", len(blocks))
	}

	if blocks[0].BlockType() != ContentBlockTypeText {
		t.Errorf("expected first block to be text, got %s", blocks[0].BlockType())
	}
	if blocks[1].BlockType() != ContentBlockType("server_tool_use") {
		t.Errorf("expected second block to be server_tool_use, got %s", blocks[1].BlockType())
	}
	if blocks[2].BlockType() != ContentBlockTypeToolUse {
		t.Errorf("expected third block to be tool_use, got %s", blocks[2].BlockType())
	}
	if blocks[3].BlockType() != ContentBlockType("image") {
		t.Errorf("expected fourth block to be image, got %s", blocks[3].BlockType())
	}

	// Verify text content preserved
	textBlock, ok := blocks[0].(TextBlock)
	if !ok {
		t.Fatal("first block is not TextBlock")
	}
	if textBlock.Text != "hello" {
		t.Errorf("expected text 'hello', got %q", textBlock.Text)
	}
}

func TestToolResultBlock_UnmarshalLegacyToolResultField(t *testing.T) {
	var block ToolResultBlock
	raw := []byte(`{"type":"tool_result","tool_use_id":"toolu_1","tool_result":[{"type":"text","text":"legacy"}],"is_error":true}`)

	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if block.ToolUseID != "toolu_1" {
		t.Fatalf("tool_use_id: %q", block.ToolUseID)
	}
	if block.IsError == nil || !*block.IsError {
		t.Fatalf("is_error: %v", block.IsError)
	}
	items, ok := block.Content.([]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("content: %#v", block.Content)
	}
}

func TestToolUseBlock_UnmarshalLegacyToolUseFields(t *testing.T) {
	var block ToolUseBlock
	raw := []byte(`{"type":"tool_use","tool_use_id":"toolu_1","tool_name":"Bash","tool_input":{"command":"pwd"}}`)

	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if block.ID != "toolu_1" {
		t.Fatalf("id: %q", block.ID)
	}
	if block.Name != "Bash" {
		t.Fatalf("name: %q", block.Name)
	}
	if block.Input["command"] != "pwd" {
		t.Fatalf("input: %#v", block.Input)
	}
}
