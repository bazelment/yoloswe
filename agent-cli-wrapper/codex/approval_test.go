package codex

import (
	"context"
	"testing"
)

func TestApprovalPolicy_Values(t *testing.T) {
	// Test the canonical policy values
	tests := []struct {
		policy   ApprovalPolicy
		expected string
	}{
		{ApprovalPolicyUntrusted, "untrusted"},
		{ApprovalPolicyOnFailure, "on-failure"},
		{ApprovalPolicyOnRequest, "on-request"},
		{ApprovalPolicyNever, "never"},
	}

	for _, tt := range tests {
		if string(tt.policy) != tt.expected {
			t.Errorf("ApprovalPolicy %v = %q, want %q", tt.policy, string(tt.policy), tt.expected)
		}
	}
}

func TestApprovalPolicy_DeprecatedAliases(t *testing.T) {
	// Test that deprecated aliases map to the new canonical values
	tests := []struct {
		deprecated ApprovalPolicy
		canonical  ApprovalPolicy
	}{
		{ApprovalPolicySuggest, ApprovalPolicyUntrusted},
		{ApprovalPolicyAutoEdit, ApprovalPolicyOnFailure},
		{ApprovalPolicyFullAuto, ApprovalPolicyNever},
	}

	for _, tt := range tests {
		if tt.deprecated != tt.canonical {
			t.Errorf("Deprecated alias %q should equal canonical %q", tt.deprecated, tt.canonical)
		}
	}
}

func TestAutoApproveHandler(t *testing.T) {
	handler := AutoApproveHandler()
	ctx := context.Background()

	req := &ApprovalRequest{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ToolName: "Write",
		Input:    map[string]interface{}{"path": "/test.txt"},
	}

	resp, err := handler.HandleApproval(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.Approved {
		t.Error("AutoApproveHandler should approve")
	}
}

func TestDenyAllHandler(t *testing.T) {
	handler := DenyAllHandler()
	ctx := context.Background()

	req := &ApprovalRequest{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ToolName: "Write",
		Input:    map[string]interface{}{"path": "/test.txt"},
	}

	resp, err := handler.HandleApproval(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Approved {
		t.Error("DenyAllHandler should deny")
	}
	if resp.Message == "" {
		t.Error("DenyAllHandler should provide a message")
	}
}

func TestApprovalHandlerFunc(t *testing.T) {
	called := false
	var receivedReq *ApprovalRequest

	handler := ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		called = true
		receivedReq = req
		return &ApprovalResponse{
			Approved: true,
			Message:  "approved by test",
		}, nil
	})

	ctx := context.Background()
	req := &ApprovalRequest{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ToolName: "Bash",
		Input:    map[string]interface{}{"command": "ls"},
	}

	resp, err := handler.HandleApproval(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !called {
		t.Error("handler function should be called")
	}
	if receivedReq != req {
		t.Error("handler should receive the request")
	}
	if !resp.Approved {
		t.Error("response should be approved")
	}
	if resp.Message != "approved by test" {
		t.Errorf("unexpected message: %q", resp.Message)
	}
}

func TestApprovalHandlerFunc_WithUpdatedInput(t *testing.T) {
	handler := ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		// Modify the input
		return &ApprovalResponse{
			Approved: true,
			UpdatedInput: map[string]interface{}{
				"command": "ls -la", // Modified command
			},
		}, nil
	})

	ctx := context.Background()
	req := &ApprovalRequest{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ToolName: "Bash",
		Input:    map[string]interface{}{"command": "rm -rf /"},
	}

	resp, err := handler.HandleApproval(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.UpdatedInput == nil {
		t.Fatal("UpdatedInput should not be nil")
	}
	if resp.UpdatedInput["command"] != "ls -la" {
		t.Errorf("unexpected updated command: %v", resp.UpdatedInput["command"])
	}
}

func TestApprovalHandlerFunc_ContextCancellation(t *testing.T) {
	handler := ApprovalHandlerFunc(func(ctx context.Context, req *ApprovalRequest) (*ApprovalResponse, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return &ApprovalResponse{Approved: true}, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	req := &ApprovalRequest{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ToolName: "Bash",
		Input:    map[string]interface{}{},
	}

	_, err := handler.HandleApproval(ctx, req)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestApprovalRequest_Fields(t *testing.T) {
	req := &ApprovalRequest{
		ThreadID: "thread-123",
		TurnID:   "turn-456",
		ToolName: "Write",
		Input: map[string]interface{}{
			"path":    "/test.txt",
			"content": "hello world",
		},
	}

	if req.ThreadID != "thread-123" {
		t.Errorf("ThreadID mismatch")
	}
	if req.TurnID != "turn-456" {
		t.Errorf("TurnID mismatch")
	}
	if req.ToolName != "Write" {
		t.Errorf("ToolName mismatch")
	}
	if req.Input["path"] != "/test.txt" {
		t.Errorf("Input path mismatch")
	}
}

func TestApprovalResponse_Fields(t *testing.T) {
	resp := &ApprovalResponse{
		Approved: false,
		Message:  "write access denied",
		UpdatedInput: map[string]interface{}{
			"path": "/allowed/path.txt",
		},
	}

	if resp.Approved {
		t.Error("Approved should be false")
	}
	if resp.Message != "write access denied" {
		t.Errorf("Message mismatch")
	}
	if resp.UpdatedInput["path"] != "/allowed/path.txt" {
		t.Errorf("UpdatedInput mismatch")
	}
}
