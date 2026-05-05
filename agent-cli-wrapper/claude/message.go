package claude

import "github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"

const (
	ContentBlockTypeText       = protocol.ContentBlockTypeText
	ContentBlockTypeThinking   = protocol.ContentBlockTypeThinking
	ContentBlockTypeToolUse    = protocol.ContentBlockTypeToolUse
	ContentBlockTypeToolResult = protocol.ContentBlockTypeToolResult
)

type ContentBlock = protocol.ContentBlock
type ContentBlocks = protocol.ContentBlocks
type TextBlock = protocol.TextBlock
type ThinkingBlock = protocol.ThinkingBlock
type ToolUseBlock = protocol.ToolUseBlock
type ToolResultBlock = protocol.ToolResultBlock
