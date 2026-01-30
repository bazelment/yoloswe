package protocol

import (
	"encoding/json"
	"testing"
)

// Test data from real CLI traces

const systemInitMessage = `{"type":"system","subtype":"init","cwd":"/tmp/test","session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","tools":["Task","Bash","Read","Write","WebSearch"],"mcp_servers":[{"name":"plugin:playwright:playwright","status":"connected"}],"model":"claude-haiku-4-5-20251001","permissionMode":"bypassPermissions","slash_commands":["compact","context","cost"],"apiKeySource":"none","claude_code_version":"2.1.12","output_style":"default","agents":["Bash","Explore","Plan"],"skills":[],"plugins":[],"uuid":"94a377da-ec31-4c26-9422-07b0cf59fcc5"}`

const systemHookResponse = `{"type":"system","subtype":"hook_response","session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","uuid":"39858897-628a-4fa3-97fb-148562ed759d","hook_name":"SessionStart:startup","hook_event":"SessionStart","stdout":"","stderr":"","exit_code":0}`

const streamMessageStart = `{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-haiku-4-5-20251001","id":"msg_01UEh6PyLVYUH279j3Hj84zz","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":8264,"cache_read_input_tokens":13856,"cache_creation":{"ephemeral_5m_input_tokens":8264,"ephemeral_1h_input_tokens":0},"output_tokens":8,"service_tier":"standard"}}},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"fe9be343-260a-4618-80d5-808a30851ca8"}`

const streamContentBlockStart = `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"ed7e534e-0376-443f-b947-80279c7fa051"}`

const streamTextDelta = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll search for the latest news about"}},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"ec5b06f2-70ba-4aa9-b43d-cc8e48e8ca4f"}`

const streamToolUseStart = `{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01W8o9N6gnPxzM5fB2R6g3mo","name":"WebSearch","input":{}}},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"0f740272-aafb-40e0-a583-13903d1a3a61"}`

const streamInputJSONDelta = `{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\": \"US "}},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"88e45d8e-6f5d-4338-a7bb-b5885c8e85ef"}`

const streamContentBlockStop = `{"type":"stream_event","event":{"type":"content_block_stop","index":1},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"fced7ca2-11b1-435b-8efd-0cb11c9e2dd6"}`

const streamMessageDelta = `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":2,"cache_creation_input_tokens":8264,"cache_read_input_tokens":13856,"output_tokens":86}},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"a4f31165-d4a1-4f89-b65b-81a3ed3025da"}`

const streamMessageStop = `{"type":"stream_event","event":{"type":"message_stop"},"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","parent_tool_use_id":null,"uuid":"89c94763-7346-4c88-b37a-a87d8169c98c"}`

const assistantMessage = `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_01UEh6PyLVYUH279j3Hj84zz","type":"message","role":"assistant","content":[{"type":"text","text":"I'll search for the latest news about US tariff rates against China, Japan, and the EU."}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":8264,"cache_read_input_tokens":13856,"cache_creation":{"ephemeral_5m_input_tokens":8264,"ephemeral_1h_input_tokens":0},"output_tokens":8,"service_tier":"standard"},"context_management":null},"parent_tool_use_id":null,"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","uuid":"0e21eeb4-5683-4858-9cce-96beeec8a2e0"}`

const assistantWithToolUse = `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_01UEh6PyLVYUH279j3Hj84zz","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_01W8o9N6gnPxzM5fB2R6g3mo","name":"WebSearch","input":{"query":"US tariff rates China Japan EU 2026 latest news"}}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":8264,"cache_read_input_tokens":13856,"cache_creation":{"ephemeral_5m_input_tokens":8264,"ephemeral_1h_input_tokens":0},"output_tokens":8,"service_tier":"standard"},"context_management":null},"parent_tool_use_id":null,"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","uuid":"acbc553e-5762-4123-ad36-2dd7db8384c5"}`

const userToolResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01W8o9N6gnPxzM5fB2R6g3mo","type":"tool_result","content":"Web search results...","is_error":false}]},"parent_tool_use_id":null,"session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","uuid":"0bc8094f-daa3-4f20-a82b-8d5d9f467a67"}`

const resultMessage = `{"type":"result","subtype":"success","is_error":false,"duration_ms":14067,"duration_api_ms":14041,"num_turns":2,"result":"Here's the latest news on US tariff rates...","session_id":"c4e0fdb8-ea8e-4900-a07b-a7977627afb2","total_cost_usd":0.05407735,"usage":{"input_tokens":6,"cache_creation_input_tokens":9332,"cache_read_input_tokens":35976,"output_tokens":644,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":9332}},"modelUsage":{"claude-haiku-4-5-20251001":{"inputTokens":2276,"outputTokens":1179,"cacheReadInputTokens":35976,"cacheCreationInputTokens":25847,"webSearchRequests":1,"costUSD":0.05407735,"contextWindow":200000,"maxOutputTokens":64000}},"permission_denials":[],"uuid":"104e47bd-f893-4420-92c8-c6c58d84118e"}`

func TestParseMessage_SystemInit(t *testing.T) {
	msg, err := ParseMessage([]byte(systemInitMessage))
	if err != nil {
		t.Fatalf("failed to parse system init message: %v", err)
	}

	sysMsg, ok := msg.(SystemMessage)
	if !ok {
		t.Fatalf("expected SystemMessage, got %T", msg)
	}

	if sysMsg.Type != MessageTypeSystem {
		t.Errorf("expected type 'system', got %q", sysMsg.Type)
	}
	if sysMsg.Subtype != "init" {
		t.Errorf("expected subtype 'init', got %q", sysMsg.Subtype)
	}
	if sysMsg.SessionID != "c4e0fdb8-ea8e-4900-a07b-a7977627afb2" {
		t.Errorf("unexpected session_id: %q", sysMsg.SessionID)
	}
	if sysMsg.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("unexpected model: %q", sysMsg.Model)
	}
	if sysMsg.PermissionMode != "bypassPermissions" {
		t.Errorf("unexpected permissionMode: %q", sysMsg.PermissionMode)
	}
	if len(sysMsg.Tools) != 5 {
		t.Errorf("expected 5 tools, got %d", len(sysMsg.Tools))
	}
	if sysMsg.ClaudeCodeVersion != "2.1.12" {
		t.Errorf("unexpected claude_code_version: %q", sysMsg.ClaudeCodeVersion)
	}
}

func TestParseMessage_SystemHookResponse(t *testing.T) {
	msg, err := ParseMessage([]byte(systemHookResponse))
	if err != nil {
		t.Fatalf("failed to parse hook response: %v", err)
	}

	sysMsg, ok := msg.(SystemMessage)
	if !ok {
		t.Fatalf("expected SystemMessage, got %T", msg)
	}

	if sysMsg.Subtype != "hook_response" {
		t.Errorf("expected subtype 'hook_response', got %q", sysMsg.Subtype)
	}
	if sysMsg.HookName != "SessionStart:startup" {
		t.Errorf("unexpected hook_name: %q", sysMsg.HookName)
	}
	if sysMsg.HookEvent != "SessionStart" {
		t.Errorf("unexpected hook_event: %q", sysMsg.HookEvent)
	}
	if sysMsg.ExitCode == nil || *sysMsg.ExitCode != 0 {
		t.Errorf("unexpected exit_code: %v", sysMsg.ExitCode)
	}
}

func TestParseMessage_StreamEvent_MessageStart(t *testing.T) {
	msg, err := ParseMessage([]byte(streamMessageStart))
	if err != nil {
		t.Fatalf("failed to parse stream event: %v", err)
	}

	streamEvent, ok := msg.(StreamEvent)
	if !ok {
		t.Fatalf("expected StreamEvent, got %T", msg)
	}

	if streamEvent.Type != MessageTypeStreamEvent {
		t.Errorf("expected type 'stream_event', got %q", streamEvent.Type)
	}
	if streamEvent.SessionID != "c4e0fdb8-ea8e-4900-a07b-a7977627afb2" {
		t.Errorf("unexpected session_id: %q", streamEvent.SessionID)
	}

	// Parse inner event
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	msgStart, ok := eventData.(MessageStartEvent)
	if !ok {
		t.Fatalf("expected MessageStartEvent, got %T", eventData)
	}

	if msgStart.Type != StreamEventTypeMessageStart {
		t.Errorf("expected type 'message_start', got %q", msgStart.Type)
	}
	if msgStart.Message.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("unexpected model: %q", msgStart.Message.Model)
	}
	if msgStart.Message.ID != "msg_01UEh6PyLVYUH279j3Hj84zz" {
		t.Errorf("unexpected message id: %q", msgStart.Message.ID)
	}
}

func TestParseMessage_StreamEvent_ContentBlockStart(t *testing.T) {
	msg, err := ParseMessage([]byte(streamContentBlockStart))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	blockStart, ok := eventData.(ContentBlockStartEvent)
	if !ok {
		t.Fatalf("expected ContentBlockStartEvent, got %T", eventData)
	}

	if blockStart.Index != 0 {
		t.Errorf("expected index 0, got %d", blockStart.Index)
	}

	// Parse the content block
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(blockStart.ContentBlock, &block); err != nil {
		t.Fatalf("failed to parse content_block: %v", err)
	}
	if block.Type != "text" {
		t.Errorf("expected content block type 'text', got %q", block.Type)
	}
}

func TestParseMessage_StreamEvent_ToolUseStart(t *testing.T) {
	msg, err := ParseMessage([]byte(streamToolUseStart))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	blockStart, ok := eventData.(ContentBlockStartEvent)
	if !ok {
		t.Fatalf("expected ContentBlockStartEvent, got %T", eventData)
	}

	if blockStart.Index != 1 {
		t.Errorf("expected index 1, got %d", blockStart.Index)
	}

	// Parse the content block
	var block struct {
		Input map[string]interface{} `json:"input"`
		Type  string                 `json:"type"`
		ID    string                 `json:"id"`
		Name  string                 `json:"name"`
	}
	if err := json.Unmarshal(blockStart.ContentBlock, &block); err != nil {
		t.Fatalf("failed to parse content_block: %v", err)
	}
	if block.Type != "tool_use" {
		t.Errorf("expected content block type 'tool_use', got %q", block.Type)
	}
	if block.ID != "toolu_01W8o9N6gnPxzM5fB2R6g3mo" {
		t.Errorf("unexpected tool id: %q", block.ID)
	}
	if block.Name != "WebSearch" {
		t.Errorf("unexpected tool name: %q", block.Name)
	}
}

func TestParseMessage_StreamEvent_TextDelta(t *testing.T) {
	msg, err := ParseMessage([]byte(streamTextDelta))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	delta, ok := eventData.(ContentBlockDeltaEvent)
	if !ok {
		t.Fatalf("expected ContentBlockDeltaEvent, got %T", eventData)
	}

	if delta.Index != 0 {
		t.Errorf("expected index 0, got %d", delta.Index)
	}

	// Parse the delta
	var textDelta TextDelta
	if err := json.Unmarshal(delta.Delta, &textDelta); err != nil {
		t.Fatalf("failed to parse delta: %v", err)
	}
	if textDelta.Type != "text_delta" {
		t.Errorf("expected delta type 'text_delta', got %q", textDelta.Type)
	}
	if textDelta.Text != "I'll search for the latest news about" {
		t.Errorf("unexpected text: %q", textDelta.Text)
	}
}

func TestParseMessage_StreamEvent_InputJsonDelta(t *testing.T) {
	msg, err := ParseMessage([]byte(streamInputJSONDelta))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	delta, ok := eventData.(ContentBlockDeltaEvent)
	if !ok {
		t.Fatalf("expected ContentBlockDeltaEvent, got %T", eventData)
	}

	if delta.Index != 1 {
		t.Errorf("expected index 1, got %d", delta.Index)
	}

	// Parse the delta
	var jsonDelta InputJSONDelta
	if err := json.Unmarshal(delta.Delta, &jsonDelta); err != nil {
		t.Fatalf("failed to parse delta: %v", err)
	}
	if jsonDelta.Type != "input_json_delta" {
		t.Errorf("expected delta type 'input_json_delta', got %q", jsonDelta.Type)
	}
	if jsonDelta.PartialJSON != `{"query": "US ` {
		t.Errorf("unexpected partial_json: %q", jsonDelta.PartialJSON)
	}
}

func TestParseMessage_StreamEvent_ContentBlockStop(t *testing.T) {
	msg, err := ParseMessage([]byte(streamContentBlockStop))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	blockStop, ok := eventData.(ContentBlockStopEvent)
	if !ok {
		t.Fatalf("expected ContentBlockStopEvent, got %T", eventData)
	}

	if blockStop.Index != 1 {
		t.Errorf("expected index 1, got %d", blockStop.Index)
	}
}

func TestParseMessage_StreamEvent_MessageDelta(t *testing.T) {
	msg, err := ParseMessage([]byte(streamMessageDelta))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	msgDelta, ok := eventData.(MessageDeltaEvent)
	if !ok {
		t.Fatalf("expected MessageDeltaEvent, got %T", eventData)
	}

	if msgDelta.Delta.StopReason == nil || *msgDelta.Delta.StopReason != "tool_use" {
		t.Errorf("unexpected stop_reason: %v", msgDelta.Delta.StopReason)
	}
	if msgDelta.Usage.OutputTokens != 86 {
		t.Errorf("unexpected output_tokens: %d", msgDelta.Usage.OutputTokens)
	}
}

func TestParseMessage_StreamEvent_MessageStop(t *testing.T) {
	msg, err := ParseMessage([]byte(streamMessageStop))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	streamEvent := msg.(StreamEvent)
	eventData, err := ParseStreamEvent(streamEvent.Event)
	if err != nil {
		t.Fatalf("failed to parse inner event: %v", err)
	}

	_, ok := eventData.(MessageStopEvent)
	if !ok {
		t.Fatalf("expected MessageStopEvent, got %T", eventData)
	}
}

func TestParseMessage_Assistant(t *testing.T) {
	msg, err := ParseMessage([]byte(assistantMessage))
	if err != nil {
		t.Fatalf("failed to parse assistant message: %v", err)
	}

	assistMsg, ok := msg.(AssistantMessage)
	if !ok {
		t.Fatalf("expected AssistantMessage, got %T", msg)
	}

	if assistMsg.Type != MessageTypeAssistant {
		t.Errorf("expected type 'assistant', got %q", assistMsg.Type)
	}
	if assistMsg.Message.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("unexpected model: %q", assistMsg.Message.Model)
	}
	if assistMsg.Message.ID != "msg_01UEh6PyLVYUH279j3Hj84zz" {
		t.Errorf("unexpected message id: %q", assistMsg.Message.ID)
	}
	if assistMsg.Message.Role != "assistant" {
		t.Errorf("unexpected role: %q", assistMsg.Message.Role)
	}

	blocks, ok := assistMsg.Message.Content.AsBlocks()
	if !ok {
		t.Fatal("expected content to be blocks")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(blocks))
	}

	textBlock, ok := blocks[0].(TextBlock)
	if !ok {
		t.Fatalf("expected TextBlock, got %T", blocks[0])
	}
	if textBlock.Type != ContentBlockTypeText {
		t.Errorf("expected type 'text', got %q", textBlock.Type)
	}
	expectedText := "I'll search for the latest news about US tariff rates against China, Japan, and the EU."
	if textBlock.Text != expectedText {
		t.Errorf("unexpected text: %q", textBlock.Text)
	}
}

func TestParseMessage_AssistantWithToolUse(t *testing.T) {
	msg, err := ParseMessage([]byte(assistantWithToolUse))
	if err != nil {
		t.Fatalf("failed to parse assistant message: %v", err)
	}

	assistMsg := msg.(AssistantMessage)
	blocks, ok := assistMsg.Message.Content.AsBlocks()
	if !ok {
		t.Fatal("expected content to be blocks")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(blocks))
	}

	toolBlock, ok := blocks[0].(ToolUseBlock)
	if !ok {
		t.Fatalf("expected ToolUseBlock, got %T", blocks[0])
	}
	if toolBlock.Type != ContentBlockTypeToolUse {
		t.Errorf("expected type 'tool_use', got %q", toolBlock.Type)
	}
	if toolBlock.ID != "toolu_01W8o9N6gnPxzM5fB2R6g3mo" {
		t.Errorf("unexpected id: %q", toolBlock.ID)
	}
	if toolBlock.Name != "WebSearch" {
		t.Errorf("unexpected name: %q", toolBlock.Name)
	}
	query, ok := toolBlock.Input["query"].(string)
	if !ok || query != "US tariff rates China Japan EU 2026 latest news" {
		t.Errorf("unexpected input query: %v", toolBlock.Input["query"])
	}
}

func TestParseMessage_UserToolResult(t *testing.T) {
	msg, err := ParseMessage([]byte(userToolResult))
	if err != nil {
		t.Fatalf("failed to parse user message: %v", err)
	}

	userMsg, ok := msg.(UserMessage)
	if !ok {
		t.Fatalf("expected UserMessage, got %T", msg)
	}

	if userMsg.Type != MessageTypeUser {
		t.Errorf("expected type 'user', got %q", userMsg.Type)
	}
	if userMsg.Message.Role != "user" {
		t.Errorf("unexpected role: %q", userMsg.Message.Role)
	}

	blocks, ok := userMsg.Message.Content.AsBlocks()
	if !ok {
		t.Fatal("expected content to be blocks")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(blocks))
	}

	resultBlock, ok := blocks[0].(ToolResultBlock)
	if !ok {
		t.Fatalf("expected ToolResultBlock, got %T", blocks[0])
	}
	if resultBlock.Type != ContentBlockTypeToolResult {
		t.Errorf("expected type 'tool_result', got %q", resultBlock.Type)
	}
	if resultBlock.ToolUseID != "toolu_01W8o9N6gnPxzM5fB2R6g3mo" {
		t.Errorf("unexpected tool_use_id: %q", resultBlock.ToolUseID)
	}
	if resultBlock.IsError != nil && *resultBlock.IsError {
		t.Errorf("expected is_error false, got %v", resultBlock.IsError)
	}
}

func TestParseMessage_Result(t *testing.T) {
	msg, err := ParseMessage([]byte(resultMessage))
	if err != nil {
		t.Fatalf("failed to parse result message: %v", err)
	}

	resultMsg, ok := msg.(ResultMessage)
	if !ok {
		t.Fatalf("expected ResultMessage, got %T", msg)
	}

	if resultMsg.Type != MessageTypeResult {
		t.Errorf("expected type 'result', got %q", resultMsg.Type)
	}
	if resultMsg.Subtype != "success" {
		t.Errorf("expected subtype 'success', got %q", resultMsg.Subtype)
	}
	if resultMsg.IsError {
		t.Error("expected is_error false")
	}
	if resultMsg.DurationMs != 14067 {
		t.Errorf("unexpected duration_ms: %d", resultMsg.DurationMs)
	}
	if resultMsg.NumTurns != 2 {
		t.Errorf("unexpected num_turns: %d", resultMsg.NumTurns)
	}
	if resultMsg.TotalCostUSD != 0.05407735 {
		t.Errorf("unexpected total_cost_usd: %f", resultMsg.TotalCostUSD)
	}
	if resultMsg.Usage.InputTokens != 6 {
		t.Errorf("unexpected input_tokens: %d", resultMsg.Usage.InputTokens)
	}
	if resultMsg.Usage.OutputTokens != 644 {
		t.Errorf("unexpected output_tokens: %d", resultMsg.Usage.OutputTokens)
	}
	if resultMsg.Usage.CacheReadInputTokens != 35976 {
		t.Errorf("unexpected cache_read_input_tokens: %d", resultMsg.Usage.CacheReadInputTokens)
	}

	// Check model usage
	modelUsage, ok := resultMsg.ModelUsage["claude-haiku-4-5-20251001"]
	if !ok {
		t.Fatal("expected model usage for claude-haiku-4-5-20251001")
	}
	if modelUsage.InputTokens != 2276 {
		t.Errorf("unexpected model inputTokens: %d", modelUsage.InputTokens)
	}
	if modelUsage.CostUSD != 0.05407735 {
		t.Errorf("unexpected model costUSD: %f", modelUsage.CostUSD)
	}
}

func TestParseMessage_InvalidJSON(t *testing.T) {
	_, err := ParseMessage([]byte("invalid json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseMessage_UnknownType(t *testing.T) {
	_, err := ParseMessage([]byte(`{"type":"unknown_type"}`))
	if err == nil {
		t.Error("expected error for unknown type")
	}
}
