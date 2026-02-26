package reviewer

import (
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

func TestBuildPrompt(t *testing.T) {
	tests := []struct {
		name     string
		goal     string
		contains []string
	}{
		{
			name: "with goal",
			goal: "add user authentication",
			contains: []string{
				"add user authentication",
				"experienced software engineer",
				"Review all changes on this branch",
			},
		},
		{
			name: "empty goal",
			goal: "",
			contains: []string{
				"Use commit messages to understand their purpose",
				"experienced software engineer",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := BuildPrompt(tt.goal)
			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("BuildPrompt(%q) should contain %q", tt.goal, s)
				}
			}
		})
	}
}

func TestBuildJSONPrompt(t *testing.T) {
	tests := []struct {
		name     string
		goal     string
		contains []string
	}{
		{
			name: "with goal",
			goal: "add user authentication",
			contains: []string{
				"add user authentication",
				"experienced software engineer",
				"Review all changes on this branch",
				"JSON",
				"verdict",
				"accepted",
				"rejected",
				"summary",
				"issues",
				"severity",
				"critical",
				"high",
				"medium",
				"low",
			},
		},
		{
			name: "empty goal",
			goal: "",
			contains: []string{
				"Use commit messages to understand their purpose",
				"JSON",
				"verdict",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := BuildJSONPrompt(tt.goal)
			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("BuildJSONPrompt(%q) should contain %q", tt.goal, s)
				}
			}
		})
	}
}

func TestNew_DefaultValues(t *testing.T) {
	r := New(Config{})

	if r.config.Model != "gpt-5.2-codex" {
		t.Errorf("expected default model gpt-5.2-codex, got %s", r.config.Model)
	}
	if r.config.ApprovalPolicy != codex.ApprovalPolicyOnFailure {
		t.Errorf("expected default approval policy on-failure, got %s", r.config.ApprovalPolicy)
	}
	if r.config.BackendType != BackendCodex {
		t.Errorf("expected default backend codex, got %s", r.config.BackendType)
	}
	if r.output == nil {
		t.Error("output should not be nil")
	}
	if r.renderer == nil {
		t.Error("renderer should not be nil")
	}
	if r.backend == nil {
		t.Error("backend should not be nil")
	}
}

func TestNew_WithVerbose(t *testing.T) {
	r := New(Config{Verbose: true})

	if r.renderer == nil {
		t.Error("renderer should not be nil when Verbose is true")
	}
}

func TestNew_WithCustomValues(t *testing.T) {
	r := New(Config{
		Model:          "gpt-4o",
		ApprovalPolicy: codex.ApprovalPolicyNever,
	})

	if r.config.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %s", r.config.Model)
	}
	if r.config.ApprovalPolicy != codex.ApprovalPolicyNever {
		t.Errorf("expected approval policy never, got %s", r.config.ApprovalPolicy)
	}
}

func TestNew_CursorBackend(t *testing.T) {
	r := New(Config{
		BackendType: BackendCursor,
		Model:       "cursor-default",
	})

	if r.config.BackendType != BackendCursor {
		t.Errorf("expected cursor backend, got %s", r.config.BackendType)
	}
	if r.backend == nil {
		t.Error("backend should not be nil for cursor")
	}
	// Cursor backend Start is a no-op, verify it doesn't error
	if err := r.backend.Start(nil); err != nil {
		t.Errorf("cursor start should be no-op, got error: %v", err)
	}
	if err := r.backend.Stop(); err != nil {
		t.Errorf("cursor stop should be no-op, got error: %v", err)
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStringHelper(s, substr))
}

func containsStringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
