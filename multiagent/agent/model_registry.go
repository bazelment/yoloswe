package agent

import (
	"strings"
	"sync"
)

// AgentModel describes a model available for session execution.
type AgentModel struct {
	ID       string // Model identifier passed to --model flag (e.g. "opus", "gpt-5.3-codex")
	Provider string // Binary/provider name: "claude", "codex", "gemini", etc.
	Label    string // Display label for the UI (e.g. "opus (claude)")
}

// AllModels is the ordered list of all known models across providers.
var AllModels = []AgentModel{
	{ID: "opus", Provider: ProviderClaude, Label: "opus"},
	{ID: "sonnet", Provider: ProviderClaude, Label: "sonnet"},
	{ID: "haiku", Provider: ProviderClaude, Label: "haiku"},
	{ID: "gpt-5.5", Provider: ProviderCodex, Label: "gpt-5.5"},
	{ID: "gpt-5.3-codex", Provider: ProviderCodex, Label: "gpt-5.3-codex"},
	{ID: "gpt-5.2", Provider: ProviderCodex, Label: "gpt-5.2"},
	{ID: "gpt-5.1-codex-max", Provider: ProviderCodex, Label: "gpt-5.1-codex-max"},
	{ID: "gemini-3.1-pro-preview", Provider: ProviderGemini, Label: "gemini-3.1-pro-preview"},
	{ID: "gemini-3-pro-preview", Provider: ProviderGemini, Label: "gemini-3-pro-preview"},
	{ID: "gemini-3-flash-preview", Provider: ProviderGemini, Label: "gemini-3-flash-preview"},
	{ID: "gemini-2.5-pro", Provider: ProviderGemini, Label: "gemini-2.5-pro"},
	{ID: "gemini-2.5-flash", Provider: ProviderGemini, Label: "gemini-2.5-flash"},
	{ID: "gemini-2.5-flash-lite", Provider: ProviderGemini, Label: "gemini-2.5-flash-lite"},
	{ID: "cursor-default", Provider: ProviderCursor, Label: "cursor-default"},
	{ID: "agy-default", Provider: ProviderAgy, Label: "agy-default"},
}

// ModelByID returns the AgentModel for the given ID from the full list, or false if not found.
func ModelByID(id string) (AgentModel, bool) {
	for _, m := range AllModels {
		if m.ID == id {
			return m, true
		}
	}
	return AgentModel{}, false
}

// modelPrefixRules maps hyphenated prefixes (e.g. "gpt-") to providers.
// Order matters: first match wins.
var modelPrefixRules = []struct {
	prefix   string
	provider string
}{
	{"gpt-", ProviderCodex},
	{"gemini-", ProviderGemini},
	{"cursor-", ProviderCursor},
	{"composer-", ProviderCursor},
	{"agy-", ProviderAgy},
	{"claude-", ProviderClaude},
}

// ProviderForModelID resolves the provider for a model ID via exact match then
// prefix rules (forward-compat for IDs not yet in AllModels).
func ProviderForModelID(id string) (provider string, ok bool) {
	if m, found := ModelByID(id); found {
		return m.Provider, true
	}
	return ProviderByModelPrefix(id)
}

// ProviderByModelPrefix infers a provider from a model ID prefix only.
// Does not consult AllModels — callers that already did an exact-match lookup
// should call this instead of ProviderForModelID to avoid a redundant scan.
func ProviderByModelPrefix(id string) (provider string, ok bool) {
	for _, rule := range modelPrefixRules {
		if strings.HasPrefix(id, rule.prefix) {
			return rule.provider, true
		}
	}
	return "", false
}

// KnownModelPrefixes returns a comma-separated list of recognized prefixes.
func KnownModelPrefixes() string {
	prefixes := make([]string, len(modelPrefixRules))
	for i, r := range modelPrefixRules {
		prefixes[i] = r.prefix
	}
	return strings.Join(prefixes, ", ")
}

// ModelRegistry provides a filtered view of models based on provider
// availability and user-enabled providers. It is safe for concurrent use.
type ModelRegistry struct {
	filtered []AgentModel
	mu       sync.RWMutex
}

// NewModelRegistry creates a registry filtered by availability and enabled providers.
// If enabledProviders is nil or empty, all installed providers are enabled.
func NewModelRegistry(availability *ProviderAvailability, enabledProviders []string) *ModelRegistry {
	r := &ModelRegistry{}
	r.Rebuild(availability, enabledProviders)
	return r
}

// Rebuild recomputes the filtered model list. A provider must be both installed
// AND enabled to appear. If enabledProviders is nil, all installed
// providers are considered enabled (default).
func (r *ModelRegistry) Rebuild(availability *ProviderAvailability, enabledProviders []string) {
	enabledSet := make(map[string]bool, len(enabledProviders))
	allEnabled := enabledProviders == nil
	for _, p := range enabledProviders {
		enabledSet[p] = true
	}

	var newFiltered []AgentModel
	for _, m := range AllModels {
		if !availability.IsInstalled(m.Provider) {
			continue
		}
		if !allEnabled && !enabledSet[m.Provider] {
			continue
		}
		newFiltered = append(newFiltered, m)
	}

	r.mu.Lock()
	r.filtered = newFiltered
	r.mu.Unlock()
}

// Models returns a snapshot of the filtered model list.
func (r *ModelRegistry) Models() []AgentModel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.filtered
}

// ModelByID returns a model from the filtered list, or false if not available.
func (r *ModelRegistry) ModelByID(id string) (AgentModel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.filtered {
		if m.ID == id {
			return m, true
		}
	}
	return AgentModel{}, false
}

// NextModel returns the next model in the filtered cycle after currentID.
// If currentID is not found, returns the first filtered model.
// If the filtered list is empty (no providers installed+enabled), returns
// the current model unchanged to avoid selecting an unavailable provider.
func (r *ModelRegistry) NextModel(currentID string) AgentModel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.filtered) == 0 {
		// No providers available — return the current model unchanged
		// rather than selecting one the user can't actually use.
		if currentID != "" {
			if m, ok := ModelByID(currentID); ok {
				return m
			}
		}
		// Last resort fallback (should not happen in practice).
		if len(AllModels) > 0 {
			return AllModels[0]
		}
		return AgentModel{ID: "sonnet", Provider: ProviderClaude, Label: "sonnet"}
	}
	for i, m := range r.filtered {
		if m.ID == currentID {
			return r.filtered[(i+1)%len(r.filtered)]
		}
	}
	return r.filtered[0]
}

// HasProvider returns true if at least one model from the given provider
// is in the filtered list.
func (r *ModelRegistry) HasProvider(provider string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.filtered {
		if m.Provider == provider {
			return true
		}
	}
	return false
}

// FirstModelForProvider returns the first model from a given provider, or false.
func (r *ModelRegistry) FirstModelForProvider(provider string) (AgentModel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.filtered {
		if m.Provider == provider {
			return m, true
		}
	}
	return AgentModel{}, false
}
