package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildShellCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want string
		args []string
	}{
		{
			name: "simple args",
			cmd:  "claude",
			args: []string{"--flag", "hello world"},
			want: "claude '--flag' 'hello world'",
		},
		{
			name: "prompt with single quote",
			cmd:  "claude",
			args: []string{"don't panic"},
			want: `claude 'don'\''t panic'`,
		},
		{
			name: "multiple single quotes",
			cmd:  "claude",
			args: []string{"it's a 'test'"},
			want: `claude 'it'\''s a '\''test'\'''`,
		},
		{
			name: "no args",
			cmd:  "claude",
			args: []string{},
			want: "claude",
		},
		{
			name: "special shell characters are safe in single quotes",
			cmd:  "claude",
			args: []string{"$(rm -rf /)", "; echo pwned"},
			want: "claude '$(rm -rf /)' '; echo pwned'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildShellCommand(tt.cmd, tt.args)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePaneDeadOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "single pane alive",
			output: "0\n",
			want:   false,
		},
		{
			name:   "single pane dead",
			output: "1\n",
			want:   true,
		},
		{
			name:   "multiple panes all alive",
			output: "0\n0\n",
			want:   false,
		},
		{
			name:   "multiple panes one dead",
			output: "0\n1\n",
			want:   true,
		},
		{
			name:   "multiple panes first dead",
			output: "1\n0\n",
			want:   true,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
		{
			name:   "whitespace only",
			output: "  \n",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePaneDeadOutput(tt.output)
			assert.Equal(t, tt.want, got)
		})
	}
}
