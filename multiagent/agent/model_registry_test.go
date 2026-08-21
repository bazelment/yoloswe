package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAvailability(installed map[string]bool) *ProviderAvailability {
	statuses := make(map[string]ProviderStatus)
	for _, p := range AllProviders {
		statuses[p] = ProviderStatus{
			Provider:  p,
			Installed: installed[p],
		}
	}
	return &ProviderAvailability{statuses: statuses}
}

func TestModelRegistry_AllInstalled(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  true,
		ProviderGemini: true,
		ProviderCursor: true,
		ProviderAgy:    true,
	})
	reg := NewModelRegistry(avail, nil)
	assert.Len(t, reg.Models(), len(AllModels))
}

func TestModelRegistry_OnlyClaude(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  false,
		ProviderGemini: false,
		ProviderCursor: false,
		ProviderAgy:    false,
	})
	reg := NewModelRegistry(avail, nil)
	for _, m := range reg.Models() {
		assert.Equal(t, ProviderClaude, m.Provider)
	}
	assert.True(t, reg.HasProvider(ProviderClaude))
	assert.False(t, reg.HasProvider(ProviderCodex))
	assert.False(t, reg.HasProvider(ProviderGemini))
}

func TestModelRegistry_FilteredCycling(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  false,
		ProviderGemini: true,
		ProviderCursor: false,
		ProviderAgy:    false,
	})
	reg := NewModelRegistry(avail, nil)

	// Cycling from last claude model should skip codex and go to gemini
	next := reg.NextModel("claude-haiku-4-5")
	assert.Equal(t, "gemini-3.1-pro-preview", next.ID)

	// Cycling from last gemini model should wrap to first claude (cursor not installed)
	next = reg.NextModel("gemini-2.5-flash-lite")
	assert.Equal(t, "opus", next.ID)
}

func TestModelRegistry_NotFoundReturnsFirst(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  false,
		ProviderGemini: false,
		ProviderCursor: false,
		ProviderAgy:    false,
	})
	reg := NewModelRegistry(avail, nil)
	next := reg.NextModel("nonexistent")
	assert.Equal(t, "opus", next.ID)
}

func TestModelRegistry_EmptyFallback(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: false,
		ProviderCodex:  false,
		ProviderGemini: false,
		ProviderCursor: false,
		ProviderAgy:    false,
	})
	reg := NewModelRegistry(avail, nil)
	assert.Empty(t, reg.Models())

	// When no providers are available, NextModel should return the
	// current model unchanged (to avoid selecting an unavailable one).
	next := reg.NextModel("opus")
	assert.Equal(t, "opus", next.ID)

	// Unknown currentID falls back to AllModels[0] as last resort
	next2 := reg.NextModel("nonexistent")
	assert.Equal(t, AllModels[0].ID, next2.ID)

	// Empty currentID also falls back
	next3 := reg.NextModel("")
	assert.Equal(t, AllModels[0].ID, next3.ID)
}

func TestModelRegistry_RebuildWithEnabled(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  true,
		ProviderGemini: true,
		ProviderCursor: false,
		ProviderAgy:    false,
	})

	reg := NewModelRegistry(avail, []string{ProviderClaude})
	// Only claude models should be present
	for _, m := range reg.Models() {
		assert.Equal(t, ProviderClaude, m.Provider)
	}
	assert.False(t, reg.HasProvider(ProviderCodex))

	// Rebuild with codex enabled too
	reg.Rebuild(avail, []string{ProviderClaude, ProviderCodex})
	assert.True(t, reg.HasProvider(ProviderClaude))
	assert.True(t, reg.HasProvider(ProviderCodex))
	assert.False(t, reg.HasProvider(ProviderGemini))
}

func TestModelRegistry_InstalledButNotEnabled(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  true,
		ProviderGemini: true,
		ProviderCursor: false,
		ProviderAgy:    false,
	})
	// Only enable gemini
	reg := NewModelRegistry(avail, []string{ProviderGemini})
	assert.False(t, reg.HasProvider(ProviderClaude))
	assert.False(t, reg.HasProvider(ProviderCodex))
	assert.True(t, reg.HasProvider(ProviderGemini))
}

func TestModelRegistry_ModelByID(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  false,
		ProviderGemini: false,
		ProviderCursor: false,
		ProviderAgy:    false,
	})
	reg := NewModelRegistry(avail, nil)

	m, ok := reg.ModelByID("opus")
	require.True(t, ok)
	assert.Equal(t, "opus", m.ID)

	_, ok = reg.ModelByID("gpt-5.4")
	assert.False(t, ok, "codex model should not be in filtered list")
}

func TestModelRegistry_FirstModelForProvider(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  true,
		ProviderGemini: false,
		ProviderCursor: false,
		ProviderAgy:    false,
	})
	reg := NewModelRegistry(avail, nil)

	m, ok := reg.FirstModelForProvider(ProviderCodex)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.5", m.ID)

	_, ok = reg.FirstModelForProvider(ProviderGemini)
	assert.False(t, ok)
}

func TestModelByID_Global(t *testing.T) {
	m, ok := ModelByID("opus")
	require.True(t, ok)
	assert.Equal(t, ProviderClaude, m.Provider)

	m, ok = ModelByID("claude-opus-4-8")
	require.True(t, ok)
	assert.Equal(t, ProviderClaude, m.Provider)

	m, ok = ModelByID("claude-fable-5")
	require.True(t, ok)
	assert.Equal(t, ProviderClaude, m.Provider)

	_, ok = ModelByID("nonexistent")
	assert.False(t, ok)
}

func TestProviderForModelID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id       string
		provider string
		ok       bool
	}{
		// Exact match wins over prefix
		{"opus", ProviderClaude, true},
		{"fable", ProviderClaude, true},
		{"gpt-5.5", ProviderCodex, true},
		// Prefix-only matches (forward-compat IDs not in AllModels)
		{"gpt-future-9000", ProviderCodex, true},
		{"gemini-99-ultra", ProviderGemini, true},
		{"cursor-fast", ProviderCursor, true},
		{"composer-3", ProviderCursor, true},
		{"agy-pro", ProviderAgy, true},
		{"claude-opus-5", ProviderClaude, true},
		{"fable-5", ProviderClaude, true},
		{"opus-4-8", ProviderClaude, true},
		{"sonnet-4-6", ProviderClaude, true},
		{"haiku-4-5", ProviderClaude, true},
		// Non-matching
		{"foo-bar", "", false},
		{"", "", false},
		{"gpt", "", false},    // bare token without hyphen must NOT match
		{"gemini", "", false}, // bare token without hyphen must NOT match
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			provider, ok := ProviderForModelID(tc.id)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.provider, provider)
		})
	}
}

func TestProviderByModelPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id       string
		provider string
		ok       bool
	}{
		{"gpt-future-9000", ProviderCodex, true},
		{"gemini-99-ultra", ProviderGemini, true},
		{"cursor-fast", ProviderCursor, true},
		{"composer-3", ProviderCursor, true},
		{"agy-pro", ProviderAgy, true},
		{"claude-opus-5", ProviderClaude, true},
		{"fable-5", ProviderClaude, true},
		{"opus-4-8", ProviderClaude, true},
		{"sonnet-4-6", ProviderClaude, true},
		{"haiku-4-5", ProviderClaude, true},
		// Exact-match IDs still work via prefix
		{"gpt-5.5", ProviderCodex, true},
		// Non-matching
		{"foo-bar", "", false},
		{"", "", false},
		{"opus", "", false}, // no prefix match for bare IDs
		{"fable", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			provider, ok := ProviderByModelPrefix(tc.id)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.provider, provider)
		})
	}
}

func TestKnownModelPrefixes(t *testing.T) {
	t.Parallel()
	s := KnownModelPrefixes()
	assert.Contains(t, s, "gpt-")
	assert.Contains(t, s, "gemini-")
	assert.Contains(t, s, "claude-")
	assert.Contains(t, s, "agy-")
}

func TestResolveModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id       string
		provider string
		ok       bool
	}{
		// Curated exact match wins.
		{"opus", ProviderClaude, true},
		// Prefix-only match synthesizes an AgentModel (not curated in AllModels).
		{"composer-2.5", ProviderCursor, true},
		// Unknown — neither curated nor prefix-recognized.
		{"totally-bogus", "", false},
		{"", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			m, ok := ResolveModel(tc.id)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.id, m.ID)
				assert.Equal(t, tc.provider, m.Provider)
				assert.Equal(t, tc.id, m.Label)
			}
		})
	}
}

func TestCLIModelArg(t *testing.T) {
	// Placeholder IDs name no model; every CLI checked rejects them outright
	// rather than falling back, so they must not reach a --model flag.
	assert.Equal(t, "", CLIModelArg("cursor-default", ProviderCursor))
	assert.Equal(t, "", CLIModelArg("agy-default", ProviderAgy))
	assert.Equal(t, "", CLIModelArg("", ProviderClaude))
	// Neither may another provider's model — backend and model are independent
	// choices in several entry points, so they do reach each other.
	assert.Equal(t, "", CLIModelArg("opus", ProviderCursor))
	assert.Equal(t, "", CLIModelArg("gpt-5.5", ProviderCursor))
	assert.Equal(t, "", CLIModelArg("claude-fable-5", ProviderCodex))
	// A model matched to its own provider passes through.
	assert.Equal(t, "opus", CLIModelArg("opus", ProviderClaude))
	assert.Equal(t, "gpt-5.5", CLIModelArg("gpt-5.5", ProviderCodex))
	// Anything the curated list does not name passes through untouched. This is
	// load-bearing, not laziness: cursor is a gateway that sells other vendors'
	// models under their own names, so a prefix rule would read these as
	// "belongs to claude/codex/gemini" and silently discard a model the user
	// named. `agent --list-models` returns all four of these.
	assert.Equal(t, "composer-2.5", CLIModelArg("composer-2.5", ProviderCursor))
	assert.Equal(t, "claude-opus-5-thinking-high", CLIModelArg("claude-opus-5-thinking-high", ProviderCursor))
	assert.Equal(t, "gpt-5.3-codex", CLIModelArg("gpt-5.3-codex", ProviderCursor))
	assert.Equal(t, "gemini-3.7-flash-high", CLIModelArg("gemini-3.7-flash-high", ProviderCursor))
	assert.Equal(t, "mystery-model", CLIModelArg("mystery-model", ProviderCursor))
	// An unknown provider means "no attribution known": placeholders still go,
	// everything else passes through rather than being stripped wholesale.
	assert.Equal(t, "opus", CLIModelArg("opus", ""))
	assert.Equal(t, "", CLIModelArg("cursor-default", ""))
}

// Every ID whose Label says "default" is bramble's own placeholder, not a
// model name. Pin the flag so a new provider's placeholder cannot be added to
// the registry without it — that omission is invisible until a session dies.
func TestAllModels_PlaceholderIDsAreMarked(t *testing.T) {
	for _, m := range AllModels {
		if strings.HasSuffix(m.ID, "-default") {
			assert.True(t, m.Placeholder, "%q looks like a placeholder ID but is not marked", m.ID)
		} else {
			assert.False(t, m.Placeholder, "%q is marked placeholder but names a real model", m.ID)
		}
	}
}

func TestModelProviderMismatch(t *testing.T) {
	t.Parallel()

	// A curated ID paired with a backend that does not own it: the user named
	// a model, so refuse rather than let CLIModelArg drop it silently.
	for _, tc := range []struct{ model, provider string }{
		{"opus", ProviderCursor},
		{"opus", ProviderCodex},
		{"gpt-5.5", ProviderCursor},
		{"claude-fable-5", ProviderCodex},
		{" FABLE ", ProviderCodex},
	} {
		err := ModelProviderMismatch(tc.model, tc.provider)
		require.Error(t, err, "%s/%s", tc.model, tc.provider)
		assert.Contains(t, err.Error(), tc.provider)
	}

	// Matched pairs, and the cases that are deliberately not a mismatch.
	for _, tc := range []struct{ model, provider string }{
		{"opus", ProviderClaude},
		{"gpt-5.5", ProviderCodex},
		// A placeholder means "this backend's own default" — nothing named,
		// so nothing to discard.
		{"cursor-default", ProviderCursor},
		{"agy-default", ProviderAgy},
		// Uncurated IDs are the CLI's business. Cursor genuinely serves these.
		{"claude-opus-5-thinking-high", ProviderCursor},
		{"gpt-5.3-codex", ProviderCursor},
		{"composer-2.5", ProviderCursor},
		{"o3", ProviderCodex},
		// Nothing to check.
		{"", ProviderCursor},
		{"opus", ""},
	} {
		assert.NoError(t, ModelProviderMismatch(tc.model, tc.provider), "%s/%s", tc.model, tc.provider)
	}
}

// The two are one rule seen from both sides: wherever the loud check fires,
// the silent one would have discarded the user's model.
func TestModelProviderMismatch_AgreesWithCLIModelArg(t *testing.T) {
	t.Parallel()

	for _, m := range AllModels {
		for _, p := range AllProviders {
			mismatch := ModelProviderMismatch(m.ID, p) != nil
			dropped := CLIModelArg(m.ID, p) == "" && !m.Placeholder
			assert.Equal(t, dropped, mismatch, "%s/%s", m.ID, p)
		}
	}
}
