package agent

import (
	"errors"
	"fmt"
)

// EffortLevel is the provider-neutral reasoning effort vocabulary used by
// ExecuteConfig.Effort. Each provider maps it to its own representation, or
// returns ErrEffortUnsupported when the provider has no effort knob.
type EffortLevel string

const (
	// EffortAuto clears explicit effort and lets the provider/model default apply.
	EffortAuto   EffortLevel = "auto"
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
	EffortMax    EffortLevel = "max"
)

// ErrInvalidEffort is returned when an unknown effort string is parsed.
var ErrInvalidEffort = errors.New("invalid effort level")

// ErrEffortUnsupported is returned by providers that have no reasoning-effort
// concept (e.g. Cursor today) when ExecuteConfig.Effort is set to an
// explicit non-auto level. EffortAuto means "use the provider default" and
// must not produce this error. Wrapped with the provider name and requested
// level.
var ErrEffortUnsupported = errors.New("provider does not support reasoning effort")

// ParseEffort parses a user-supplied string into an EffortLevel. It accepts
// EffortAuto in addition to the explicit levels — callers that need to forbid
// "auto" should compare the result against EffortAuto themselves.
func ParseEffort(s string) (EffortLevel, error) {
	level := EffortLevel(s)
	switch level {
	case EffortAuto, EffortLow, EffortMedium, EffortHigh, EffortMax:
		return level, nil
	}
	return "", fmt.Errorf("%w: %q (valid: low, medium, high, max, auto)", ErrInvalidEffort, s)
}

// ProviderSupportsEffort reports whether a provider honors an explicit non-auto
// reasoning-effort level. Claude, Codex, and Agy do; Cursor has no effort knob
// and returns ErrEffortUnsupported for any non-auto level (see the respective
// *_provider.go Execute guards).
//
// This answers a PROVIDER-level question, which for agy is not the whole
// story: agy encodes the level in the model id, and its catalog is not a full
// cross product, so a specific agy model can refuse a specific level even
// though the provider supports effort in general. Callers deciding what to do
// with one concrete model should ask ModelSupportsEffort instead.
// Unknown providers are assumed not to support effort (safe: caller drops it).
func ProviderSupportsEffort(provider string) bool {
	switch provider {
	case ProviderClaude, ProviderCodex, ProviderAgy:
		return true
	default:
		return false
	}
}

// ModelSupportsEffort reports whether a specific model can actually run at a
// given non-auto effort level, which is the question a caller holding one
// model ID wants answered before handing it an effort - notably a fallback
// deciding whether to carry a level over to the next model, where the
// alternative is a rescue attempt that dies on ErrEffortUnsupported.
//
// It is ProviderSupportsEffort for every provider but agy. agy spells the
// level in the model id and ships an incomplete matrix (gemini-3.1-pro has
// -low and -high but no -medium), so a curated agy model answers for the
// exact variant the level would need. An uncurated ID answers true: this list
// is not the authority on models it does not name.
func ModelSupportsEffort(modelID string, level EffortLevel) bool {
	m, ok := ResolveModel(modelID)
	if !ok {
		return false
	}
	if m.Provider != ProviderAgy || level == "" || level == EffortAuto {
		return ProviderSupportsEffort(m.Provider)
	}
	want := agyEffortLevel(level)
	if want == "" {
		return false
	}
	// Same helper the provider enforces with, so advice and behavior agree.
	_, ok = agyRetarget(modelID, want)
	return ok
}

// EffortUnsupportedError builds the canonical ErrEffortUnsupported wrap with
// the provider name and the level that was rejected. Providers should call
// this when cfg.Effort is an explicit non-auto level and they have no way to
// honor it. EffortAuto and the empty level should never be passed here.
func EffortUnsupportedError(provider string, level EffortLevel) error {
	return fmt.Errorf("%w: provider=%s level=%q", ErrEffortUnsupported, provider, string(level))
}
