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
// concept (e.g. Cursor, Gemini today) when ExecuteConfig.Effort is set to an
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
// reasoning-effort level. Claude and Codex do; Cursor, Gemini, and Agy have no
// effort knob and return ErrEffortUnsupported for any non-auto level (see the
// respective *_provider.go Execute guards). This is the single source of truth
// callers should consult before handing a model an effort level — e.g. a model
// fallback must drop effort when routing from a Claude model to a Cursor model.
// Unknown providers are assumed not to support effort (safe: caller drops it).
func ProviderSupportsEffort(provider string) bool {
	switch provider {
	case ProviderClaude, ProviderCodex:
		return true
	default:
		return false
	}
}

// EffortUnsupportedError builds the canonical ErrEffortUnsupported wrap with
// the provider name and the level that was rejected. Providers should call
// this when cfg.Effort is an explicit non-auto level and they have no way to
// honor it. EffortAuto and the empty level should never be passed here.
func EffortUnsupportedError(provider string, level EffortLevel) error {
	return fmt.Errorf("%w: provider=%s level=%q", ErrEffortUnsupported, provider, string(level))
}
