package reviewer

import (
	"testing"
)

func TestFormatCursorToolDisplay(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    map[string]interface{}
		want     string
	}{
		{
			name:     "read with path",
			toolName: "readToolCall",
			input:    map[string]interface{}{"path": "/home/user/project/pkg/file.go"},
			want:     "read .../pkg/file.go",
		},
		{
			name:     "shell with command",
			toolName: "shellToolCall",
			input:    map[string]interface{}{"command": "git diff HEAD~1"},
			want:     "shell: git diff HEAD~1",
		},
		{
			name:     "shell with long command truncated",
			toolName: "shellToolCall",
			input:    map[string]interface{}{"command": "git diff HEAD~1 --name-only -- some/very/long/path/that/exceeds/limit"},
			want:     "shell: git diff HEAD~1 --name-only -- some/very/long/p...",
		},
		{
			name:     "glob with pattern",
			toolName: "globToolCall",
			input:    map[string]interface{}{"globPattern": "**/*.go"},
			want:     "glob **/*.go",
		},
		{
			name:     "grep with pattern",
			toolName: "grepToolCall",
			input:    map[string]interface{}{"pattern": "ParseMessage"},
			want:     "grep ParseMessage",
		},
		{
			name:     "edit with path",
			toolName: "editToolCall",
			input:    map[string]interface{}{"path": "/home/user/project/session.go"},
			want:     "edit .../project/session.go",
		},
		{
			name:     "write with path",
			toolName: "writeToolCall",
			input:    map[string]interface{}{"path": "/home/user/new_file.go"},
			want:     "write .../user/new_file.go",
		},
		{
			name:     "updateTodos no arg",
			toolName: "updateTodosToolCall",
			input:    map[string]interface{}{},
			want:     "updateTodos",
		},
		{
			name:     "unknown tool with ToolCall suffix",
			toolName: "listFilesToolCall",
			input:    nil,
			want:     "listFiles",
		},
		{
			name:     "unknown tool without suffix",
			toolName: "customThing",
			input:    nil,
			want:     "customThing",
		},
		{
			name:     "read with nil input",
			toolName: "readToolCall",
			input:    nil,
			want:     "read",
		},
		{
			name:     "read with missing path key",
			toolName: "readToolCall",
			input:    map[string]interface{}{"other": "value"},
			want:     "read",
		},
		{
			name:     "read with empty path",
			toolName: "readToolCall",
			input:    map[string]interface{}{"path": ""},
			want:     "read",
		},
		{
			name:     "read with non-string path",
			toolName: "readToolCall",
			input:    map[string]interface{}{"path": 42},
			want:     "read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatCursorToolDisplay(tt.toolName, tt.input)
			if got != tt.want {
				t.Errorf("formatCursorToolDisplay(%q, %v) = %q, want %q", tt.toolName, tt.input, got, tt.want)
			}
		})
	}
}

func TestShortPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/home/user/project/pkg/file.go", ".../pkg/file.go"},
		{"file.go", "file.go"},
		{"/root/file.go", ".../root/file.go"},
		{"/a/b/c/d/e.go", ".../d/e.go"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shortPath(tt.input)
			if got != tt.want {
				t.Errorf("shortPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSerializeToolInput(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]interface{}
		want  string
	}{
		{"nil input", nil, ""},
		{"empty input", map[string]interface{}{}, ""},
		{"single key", map[string]interface{}{"path": "/foo"}, `{"path":"/foo"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serializeToolInput(tt.input)
			if got != tt.want {
				t.Errorf("serializeToolInput(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
