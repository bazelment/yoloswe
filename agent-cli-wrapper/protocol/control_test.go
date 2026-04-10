package protocol

import (
	"encoding/json"
	"testing"
)

func TestPermissionResultAllow_WithNilUpdatedInput(t *testing.T) {
	// Per Python SDK behavior: updatedInput must be an object, never null
	// The permission manager should handle nil -> original input fallback
	// But at the protocol level, nil serializes to null
	result := PermissionResultAllow{
		Behavior:           PermissionBehaviorAllow,
		UpdatedInput:       nil,
		UpdatedPermissions: nil,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Parse back to verify structure
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Verify behavior is present
	if parsed["behavior"] != "allow" {
		t.Errorf("expected behavior 'allow', got %v", parsed["behavior"])
	}

	// updatedInput with nil serializes to null (but should be avoided by permission manager)
	if _, exists := parsed["updatedInput"]; !exists {
		t.Error("updatedInput field must be present")
	}

	// updatedPermissions with nil should be omitted (per Python SDK)
	if _, exists := parsed["updatedPermissions"]; exists {
		t.Error("updatedPermissions should be omitted when nil")
	}
}

func TestPermissionResultAllow_WithEmptyInput(t *testing.T) {
	// Test with empty map - this is valid per CLI schema
	result := PermissionResultAllow{
		Behavior:           PermissionBehaviorAllow,
		UpdatedInput:       map[string]interface{}{},
		UpdatedPermissions: nil,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// updatedInput should be an empty object
	updatedInput, exists := parsed["updatedInput"]
	if !exists {
		t.Error("updatedInput field must be present")
	}
	if updatedInput == nil {
		t.Error("updatedInput should be empty object, not null")
	}

	// updatedPermissions should be omitted
	if _, exists := parsed["updatedPermissions"]; exists {
		t.Error("updatedPermissions should be omitted when nil")
	}
}

func TestPermissionResultAllow_WithValues(t *testing.T) {
	// Test with actual values - the normal case
	result := PermissionResultAllow{
		Behavior: PermissionBehaviorAllow,
		UpdatedInput: map[string]interface{}{
			"command": "echo hello",
		},
		UpdatedPermissions: []PermissionUpdate{
			{
				Type:        "setMode",
				Mode:        "acceptEdits",
				Destination: "session",
			},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Parse back
	var parsed PermissionResultAllow
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.Behavior != PermissionBehaviorAllow {
		t.Errorf("expected behavior 'allow', got %v", parsed.Behavior)
	}
	if parsed.UpdatedInput["command"] != "echo hello" {
		t.Errorf("expected command 'echo hello', got %v", parsed.UpdatedInput["command"])
	}
	if len(parsed.UpdatedPermissions) != 1 {
		t.Errorf("expected 1 permission update, got %d", len(parsed.UpdatedPermissions))
	}
	if parsed.UpdatedPermissions[0].Mode != "acceptEdits" {
		t.Errorf("expected mode 'acceptEdits', got %v", parsed.UpdatedPermissions[0].Mode)
	}
}

func TestPermissionResultAllow_MatchesPythonSDKFormat(t *testing.T) {
	// Test exact format that Python SDK sends:
	// {"behavior": "allow", "updatedInput": {"command": "echo hi"}}
	// Note: no updatedPermissions when nil
	result := PermissionResultAllow{
		Behavior: PermissionBehaviorAllow,
		UpdatedInput: map[string]interface{}{
			"command": "echo hi",
		},
		UpdatedPermissions: nil,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Check raw JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Should have exactly 2 keys: behavior and updatedInput
	if len(parsed) != 2 {
		t.Errorf("expected 2 keys (behavior, updatedInput), got %d: %v", len(parsed), parsed)
	}

	if parsed["behavior"] != "allow" {
		t.Errorf("expected behavior 'allow', got %v", parsed["behavior"])
	}

	updatedInput := parsed["updatedInput"].(map[string]interface{})
	if updatedInput["command"] != "echo hi" {
		t.Errorf("expected command 'echo hi', got %v", updatedInput["command"])
	}
}

func TestPermissionResultDeny_JSONSerialization(t *testing.T) {
	result := PermissionResultDeny{
		Behavior:  PermissionBehaviorDeny,
		Message:   "Permission denied",
		Interrupt: false,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["behavior"] != "deny" {
		t.Errorf("expected behavior 'deny', got %v", parsed["behavior"])
	}
	if parsed["message"] != "Permission denied" {
		t.Errorf("expected message 'Permission denied', got %v", parsed["message"])
	}
}

func TestParseToolUseRequest(t *testing.T) {
	tests := []struct {
		name         string
		expectedTool string
		request      ControlRequest
		expectNil    bool
	}{
		{
			name: "can_use_tool request",
			request: ControlRequest{
				Type:      MessageTypeControlRequest,
				RequestID: "req_123",
				Request:   json.RawMessage(`{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"echo hi"}}`),
			},
			expectNil:    false,
			expectedTool: "Bash",
		},
		{
			name: "AskUserQuestion request",
			request: ControlRequest{
				Type:      MessageTypeControlRequest,
				RequestID: "req_456",
				Request:   json.RawMessage(`{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[]}}`),
			},
			expectNil:    false,
			expectedTool: "AskUserQuestion",
		},
		{
			name: "ExitPlanMode request",
			request: ControlRequest{
				Type:      MessageTypeControlRequest,
				RequestID: "req_789",
				Request:   json.RawMessage(`{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{}}`),
			},
			expectNil:    false,
			expectedTool: "ExitPlanMode",
		},
		{
			name: "set_permission_mode request - returns nil",
			request: ControlRequest{
				Type:      MessageTypeControlRequest,
				RequestID: "req_000",
				Request:   json.RawMessage(`{"subtype":"set_permission_mode","mode":"plan"}`),
			},
			expectNil: true,
		},
		{
			name: "interrupt request - returns nil",
			request: ControlRequest{
				Type:      MessageTypeControlRequest,
				RequestID: "req_111",
				Request:   json.RawMessage(`{"subtype":"interrupt"}`),
			},
			expectNil: true,
		},
		{
			name: "invalid JSON - returns nil",
			request: ControlRequest{
				Type:      MessageTypeControlRequest,
				RequestID: "req_222",
				Request:   json.RawMessage(`not valid json`),
			},
			expectNil: true,
		},
		{
			name: "with blocked_path",
			request: ControlRequest{
				Type:      MessageTypeControlRequest,
				RequestID: "req_333",
				Request:   json.RawMessage(`{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/etc/passwd"},"blocked_path":"/etc/passwd"}`),
			},
			expectNil:    false,
			expectedTool: "Write",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseToolUseRequest(tt.request)

			if tt.expectNil {
				if result != nil {
					t.Errorf("expected nil, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("expected non-nil result")
			}

			if result.RequestID != tt.request.RequestID {
				t.Errorf("expected request ID %q, got %q", tt.request.RequestID, result.RequestID)
			}

			if result.ToolName != tt.expectedTool {
				t.Errorf("expected tool name %q, got %q", tt.expectedTool, result.ToolName)
			}

			if result.Input == nil {
				t.Error("expected non-nil input")
			}
		})
	}
}

func TestParseToolUseRequest_BlockedPath(t *testing.T) {
	blockedPath := "/etc/passwd"
	request := ControlRequest{
		Type:      MessageTypeControlRequest,
		RequestID: "req_blocked",
		Request:   json.RawMessage(`{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/etc/passwd"},"blocked_path":"/etc/passwd"}`),
	}

	result := ParseToolUseRequest(request)
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	if result.BlockedPath == nil {
		t.Fatal("expected non-nil blocked_path")
	}

	if *result.BlockedPath != blockedPath {
		t.Errorf("expected blocked_path %q, got %q", blockedPath, *result.BlockedPath)
	}
}

func TestParseControlRequest_NewSubtypes(t *testing.T) {
	tests := []struct {
		verify func(t *testing.T, r ControlRequestData)
		name   string
		raw    string
	}{
		{
			name: "initialize",
			raw:  `{"subtype":"initialize","promptSuggestions":true}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(InitializeRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.PromptSuggestions == nil || !*v.PromptSuggestions {
					t.Errorf("promptSuggestions: %v", v.PromptSuggestions)
				}
			},
		},
		{
			name: "set_max_thinking_tokens",
			raw:  `{"subtype":"set_max_thinking_tokens","max_thinking_tokens":8000}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(SetMaxThinkingTokensRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.MaxThinkingTokens == nil || *v.MaxThinkingTokens != 8000 {
					t.Errorf("max_thinking_tokens: %v", v.MaxThinkingTokens)
				}
			},
		},
		{
			name: "mcp_status",
			raw:  `{"subtype":"mcp_status"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				if _, ok := r.(MCPStatusRequest); !ok {
					t.Fatalf("type: %T", r)
				}
			},
		},
		{
			name: "mcp_set_servers",
			raw:  `{"subtype":"mcp_set_servers","servers":{"foo":{"type":"stdio","command":"x"}}}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(MCPSetServersRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if _, exists := v.Servers["foo"]; !exists {
					t.Errorf("missing server foo: %v", v.Servers)
				}
			},
		},
		{
			name: "mcp_reconnect",
			raw:  `{"subtype":"mcp_reconnect","serverName":"foo"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(MCPReconnectRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.ServerName != "foo" {
					t.Errorf("serverName: %q", v.ServerName)
				}
			},
		},
		{
			name: "mcp_toggle",
			raw:  `{"subtype":"mcp_toggle","serverName":"foo","enabled":true}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(MCPToggleRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.ServerName != "foo" || !v.Enabled {
					t.Errorf("fields: %+v", v)
				}
			},
		},
		{
			name: "get_context_usage",
			raw:  `{"subtype":"get_context_usage"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				if _, ok := r.(GetContextUsageRequest); !ok {
					t.Fatalf("type: %T", r)
				}
			},
		},
		{
			name: "reload_plugins",
			raw:  `{"subtype":"reload_plugins"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				if _, ok := r.(ReloadPluginsRequest); !ok {
					t.Fatalf("type: %T", r)
				}
			},
		},
		{
			name: "rewind_files",
			raw:  `{"subtype":"rewind_files","user_message_id":"um_1","dry_run":true}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(RewindFilesRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.UserMessageID != "um_1" {
					t.Errorf("user_message_id: %q", v.UserMessageID)
				}
				if v.DryRun == nil || !*v.DryRun {
					t.Errorf("dry_run: %v", v.DryRun)
				}
			},
		},
		{
			name: "cancel_async_message",
			raw:  `{"subtype":"cancel_async_message","message_uuid":"mu_1"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(CancelAsyncMessageRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.MessageUUID != "mu_1" {
					t.Errorf("message_uuid: %q", v.MessageUUID)
				}
			},
		},
		{
			name: "seed_read_state",
			raw:  `{"subtype":"seed_read_state","path":"/tmp/a","mtime":1234.5}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(SeedReadStateRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.Path != "/tmp/a" || v.Mtime != 1234.5 {
					t.Errorf("fields: %+v", v)
				}
			},
		},
		{
			name: "hook_callback",
			raw:  `{"subtype":"hook_callback","callback_id":"cb_1","input":{"k":"v"}}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(HookCallbackRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.CallbackID != "cb_1" {
					t.Errorf("callback_id: %q", v.CallbackID)
				}
				if v.Input["k"] != "v" {
					t.Errorf("input: %v", v.Input)
				}
			},
		},
		{
			name: "get_settings",
			raw:  `{"subtype":"get_settings"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				if _, ok := r.(GetSettingsRequest); !ok {
					t.Fatalf("type: %T", r)
				}
			},
		},
		{
			name: "apply_flag_settings",
			raw:  `{"subtype":"apply_flag_settings","settings":{"foo":1}}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(ApplyFlagSettingsRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.Settings["foo"] == nil {
					t.Errorf("settings: %v", v.Settings)
				}
			},
		},
		{
			name: "stop_task",
			raw:  `{"subtype":"stop_task","task_id":"t1"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(StopTaskRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.TaskID != "t1" {
					t.Errorf("task_id: %q", v.TaskID)
				}
			},
		},
		{
			name: "elicitation",
			raw:  `{"subtype":"elicitation","mcp_server_name":"m","message":"hi","elicitation_id":"el_1"}`,
			verify: func(t *testing.T, r ControlRequestData) {
				v, ok := r.(ElicitationRequest)
				if !ok {
					t.Fatalf("type: %T", r)
				}
				if v.MCPServerName != "m" || v.Message != "hi" || v.ElicitationID != "el_1" {
					t.Errorf("fields: %+v", v)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := ParseControlRequest(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tt.verify(t, r)
		})
	}
}

func TestParseControlRequest_UnknownSubtype(t *testing.T) {
	raw := `{"subtype":"brand_new_subtype","foo":"bar"}`
	r, err := ParseControlRequest(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	u, ok := r.(UnknownControlRequest)
	if !ok {
		t.Fatalf("expected UnknownControlRequest, got %T", r)
	}
	if u.Subtype() != ControlRequestSubtype("brand_new_subtype") {
		t.Errorf("subtype: %q", u.Subtype())
	}
	if len(u.Raw) == 0 {
		t.Fatal("expected Raw populated")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(u.Raw, &parsed); err != nil {
		t.Fatalf("Raw not valid JSON: %v", err)
	}
	if parsed["foo"] != "bar" {
		t.Errorf("Raw missing fields: %v", parsed)
	}
}

func TestControlResponse_FullStructure(t *testing.T) {
	// Test the complete control response structure with proper input
	response := ControlResponse{
		Type: MessageTypeControlResponse,
		Response: ControlResponsePayload{
			Subtype:   "success",
			RequestID: "req_123",
			Response: PermissionResultAllow{
				Behavior: PermissionBehaviorAllow,
				UpdatedInput: map[string]interface{}{
					"command": "echo hello",
				},
				UpdatedPermissions: nil, // Will be omitted
			},
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Verify the JSON structure matches what CLI expects
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["type"] != "control_response" {
		t.Errorf("expected type 'control_response', got %v", parsed["type"])
	}

	respPayload := parsed["response"].(map[string]interface{})
	if respPayload["subtype"] != "success" {
		t.Errorf("expected subtype 'success', got %v", respPayload["subtype"])
	}
	if respPayload["request_id"] != "req_123" {
		t.Errorf("expected request_id 'req_123', got %v", respPayload["request_id"])
	}

	innerResp := respPayload["response"].(map[string]interface{})
	if innerResp["behavior"] != "allow" {
		t.Errorf("expected behavior 'allow', got %v", innerResp["behavior"])
	}

	// updatedInput must be present as an object
	updatedInput, exists := innerResp["updatedInput"]
	if !exists {
		t.Error("updatedInput must be present in response")
	}
	if updatedInput == nil {
		t.Error("updatedInput must be an object, not null")
	}

	// updatedPermissions should be omitted when nil
	if _, exists := innerResp["updatedPermissions"]; exists {
		t.Error("updatedPermissions should be omitted when nil")
	}
}
