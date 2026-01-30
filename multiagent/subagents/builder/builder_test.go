package builder

import (
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

func TestNew(t *testing.T) {
	config := agent.DefaultConfig(agent.RoleBuilder)
	config.Model = "haiku"

	b := New(config, "test-session")

	if b.Role() != agent.RoleBuilder {
		t.Errorf("expected role %v, got %v", agent.RoleBuilder, b.Role())
	}

	if b.TotalCost() != 0 {
		t.Errorf("expected initial cost 0, got %v", b.TotalCost())
	}

	if b.TaskCount() != 0 {
		t.Errorf("expected initial task count 0, got %v", b.TaskCount())
	}
}

func TestFormatBuildPrompt(t *testing.T) {
	tests := []struct {
		name     string
		req      *protocol.BuildRequest
		contains []string
	}{
		{
			name: "basic request",
			req: &protocol.BuildRequest{
				Task:    "Create main.go",
				WorkDir: "/tmp/test",
			},
			contains: []string{
				"Create main.go",
				"/tmp/test",
				"Working Directory",
			},
		},
		{
			name: "request with design",
			req: &protocol.BuildRequest{
				Task:    "Implement CLI",
				WorkDir: ".",
				Design: &protocol.DesignResponse{
					Architecture: "Simple CLI with flag package",
					Files: []protocol.FileSpec{
						{Path: "main.go", Purpose: "Entry point", Action: "create"},
					},
				},
			},
			contains: []string{
				"Implement CLI",
				"Simple CLI with flag package",
				"Design",
			},
		},
		{
			name: "request with feedback",
			req: &protocol.BuildRequest{
				Task:    "Fix issues",
				WorkDir: ".",
				Feedback: &protocol.ReviewResponse{
					Issues: []protocol.Issue{
						{Severity: "critical", File: "main.go", Message: "Missing error handling"},
					},
				},
			},
			contains: []string{
				"Fix issues",
				"Feedback",
				"critical",
				"Missing error handling",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := formatBuildPrompt(tt.req)

			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("prompt should contain %q, got:\n%s", s, prompt)
				}
			}
		})
	}
}

func TestParseBuildResponse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(*protocol.BuildResponse) bool
	}{
		{
			name: "valid JSON",
			input: `{
				"files_created": ["main.go"],
				"files_modified": [],
				"tests_run": true,
				"tests_passed": true,
				"test_output": "PASS",
				"notes": "All good"
			}`,
			wantErr: false,
			check: func(r *protocol.BuildResponse) bool {
				return len(r.FilesCreated) == 1 && r.TestsPassed
			},
		},
		{
			name: "JSON in code block",
			input: "```json\n{\"files_created\": [\"test.go\"], \"files_modified\": [], \"tests_run\": false, \"tests_passed\": false}\n```",
			wantErr: false,
			check: func(r *protocol.BuildResponse) bool {
				return len(r.FilesCreated) == 1 && r.FilesCreated[0] == "test.go"
			},
		},
		{
			name:    "invalid JSON",
			input:   "not json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := ParseBuildResponse(tt.input)

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
			name:  "JSON with text before",
			input: "Result: {\"key\": \"value\"}",
			want:  `{"key": "value"}`,
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
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
