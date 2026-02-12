package agent

// AgentModel describes a model available for session execution.
type AgentModel struct {
	ID       string // Model identifier passed to --model flag (e.g. "opus", "gpt-5.3-codex")
	Provider string // Binary/provider name: "claude", "codex", or "gemini"
	Label    string // Display label for the UI (e.g. "opus (claude)")
}

// AllModels is the ordered list of all known models across providers.
var AllModels = []AgentModel{
	{ID: "opus", Provider: ProviderClaude, Label: "opus"},
	{ID: "sonnet", Provider: ProviderClaude, Label: "sonnet"},
	{ID: "haiku", Provider: ProviderClaude, Label: "haiku"},
	{ID: "gpt-5.3-codex", Provider: ProviderCodex, Label: "gpt-5.3-codex"},
	{ID: "gpt-5.2", Provider: ProviderCodex, Label: "gpt-5.2"},
	{ID: "gpt-5.1-codex-max", Provider: ProviderCodex, Label: "gpt-5.1-codex-max"},
	{ID: "gemini-3-pro", Provider: ProviderGemini, Label: "gemini-3-pro"},
	{ID: "gemini-3-flash", Provider: ProviderGemini, Label: "gemini-3-flash"},
	{ID: "gemini-2.5-pro", Provider: ProviderGemini, Label: "gemini-2.5-pro"},
	{ID: "gemini-2.5-flash", Provider: ProviderGemini, Label: "gemini-2.5-flash"},
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

// ModelRegistry provides a filtered view of models based on provider
// availability and user-enabled providers.
type ModelRegistry struct {
	filtered []AgentModel
}

// NewModelRegistry creates a registry filtered by availability and enabled providers.
// If enabledProviders is nil or empty, all installed providers are enabled.
func NewModelRegistry(availability *ProviderAvailability, enabledProviders []string) *ModelRegistry {
	r := &ModelRegistry{}
	r.Rebuild(availability, enabledProviders)
	return r
}

// Rebuild recomputes the filtered model list. A provider must be both installed
// AND enabled to appear. If enabledProviders is nil/empty, all installed
// providers are considered enabled.
func (r *ModelRegistry) Rebuild(availability *ProviderAvailability, enabledProviders []string) {
	enabledSet := make(map[string]bool, len(enabledProviders))
	allEnabled := len(enabledProviders) == 0
	for _, p := range enabledProviders {
		enabledSet[p] = true
	}

	r.filtered = nil
	for _, m := range AllModels {
		if !availability.IsInstalled(m.Provider) {
			continue
		}
		if !allEnabled && !enabledSet[m.Provider] {
			continue
		}
		r.filtered = append(r.filtered, m)
	}
}

// Models returns the filtered model list.
func (r *ModelRegistry) Models() []AgentModel {
	return r.filtered
}

// ModelByID returns a model from the filtered list, or false if not available.
func (r *ModelRegistry) ModelByID(id string) (AgentModel, bool) {
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
	for _, m := range r.filtered {
		if m.Provider == provider {
			return true
		}
	}
	return false
}

// FirstModelForProvider returns the first model from a given provider, or false.
func (r *ModelRegistry) FirstModelForProvider(provider string) (AgentModel, bool) {
	for _, m := range r.filtered {
		if m.Provider == provider {
			return m, true
		}
	}
	return AgentModel{}, false
}
