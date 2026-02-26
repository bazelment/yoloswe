package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShellSplit(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"simple command", "code", []string{"code"}},
		{"command with flag", "emacsclient -n", []string{"emacsclient", "-n"}},
		{"command with multiple flags", "code --wait --new-window", []string{"code", "--wait", "--new-window"}},
		{"double-quoted path", `"/path/to/My Editor" --wait`, []string{"/path/to/My Editor", "--wait"}},
		{"single-quoted path", `'/path/to/My Editor' --wait`, []string{"/path/to/My Editor", "--wait"}},
		{"empty string", "", nil},
		{"only spaces", "   ", nil},
		{"extra whitespace", "  code   --wait  ", []string{"code", "--wait"}},
		{"quoted flag value", `editor "--flag=hello world"`, []string{"editor", "--flag=hello world"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellSplit(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEditorCommand(t *testing.T) {
	tests := []struct {
		name       string
		editor     string
		path       string
		wantBin    string
		wantArgs   []string
	}{
		{
			name:     "simple editor",
			editor:   "code",
			path:     "/tmp/file.go",
			wantBin:  "code",
			wantArgs: []string{"/tmp/file.go"},
		},
		{
			name:     "editor with flag",
			editor:   "emacsclient -n",
			path:     "/tmp/file.go",
			wantBin:  "emacsclient",
			wantArgs: []string{"-n", "/tmp/file.go"},
		},
		{
			name:     "quoted path with spaces",
			editor:   `"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code" --wait`,
			path:     "/tmp/file.go",
			wantBin:  "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code",
			wantArgs: []string{"--wait", "/tmp/file.go"},
		},
		{
			name:     "empty editor falls back to code",
			editor:   "",
			path:     "/tmp/file.go",
			wantBin:  "code",
			wantArgs: []string{"/tmp/file.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := editorCommand(tt.editor, tt.path)
			// cmd.Args[0] is the command name as given, rest are arguments
			assert.Equal(t, tt.wantBin, cmd.Args[0])
			assert.Equal(t, tt.wantArgs, cmd.Args[1:])
		})
	}
}
