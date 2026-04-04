package jiradozer

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for jiradozer.
type Config struct {
	States       StatesConfig     `yaml:"states"`
	Tracker      TrackerConfig    `yaml:"tracker"`
	Plan         StepConfig       `yaml:"plan"`
	Build        StepConfig       `yaml:"build"`
	Agent        AgentConfig      `yaml:"agent"`
	WorkDir      string           `yaml:"work_dir"`
	BaseBranch   string           `yaml:"base_branch"`
	Validation   ValidationConfig `yaml:"validation"`
	PollInterval time.Duration    `yaml:"poll_interval"`
	MaxBudgetUSD float64          `yaml:"max_budget_usd"`
}

// TrackerConfig specifies the issue tracker backend.
type TrackerConfig struct {
	Kind   string `yaml:"kind"`    // "linear", future: "github", "jira"
	APIKey string `yaml:"api_key"` // supports $ENV_VAR expansion
}

// AgentConfig specifies the agent backend.
type AgentConfig struct {
	Model string `yaml:"model"` // model ID from agent.AllModels (e.g. "sonnet", "gpt-5.3-codex")
}

// StepConfig configures a single workflow step (plan or build).
type StepConfig struct {
	SystemPrompt string `yaml:"system_prompt"`
	MaxTurns     int    `yaml:"max_turns"`
}

// ValidationConfig specifies validation commands to run.
type ValidationConfig struct {
	Commands       []string `yaml:"commands"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

// StatesConfig maps logical workflow states to tracker-specific state names.
type StatesConfig struct {
	InProgress string `yaml:"in_progress"`
	InReview   string `yaml:"in_review"`
	Done       string `yaml:"done"`
}

// LoadConfig reads and parses a jiradozer YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Expand environment variables in sensitive fields.
	cfg.Tracker.APIKey = resolveEnv(cfg.Tracker.APIKey)

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func defaultConfig() Config {
	return Config{
		Tracker: TrackerConfig{Kind: "linear"},
		Agent:   AgentConfig{Model: "sonnet"},
		Plan:    StepConfig{MaxTurns: 10},
		Build:   StepConfig{MaxTurns: 30},
		Validation: ValidationConfig{
			TimeoutSeconds: 300,
		},
		States: StatesConfig{
			InProgress: "In Progress",
			InReview:   "In Review",
			Done:       "Done",
		},
		WorkDir:      ".",
		BaseBranch:   "main",
		PollInterval: 15 * time.Second,
		MaxBudgetUSD: 50.0,
	}
}

func (c *Config) validate() error {
	if c.Tracker.Kind == "" {
		return fmt.Errorf("tracker.kind is required")
	}
	if c.Tracker.APIKey == "" {
		return fmt.Errorf("tracker.api_key is required (set via config or $LINEAR_API_KEY)")
	}
	if c.Agent.Model == "" {
		return fmt.Errorf("agent.model is required")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 15 * time.Second
	}
	return nil
}

// resolveEnv expands a value that starts with $ as an environment variable.
func resolveEnv(value string) string {
	if strings.HasPrefix(value, "$") {
		envName := strings.TrimPrefix(value, "$")
		if v := os.Getenv(envName); v != "" {
			return v
		}
	}
	return value
}
