package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
)

// writeFakeAgy writes a JSON-printing fake agy binary.
func writeFakeAgy(t *testing.T, resultJSON string, expectedArgs ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\n"
	if len(expectedArgs) > 0 {
		script += "case \" $* \" in\n" +
			"  *' " + strings.Join(expectedArgs, " ") + " '*) ;;\n" +
			"  *) echo 'fake agy: missing expected arguments' >&2; exit 2 ;;\n" +
			"esac\n"
	}
	script += "cat <<'EOF'\n" + resultJSON + "\nEOF\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

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
		effort EffortLevel
		want   string
	}{
		{name: "unset", effort: "", want: ""},
		{name: "auto omits the flag", effort: EffortAuto, want: ""},
		{name: "low", effort: EffortLow, want: "low"},
		{name: "medium", effort: EffortMedium, want: "medium"},
		{name: "high", effort: EffortHigh, want: "high"},
		{name: "max clamps to high", effort: EffortMax, want: "high"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got agy.SessionConfig
			for _, opt := range agySessionOpts(ExecuteConfig{Effort: tt.effort}) {
				opt(&got)
			}
			assert.Equal(t, tt.want, got.Effort)
		})
	}
}

// agy encodes the reasoning level both in the model id and in --effort, and
// rejects a command line carrying two that disagree. Pin the reconciliation
// against the config that actually reaches the CLI: a requested level must be
// honored by retargeting the model, never silently dropped, and never
// retargeted onto a variant agy's catalog does not have.
func TestReconcileAgyEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		model      string
		effort     string
		wantModel  string
		wantEffort string
	}{
		{name: "no effort requested leaves the model alone",
			model: "gemini-3.8-flash-low", effort: "", wantModel: "gemini-3.8-flash-low", wantEffort: ""},
		{name: "unpinned model keeps the effort flag",
			model: "agy-custom", effort: "high", wantModel: "agy-custom", wantEffort: "high"},
		{name: "no model at all keeps the effort flag",
			model: "", effort: "high", wantModel: "", wantEffort: "high"},
		{name: "matching level drops the redundant flag",
			model: "gemini-3.8-flash-high", effort: "high", wantModel: "gemini-3.8-flash-high", wantEffort: ""},
		{name: "conflicting level retargets the model",
			model: "gemini-3.8-flash-low", effort: "high", wantModel: "gemini-3.8-flash-high", wantEffort: ""},
		{name: "retarget also works downward",
			model: "gemini-3.1-pro-high", effort: "low", wantModel: "gemini-3.1-pro-low", wantEffort: ""},
		// An uncurated id still pins a level syntactically, so shipping both
		// flags would be a certain conflict. Retarget optimistically and let
		// agy judge the id - never emit the pair we know it rejects.
		{name: "uncurated pinned model is retargeted, not sent as a conflict",
			model: "gemini-9.9-flash-low", effort: "high", wantModel: "gemini-9.9-flash-high", wantEffort: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agy.SessionConfig{Model: tt.model, Effort: tt.effort}
			require.NoError(t, reconcileAgyEffort(&got))
			assert.Equal(t, tt.wantModel, got.Model, "model")
			assert.Equal(t, tt.wantEffort, got.Effort, "effort")
		})
	}
}

// agy's catalog is not a full cross product, so some requested levels simply
// cannot be run. Surface that rather than quietly running at another level:
// the whole point of ProviderSupportsEffort(agy)=true is that a caller can
// trust the level it asked for, and jiradozer already handles this error.
func TestReconcileAgyEffort_UnrepresentableLevelIsRejected(t *testing.T) {
	t.Parallel()

	// gemini-3.1-pro ships -low and -high but no -medium.
	got := agy.SessionConfig{Model: "gemini-3.1-pro-high", Effort: "medium"}
	err := reconcileAgyEffort(&got)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEffortUnsupported)
	assert.Contains(t, err.Error(), "gemini-3.1-pro", "error should name the model")
	assert.Contains(t, err.Error(), "medium", "error should name the requested level")
}

func TestAgyProvider_ExecuteRejectsUnrepresentableEffort(t *testing.T) {
	t.Parallel()

	p := NewAgyProvider(agy.WithCLIPath("missing-agy-effort-test-binary"))
	defer p.Close()

	_, err := p.Execute(context.Background(), "ignored", nil,
		WithProviderModel("gemini-3.1-pro-high"), WithProviderEffort(EffortMedium))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEffortUnsupported,
		"an unrepresentable model/effort pair must fail before the CLI runs, got %v", err)
}

// The conflict is resolved against the MERGED config, so a model pinned by the
// provider's constructor counts too - the shape the conformance test uses.
func TestAgyProvider_ConstructorPinnedModelReconcilesEffort(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		ctorModel  string
		effort     EffortLevel
		wantModel  string
		wantEffort string
	}{
		{name: "constructor model retargeted to the requested level",
			ctorModel: "gemini-3.8-flash-low", effort: EffortHigh,
			wantModel: "gemini-3.8-flash-high", wantEffort: ""},
		{name: "constructor model already at the requested level",
			ctorModel: "gemini-3.1-pro-low", effort: EffortLow,
			wantModel: "gemini-3.1-pro-low", wantEffort: ""},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := NewAgyProvider(agy.WithModel(tt.ctorModel))
			defer p.Close()

			// Go through the same assembly Execute uses, so dropping the
			// reconciliation pass from it fails this test.
			opts, err := p.sessionOptsFor(ExecuteConfig{Effort: tt.effort})
			require.NoError(t, err)
			var got agy.SessionConfig
			for _, opt := range opts {
				opt(&got)
			}
			assert.Equal(t, tt.wantModel, got.Model, "model")
			assert.Equal(t, tt.wantEffort, got.Effort, "effort")
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

// ModelSupportsEffort advises callers and reconcileAgyEffort enforces at
// execute. They now share agyRetarget, and this pins that they cannot drift:
// whenever the predicate says a pair is supported, reconciliation must accept
// it, and whenever it says no, reconciliation must reject it.
func TestModelSupportsEffort_AgreesWithReconciliation(t *testing.T) {
	t.Parallel()

	levels := []EffortLevel{EffortAuto, EffortLow, EffortMedium, EffortHigh, EffortMax}
	for _, m := range AllModels {
		if m.Provider != ProviderAgy {
			continue
		}
		for _, level := range levels {
			m, level := m, level
			t.Run(m.ID+"/"+string(level), func(t *testing.T) {
				t.Parallel()

				cfg := agy.SessionConfig{Model: m.ID, Effort: agyEffortLevel(level)}
				err := reconcileAgyEffort(&cfg)

				if ModelSupportsEffort(m.ID, level) {
					require.NoError(t, err,
						"predicate says %q supports %q, so reconciliation must accept it", m.ID, level)
					return
				}
				require.Error(t, err,
					"predicate says %q cannot serve %q, so reconciliation must reject it", m.ID, level)
				assert.ErrorIs(t, err, ErrEffortUnsupported)
			})
		}
	}
}

// TestAgyProvider_PopulatesSessionIDAndUsage guards the ID and usage mapping.
func TestAgyProvider_PopulatesSessionIDAndUsage(t *testing.T) {
	// Keep fake-CLI tests serial: concurrent fork/exec can race ETXTBSY under
	// Bazel load (the documented jiradozer/orchestrator_test.go precedent).

	resultJSON := `{"conversation_id":"conv-provider-1","status":"SUCCESS","response":"PLATYPUS",` +
		`"duration_seconds":0.4,"num_turns":1,` +
		`"usage":{"input_tokens":200,"output_tokens":7,"thinking_tokens":0,"cache_read_tokens":30,"total_tokens":237}}`
	cliPath := writeFakeAgy(t, resultJSON)

	p := NewAgyProvider(agy.WithCLIPath(cliPath))
	defer p.Close()

	result, err := p.Execute(context.Background(), "remember PLATYPUS", nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "conv-provider-1", result.SessionID, "AgentResult.SessionID must carry agy's conversation_id")
	assert.Equal(t, "PLATYPUS", result.Text)
	assert.Equal(t, 200, result.Usage.InputTokens)
	assert.Equal(t, 7, result.Usage.OutputTokens)
	assert.Equal(t, 30, result.Usage.CacheReadTokens)
}

// TestAgyProvider_ResumeSessionIDReachesConversationFlag checks the real argv.
func TestAgyProvider_ResumeSessionIDReachesConversationFlag(t *testing.T) {
	resultJSON := `{"conversation_id":"conv-provider-1","status":"SUCCESS","response":"PLATYPUS",` +
		`"duration_seconds":0.2,"num_turns":2,` +
		`"usage":{"input_tokens":50,"output_tokens":3,"thinking_tokens":0,"cache_read_tokens":10,"total_tokens":63}}`
	cliPath := writeFakeAgy(t, resultJSON, "--conversation", "conv-provider-1")

	p := NewAgyProvider(agy.WithCLIPath(cliPath))
	defer p.Close()

	result, err := p.Execute(context.Background(), "what was the word?", nil,
		WithProviderResumeSessionID("conv-provider-1"))
	require.NoError(t, err)
	assert.Equal(t, "PLATYPUS", result.Text)
	assert.Equal(t, "conv-provider-1", result.SessionID)
}
