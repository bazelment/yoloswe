// Package protocol defines the wire protocol types for the Claude CLI.
package protocol

import (
	"encoding/json"
	"fmt"
)

// ContentBlockType discriminates between content block kinds.
type ContentBlockType string

const (
	ContentBlockTypeText       ContentBlockType = "text"
	ContentBlockTypeThinking   ContentBlockType = "thinking"
	ContentBlockTypeToolUse    ContentBlockType = "tool_use"
	ContentBlockTypeToolResult ContentBlockType = "tool_result"
)

// ContentBlock is the interface for all content block types.
type ContentBlock interface {
	BlockType() ContentBlockType
}

// TextBlock contains text content.
type TextBlock struct {
	Type ContentBlockType `json:"type"`
	Text string           `json:"text"`
}

// BlockType returns the content block type.
func (t TextBlock) BlockType() ContentBlockType { return ContentBlockTypeText }

// ThinkingBlock contains Claude's reasoning.
type ThinkingBlock struct {
	Type      ContentBlockType `json:"type"`
	Thinking  string           `json:"thinking"`
	Signature string           `json:"signature,omitempty"`
}

// BlockType returns the content block type.
func (t ThinkingBlock) BlockType() ContentBlockType { return ContentBlockTypeThinking }

// ToolUseBlock represents a tool invocation.
type ToolUseBlock struct {
	Input map[string]interface{} `json:"input"`
	Type  ContentBlockType       `json:"type"`
	ID    string                 `json:"id"`
	Name  string                 `json:"name"`
}

// BlockType returns the content block type.
func (t ToolUseBlock) BlockType() ContentBlockType { return ContentBlockTypeToolUse }

// ToolResultBlock contains tool execution results.
type ToolResultBlock struct {
	Content   interface{}      `json:"content"`
	IsError   *bool            `json:"is_error"`
	Type      ContentBlockType `json:"type"`
	ToolUseID string           `json:"tool_use_id"`
}

// BlockType returns the content block type.
func (t ToolResultBlock) BlockType() ContentBlockType { return ContentBlockTypeToolResult }

// ContentBlocks is a slice of ContentBlock that handles JSON unmarshaling.
type ContentBlocks []ContentBlock

// UnmarshalJSON implements json.Unmarshaler for ContentBlocks.
func (c *ContentBlocks) UnmarshalJSON(data []byte) error {
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(data, &rawBlocks); err != nil {
		return err
	}

	*c = make(ContentBlocks, 0, len(rawBlocks))
	for _, raw := range rawBlocks {
		block, err := UnmarshalContentBlock(raw)
		if err != nil {
			return err
		}
		*c = append(*c, block)
	}
	return nil
}

// UnmarshalContentBlock parses raw JSON into a typed ContentBlock.
func UnmarshalContentBlock(data json.RawMessage) (ContentBlock, error) {
	var base struct {
		Type ContentBlockType `json:"type"`
	}
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, err
	}

	switch base.Type {
	case ContentBlockTypeText:
		var block TextBlock
		if err := json.Unmarshal(data, &block); err != nil {
			return nil, err
		}
		return block, nil
	case ContentBlockTypeThinking:
		var block ThinkingBlock
		if err := json.Unmarshal(data, &block); err != nil {
			return nil, err
		}
		return block, nil
	case ContentBlockTypeToolUse:
		var block ToolUseBlock
		if err := json.Unmarshal(data, &block); err != nil {
			return nil, err
		}
		return block, nil
	case ContentBlockTypeToolResult:
		var block ToolResultBlock
		if err := json.Unmarshal(data, &block); err != nil {
			return nil, err
		}
		return block, nil
	default:
		return nil, fmt.Errorf("unknown content block type: %s", base.Type)
	}
}
