package agent

import (
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
	})
	reg := NewModelRegistry(avail, nil)
	assert.Len(t, reg.Models(), len(AllModels))
}

func TestModelRegistry_OnlyClaude(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  false,
		ProviderGemini: false,
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
	})
	reg := NewModelRegistry(avail, nil)

	// Cycling from last claude model should skip codex and go to gemini
	next := reg.NextModel("haiku")
	assert.Equal(t, "gemini-3-pro", next.ID)

	// Cycling from last gemini model should wrap to first claude
	next = reg.NextModel("gemini-2.5-flash")
	assert.Equal(t, "opus", next.ID)
}

func TestModelRegistry_NotFoundReturnsFirst(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  false,
		ProviderGemini: false,
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
	})
	reg := NewModelRegistry(avail, nil)
	assert.Empty(t, reg.Models())

	// Should fall back to first in AllModels
	next := reg.NextModel("anything")
	assert.Equal(t, AllModels[0].ID, next.ID)
}

func TestModelRegistry_RebuildWithEnabled(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  true,
		ProviderGemini: true,
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
	})
	reg := NewModelRegistry(avail, nil)

	m, ok := reg.ModelByID("opus")
	require.True(t, ok)
	assert.Equal(t, "opus", m.ID)

	_, ok = reg.ModelByID("gpt-5.3-codex")
	assert.False(t, ok, "codex model should not be in filtered list")
}

func TestModelRegistry_FirstModelForProvider(t *testing.T) {
	avail := newTestAvailability(map[string]bool{
		ProviderClaude: true,
		ProviderCodex:  true,
		ProviderGemini: false,
	})
	reg := NewModelRegistry(avail, nil)

	m, ok := reg.FirstModelForProvider(ProviderCodex)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.3-codex", m.ID)

	_, ok = reg.FirstModelForProvider(ProviderGemini)
	assert.False(t, ok)
}

func TestModelByID_Global(t *testing.T) {
	m, ok := ModelByID("opus")
	require.True(t, ok)
	assert.Equal(t, ProviderClaude, m.Provider)

	_, ok = ModelByID("nonexistent")
	assert.False(t, ok)
}
