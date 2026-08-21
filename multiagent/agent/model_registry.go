package agent

import (
	"fmt"
	"strings"
	"sync"
)

// AgentModel describes a model available for session execution.
type AgentModel struct {
	ID       string // Model identifier passed to --model flag (e.g. "opus", "gpt-5.5")
	Provider string // Binary/provider name: "claude", "codex", "gemini", etc.
	Label    string // Display label for the UI (e.g. "opus (claude)")
	// Placeholder marks an ID that names no real model — it is bramble's own
	// label for "let this provider's CLI pick its default". Such an ID must
	// never reach a --model flag: the CLIs reject it outright rather than
	// falling back ("Cannot use this model: cursor-default", "model
	// agy-default is not recognized"). Use CLIModelArg to strip it.
	Placeholder bool
}

// CLIModelArg returns the value to pass to provider's CLI on --model for a
// model ID, or "" when the CLI must be left to pick for itself. Two kinds of
// curated ID name nothing that CLI can run: placeholders, and IDs curated
// against a *different* provider — backend and model are separate choices in
// several entry points (`yoloswe codetalk --backend cursor --model opus`), so
// they do reach each other. Neither is a soft fallback at the CLI; both are a
// hard "cannot use this model" that stops the session before it starts.
//
// Attribution is by exact curated ID only — deliberately ModelByID, not
// ResolveModel. The prefix rules answer a different question: "which CLI does
// bramble route this ID to by default", not "which CLI can run it". Cursor is
// a gateway that serves other vendors' models under their own names — 181 of
// the 204 IDs `agent --list-models` returns start with claude-, gpt- or
// gemini- — so attributing by prefix would silently discard a model the user
// asked for by name. None of the curated IDs collide with that catalog, which
// is what makes the exact check safe. Anything the curated list does not name
// passes through: there the CLI is the authority, not this list. An empty
// provider means "no attribution known", so only the placeholder rule applies.
func CLIModelArg(model, provider string) string {
	m, ok := curatedModel(model)
	if !ok {
		return model
	}
	if m.Placeholder {
		return ""
	}
	if provider != "" && m.Provider != provider {
		return ""
	}
	return model
}

// ModelProviderMismatch reports an error when a curated model ID is paired with
// a provider that does not own it. It is the loud counterpart to CLIModelArg
// over exactly the same rule: CLIModelArg drops such an ID because a CLI given
// one dies, but dropping is only the right answer where nobody chose the
// pairing. At an entry point where a human typed both halves, silently running
// a different model than the one they named is the worse failure — so those
// call it first and refuse.
//
// A placeholder is not a mismatch: it means "this provider's own default", so
// the caller named no model to discard. Neither is an ID the curated list does
// not name — see CLIModelArg on why bramble does not police a CLI's catalog.
func ModelProviderMismatch(model, provider string) error {
	if model == "" || provider == "" {
		return nil
	}
	m, ok := curatedModel(model)
	if !ok || m.Placeholder || m.Provider == provider {
		return nil
	}
	return fmt.Errorf("model %q belongs to provider %q, but backend %q was selected; "+
		"pass a %[3]s model or omit the model flag to use %[3]s's own default", model, m.Provider, provider)
}

// curatedModel looks a model ID up in the curated list, tolerating the case and
// surrounding whitespace that a hand-typed flag carries — every curated ID is
// lowercase, so lowering the needle is enough. Only the curated list is
// consulted; CLIModelArg documents why prefix attribution is deliberately not
// used for this question.
func curatedModel(model string) (AgentModel, bool) {
	return ModelByID(strings.ToLower(strings.TrimSpace(model)))
}

// AllModels is the ordered list of all known models across providers.
var AllModels = []AgentModel{
	{ID: "opus", Provider: ProviderClaude, Label: "opus"},
	{ID: "sonnet", Provider: ProviderClaude, Label: "sonnet"},
	{ID: "haiku", Provider: ProviderClaude, Label: "haiku"},
	{ID: "fable", Provider: ProviderClaude, Label: "fable"},
	{ID: "claude-fable-5", Provider: ProviderClaude, Label: "claude-fable-5"},
	{ID: "claude-opus-4-8", Provider: ProviderClaude, Label: "claude-opus-4-8"},
	{ID: "claude-sonnet-4-6", Provider: ProviderClaude, Label: "claude-sonnet-4-6"},
	{ID: "claude-haiku-4-5", Provider: ProviderClaude, Label: "claude-haiku-4-5"},
	{ID: "gpt-5.5", Provider: ProviderCodex, Label: "gpt-5.5"},
	{ID: "gpt-5.4", Provider: ProviderCodex, Label: "gpt-5.4"},
	{ID: "gpt-5.4-mini", Provider: ProviderCodex, Label: "gpt-5.4-mini"},
	{ID: "gemini-3.1-pro-preview", Provider: ProviderGemini, Label: "gemini-3.1-pro-preview"},
	{ID: "gemini-3-pro-preview", Provider: ProviderGemini, Label: "gemini-3-pro-preview"},
	{ID: "gemini-3-flash-preview", Provider: ProviderGemini, Label: "gemini-3-flash-preview"},
	{ID: "gemini-2.5-pro", Provider: ProviderGemini, Label: "gemini-2.5-pro"},
	{ID: "gemini-2.5-flash", Provider: ProviderGemini, Label: "gemini-2.5-flash"},
	{ID: "gemini-2.5-flash-lite", Provider: ProviderGemini, Label: "gemini-2.5-flash-lite"},
	{ID: "cursor-default", Provider: ProviderCursor, Label: "cursor-default", Placeholder: true},
	{ID: "agy-default", Provider: ProviderAgy, Label: "agy-default", Placeholder: true},
}

// AllModelIDs returns the IDs of every curated model, in AllModels order.
func AllModelIDs() []string {
	ids := make([]string, len(AllModels))
	for i, m := range AllModels {
		ids[i] = m.ID
	}
	return ids
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

// ResolveModel resolves a model ID via exact match against AllModels, then a
// prefix rule (e.g. "composer-2.5" → ProviderCursor) so not-yet-curated IDs
// still work. Returns false only when neither matches.
func ResolveModel(id string) (AgentModel, bool) {
	if m, ok := ModelByID(id); ok {
		return m, true
	}
	if provider, ok := ProviderByModelPrefix(id); ok {
		return AgentModel{ID: id, Provider: provider, Label: id}, true
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
	{"fable-", ProviderClaude},
	{"opus-", ProviderClaude},
	{"sonnet-", ProviderClaude},
	{"haiku-", ProviderClaude},
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
