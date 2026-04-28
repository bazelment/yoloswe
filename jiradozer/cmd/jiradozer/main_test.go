package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/cliapp"
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

// Redaction is tested in cliapp/redact_test.go; jiradozer just composes its
// sensitive flag list into cliapp.Options.SensitiveFlags.

func TestBuildChildArgs(t *testing.T) {
	tests := []struct {
		wantContain []string
		wantOmit    []string
		name        string
		args        runArgs
	}{
		{
			name:        "thinking-level set is propagated",
			args:        runArgs{thinkingLevel: "high"},
			wantContain: []string{"--thinking-level", "high"},
		},
		{
			name:     "thinking-level empty is omitted",
			args:     runArgs{},
			wantOmit: []string{"--thinking-level"},
		},
		{
			name:        "model + thinking-level both propagated",
			args:        runArgs{modelID: "opus", thinkingLevel: "max"},
			wantContain: []string{"--model", "opus", "--thinking-level", "max"},
		},
	}

	app := &cliapp.App{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildChildArgs(app, tt.args, "/tmp/jiradozer.yaml")
			joined := ""
			for _, a := range got {
				joined += a + " "
			}
			for _, want := range tt.wantContain {
				assert.Contains(t, joined, want)
			}
			for _, want := range tt.wantOmit {
				assert.NotContains(t, joined, want)
			}
		})
	}
}
