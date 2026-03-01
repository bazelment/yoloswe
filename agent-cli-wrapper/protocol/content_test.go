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
	if block != nil {
		t.Fatalf("expected nil block for unknown type, got: %v", block)
	}
}

func TestContentBlocks_SkipsUnknownTypes(t *testing.T) {
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

	// Should only have the two known blocks (text + tool_use)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	if blocks[0].BlockType() != ContentBlockTypeText {
		t.Errorf("expected first block to be text, got %s", blocks[0].BlockType())
	}
	if blocks[1].BlockType() != ContentBlockTypeToolUse {
		t.Errorf("expected second block to be tool_use, got %s", blocks[1].BlockType())
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
