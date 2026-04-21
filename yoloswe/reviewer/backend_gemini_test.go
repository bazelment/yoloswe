package reviewer

import (
	"testing"
)

func TestFormatGeminiToolDisplay(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    map[string]interface{}
		want     string
	}{
		{
			name:     "read_file with path",
			toolName: "read_file",
			input:    map[string]interface{}{"path": "/home/user/project/pkg/file.go"},
			want:     "read .../pkg/file.go",
		},
		{
			name:     "write_file with path",
			toolName: "write_file",
			input:    map[string]interface{}{"path": "/home/user/project/main.go"},
			want:     "write .../project/main.go",
		},
		{
			name:     "run_shell with command",
			toolName: "run_shell",
			input:    map[string]interface{}{"command": "git diff HEAD~1"},
			want:     "shell: git diff HEAD~1",
		},
		{
			name:     "run_shell with long command truncated",
			toolName: "run_shell",
			input:    map[string]interface{}{"command": "git diff HEAD~1 --name-only -- some/very/long/path/that/exceeds/limit"},
			want:     "shell: git diff HEAD~1 --name-only -- some/very/long/p...",
		},
		{
			name:     "bash with command",
			toolName: "bash",
			input:    map[string]interface{}{"command": "ls -la"},
			want:     "shell: ls -la",
		},
		{
			name:     "glob with pattern",
			toolName: "glob",
			input:    map[string]interface{}{"pattern": "**/*.go"},
			want:     "glob **/*.go",
		},
		{
			name:     "grep with pattern",
			toolName: "grep",
			input:    map[string]interface{}{"pattern": "ParseMessage"},
			want:     "grep ParseMessage",
		},
		{
			name:     "list_dir with path",
			toolName: "list_dir",
			input:    map[string]interface{}{"path": "/home/user/project"},
			want:     "ls .../user/project",
		},
		{
			name:     "web_fetch with url",
			toolName: "web_fetch",
			input:    map[string]interface{}{"url": "https://example.com/api"},
			want:     "fetch https://example.com/api",
		},
		{
			name:     "web_search with long query truncated",
			toolName: "web_search",
			input:    map[string]interface{}{"query": "this is a very long search query that exceeds the limit set by our formatter implementation"},
			want:     "search this is a very long search query that exceeds the limit s...",
		},
		{
			name:     "unknown tool with _file suffix",
			toolName: "custom_file",
			input:    nil,
			want:     "custom",
		},
		{
			name:     "unknown tool without suffix",
			toolName: "custom_thing",
			input:    nil,
			want:     "custom_thing",
		},
		{
			name:     "read_file with nil input",
			toolName: "read_file",
			input:    nil,
			want:     "read",
		},
		{
			name:     "read_file with missing path key",
			toolName: "read_file",
			input:    map[string]interface{}{"other": "value"},
			want:     "read",
		},
		{
			name:     "read_file with empty path",
			toolName: "read_file",
			input:    map[string]interface{}{"path": ""},
			want:     "read",
		},
		{
			name:     "read_file with non-string path",
			toolName: "read_file",
			input:    map[string]interface{}{"path": 42},
			want:     "read",
		},
		{
			name:     "edit with path",
			toolName: "edit",
			input:    map[string]interface{}{"path": "/home/user/project/session.go"},
			want:     "edit .../project/session.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatGeminiToolDisplay(tt.toolName, tt.input)
			if got != tt.want {
				t.Errorf("formatGeminiToolDisplay(%q, %v) = %q, want %q", tt.toolName, tt.input, got, tt.want)
			}
		})
	}
}

func TestGeminiShortName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"create_file", "create"},
		{"delete_file", "delete"},
		{"read_text", "read"},
		{"list_dir", "list"},
		{"custom_thing", "custom_thing"},
		{"tool", "tool"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := geminiShortName(tt.input)
			if got != tt.want {
				t.Errorf("geminiShortName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewGeminiBackend_StartsAndStopsCleanly(t *testing.T) {
	b := newGeminiBackend(Config{
		BackendType: BackendGemini,
		Model:       "gemini-2.5-pro",
	})
	if b == nil {
		t.Fatal("expected non-nil backend")
	}
	// Start and Stop are no-ops for Gemini
	if err := b.Start(nil); err != nil { //nolint:staticcheck
		t.Errorf("Start should be no-op, got error: %v", err)
	}
	if err := b.Stop(); err != nil {
		t.Errorf("Stop should be no-op, got error: %v", err)
	}
}
