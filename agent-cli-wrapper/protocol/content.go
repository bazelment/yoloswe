// Package protocol defines the wire protocol types for the Claude CLI.
package protocol

import (
	"encoding/json"
	"fmt"
	"log/slog"
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

// UnknownContentBlock preserves content blocks whose type is not known by this
// package yet.
type UnknownContentBlock struct {
	Type ContentBlockType
	Raw  json.RawMessage
}

// BlockType returns the content block type.
func (u UnknownContentBlock) BlockType() ContentBlockType { return u.Type }

// MarshalJSON implements json.Marshaler.
func (u UnknownContentBlock) MarshalJSON() ([]byte, error) {
	if len(u.Raw) == 0 {
		return json.Marshal(struct {
			Type ContentBlockType `json:"type"`
		}{Type: u.Type})
	}
	return u.Raw, nil
}

// DisplayString returns a human-readable representation of an unknown content
// block for transcript surfaces (sessionmodel parser, sessionanalysis turn
// aggregation, etc.) so the formatting stays consistent across consumers.
func (u UnknownContentBlock) DisplayString() string {
	if len(u.Raw) == 0 {
		return fmt.Sprintf("[unknown content block: %s]", u.Type)
	}
	return fmt.Sprintf("[unknown content block: %s] %s", u.Type, string(u.Raw))
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

// UnmarshalJSON implements json.Unmarshaler. Older recordings used
// tool_use_id/tool_name/tool_input field names; accept them when the current
// id/name/input fields are absent.
func (t *ToolUseBlock) UnmarshalJSON(data []byte) error {
	type toolUseBlock ToolUseBlock
	var raw struct {
		*toolUseBlock
		LegacyInput map[string]interface{} `json:"tool_input"`
		LegacyID    string                 `json:"tool_use_id"`
		LegacyName  string                 `json:"tool_name"`
	}
	raw.toolUseBlock = (*toolUseBlock)(t)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if t.ID == "" {
		t.ID = raw.LegacyID
	}
	if t.Name == "" {
		t.Name = raw.LegacyName
	}
	if t.Input == nil {
		t.Input = raw.LegacyInput
	}
	return nil
}

// ToolResultBlock contains tool execution results.
type ToolResultBlock struct {
	Content   interface{}      `json:"content"`
	IsError   *bool            `json:"is_error"`
	Type      ContentBlockType `json:"type"`
	ToolUseID string           `json:"tool_use_id"`
}

// BlockType returns the content block type.
func (t ToolResultBlock) BlockType() ContentBlockType { return ContentBlockTypeToolResult }

// UnmarshalJSON implements json.Unmarshaler. Older recordings used
// "tool_result" for the payload field; accept it when "content" is absent.
func (t *ToolResultBlock) UnmarshalJSON(data []byte) error {
	type toolResultBlock ToolResultBlock
	var raw struct {
		*toolResultBlock
		LegacyContent json.RawMessage `json:"tool_result"`
	}
	raw.toolResultBlock = (*toolResultBlock)(t)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if t.Content != nil || len(raw.LegacyContent) == 0 {
		return nil
	}
	return json.Unmarshal(raw.LegacyContent, &t.Content)
}

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
		slog.Warn("preserving unknown content block type", "type", base.Type)
		raw := make(json.RawMessage, len(data))
		copy(raw, data)
		return UnknownContentBlock{Type: base.Type, Raw: raw}, nil
	}
}
