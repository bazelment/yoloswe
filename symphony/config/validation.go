package config

import "fmt"

// ValidationError represents a dispatch preflight validation failure.
type ValidationError struct {
	Checks []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("dispatch validation failed: %v", e.Checks)
}

// ValidateForDispatch runs preflight validation before dispatch.
// Spec Section 6.3.
func ValidateForDispatch(cfg *ServiceConfig) error {
	var checks []string

	if cfg.TrackerKind == "" {
		checks = append(checks, "tracker.kind is required")
	} else if cfg.TrackerKind != "linear" {
		checks = append(checks, fmt.Sprintf("unsupported tracker.kind: %q", cfg.TrackerKind))
	}

	if cfg.TrackerAPIKey == "" {
		checks = append(checks, "tracker.api_key is required (after $ resolution)")
	}

	if cfg.TrackerKind == "linear" && cfg.TrackerProjectSlug == "" {
		checks = append(checks, "tracker.project_slug is required for linear tracker")
	}

	if cfg.CodexCommand == "" {
		checks = append(checks, "codex.command must be present and non-empty")
	}

	if len(checks) > 0 {
		return &ValidationError{Checks: checks}
	}
	return nil
}
