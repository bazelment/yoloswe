package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		wantCheck func(t *testing.T, args []string) // optional custom check
		name      string
		runner    tmuxRunner
		wantBin   string
		wantArgs  []string
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
		{
			name: "claude with session ID injects notify hook",
			runner: tmuxRunner{
				model:       "opus",
				provider:    ProviderClaude,
				prompt:      "do it",
				sessionID:   "sess-1",
				brambleBin:  "/usr/bin/bramble",
				brambleSock: "/tmp/bramble-123.sock",
			},
			wantBin: "claude",
			wantCheck: func(t *testing.T, args []string) {
				for i, a := range args {
					if a == "--settings" && i+1 < len(args) {
						assert.Contains(t, args[i+1], "notify")
						assert.Contains(t, args[i+1], "sess-1")
						return
					}
				}
				t.Fatal("expected --settings arg with hook")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBin, gotArgs := tt.runner.buildCommand()
			assert.Equal(t, tt.wantBin, gotBin)
			if tt.wantCheck != nil {
				tt.wantCheck(t, gotArgs)
			} else {
				assert.Equal(t, tt.wantArgs, gotArgs)
			}
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

func TestParsePaneExitStatus(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		wantCode int
		wantOk   bool
	}{
		{
			name:     "single pane success exit",
			output:   "1 0\n",
			wantCode: 0,
			wantOk:   true,
		},
		{
			name:     "single pane failure exit code 1",
			output:   "1 1\n",
			wantCode: 1,
			wantOk:   true,
		},
		{
			name:     "single pane failure exit code 127",
			output:   "1 127\n",
			wantCode: 127,
			wantOk:   true,
		},
		{
			name:     "single pane alive",
			output:   "0 \n",
			wantCode: 0,
			wantOk:   false,
		},
		{
			name:     "multi-pane first dead with exit 0",
			output:   "1 0\n0 \n",
			wantCode: 0,
			wantOk:   true,
		},
		{
			name:     "multi-pane first alive second dead",
			output:   "0 \n1 2\n",
			wantCode: 2,
			wantOk:   true,
		},
		{
			name:     "multi-pane all alive",
			output:   "0 \n0 \n",
			wantCode: 0,
			wantOk:   false,
		},
		{
			name:     "empty output",
			output:   "",
			wantCode: 0,
			wantOk:   false,
		},
		{
			name:     "whitespace only",
			output:   "  \n",
			wantCode: 0,
			wantOk:   false,
		},
		{
			name:     "dead pane trailing space only",
			output:   "1 \n",
			wantCode: 0,
			wantOk:   false,
		},
		{
			name:     "dead pane with non-numeric status",
			output:   "1 abc\n",
			wantCode: 1,
			wantOk:   true,
		},
		{
			name:     "malformed single field line",
			output:   "1\n",
			wantCode: 0,
			wantOk:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCode, gotOk := parsePaneExitStatus(tt.output)
			assert.Equal(t, tt.wantCode, gotCode)
			assert.Equal(t, tt.wantOk, gotOk)
		})
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no ansi",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "color codes",
			input: "\x1b[32mgreen\x1b[0m text",
			want:  "green text",
		},
		{
			name:  "bold and underline",
			input: "\x1b[1mbold\x1b[22m \x1b[4munderline\x1b[24m",
			want:  "bold underline",
		},
		{
			name:  "cursor movement",
			input: "\x1b[2Kline content",
			want:  "line content",
		},
		{
			name:  "mixed content",
			input: "\x1b[36m⠋\x1b[0m Working on \x1b[1mtask\x1b[0m...",
			want:  "⠋ Working on task...",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripANSI(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseClaudeStatusBar(t *testing.T) {
	t.Parallel()

	tests := []struct { //nolint:govet // test struct
		name  string
		lines []string
		want  *PaneStatus
	}{
		{
			name: "idle session with PR",
			lines: []string{
				"  Shall I merge now?",
				"",
				"❯ ",
				"───────────────────────────────────────",
				"  ~/worktrees/kernel/feature/better-ci  feature/better-ci  Opus 4.6  ctx:43%  tokens:20k",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle) · PR #930",
			},
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "43%",
				TokenCount:  "20k",
				Branch:      "feature/better-ci",
				WorkDir:     "~/worktrees/kernel/feature/better-ci",
				PRNumber:    "930",
				IsIdle:      true,
				Permissions: "bypass permissions on",
			},
		},
		{
			name: "idle session without PR",
			lines: []string{
				"❯ ",
				"───────────────────────────────────────",
				"  ~/worktrees/yoloswe/feature/cc-tmux  feature/cc-tmux  Opus 4.6  ctx:48%  tokens:24k",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			},
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "48%",
				TokenCount:  "24k",
				Branch:      "feature/cc-tmux",
				WorkDir:     "~/worktrees/yoloswe/feature/cc-tmux",
				IsIdle:      true,
				Permissions: "bypass permissions on",
			},
		},
		{
			name: "working session with spinner",
			lines: []string{
				"● Bash(git status)",
				"───────────────────────────────────────",
				"  ~/worktrees/kernel/feature/ci  feature/ci  Opus 4.6  ctx:10%  tokens:5k",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			},
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "10%",
				TokenCount:  "5k",
				Branch:      "feature/ci",
				WorkDir:     "~/worktrees/kernel/feature/ci",
				IsWorking:   true,
				StatusLine:  "● Bash(git status)",
				Permissions: "bypass permissions on",
			},
		},
		{
			name: "just finished work — completion indicator means idle",
			lines: []string{
				"  Worktree is ready for your next task.",
				"",
				"✻ Worked for 36m 36s",
				"───────────────────────────────────────",
				"  ~/worktrees/kernel/feature/x  feature/x  Opus 4.6  ctx:26%  tokens:410k",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			},
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "26%",
				TokenCount:  "410k",
				Branch:      "feature/x",
				WorkDir:     "~/worktrees/kernel/feature/x",
				IsIdle:      true,
				StatusLine:  "✻ Worked for 36m 36s",
				Permissions: "bypass permissions on",
			},
		},
		{
			name: "actively working with spinner",
			lines: []string{
				"● Bash(pytest tests/ -x)",
				"  ⎿  Running…",
				"* Frosting… (2m 30s · ↓ 5k tokens)",
				"───────────────────────────────────────",
				"  ~/worktrees/kernel/feature/ci  feature/ci  Opus 4.6  ctx:30%  tokens:15k",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			},
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "30%",
				TokenCount:  "15k",
				Branch:      "feature/ci",
				WorkDir:     "~/worktrees/kernel/feature/ci",
				IsWorking:   true,
				StatusLine:  "* Frosting… (2m 30s · ↓ 5k tokens)",
				Permissions: "bypass permissions on",
			},
		},
		{
			name: "trailing context warning after tokens",
			lines: []string{
				"❯ ",
				"───────────────────────────────────────",
				"  ~/worktrees/kernel/feature/x  feature/x  Opus 4.6  ctx:79%  tokens:50k                                                                                         Context left until auto-compact: 5%",
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
			},
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "79%",
				TokenCount:  "50k",
				Branch:      "feature/x",
				WorkDir:     "~/worktrees/kernel/feature/x",
				IsIdle:      true,
				Permissions: "bypass permissions on",
			},
		},
		{
			name:  "too few lines",
			lines: []string{"hello"},
			want:  nil,
		},
		{
			name:  "no separator",
			lines: []string{"line1", "line2", "line3"},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseClaudeStatusBar(tt.lines)
			if tt.want == nil {
				assert.Nil(t, got)
			} else {
				assert.NotNil(t, got)
				assert.Equal(t, tt.want.Model, got.Model, "Model")
				assert.Equal(t, tt.want.ContextPct, got.ContextPct, "ContextPct")
				assert.Equal(t, tt.want.TokenCount, got.TokenCount, "TokenCount")
				assert.Equal(t, tt.want.Branch, got.Branch, "Branch")
				assert.Equal(t, tt.want.WorkDir, got.WorkDir, "WorkDir")
				assert.Equal(t, tt.want.PRNumber, got.PRNumber, "PRNumber")
				assert.Equal(t, tt.want.IsIdle, got.IsIdle, "IsIdle")
				assert.Equal(t, tt.want.IsWorking, got.IsWorking, "IsWorking")
				assert.Equal(t, tt.want.StatusLine, got.StatusLine, "StatusLine")
				assert.Equal(t, tt.want.Permissions, got.Permissions, "Permissions")
			}
		})
	}
}

func TestContentLines(t *testing.T) {
	t.Parallel()

	lines := []string{
		"● Bash(git log --oneline)",
		"  ⎿  b723247b fix(metrics): address review feedback",
		"",
		"● Branch is cleanly rebased.",
		"",
		"✻ Worked for 36m 36s",
		"",
		"───────────────────────────────────────",
		"  ~/worktrees/kernel/feature/x  feature/x  Opus 4.6  ctx:26%  tokens:410k",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}

	ps := ParseClaudeStatusBar(lines)
	require.NotNil(t, ps)

	content := ContentLines(lines, ps)
	// Should contain the tool output and agent text, but not the separator,
	// status bar, completion indicator, or blank lines.
	assert.Equal(t, []string{
		"● Bash(git log --oneline)",
		"  ⎿  b723247b fix(metrics): address review feedback",
		"● Branch is cleanly rebased.",
	}, content)
}

func TestContentLines_NilStatus(t *testing.T) {
	t.Parallel()

	lines := []string{"line1", "line2", ""}
	content := ContentLines(lines, nil)
	assert.Equal(t, []string{"line1", "line2"}, content)
}

func TestParseClaudeStatusBarWithCursor(t *testing.T) {
	t.Parallel()

	tests := []struct { //nolint:govet // test struct
		name    string
		lines   []string
		cursorY int
		want    *PaneStatus
	}{
		{
			name: "standard idle session",
			// Full pane with positional lines (including empty lines)
			lines: []string{
				"some content", // 0
				"more content", // 1
				"",             // 2
				"───────── ▪▪▪ ─", // 3 input area separator
				"❯ ", // 4 input prompt
				"───────────────────────────────────────",          // 5 status separator
				"  ~/project  main  Opus 4.6  ctx:20%  tokens:10k", // 6 info
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",  // 7 perms
				"", // 8 cursor
			},
			cursorY: 8,
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "20%",
				TokenCount:  "10k",
				Branch:      "main",
				WorkDir:     "~/project",
				IsIdle:      true,
				Permissions: "bypass permissions on",
			},
		},
		{
			name: "unfilled terminal — cursor_y < height",
			lines: []string{
				"", // 0
				" ▐▛███▜▌   Claude Code v2.1.70",         // 1 splash
				"▝▜█████▛▘  Opus 4.6 with medium effort", // 2 splash
				"  ▘▘ ▝▝    ~/worktrees/yoloswe",         // 3 splash
				"",                                       // 4
				"❯ do something tricky",                  // 5 user prompt
				"",                                       // 6
				"● Sure! Here's a tricky one-liner:",     // 7
				"",                                       // 8
				"  echo hello",                           // 9
				"",                                       // 10
				"───────── ▪▪▪ ─", // 11 input sep
				"❯ ", // 12 input prompt
				"───────────────────────────────────────",                             // 13 status sep
				"  ~/worktrees/yoloswe  feature/cc-tmux  Opus 4.6  ctx:4%  tokens:73", // 14
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",                     // 15
				"", // 16 cursor
			},
			cursorY: 16,
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "4%",
				TokenCount:  "73",
				Branch:      "feature/cc-tmux",
				WorkDir:     "~/worktrees/yoloswe",
				IsIdle:      true,
				Permissions: "bypass permissions on",
			},
		},
		{
			name: "working with completion indicator",
			lines: []string{
				"● Bash(pytest tests/)",  // 0
				"  ⎿  Running…",          // 1
				"✢ Fluttering… (4m 16s)", // 2
				"",                       // 3
				"───────── ▪▪▪ ─", // 4 input sep
				"❯ ", // 5
				"───────────────────────────────────────",          // 6 status sep
				"  ~/project  main  Opus 4.6  ctx:19%  tokens:67k", // 7
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)",  // 8
				"", // 9
			},
			cursorY: 9,
			want: &PaneStatus{
				Model:       "Opus 4.6",
				ContextPct:  "19%",
				TokenCount:  "67k",
				Branch:      "main",
				WorkDir:     "~/project",
				IsIdle:      true,
				Permissions: "bypass permissions on",
			},
		},
		{
			name:    "cursor_y too small",
			lines:   []string{"hello", "world"},
			cursorY: 1,
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseClaudeStatusBarWithCursor(tt.lines, tt.cursorY)
			if tt.want == nil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.want.Model, got.Model, "Model")
				assert.Equal(t, tt.want.ContextPct, got.ContextPct, "ContextPct")
				assert.Equal(t, tt.want.TokenCount, got.TokenCount, "TokenCount")
				assert.Equal(t, tt.want.Branch, got.Branch, "Branch")
				assert.Equal(t, tt.want.WorkDir, got.WorkDir, "WorkDir")
				assert.Equal(t, tt.want.PRNumber, got.PRNumber, "PRNumber")
				assert.Equal(t, tt.want.IsIdle, got.IsIdle, "IsIdle")
				assert.Equal(t, tt.want.IsWorking, got.IsWorking, "IsWorking")
				assert.Equal(t, tt.want.StatusLine, got.StatusLine, "StatusLine")
				assert.Equal(t, tt.want.Permissions, got.Permissions, "Permissions")
			}
		})
	}
}

func TestContentLines_SplashFiltering(t *testing.T) {
	t.Parallel()

	lines := []string{
		" ▐▛███▜▌   Claude Code v2.1.70",
		"▝▜█████▛▘  Opus 4.6 with medium effort",
		"  ▘▘ ▝▝    ~/worktrees/yoloswe",
		"❯ do something tricky",
		"● Sure! Here's a tricky one-liner:",
		"  echo hello",
		"───────── ▪▪▪ ─",
		"❯ ",
		"───────────────────────────────────────",
		"  ~/project  main  Opus 4.6  ctx:4%  tokens:73",
		"  ⏵⏵ bypass permissions on",
	}

	// Use input separator (▪▪▪) index as SepIdx
	ps := &PaneStatus{SepIdx: 6}
	content := ContentLines(lines, ps)
	// Splash lines and user prompt (❯ ...) should be filtered out.
	// Only agent output should remain.
	assert.Equal(t, []string{
		"● Sure! Here's a tricky one-liner:",
		"  echo hello",
	}, content)
}

func TestIsChromeLine_NonBreakingSpace(t *testing.T) {
	t.Parallel()

	// Claude Code sometimes uses non-breaking space after ❯
	assert.True(t, isChromeLine("❯\u00a0"), "non-breaking space after ❯")
	assert.True(t, isChromeLine("❯ "), "regular space after ❯")
	assert.True(t, isChromeLine("❯"), "bare ❯")
	assert.True(t, isChromeLine("❯ do something"), "user prompt with text")
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
