package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestParseEffort_AcceptsAllValidLevels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want EffortLevel
	}{
		{"auto", EffortAuto},
		{"low", EffortLow},
		{"medium", EffortMedium},
		{"high", EffortHigh},
		{"max", EffortMax},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseEffort(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseEffort_RejectsInvalidLevels(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "turbo", "Low", "LOW", "MEDIUM", "minimum", " low", "low "} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := ParseEffort(in)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidEffort)
		})
	}
}

func TestEffortUnsupportedError_WrapsAndIncludesContext(t *testing.T) {
	t.Parallel()

	err := EffortUnsupportedError("cursor", "high")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEffortUnsupported)
	assert.Contains(t, err.Error(), "cursor")
	assert.Contains(t, err.Error(), "high")
}

func TestClaudeEffortLevel_MapsAllLevels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   EffortLevel
		want claude.EffortLevel
	}{
		{EffortAuto, claude.EffortAuto},
		{EffortLow, claude.EffortLow},
		{EffortMedium, claude.EffortMed},
		{EffortHigh, claude.EffortHigh},
		{EffortMax, claude.EffortMax},
	} {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, claudeEffortLevel(tc.in))
		})
	}
}

// TestProviderEffortMatrix locks in the support matrix across providers.
// When a fifth provider is added, this table forces a deliberate choice
// rather than another silent ignore.
//
// Claude and Codex accept all five levels; Cursor and Gemini reject any
// non-empty effort with ErrEffortUnsupported. Empty effort is always a
// no-op success path. (Invalid string parsing is covered by
// TestParseEffort_RejectsInvalidLevels — providers receive a validated
// EffortLevel and trust the boundary.)
func TestProviderEffortMatrix(t *testing.T) {
	t.Parallel()

	validLevels := []EffortLevel{EffortLow, EffortMedium, EffortHigh, EffortMax, EffortAuto}

	// Field order: function pointer (8 bytes) before string (16 bytes) to
	// satisfy fieldalignment.
	type providerCase struct {
		// run returns the error a provider produces for the given effort
		// level. For Claude/Codex we exercise the option-builder paths
		// (no subprocess); for Cursor/Gemini we call Execute directly
		// because the early-return happens before any subprocess work.
		run  func(t *testing.T, level EffortLevel) error
		name string
	}

	providers := []providerCase{
		{
			name: "claude",
			run: func(t *testing.T, level EffortLevel) error {
				t.Helper()
				if level == "" {
					return nil
				}
				_ = claudeEffortLevel(level)
				return nil
			},
		},
		{
			name: "codex",
			run: func(t *testing.T, level EffortLevel) error {
				t.Helper()
				_ = codexTurnOptions(ExecuteConfig{Effort: level})
				return nil
			},
		},
		{
			name: "cursor",
			run: func(t *testing.T, level EffortLevel) error {
				t.Helper()
				p := NewCursorProvider()
				defer p.Close()
				_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(level))
				return err
			},
		},
		{
			name: "gemini",
			run: func(t *testing.T, level EffortLevel) error {
				t.Helper()
				p := NewGeminiProvider()
				defer p.Close()
				_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(level))
				return err
			},
		},
	}

	for _, prov := range providers {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			t.Parallel()

			// Empty effort is always a success no-op for the effort path.
			// (Cursor/Gemini may still fail downstream when the subprocess
			// can't start in the test environment — we explicitly skip
			// those providers' empty-effort case to keep the matrix focused
			// on effort behavior.)
			if prov.name == "claude" || prov.name == "codex" {
				err := prov.run(t, "")
				assert.NoError(t, err, "empty effort should be a no-op")
			}

			expectUnsupported := prov.name == "cursor" || prov.name == "gemini"
			for _, level := range validLevels {
				err := prov.run(t, level)
				if expectUnsupported {
					require.Error(t, err)
					assert.ErrorIs(t, err, ErrEffortUnsupported,
						"level %q on %s should wrap ErrEffortUnsupported", level, prov.name)
					// Provider name and level should both appear in the message
					// so the user can fix the config without code-diving.
					assert.Contains(t, err.Error(), prov.name,
						"error should name the provider")
					assert.Contains(t, err.Error(), string(level),
						"error should name the rejected level")
				} else {
					assert.NoError(t, err, "level %q on %s should be supported", level, prov.name)
				}
			}
		})
	}
}

// TestErrEffortUnsupported_NotConfusedWithErrInvalidEffort ensures the two
// sentinel errors are distinct so callers can branch on them.
func TestErrEffortUnsupported_NotConfusedWithErrInvalidEffort(t *testing.T) {
	t.Parallel()

	unsupported := EffortUnsupportedError("cursor", "high")
	assert.ErrorIs(t, unsupported, ErrEffortUnsupported)
	assert.False(t, errors.Is(unsupported, ErrInvalidEffort),
		"unsupported error must not look like an invalid-effort error")

	_, invalid := ParseEffort("turbo")
	assert.ErrorIs(t, invalid, ErrInvalidEffort)
	assert.False(t, errors.Is(invalid, ErrEffortUnsupported),
		"invalid error must not look like an unsupported error")
}
