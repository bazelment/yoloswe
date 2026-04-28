package cliapp

import (
	"log/slog"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

func TestResolveVerbosity(t *testing.T) {
	t.Parallel()
	tests := []struct { //nolint:govet // fieldalignment: readability
		name      string
		verbosity string
		want      render.Verbosity
		verbose   bool
		wantErr   bool
	}{
		{name: "default", verbosity: "normal", want: render.VerbosityNormal},
		{name: "empty defaults to normal", verbosity: "", want: render.VerbosityNormal},
		{name: "quiet", verbosity: "quiet", want: render.VerbosityQuiet},
		{name: "verbose explicit", verbosity: "verbose", want: render.VerbosityVerbose},
		{name: "debug", verbosity: "debug", want: render.VerbosityDebug},
		{name: "verbose flag promotes normal to verbose", verbose: true, verbosity: "normal", want: render.VerbosityVerbose},
		{name: "verbose flag does not downgrade debug", verbose: true, verbosity: "debug", want: render.VerbosityDebug},
		{name: "verbose flag does not downgrade verbose", verbose: true, verbosity: "verbose", want: render.VerbosityVerbose},
		{name: "verbose flag with quiet upgrades to verbose", verbose: true, verbosity: "quiet", want: render.VerbosityVerbose},
		{name: "unknown rejected", verbosity: "loud", wantErr: true},
		{name: "typo rejected", verbosity: "verbosee", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveVerbosity(tt.verbose, tt.verbosity)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveVerbosity(%v, %q) want error, got nil", tt.verbose, tt.verbosity)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveVerbosity(%v, %q) unexpected error: %v", tt.verbose, tt.verbosity, err)
			}
			if got != tt.want {
				t.Errorf("resolveVerbosity(%v, %q) = %v, want %v", tt.verbose, tt.verbosity, got, tt.want)
			}
		})
	}
}

func TestResolveColor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    render.ColorMode
		wantErr bool
	}{
		{name: "auto", input: "auto", want: render.ColorAuto},
		{name: "empty defaults to auto", input: "", want: render.ColorAuto},
		{name: "always", input: "always", want: render.ColorAlways},
		{name: "never", input: "never", want: render.ColorNever},
		{name: "unknown rejected", input: "rainbow", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveColor(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveColor(%q) want error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveColor(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("resolveColor(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestStderrLevelFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    render.Verbosity
		want slog.Level
	}{
		{render.VerbosityQuiet, slog.LevelWarn},
		{render.VerbosityNormal, slog.LevelInfo},
		{render.VerbosityVerbose, slog.LevelInfo},
		{render.VerbosityDebug, slog.LevelDebug},
	}
	for _, tt := range tests {
		t.Run(tt.v.String(), func(t *testing.T) {
			t.Parallel()
			got := stderrLevelFor(tt.v)
			if got != tt.want {
				t.Errorf("stderrLevelFor(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}
