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
// no-op success path. Invalid strings always surface ErrInvalidEffort
// before any provider-specific work.
func TestProviderEffortMatrix(t *testing.T) {
	t.Parallel()

	type expect int
	const (
		expectOK expect = iota
		expectUnsupported
		expectInvalid
	)

	validLevels := []string{"low", "medium", "high", "max", "auto"}

	// Field order: function pointer (8 bytes) before string (16 bytes) to
	// satisfy fieldalignment.
	type providerCase struct {
		// run returns the error from a hypothetical Execute call with the
		// given effort string. For Claude/Codex we exercise the actual
		// option-builder paths (no subprocess); for Cursor/Gemini we call
		// Execute directly because the early-return happens before any
		// subprocess work.
		run  func(t *testing.T, effort string) error
		name string
	}

	providers := []providerCase{
		{
			name: "claude",
			run: func(t *testing.T, effort string) error {
				t.Helper()
				if effort == "" {
					return nil
				}
				level, err := ParseEffort(effort)
				if err != nil {
					return err
				}
				// Sanity-check the mapper for valid inputs.
				_ = claudeEffortLevel(level)
				return nil
			},
		},
		{
			name: "codex",
			run: func(t *testing.T, effort string) error {
				t.Helper()
				_, err := codexTurnOptions(ExecuteConfig{Effort: effort})
				return err
			},
		},
		{
			name: "cursor",
			run: func(t *testing.T, effort string) error {
				t.Helper()
				p := NewCursorProvider()
				defer p.Close()
				_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(effort))
				return err
			},
		},
		{
			name: "gemini",
			run: func(t *testing.T, effort string) error {
				t.Helper()
				p := NewGeminiProvider()
				defer p.Close()
				_, err := p.Execute(context.Background(), "ignored", nil, WithProviderEffort(effort))
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

			// Invalid strings must surface ErrInvalidEffort, regardless of provider.
			for _, bad := range []string{"turbo", "Low", "minimum"} {
				err := prov.run(t, bad)
				require.Error(t, err, "bad effort %q on %s should error", bad, prov.name)
				assert.ErrorIs(t, err, ErrInvalidEffort,
					"bad effort %q on %s should wrap ErrInvalidEffort", bad, prov.name)
			}

			// Valid levels: Claude and Codex accept; Cursor and Gemini reject.
			want := expectOK
			if prov.name == "cursor" || prov.name == "gemini" {
				want = expectUnsupported
			}
			for _, level := range validLevels {
				err := prov.run(t, level)
				switch want {
				case expectOK:
					assert.NoError(t, err, "level %q on %s should be supported", level, prov.name)
				case expectUnsupported:
					require.Error(t, err)
					assert.ErrorIs(t, err, ErrEffortUnsupported,
						"level %q on %s should wrap ErrEffortUnsupported", level, prov.name)
					// Provider name and level should both appear in the message
					// so the user can fix the config without code-diving.
					assert.Contains(t, err.Error(), prov.name,
						"error should name the provider")
					assert.Contains(t, err.Error(), level,
						"error should name the rejected level")
				case expectInvalid:
					require.Error(t, err)
					assert.ErrorIs(t, err, ErrInvalidEffort)
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
