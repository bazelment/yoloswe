package claude

// ContentBlockType identifies the kind of content block.
type ContentBlockType string

const (
	ContentBlockTypeText       ContentBlockType = "text"
	ContentBlockTypeThinking   ContentBlockType = "thinking"
	ContentBlockTypeToolUse    ContentBlockType = "tool_use"
	ContentBlockTypeToolResult ContentBlockType = "tool_result"
)

// ContentBlock is a structured content block from a Claude response.
type ContentBlock struct {
	Type       ContentBlockType       `json:"type"`
	Text       string                 `json:"text,omitempty"`
	Thinking   string                 `json:"thinking,omitempty"`
	ToolUseID  string                 `json:"tool_use_id,omitempty"`
	ToolName   string                 `json:"tool_name,omitempty"`
	ToolInput  map[string]interface{} `json:"tool_input,omitempty"`
	ToolResult interface{}            `json:"tool_result,omitempty"`
	IsError    bool                   `json:"is_error,omitempty"`
}
