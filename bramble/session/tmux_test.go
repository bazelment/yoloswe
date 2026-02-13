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

func TestBuildShellCommandCodex(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want string
		args []string
	}{
		{
			name: "codex with model",
			cmd:  "codex",
			args: []string{"--model", "gpt-5.3-codex", "fix the bug"},
			want: "codex '--model' 'gpt-5.3-codex' 'fix the bug'",
		},
		{
			name: "codex with yolo",
			cmd:  "codex",
			args: []string{"--model", "gpt-5.2", "--dangerously-bypass-approvals-and-sandbox", "build feature"},
			want: "codex '--model' 'gpt-5.2' '--dangerously-bypass-approvals-and-sandbox' 'build feature'",
		},
		{
			name: "claude with model flag",
			cmd:  "claude",
			args: []string{"--model", "opus", "--permission-mode", "plan", "plan this"},
			want: "claude '--model' 'opus' '--permission-mode' 'plan' 'plan this'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildShellCommand(tt.cmd, tt.args)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTmuxRunnerBuildCommand(t *testing.T) {
	tests := []struct {
		name     string
		runner   tmuxRunner
		wantBin  string
		wantArgs []string
	}{
		{
			name: "claude planner with model",
			runner: tmuxRunner{
				model:          "opus",
				provider:       ProviderClaude,
				permissionMode: "plan",
				prompt:         "plan this",
			},
			wantBin:  "claude",
			wantArgs: []string{"--model", "opus", "--permission-mode", "plan", "plan this"},
		},
		{
			name: "codex builder",
			runner: tmuxRunner{
				model:    "gpt-5.3-codex",
				provider: ProviderCodex,
				prompt:   "build it",
			},
			wantBin:  "codex",
			wantArgs: []string{"--model", "gpt-5.3-codex", "build it"},
		},
		{
			name: "codex with yolo",
			runner: tmuxRunner{
				model:    "gpt-5.2",
				provider: ProviderCodex,
				prompt:   "build it",
				yoloMode: true,
			},
			wantBin:  "codex",
			wantArgs: []string{"--model", "gpt-5.2", "--dangerously-bypass-approvals-and-sandbox", "build it"},
		},
		{
			name: "claude with yolo",
			runner: tmuxRunner{
				model:    "sonnet",
				provider: ProviderClaude,
				prompt:   "build it",
				yoloMode: true,
			},
			wantBin:  "claude",
			wantArgs: []string{"--model", "sonnet", "--allow-dangerously-skip-permissions", "--dangerously-skip-permissions", "build it"},
		},
		{
			name: "empty provider defaults to claude",
			runner: tmuxRunner{
				model:  "haiku",
				prompt: "quick task",
			},
			wantBin:  "claude",
			wantArgs: []string{"--model", "haiku", "quick task"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBin, gotArgs := tt.runner.buildCommand()
			assert.Equal(t, tt.wantBin, gotBin)
			assert.Equal(t, tt.wantArgs, gotArgs)
		})
	}
}

func TestTmuxRunnerStop_NoKillOnStop(t *testing.T) {
	runner := tmuxRunner{
		windowName: "test-window",
		killOnStop: false,
	}

	err := runner.Stop()
	assert.NoError(t, err)
}

func TestModelByID(t *testing.T) {
	m, ok := ModelByID("opus")
	assert.True(t, ok)
	assert.Equal(t, "opus", m.ID)
	assert.Equal(t, ProviderClaude, m.Provider)

	m, ok = ModelByID("gpt-5.3-codex")
	assert.True(t, ok)
	assert.Equal(t, "gpt-5.3-codex", m.ID)
	assert.Equal(t, ProviderCodex, m.Provider)

	_, ok = ModelByID("nonexistent")
	assert.False(t, ok)
}

func TestNextModel(t *testing.T) {
	// Starting from opus, cycle through all models
	current := "opus"
	seen := make(map[string]bool)
	for i := 0; i < len(AvailableModels); i++ {
		next := NextModel(current)
		assert.False(t, seen[next.ID], "cycle should not repeat before visiting all models")
		seen[next.ID] = true
		current = next.ID
	}
	// After a full cycle, should be back to opus
	assert.Equal(t, "opus", current)

	// Unknown model returns first model
	m := NextModel("nonexistent")
	assert.Equal(t, AvailableModels[0].ID, m.ID)
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
