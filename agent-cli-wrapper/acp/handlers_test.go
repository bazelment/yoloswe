package acp

import (
	"context"
	"testing"
)

func TestPlanOnlyPermissionHandler_ReadOperations(t *testing.T) {
	handler := &PlanOnlyPermissionHandler{}

	readOnlyTools := []string{"read_file", "read_text_file", "list_directory", "glob", "grep"}

	for _, toolName := range readOnlyTools {
		t.Run(toolName, func(t *testing.T) {
			req := RequestPermissionRequest{
				ToolCall: ToolCallInfo{
					ToolName: toolName,
				},
				Options: []PermissionOption{
					{ID: "allow-1", Name: "Allow", Kind: "allow_once"},
					{ID: "reject-1", Name: "Reject", Kind: "reject_once"},
				},
			}

			resp, err := handler.RequestPermission(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp.Outcome.Type != "selected" {
				t.Errorf("expected outcome type 'selected', got '%s'", resp.Outcome.Type)
			}

			if resp.Outcome.OptionID != "allow-1" {
				t.Errorf("expected to select 'allow-1', got '%s'", resp.Outcome.OptionID)
			}
		})
	}
}

func TestPlanOnlyPermissionHandler_WriteOperations(t *testing.T) {
	handler := &PlanOnlyPermissionHandler{}

	writeTools := []string{"write_file", "write_text_file", "bash_command", "execute_command", "delete_file"}

	for _, toolName := range writeTools {
		t.Run(toolName, func(t *testing.T) {
			req := RequestPermissionRequest{
				ToolCall: ToolCallInfo{
					ToolName: toolName,
				},
				Options: []PermissionOption{
					{ID: "allow-1", Name: "Allow", Kind: "allow_once"},
					{ID: "reject-1", Name: "Reject", Kind: "reject_once"},
				},
			}

			resp, err := handler.RequestPermission(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp.Outcome.Type != "selected" {
				t.Errorf("expected outcome type 'selected', got '%s'", resp.Outcome.Type)
			}

			if resp.Outcome.OptionID != "reject-1" {
				t.Errorf("expected to select 'reject-1', got '%s'", resp.Outcome.OptionID)
			}
		})
	}
}

func TestPlanOnlyPermissionHandler_UnknownTool(t *testing.T) {
	handler := &PlanOnlyPermissionHandler{}

	req := RequestPermissionRequest{
		ToolCall: ToolCallInfo{
			ToolName: "unknown_tool",
		},
		Options: []PermissionOption{
			{ID: "allow-1", Name: "Allow", Kind: "allow_once"},
			{ID: "reject-1", Name: "Reject", Kind: "reject_once"},
		},
	}

	resp, err := handler.RequestPermission(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Outcome.Type != "selected" {
		t.Errorf("expected outcome type 'selected', got '%s'", resp.Outcome.Type)
	}

	// Unknown tools should be rejected
	if resp.Outcome.OptionID != "reject-1" {
		t.Errorf("expected to select 'reject-1', got '%s'", resp.Outcome.OptionID)
	}
}

func TestPlanOnlyPermissionHandler_NoRejectOption(t *testing.T) {
	handler := &PlanOnlyPermissionHandler{}

	req := RequestPermissionRequest{
		ToolCall: ToolCallInfo{
			ToolName: "write_file",
		},
		Options: []PermissionOption{
			{ID: "allow-1", Name: "Allow", Kind: "allow_once"},
			{ID: "allow-2", Name: "Allow Always", Kind: "allow_always"},
		},
	}

	resp, err := handler.RequestPermission(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should cancel if no reject option is available for write operations
	if resp.Outcome.Type != "cancelled" {
		t.Errorf("expected outcome type 'cancelled', got '%s'", resp.Outcome.Type)
	}
}

func TestPlanOnlyPermissionHandler_NoAllowOption(t *testing.T) {
	handler := &PlanOnlyPermissionHandler{}

	req := RequestPermissionRequest{
		ToolCall: ToolCallInfo{
			ToolName: "read_file",
		},
		Options: []PermissionOption{
			{ID: "reject-1", Name: "Reject", Kind: "reject_once"},
			{ID: "reject-2", Name: "Reject Always", Kind: "reject_always"},
		},
	}

	resp, err := handler.RequestPermission(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should reject if no allow option is available even for read operations
	if resp.Outcome.Type != "selected" {
		t.Errorf("expected outcome type 'selected', got '%s'", resp.Outcome.Type)
	}

	if resp.Outcome.OptionID != "reject-1" {
		t.Errorf("expected to select 'reject-1', got '%s'", resp.Outcome.OptionID)
	}
}

func TestPlanOnlyPermissionHandler_ToolNameInCallID(t *testing.T) {
	// Test that the handler extracts tool names from ToolCallID when ToolName is empty
	// This is how Gemini CLI often encodes tool names: "tool_name-timestamp"
	handler := &PlanOnlyPermissionHandler{}

	testCases := []struct {
		name        string
		toolCallID  string
		shouldAllow bool
	}{
		{"read_file from ID", "read_file-1770849300776", true},
		{"write_file from ID", "write_file-1770849300776", false},
		{"grep from ID", "grep-1770849300777", true},
		{"bash_command from ID", "bash_command-1770849300778", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := RequestPermissionRequest{
				ToolCall: ToolCallInfo{
					ToolName:   "", // Empty - tool name is in ToolCallID
					ToolCallID: tc.toolCallID,
				},
				Options: []PermissionOption{
					{ID: "allow-1", Name: "Allow", Kind: "allow_once"},
					{ID: "reject-1", Name: "Reject", Kind: "reject_once"},
				},
			}

			resp, err := handler.RequestPermission(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp.Outcome.Type != "selected" {
				t.Errorf("expected outcome type 'selected', got '%s'", resp.Outcome.Type)
			}

			expectedOption := "reject-1"
			if tc.shouldAllow {
				expectedOption = "allow-1"
			}

			if resp.Outcome.OptionID != expectedOption {
				t.Errorf("expected to select '%s', got '%s'", expectedOption, resp.Outcome.OptionID)
			}
		})
	}
}

func TestBypassPermissionHandler_AllowsEverything(t *testing.T) {
	handler := &BypassPermissionHandler{}

	tools := []string{"read_file", "write_file", "bash_command", "unknown_tool"}

	for _, toolName := range tools {
		t.Run(toolName, func(t *testing.T) {
			req := RequestPermissionRequest{
				ToolCall: ToolCallInfo{
					ToolName: toolName,
				},
				Options: []PermissionOption{
					{ID: "allow-1", Name: "Allow", Kind: "allow_once"},
					{ID: "reject-1", Name: "Reject", Kind: "reject_once"},
				},
			}

			resp, err := handler.RequestPermission(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp.Outcome.Type != "selected" {
				t.Errorf("expected outcome type 'selected', got '%s'", resp.Outcome.Type)
			}

			if resp.Outcome.OptionID != "allow-1" {
				t.Errorf("expected to select 'allow-1', got '%s'", resp.Outcome.OptionID)
			}
		})
	}
}
