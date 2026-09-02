package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
)

func TestAgyProvider_Name(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	assert.Equal(t, ProviderAgy, p.Name())
}

func TestAgyProvider_EventsChannel(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider()
	defer p.Close()

	ch := p.Events()
	require.NotNil(t, ch)
	assert.Equal(t, (<-chan AgentEvent)(p.events), ch)
}

func TestAgyEffortLevel_MapsAllLevels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   EffortLevel
		want string
	}{
		{EffortAuto, ""},
		{EffortLow, "low"},
		{EffortMedium, "medium"},
		{EffortHigh, "high"},
		{EffortMax, "high"}, // EffortMax clamps to agy's highest level.
		{EffortLevel("unexpected"), ""},
	} {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, agyEffortLevel(tc.in))
		})
	}
}

// Pin what actually reaches the CLI, not just the mapping rule in isolation:
// the model and effort wiring used to be inlined in Execute, where deleting it
// left every test in this package green (the same regression shape
// cursor_provider_test.go documents for cursor.WithModel).
func TestAgySessionOpts_Model(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "unset", model: "", want: ""},
		{name: "claude default applyOptions fills in", model: "sonnet", want: ""},
		{name: "bramble placeholder", model: "agy-default", want: ""},
		{name: "another provider's model", model: "gpt-5.5", want: ""},
		{name: "curated agy model passes through", model: "gemini-3.8-flash-low", want: "gemini-3.8-flash-low"},
		{name: "uncurated gemini id passes through", model: "gemini-99-ultra", want: "gemini-99-ultra"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got agy.SessionConfig
			for _, opt := range agySessionOpts(ExecuteConfig{Model: tt.model}) {
				opt(&got)
			}
			assert.Equal(t, tt.want, got.Model)
		})
	}
}

func TestAgySessionOpts_Effort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		model  string
		effort EffortLevel
		want   string
	}{
		{name: "unset", effort: "", want: ""},
		{name: "auto omits the flag", effort: EffortAuto, want: ""},
		{name: "low", effort: EffortLow, want: "low"},
		{name: "medium", effort: EffortMedium, want: "medium"},
		{name: "high", effort: EffortHigh, want: "high"},
		{name: "max clamps to high", effort: EffortMax, want: "high"},
		// agy rejects --model <id ending in a level> together with a
		// conflicting --effort, so the level pinned by the model wins.
		{name: "model pinning low drops a conflicting effort", model: "gemini-3.8-flash-low", effort: EffortHigh, want: ""},
		{name: "model pinning high drops a matching effort too", model: "gemini-3.1-pro-high", effort: EffortHigh, want: ""},
		{name: "unpinned model still carries effort", model: "gemini-99-ultra", effort: EffortLow, want: "low"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got agy.SessionConfig
			for _, opt := range agySessionOpts(ExecuteConfig{Model: tt.model, Effort: tt.effort}) {
				opt(&got)
			}
			assert.Equal(t, tt.want, got.Effort)
		})
	}
}

func TestAgyProvider_AcceptsExplicitEffort(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider(agy.WithCLIPath("missing-agy-effort-test-binary"))
	defer p.Close()

	// Point at a binary that cannot exist so this stays subprocess-free and
	// does not reach the real agy CLI on a machine where it is installed:
	// Execute must fail on startup, not on the effort guard that used to
	// reject any explicit non-auto level.
	_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(EffortHigh))
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrEffortUnsupported),
		"agy now supports effort; must not reject with ErrEffortUnsupported, got %v", err)
}
