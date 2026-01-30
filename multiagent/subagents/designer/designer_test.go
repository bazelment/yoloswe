package designer

import (
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

func TestNew(t *testing.T) {
	config := agent.DefaultConfig(agent.RoleDesigner)
	config.Model = "haiku"

	d := New(config, "test-session")

	if d.Role() != agent.RoleDesigner {
		t.Errorf("expected role %v, got %v", agent.RoleDesigner, d.Role())
	}

	if d.TotalCost() != 0 {
		t.Errorf("expected initial cost 0, got %v", d.TotalCost())
	}

	if d.TaskCount() != 0 {
		t.Errorf("expected initial task count 0, got %v", d.TaskCount())
	}
}

func TestFormatDesignPrompt(t *testing.T) {
	tests := []struct {
		name     string
		req      *protocol.DesignRequest
		contains []string
	}{
		{
			name: "basic request",
			req: &protocol.DesignRequest{
				Task: "Create a hello world CLI",
			},
			contains: []string{
				"Create a hello world CLI",
				"Task",
			},
		},
		{
			name: "request with context",
			req: &protocol.DesignRequest{
				Task:    "Add logging",
				Context: "This is a Go project using slog",
			},
			contains: []string{
				"Add logging",
				"Context",
				"Go project using slog",
			},
		},
		{
			name: "request with constraints",
			req: &protocol.DesignRequest{
				Task:        "Create API endpoint",
				Constraints: []string{"Must be REST", "Use JSON"},
			},
			contains: []string{
				"Create API endpoint",
				"Constraints",
				"Must be REST",
				"Use JSON",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := formatDesignPrompt(tt.req)

			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("prompt should contain %q, got:\n%s", s, prompt)
				}
			}
		})
	}
}

func TestParseDesignResponse(t *testing.T) {
	tests := []struct {
		check   func(*protocol.DesignResponse) bool
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "valid JSON",
			input: `{
				"architecture": "Simple CLI with main.go",
				"files": [{"path": "main.go", "purpose": "Entry point", "action": "create"}],
				"implementation_notes": ["Use flag package"]
			}`,
			wantErr: false,
			check: func(r *protocol.DesignResponse) bool {
				return r.Architecture == "Simple CLI with main.go" && len(r.Files) == 1
			},
		},
		{
			name:    "JSON in code block",
			input:   "```json\n{\"architecture\": \"Test\", \"files\": [], \"implementation_notes\": []}\n```",
			wantErr: false,
			check: func(r *protocol.DesignResponse) bool {
				return r.Architecture == "Test"
			},
		},
		{
			name:    "JSON with surrounding text",
			input:   "Here is the design:\n{\"architecture\": \"Design\", \"files\": [], \"implementation_notes\": []}\nDone.",
			wantErr: false,
			check: func(r *protocol.DesignResponse) bool {
				return r.Architecture == "Design"
			},
		},
		{
			name:    "invalid JSON",
			input:   "not valid json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := ParseDesignResponse(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if tt.check != nil && !tt.check(resp) {
				t.Errorf("check failed for response: %+v", resp)
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain JSON",
			input: `{"key": "value"}`,
			want:  `{"key": "value"}`,
		},
		{
			name:  "JSON code block",
			input: "```json\n{\"key\": \"value\"}\n```",
			want:  `{"key": "value"}`,
		},
		{
			name:  "generic code block",
			input: "```\n{\"key\": \"value\"}\n```",
			want:  `{"key": "value"}`,
		},
		{
			name:  "JSON with prefix",
			input: "Here is the result: {\"key\": \"value\"}",
			want:  `{"key": "value"}`,
		},
		{
			name:  "nested JSON",
			input: `{"outer": {"inner": "value"}}`,
			want:  `{"outer": {"inner": "value"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSON(tt.input)
			if got != tt.want {
				t.Errorf("extractJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
