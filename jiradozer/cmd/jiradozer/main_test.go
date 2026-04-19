package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestValidateDryRunMode(t *testing.T) {
	dryRunCfg := func() *jiradozer.Config {
		return &jiradozer.Config{Source: jiradozer.SourceConfig{DryRun: true}}
	}
	tests := []struct {
		cfg     *jiradozer.Config
		name    string
		wantErr string
		args    runArgs
	}{
		{
			name: "dry-run off: any args accepted",
			cfg:  &jiradozer.Config{Source: jiradozer.SourceConfig{DryRun: false}},
			args: runArgs{issueID: "ENG-1", description: "local task"},
		},
		{
			name: "dry-run + team mode: accepted",
			cfg:  dryRunCfg(),
			args: runArgs{},
		},
		{
			name:    "dry-run + single-issue: rejected",
			cfg:     dryRunCfg(),
			args:    runArgs{issueID: "ENG-1"},
			wantErr: "--dry-run only applies to team mode",
		},
		{
			name:    "dry-run + description: rejected",
			cfg:     dryRunCfg(),
			args:    runArgs{description: "local task"},
			wantErr: "--dry-run only applies to team mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDryRunMode(tt.cfg, tt.args)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestResolveRepoName(t *testing.T) {
	tests := []struct {
		name string
		cfg  *jiradozer.Config
		want string
	}{
		{
			name: "no team filter defaults to jiradozer",
			cfg: &jiradozer.Config{
				Source: jiradozer.SourceConfig{Filters: map[string]string{}},
			},
			want: "jiradozer",
		},
		{
			name: "linear team filter used verbatim",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "linear"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "ENG",
				}},
			},
			want: "ENG",
		},
		{
			name: "github owner/repo collapsed to repo portion",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "github"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "bazelment/yoloswe",
				}},
			},
			want: "yoloswe",
		},
		{
			name: "github malformed team falls through to raw value",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "github"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "not-an-owner-repo",
				}},
			},
			want: "not-an-owner-repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveRepoName(tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRedactArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "no sensitive flags",
			args: []string{"--issue", "ENG-123", "--verbose"},
			want: []string{"--issue", "ENG-123", "--verbose"},
		},
		{
			name: "api-key equals form",
			args: []string{"--api-key=sk-secret123", "--verbose"},
			want: []string{"--api-key=***", "--verbose"},
		},
		{
			name: "api-key space form",
			args: []string{"--api-key", "sk-secret123", "--verbose"},
			want: []string{"--api-key", "***", "--verbose"},
		},
		{
			name: "description redacted",
			args: []string{"--description", "my secret plan", "--verbose"},
			want: []string{"--description", "***", "--verbose"},
		},
		{
			name: "description equals form",
			args: []string{"--description=my secret plan"},
			want: []string{"--description=***"},
		},
		{
			name: "multiple sensitive flags",
			args: []string{"--token", "tok123", "--password=hunter2"},
			want: []string{"--token", "***", "--password=***"},
		},
		{
			name: "sensitive flag at end without value",
			args: []string{"--verbose", "--api-key"},
			want: []string{"--verbose", "--api-key"},
		},
		{
			name: "empty args",
			args: []string{},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactArgs(tt.args)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRunSingleStepRounds_RejectsLiveBackgroundWork asserts the multi-round
// single-step CLI path refuses to advance past a round whose agent result
// reports HasLiveBackgroundWork.
func TestRunSingleStepRounds_RejectsLiveBackgroundWork(t *testing.T) {
	prev := runStepAgentDetailed
	t.Cleanup(func() { runStepAgentDetailed = prev })

	var calls int
	runStepAgentDetailed = func(_ context.Context, _ string, _ jiradozer.PromptData, _ jiradozer.StepConfig, _, _, _ string, _ *render.Renderer, _ *slog.Logger) (jiradozer.StepAgentResult, error) {
		calls++
		return jiradozer.StepAgentResult{
			Output:                "round output",
			SessionID:             "sess-live",
			HasLiveBackgroundWork: true,
		}, nil
	}

	resolved := jiradozer.StepConfig{
		Rounds: []jiradozer.RoundConfig{
			{Prompt: "first round"},
			{Prompt: "second round"}, // must NOT run — first round's guard must short-circuit.
		},
	}

	err := runSingleStepRounds(context.Background(), "plan", jiradozer.PromptData{}, resolved, t.TempDir(), nil, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "live background work")
	assert.Contains(t, err.Error(), "sess-live")
	assert.Equal(t, 1, calls, "guard must short-circuit before round 2")
}

// TestRunSingleStep_RejectsLiveBackgroundWork asserts the non-round single-step
// CLI path (runSingleStep → RunStepAgentDetailed) also refuses to advance.
func TestRunSingleStep_RejectsLiveBackgroundWork(t *testing.T) {
	prev := runStepAgentDetailed
	t.Cleanup(func() { runStepAgentDetailed = prev })

	runStepAgentDetailed = func(_ context.Context, _ string, _ jiradozer.PromptData, _ jiradozer.StepConfig, _, _, _ string, _ *render.Renderer, _ *slog.Logger) (jiradozer.StepAgentResult, error) {
		return jiradozer.StepAgentResult{
			Output:                "oneshot output",
			SessionID:             "sess-oneshot",
			HasLiveBackgroundWork: true,
		}, nil
	}

	cfg := &jiradozer.Config{
		Plan: jiradozer.StepConfig{Prompt: "inline"},
	}
	issue := &tracker.Issue{Identifier: "ENG-1", Title: "t"}

	err := runSingleStep(context.Background(), "plan", issue, cfg, "", nil, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "live background work")
	assert.Contains(t, err.Error(), "sess-oneshot")
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
