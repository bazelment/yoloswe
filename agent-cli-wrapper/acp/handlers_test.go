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
