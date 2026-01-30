package reviewer

import (
	"testing"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

func TestNew(t *testing.T) {
	config := agent.DefaultConfig(agent.RoleReviewer)
	config.Model = "haiku"

	r := New(config, "test-session")

	if r.Role() != agent.RoleReviewer {
		t.Errorf("expected role %v, got %v", agent.RoleReviewer, r.Role())
	}

	if r.TotalCost() != 0 {
		t.Errorf("expected initial cost 0, got %v", r.TotalCost())
	}

	if r.TaskCount() != 0 {
		t.Errorf("expected initial task count 0, got %v", r.TaskCount())
	}
}

func TestFormatReviewPrompt(t *testing.T) {
	tests := []struct {
		name     string
		req      *protocol.ReviewRequest
		contains []string
	}{
		{
			name: "basic request",
			req: &protocol.ReviewRequest{
				Task:         "Review CLI implementation",
				FilesChanged: []string{"main.go", "cli.go"},
			},
			contains: []string{
				"Review CLI implementation",
				"main.go",
				"cli.go",
				"Files",
			},
		},
		{
			name: "request with design",
			req: &protocol.ReviewRequest{
				Task:         "Review implementation",
				FilesChanged: []string{"main.go"},
				OriginalDesign: &protocol.DesignResponse{
					Architecture: "Simple CLI architecture",
				},
			},
			contains: []string{
				"Review implementation",
				"Simple CLI architecture",
				"Original Design",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := formatReviewPrompt(tt.req)

			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("prompt should contain %q, got:\n%s", s, prompt)
				}
			}
		})
	}
}

func TestParseReviewResponse(t *testing.T) {
	tests := []struct {
		check   func(*protocol.ReviewResponse) bool
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "valid JSON with issues",
			input: `{
				"summary": "Good implementation with minor issues",
				"issues": [
					{"severity": "minor", "file": "main.go", "line": 10, "message": "Consider adding context"}
				],
				"positives": ["Clean code structure"],
				"suggestions": ["Add more comments"]
			}`,
			wantErr: false,
			check: func(r *protocol.ReviewResponse) bool {
				return len(r.Issues) == 1 && r.Issues[0].Severity == "minor"
			},
		},
		{
			name: "valid JSON no issues",
			input: `{
				"summary": "Excellent implementation",
				"issues": [],
				"positives": ["Well structured", "Good error handling"]
			}`,
			wantErr: false,
			check: func(r *protocol.ReviewResponse) bool {
				return len(r.Issues) == 0 && len(r.Positives) == 2
			},
		},
		{
			name:    "JSON in code block",
			input:   "```json\n{\"summary\": \"OK\", \"issues\": [{\"severity\": \"critical\", \"file\": \"x.go\", \"message\": \"Bug\"}]}\n```",
			wantErr: false,
			check: func(r *protocol.ReviewResponse) bool {
				return len(r.Issues) == 1 && r.Issues[0].Severity == "critical"
			},
		},
		{
			name:    "invalid JSON",
			input:   "not json at all",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := ParseReviewResponse(tt.input)

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

func TestReviewResponseHasCriticalIssues(t *testing.T) {
	tests := []struct {
		response *protocol.ReviewResponse
		name     string
		want     bool
	}{
		{
			name: "no issues",
			response: &protocol.ReviewResponse{
				Issues: []protocol.Issue{},
			},
			want: false,
		},
		{
			name: "only minor issues",
			response: &protocol.ReviewResponse{
				Issues: []protocol.Issue{
					{Severity: "minor", Message: "Style issue"},
					{Severity: "nitpick", Message: "Naming"},
				},
			},
			want: false,
		},
		{
			name: "has critical issue",
			response: &protocol.ReviewResponse{
				Issues: []protocol.Issue{
					{Severity: "minor", Message: "Style issue"},
					{Severity: "critical", Message: "Security bug"},
				},
			},
			want: true,
		},
		{
			name: "only critical issues",
			response: &protocol.ReviewResponse{
				Issues: []protocol.Issue{
					{Severity: "critical", Message: "Bug 1"},
					{Severity: "critical", Message: "Bug 2"},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.response.HasCriticalIssues()
			if got != tt.want {
				t.Errorf("HasCriticalIssues() = %v, want %v", got, tt.want)
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
			input: `{"summary": "test"}`,
			want:  `{"summary": "test"}`,
		},
		{
			name:  "JSON code block",
			input: "```json\n{\"summary\": \"test\"}\n```",
			want:  `{"summary": "test"}`,
		},
		{
			name:  "JSON with surrounding text",
			input: "Here's my review:\n{\"summary\": \"test\"}\nEnd.",
			want:  `{"summary": "test"}`,
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
