package yoloswe

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateConfig validates the configuration and returns an error if invalid.
// This provides early detection of configuration issues before starting the SWE loop.
//
// Validation checks:
//   - Model names: Validates builder model (haiku/sonnet/opus) and warns on unknown reviewer models
//   - Directories: Checks working directory exists and is accessible, validates recording directory parent
//   - Budget: Rejects negative values, minimum $0.01, warns if > $1000
//   - Timeout: Rejects negative values, minimum 10s, warns if > 24 hours
//   - Iterations: Rejects negative values, warns if > 100
//   - System prompt: Maximum 10,000 characters
//
// The function distinguishes between:
//   - Hard errors (invalid/impossible configurations) -> returns error
//   - Warnings (unusual but valid configurations) -> prints to stderr in verbose mode
//
// Returns:
//   - nil if configuration is valid
//   - error with detailed message listing all validation failures
func ValidateConfig(config Config) error {
	var errors []string

	// Validate builder model
	validBuilderModels := map[string]bool{
		"haiku":  true,
		"sonnet": true,
		"opus":   true,
	}
	if config.BuilderModel != "" && !validBuilderModels[config.BuilderModel] {
		errors = append(errors, fmt.Sprintf("invalid builder model %q (must be haiku, sonnet, or opus)", config.BuilderModel))
	}

	// Validate reviewer model
	validReviewerModels := map[string]bool{
		"gpt-5.2-codex": true,
		"o4-mini":       true,
		"o4":            true,
	}
	if config.ReviewerModel != "" && !validReviewerModels[config.ReviewerModel] {
		// Warning only, allow custom models
		if config.Verbose {
			fmt.Fprintf(os.Stderr, "Warning: unknown reviewer model %q, proceeding anyway\n", config.ReviewerModel)
		}
	}

	// Validate working directory exists
	if config.BuilderWorkDir != "" {
		if info, err := os.Stat(config.BuilderWorkDir); err != nil {
			if os.IsNotExist(err) {
				errors = append(errors, fmt.Sprintf("working directory does not exist: %s", config.BuilderWorkDir))
			} else {
				errors = append(errors, fmt.Sprintf("cannot access working directory %s: %v", config.BuilderWorkDir, err))
			}
		} else if !info.IsDir() {
			errors = append(errors, fmt.Sprintf("working directory path is not a directory: %s", config.BuilderWorkDir))
		}
	}

	// Validate recording directory can be created
	if config.RecordingDir != "" {
		absPath, err := filepath.Abs(config.RecordingDir)
		if err != nil {
			errors = append(errors, fmt.Sprintf("invalid recording directory path: %v", err))
		} else {
			// Check if parent directory exists
			parentDir := filepath.Dir(absPath)
			if info, err := os.Stat(parentDir); err != nil {
				if os.IsNotExist(err) {
					errors = append(errors, fmt.Sprintf("recording directory parent does not exist: %s", parentDir))
				}
			} else if !info.IsDir() {
				errors = append(errors, fmt.Sprintf("recording directory parent is not a directory: %s", parentDir))
			}
		}
	}

	// Validate budget
	if config.MaxBudgetUSD < 0 {
		errors = append(errors, fmt.Sprintf("max budget cannot be negative: %.2f", config.MaxBudgetUSD))
	}
	if config.MaxBudgetUSD > 0 && config.MaxBudgetUSD < 0.01 {
		errors = append(errors, fmt.Sprintf("max budget too low (minimum $0.01): %.4f", config.MaxBudgetUSD))
	}
	if config.MaxBudgetUSD > 1000 {
		// Warning only, allow high budgets
		if config.Verbose {
			fmt.Fprintf(os.Stderr, "Warning: max budget is very high: $%.2f\n", config.MaxBudgetUSD)
		}
	}

	// Validate timeout
	if config.MaxTimeSeconds < 0 {
		errors = append(errors, fmt.Sprintf("max timeout cannot be negative: %d", config.MaxTimeSeconds))
	}
	if config.MaxTimeSeconds > 0 && config.MaxTimeSeconds < 10 {
		errors = append(errors, fmt.Sprintf("max timeout too low (minimum 10s): %d", config.MaxTimeSeconds))
	}
	if config.MaxTimeSeconds > 86400 { // 24 hours
		if config.Verbose {
			fmt.Fprintf(os.Stderr, "Warning: max timeout is very high: %ds (%.1f hours)\n",
				config.MaxTimeSeconds, float64(config.MaxTimeSeconds)/3600)
		}
	}

	// Validate max iterations
	if config.MaxIterations < 0 {
		errors = append(errors, fmt.Sprintf("max iterations cannot be negative: %d", config.MaxIterations))
	}
	if config.MaxIterations > 0 && config.MaxIterations > 100 {
		if config.Verbose {
			fmt.Fprintf(os.Stderr, "Warning: max iterations is very high: %d\n", config.MaxIterations)
		}
	}

	// Validate system prompt is not too long
	if len(config.SystemPrompt) > 10000 {
		errors = append(errors, fmt.Sprintf("system prompt too long: %d characters (max 10000)", len(config.SystemPrompt)))
	}

	if len(errors) > 0 {
		return fmt.Errorf("configuration validation failed:\n  - %s", strings.Join(errors, "\n  - "))
	}

	return nil
}

// ValidatePrompt validates the user prompt before starting the builder-reviewer loop.
//
// Validation rules:
//   - Minimum length: 3 characters (after trimming whitespace)
//   - Maximum length: 50,000 characters
//   - Cannot be empty or whitespace-only
//
// The prompt is the initial instruction given to the builder, so it must be
// substantive enough to guide the implementation.
//
// Returns:
//   - nil if prompt is valid
//   - error describing why the prompt is invalid
func ValidatePrompt(prompt string) error {
	trimmed := strings.TrimSpace(prompt)

	if trimmed == "" {
		return fmt.Errorf("prompt cannot be empty")
	}

	if len(trimmed) < 3 {
		return fmt.Errorf("prompt too short (minimum 3 characters): %q", trimmed)
	}

	if len(trimmed) > 50000 {
		return fmt.Errorf("prompt too long (maximum 50000 characters): %d", len(trimmed))
	}

	return nil
}

// SanitizeConfig applies defaults and sanitizes configuration values.
// This is called by New() to ensure the config is valid and has reasonable defaults.
//
// Default values applied:
//   - BuilderModel: "sonnet" (good balance of capability and cost)
//   - ReviewerModel: "gpt-5.2-codex" (specialized code reviewer)
//   - RecordingDir: "~/.yoloswe" (home directory for session logs)
//   - MaxBudgetUSD: $100.00 (prevents runaway costs)
//   - MaxTimeSeconds: 3600 (1 hour wall-clock time)
//   - MaxIterations: 10 (safety limit on builder-reviewer cycles)
//
// Sanitization performed:
//   - Trims whitespace from all string fields (paths, prompts, etc.)
//   - Replaces zero/negative values with defaults for numeric fields
//
// Note: This function modifies the config in-place.
func SanitizeConfig(config *Config) {
	// Apply model defaults
	if config.BuilderModel == "" {
		config.BuilderModel = "sonnet"
	}
	if config.ReviewerModel == "" {
		config.ReviewerModel = "gpt-5.2-codex"
	}

	// Apply recording directory default (expand ~ to home directory)
	if config.RecordingDir == "" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			config.RecordingDir = filepath.Join(homeDir, ".yoloswe")
		} else {
			config.RecordingDir = ".yoloswe"
		}
	}

	// Apply budget default
	if config.MaxBudgetUSD <= 0 {
		config.MaxBudgetUSD = 100.0
	}

	// Apply timeout default
	if config.MaxTimeSeconds <= 0 {
		config.MaxTimeSeconds = 3600 // 1 hour
	}

	// Apply max iterations default
	if config.MaxIterations <= 0 {
		config.MaxIterations = 10
	}

	// Trim whitespace from paths
	config.BuilderWorkDir = strings.TrimSpace(config.BuilderWorkDir)
	config.RecordingDir = strings.TrimSpace(config.RecordingDir)
	config.SystemPrompt = strings.TrimSpace(config.SystemPrompt)
	config.Goal = strings.TrimSpace(config.Goal)
}
