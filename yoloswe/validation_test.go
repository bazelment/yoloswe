package yoloswe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfig(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir := t.TempDir()

	tests := []struct {
		name      string
		errorText string
		config    Config
		wantError bool
	}{
		{
			name: "valid minimal config",
			config: Config{
				BuilderWorkDir: tmpDir,
			},
			wantError: false,
		},
		{
			name: "valid full config",
			config: Config{
				BuilderModel:   "sonnet",
				ReviewerModel:  "gpt-5.2-codex",
				BuilderWorkDir: tmpDir,
				RecordingDir:   tmpDir,
				MaxBudgetUSD:   5.0,
				MaxTimeSeconds: 600,
				MaxIterations:  10,
			},
			wantError: false,
		},
		{
			name: "invalid builder model",
			config: Config{
				BuilderModel: "invalid-model",
			},
			wantError: true,
			errorText: "invalid builder model",
		},
		{
			name: "nonexistent working directory",
			config: Config{
				BuilderWorkDir: "/nonexistent/path/to/nowhere",
			},
			wantError: true,
			errorText: "does not exist",
		},
		{
			name: "negative budget",
			config: Config{
				MaxBudgetUSD: -5.0,
			},
			wantError: true,
			errorText: "cannot be negative",
		},
		{
			name: "budget too low",
			config: Config{
				MaxBudgetUSD: 0.005,
			},
			wantError: true,
			errorText: "too low",
		},
		{
			name: "negative timeout",
			config: Config{
				MaxTimeSeconds: -100,
			},
			wantError: true,
			errorText: "cannot be negative",
		},
		{
			name: "timeout too low",
			config: Config{
				MaxTimeSeconds: 5,
			},
			wantError: true,
			errorText: "too low",
		},
		{
			name: "negative iterations",
			config: Config{
				MaxIterations: -5,
			},
			wantError: true,
			errorText: "cannot be negative",
		},
		{
			name: "system prompt too long",
			config: Config{
				SystemPrompt: strings.Repeat("a", 10001),
			},
			wantError: true,
			errorText: "too long",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConfig(tt.config)

			if tt.wantError && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("expected no error but got: %v", err)
			}
			if tt.wantError && err != nil && tt.errorText != "" {
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errorText)) {
					t.Errorf("expected error to contain %q, got: %v", tt.errorText, err)
				}
			}
		})
	}
}

func TestValidatePrompt(t *testing.T) {
	tests := []struct {
		name      string
		prompt    string
		errorText string
		wantError bool
	}{
		{
			name:      "valid prompt",
			prompt:    "Add unit tests for the service",
			wantError: false,
		},
		{
			name:      "valid long prompt",
			prompt:    strings.Repeat("a", 1000),
			wantError: false,
		},
		{
			name:      "empty prompt",
			prompt:    "",
			wantError: true,
			errorText: "cannot be empty",
		},
		{
			name:      "whitespace only prompt",
			prompt:    "   \n\t  ",
			wantError: true,
			errorText: "cannot be empty",
		},
		{
			name:      "prompt too short",
			prompt:    "ab",
			wantError: true,
			errorText: "too short",
		},
		{
			name:      "prompt too long",
			prompt:    strings.Repeat("a", 50001),
			wantError: true,
			errorText: "too long",
		},
		{
			name:      "prompt with leading/trailing whitespace",
			prompt:    "  valid prompt  ",
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePrompt(tt.prompt)

			if tt.wantError && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("expected no error but got: %v", err)
			}
			if tt.wantError && err != nil && tt.errorText != "" {
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errorText)) {
					t.Errorf("expected error to contain %q, got: %v", tt.errorText, err)
				}
			}
		})
	}
}

func TestSanitizeConfig(t *testing.T) {
	// Get expected default recording dir
	homeDir, _ := os.UserHomeDir()
	defaultRecordingDir := filepath.Join(homeDir, ".yoloswe")

	tests := []struct {
		name     string
		input    Config
		expected Config
	}{
		{
			name:  "empty config gets defaults",
			input: Config{},
			expected: Config{
				BuilderModel:   "sonnet",
				ReviewerModel:  "gpt-5.2-codex",
				RecordingDir:   defaultRecordingDir,
				MaxBudgetUSD:   100.0,
				MaxTimeSeconds: 3600,
				MaxIterations:  10,
			},
		},
		{
			name: "custom values preserved",
			input: Config{
				BuilderModel:   "opus",
				ReviewerModel:  "o4-mini",
				RecordingDir:   "/custom",
				MaxBudgetUSD:   10.0,
				MaxTimeSeconds: 1800,
				MaxIterations:  20,
			},
			expected: Config{
				BuilderModel:   "opus",
				ReviewerModel:  "o4-mini",
				RecordingDir:   "/custom",
				MaxBudgetUSD:   10.0,
				MaxTimeSeconds: 1800,
				MaxIterations:  20,
			},
		},
		{
			name: "zero values replaced with defaults",
			input: Config{
				BuilderModel:   "haiku",
				MaxBudgetUSD:   0,
				MaxTimeSeconds: 0,
				MaxIterations:  0,
			},
			expected: Config{
				BuilderModel:   "haiku",
				ReviewerModel:  "gpt-5.2-codex",
				RecordingDir:   defaultRecordingDir,
				MaxBudgetUSD:   100.0,
				MaxTimeSeconds: 3600,
				MaxIterations:  10,
			},
		},
		{
			name: "negative values replaced with defaults",
			input: Config{
				MaxBudgetUSD:   -1.0,
				MaxTimeSeconds: -100,
				MaxIterations:  -5,
			},
			expected: Config{
				BuilderModel:   "sonnet",
				ReviewerModel:  "gpt-5.2-codex",
				RecordingDir:   defaultRecordingDir,
				MaxBudgetUSD:   100.0,
				MaxTimeSeconds: 3600,
				MaxIterations:  10,
			},
		},
		{
			name: "whitespace trimmed from strings",
			input: Config{
				BuilderWorkDir: "  /tmp/work  ",
				RecordingDir:   "  /tmp/recordings  ",
				SystemPrompt:   "  custom prompt  ",
				Goal:           "  task goal  ",
			},
			expected: Config{
				BuilderModel:   "sonnet",
				ReviewerModel:  "gpt-5.2-codex",
				BuilderWorkDir: "/tmp/work",
				RecordingDir:   "/tmp/recordings",
				SystemPrompt:   "custom prompt",
				Goal:           "task goal",
				MaxBudgetUSD:   100.0,
				MaxTimeSeconds: 3600,
				MaxIterations:  10,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := tt.input
			SanitizeConfig(&config)

			if config.BuilderModel != tt.expected.BuilderModel {
				t.Errorf("BuilderModel: got %q, want %q", config.BuilderModel, tt.expected.BuilderModel)
			}
			if config.ReviewerModel != tt.expected.ReviewerModel {
				t.Errorf("ReviewerModel: got %q, want %q", config.ReviewerModel, tt.expected.ReviewerModel)
			}
			if config.RecordingDir != tt.expected.RecordingDir {
				t.Errorf("RecordingDir: got %q, want %q", config.RecordingDir, tt.expected.RecordingDir)
			}
			if config.MaxBudgetUSD != tt.expected.MaxBudgetUSD {
				t.Errorf("MaxBudgetUSD: got %.2f, want %.2f", config.MaxBudgetUSD, tt.expected.MaxBudgetUSD)
			}
			if config.MaxTimeSeconds != tt.expected.MaxTimeSeconds {
				t.Errorf("MaxTimeSeconds: got %d, want %d", config.MaxTimeSeconds, tt.expected.MaxTimeSeconds)
			}
			if config.MaxIterations != tt.expected.MaxIterations {
				t.Errorf("MaxIterations: got %d, want %d", config.MaxIterations, tt.expected.MaxIterations)
			}
			if config.BuilderWorkDir != tt.expected.BuilderWorkDir {
				t.Errorf("BuilderWorkDir: got %q, want %q", config.BuilderWorkDir, tt.expected.BuilderWorkDir)
			}
			if config.SystemPrompt != tt.expected.SystemPrompt {
				t.Errorf("SystemPrompt: got %q, want %q", config.SystemPrompt, tt.expected.SystemPrompt)
			}
			if config.Goal != tt.expected.Goal {
				t.Errorf("Goal: got %q, want %q", config.Goal, tt.expected.Goal)
			}
		})
	}
}

func TestValidateConfigWithFileOperations(t *testing.T) {
	// Create a temporary file (not a directory)
	tmpFile, err := os.CreateTemp("", "test-file")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	t.Run("working directory is a file not a directory", func(t *testing.T) {
		config := Config{
			BuilderWorkDir: tmpFile.Name(),
		}
		err := ValidateConfig(config)
		if err == nil {
			t.Error("expected error for file path as directory")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("expected 'not a directory' error, got: %v", err)
		}
	})

	t.Run("recording directory parent does not exist", func(t *testing.T) {
		config := Config{
			RecordingDir: "/nonexistent/parent/dir/recordings",
		}
		err := ValidateConfig(config)
		if err == nil {
			t.Error("expected error for nonexistent parent directory")
		}
	})
}

func TestValidateConfigWarnings(t *testing.T) {
	// These tests check that warnings are issued for unusual but valid configs
	// We can't easily test stderr output, so we just ensure they don't error

	t.Run("high budget does not error", func(t *testing.T) {
		config := Config{
			MaxBudgetUSD: 1500,
			Verbose:      true,
		}
		err := ValidateConfig(config)
		if err != nil {
			t.Errorf("high budget should only warn, not error: %v", err)
		}
	})

	t.Run("high timeout does not error", func(t *testing.T) {
		config := Config{
			MaxTimeSeconds: 100000,
			Verbose:        true,
		}
		err := ValidateConfig(config)
		if err != nil {
			t.Errorf("high timeout should only warn, not error: %v", err)
		}
	})

	t.Run("high iterations does not error", func(t *testing.T) {
		config := Config{
			MaxIterations: 150,
			Verbose:       true,
		}
		err := ValidateConfig(config)
		if err != nil {
			t.Errorf("high iterations should only warn, not error: %v", err)
		}
	})

	t.Run("unknown reviewer model does not error", func(t *testing.T) {
		config := Config{
			ReviewerModel: "custom-model",
			Verbose:       true,
		}
		err := ValidateConfig(config)
		if err != nil {
			t.Errorf("unknown reviewer model should only warn, not error: %v", err)
		}
	})
}
