package protocol

import (
	"encoding/json"
	"testing"
)

func TestNewUserTextMessage(t *testing.T) {
	msg := NewUserTextMessage("hello world")

	if msg.Type != "user" {
		t.Errorf("expected type 'user', got %q", msg.Type)
	}
	if msg.Message.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Message.Role)
	}
	if msg.Message.Content != "hello world" {
		t.Errorf("expected content 'hello world', got %v", msg.Message.Content)
	}
}

func TestNewUserTextMessage_Marshal(t *testing.T) {
	msg := NewUserTextMessage("ping")

	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if parsed["type"] != "user" {
		t.Errorf("expected type 'user', got %v", parsed["type"])
	}
	inner := parsed["message"].(map[string]interface{})
	if inner["role"] != "user" {
		t.Errorf("expected role 'user', got %v", inner["role"])
	}
	if inner["content"] != "ping" {
		t.Errorf("expected content 'ping', got %v", inner["content"])
	}
}

func TestNewPermissionAllow_Structure(t *testing.T) {
	input := map[string]interface{}{"command": "echo hi"}
	resp := NewPermissionAllow("req_1", input, nil)

	if resp.Type != MessageTypeControlResponse {
		t.Errorf("expected type %q, got %q", MessageTypeControlResponse, resp.Type)
	}
	if resp.Response.Subtype != "success" {
		t.Errorf("expected subtype 'success', got %q", resp.Response.Subtype)
	}
	if resp.Response.RequestID != "req_1" {
		t.Errorf("expected request_id 'req_1', got %q", resp.Response.RequestID)
	}

	allow, ok := resp.Response.Response.(PermissionResultAllow)
	if !ok {
		t.Fatalf("expected PermissionResultAllow, got %T", resp.Response.Response)
	}
	if allow.Behavior != PermissionBehaviorAllow {
		t.Errorf("expected behavior 'allow', got %q", allow.Behavior)
	}
	if allow.UpdatedInput["command"] != "echo hi" {
		t.Errorf("expected command 'echo hi', got %v", allow.UpdatedInput["command"])
	}
	if allow.UpdatedPermissions != nil {
		t.Error("expected nil UpdatedPermissions")
	}
}

func TestNewPermissionAllow_NilInputBecomesEmptyMap(t *testing.T) {
	resp := NewPermissionAllow("req_nil", nil, nil)

	allow := resp.Response.Response.(PermissionResultAllow)
	if allow.UpdatedInput == nil {
		t.Error("nil input should be normalized to empty map")
	}

	// Must serialize as object, not null
	data, _ := resp.Marshal()
	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)
	inner := parsed["response"].(map[string]interface{})["response"].(map[string]interface{})
	if inner["updatedInput"] == nil {
		t.Error("updatedInput must be an object, not null")
	}
}

func TestNewPermissionAllow_WithPermissions(t *testing.T) {
	perms := []PermissionUpdate{{Type: "setMode", Mode: "acceptEdits", Destination: "session"}}
	resp := NewPermissionAllow("req_2", map[string]interface{}{}, perms)

	allow := resp.Response.Response.(PermissionResultAllow)
	if len(allow.UpdatedPermissions) != 1 {
		t.Fatalf("expected 1 permission, got %d", len(allow.UpdatedPermissions))
	}
	if allow.UpdatedPermissions[0].Mode != "acceptEdits" {
		t.Errorf("unexpected mode: %q", allow.UpdatedPermissions[0].Mode)
	}
}

func TestNewPermissionDeny_Structure(t *testing.T) {
	resp := NewPermissionDeny("req_3", "not allowed", true)

	if resp.Response.Subtype != "success" {
		t.Errorf("expected subtype 'success', got %q", resp.Response.Subtype)
	}
	if resp.Response.RequestID != "req_3" {
		t.Errorf("expected request_id 'req_3', got %q", resp.Response.RequestID)
	}

	deny, ok := resp.Response.Response.(PermissionResultDeny)
	if !ok {
		t.Fatalf("expected PermissionResultDeny, got %T", resp.Response.Response)
	}
	if deny.Behavior != PermissionBehaviorDeny {
		t.Errorf("expected behavior 'deny', got %q", deny.Behavior)
	}
	if deny.Message != "not allowed" {
		t.Errorf("expected message 'not allowed', got %q", deny.Message)
	}
	if !deny.Interrupt {
		t.Error("expected interrupt=true")
	}
}

func TestNewPermissionDeny_Marshal(t *testing.T) {
	resp := NewPermissionDeny("req_4", "blocked", false)

	data, err := resp.Marshal()
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)

	if parsed["type"] != "control_response" {
		t.Errorf("expected type 'control_response', got %v", parsed["type"])
	}
	payload := parsed["response"].(map[string]interface{})
	if payload["subtype"] != "success" {
		t.Errorf("expected subtype 'success', got %v", payload["subtype"])
	}
	inner := payload["response"].(map[string]interface{})
	if inner["behavior"] != "deny" {
		t.Errorf("expected behavior 'deny', got %v", inner["behavior"])
	}
	if inner["message"] != "blocked" {
		t.Errorf("expected message 'blocked', got %v", inner["message"])
	}
}

func TestNewMCPResponse_Structure(t *testing.T) {
	rpcResp := JSONRPCResponse{JSONRPC: "2.0", ID: float64(1), Result: map[string]interface{}{"ok": true}}
	resp := NewMCPResponse("req_mcp", rpcResp)

	if resp.Response.Subtype != "success" {
		t.Errorf("expected subtype 'success', got %q", resp.Response.Subtype)
	}
	if resp.Response.RequestID != "req_mcp" {
		t.Errorf("expected request_id 'req_mcp', got %q", resp.Response.RequestID)
	}

	mcpPayload, ok := resp.Response.Response.(MCPResponsePayload)
	if !ok {
		t.Fatalf("expected MCPResponsePayload, got %T", resp.Response.Response)
	}
	if mcpPayload.MCPResponse == nil {
		t.Error("expected non-nil MCPResponse")
	}
}

func TestNewInterrupt_Structure(t *testing.T) {
	req := NewInterrupt("req_int")

	if req.Type != "control_request" {
		t.Errorf("expected type 'control_request', got %q", req.Type)
	}
	if req.RequestID != "req_int" {
		t.Errorf("expected request_id 'req_int', got %q", req.RequestID)
	}

	body, ok := req.Request.(InterruptRequestToSend)
	if !ok {
		t.Fatalf("expected InterruptRequestToSend, got %T", req.Request)
	}
	if body.Subtype != "interrupt" {
		t.Errorf("expected subtype 'interrupt', got %q", body.Subtype)
	}
}

func TestNewInterrupt_Marshal(t *testing.T) {
	req := NewInterrupt("req_5")
	data, err := req.Marshal()
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)
	inner := parsed["request"].(map[string]interface{})
	if inner["subtype"] != "interrupt" {
		t.Errorf("expected subtype 'interrupt', got %v", inner["subtype"])
	}
}

func TestNewSetPermissionMode_Structure(t *testing.T) {
	req := NewSetPermissionMode("req_6", "plan")

	body, ok := req.Request.(SetPermissionModeRequestToSend)
	if !ok {
		t.Fatalf("expected SetPermissionModeRequestToSend, got %T", req.Request)
	}
	if body.Subtype != "set_permission_mode" {
		t.Errorf("expected subtype 'set_permission_mode', got %q", body.Subtype)
	}
	if body.Mode != "plan" {
		t.Errorf("expected mode 'plan', got %q", body.Mode)
	}
}

func TestNewSetModel_Structure(t *testing.T) {
	req := NewSetModel("req_7", "claude-sonnet-4-6")

	body, ok := req.Request.(SetModelRequestToSend)
	if !ok {
		t.Fatalf("expected SetModelRequestToSend, got %T", req.Request)
	}
	if body.Subtype != "set_model" {
		t.Errorf("expected subtype 'set_model', got %q", body.Subtype)
	}
	if body.Model != "claude-sonnet-4-6" {
		t.Errorf("expected model 'claude-sonnet-4-6', got %q", body.Model)
	}
}
