package claude

import (
	"testing"
)

func TestQuery_DefaultBypassPermissions(t *testing.T) {
	// Test that Query defaults to bypass permissions when not specified
	config := defaultConfig()

	// Verify default is PermissionModeDefault
	if config.PermissionMode != PermissionModeDefault {
		t.Errorf("expected default permission mode to be 'default', got %v", config.PermissionMode)
	}

	// Simulate what Query does: if mode is default, prepend bypass
	opts := []SessionOption{}
	for _, opt := range opts {
		opt(&config)
	}
	if config.PermissionMode == PermissionModeDefault {
		// This is what Query does
		opts = append([]SessionOption{WithPermissionMode(PermissionModeBypass)}, opts...)
		// Apply the new opts
		config2 := defaultConfig()
		for _, opt := range opts {
			opt(&config2)
		}
		if config2.PermissionMode != PermissionModeBypass {
			t.Errorf("expected Query to default to bypass, got %v", config2.PermissionMode)
		}
	}
}

func TestQuery_ExplicitPermissionModePreserved(t *testing.T) {
	// Test that explicit permission mode is preserved in Query
	config := defaultConfig()

	// Apply explicit permission mode
	opts := []SessionOption{WithPermissionMode(PermissionModePlan)}
	for _, opt := range opts {
		opt(&config)
	}

	// Simulate Query logic: only prepend bypass if mode is still default
	if config.PermissionMode == PermissionModeDefault {
		opts = append([]SessionOption{WithPermissionMode(PermissionModeBypass)}, opts...)
	}

	// Apply all options
	config2 := defaultConfig()
	for _, opt := range opts {
		opt(&config2)
	}

	// Should keep the explicit mode
	if config2.PermissionMode != PermissionModePlan {
		t.Errorf("expected explicit permission mode 'plan' to be preserved, got %v", config2.PermissionMode)
	}
}

func TestQueryResult_HasSessionID(t *testing.T) {
	// Test that QueryResult struct has SessionID field
	result := QueryResult{
		TurnResult: TurnResult{
			TurnNumber: 1,
			Success:    true,
		},
		SessionID: "test-session-123",
	}

	if result.SessionID != "test-session-123" {
		t.Errorf("expected SessionID 'test-session-123', got %q", result.SessionID)
	}

	// Test that it embeds TurnResult fields
	if result.TurnNumber != 1 {
		t.Errorf("expected TurnNumber 1, got %d", result.TurnNumber)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}
}
