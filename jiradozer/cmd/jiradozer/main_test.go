package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

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
