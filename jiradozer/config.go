package jiradozer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

// Config is the top-level configuration for jiradozer.
type Config struct {
	Tracker      TrackerConfig `yaml:"tracker"`
	Source       SourceConfig  `yaml:"source"`
	States       StatesConfig  `yaml:"states"`
	Agent        AgentConfig   `yaml:"agent"`
	WorkDir      string        `yaml:"work_dir"`
	BaseBranch   string        `yaml:"base_branch"`
	Plan         StepConfig    `yaml:"plan"`
	Build        StepConfig    `yaml:"build"`
	Validate     StepConfig    `yaml:"validate"`
	Ship         StepConfig    `yaml:"ship"`
	MaxBudgetUSD float64       `yaml:"max_budget_usd"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// TrackerConfig specifies the issue tracker backend.
type TrackerConfig struct {
	Kind   string `yaml:"kind"`    // "linear", "github", "local"
	APIKey string `yaml:"api_key"` // supports $ENV_VAR expansion
}

// AgentConfig specifies the agent backend.
type AgentConfig struct {
	Model string `yaml:"model"` // model ID from agent.AllModels (e.g. "sonnet", "gpt-5.3-codex")
}

// SourceConfig specifies how to discover issues for multi-issue mode.
type SourceConfig struct {
	Team          string   `yaml:"team"`           // Team or repo identifier (e.g. "ENG" for Linear, "owner/repo" for GitHub)
	BranchPrefix  string   `yaml:"branch_prefix"`  // Worktree branch prefix (default: "jiradozer")
	States        []string `yaml:"states"`         // Issue states to pick up (default: ["Todo"])
	Labels        []string `yaml:"labels"`         // Optional label filter
	MaxConcurrent int      `yaml:"max_concurrent"` // Max parallel workflows (default: 3)
}

// ToFilter converts the source config to a tracker.IssueFilter.
func (s SourceConfig) ToFilter() tracker.IssueFilter {
	return tracker.IssueFilter{
		TeamKey: s.Team,
		States:  s.States,
		Labels:  s.Labels,
	}
}

// StepConfig configures a single workflow step (plan or build).
type StepConfig struct {
	Prompt         string        `yaml:"prompt"`          // Go text/template; empty = built-in default
	SystemPrompt   string        `yaml:"system_prompt"`   // optional system prompt passed to the agent
	Model          string        `yaml:"model"`           // override agent.model; empty = inherit
	PermissionMode string        `yaml:"permission_mode"` // "plan", "bypass", etc.; empty = step default
	Rounds         []RoundConfig `yaml:"rounds"`          // multi-round execution; mutually exclusive with Prompt
	MaxBudgetUSD   float64       `yaml:"max_budget_usd"`  // override top-level; 0 = inherit
	MaxTurns       int           `yaml:"max_turns"`
	AutoApprove    bool          `yaml:"auto_approve"` // skip human review after this step
}

// RoundConfig configures a single round within a multi-round step.
// Zero-value fields inherit from the parent StepConfig.
type RoundConfig struct {
	Prompt       string  `yaml:"prompt"`         // Go text/template (required)
	SystemPrompt string  `yaml:"system_prompt"`  // optional system prompt
	Model        string  `yaml:"model"`          // override; empty = inherit from step
	MaxTurns     int     `yaml:"max_turns"`      // override; 0 = inherit from step
	MaxBudgetUSD float64 `yaml:"max_budget_usd"` // override; 0 = inherit from step
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

	// Expand environment variables in API key when present.
	// Local tracker has no API key; GitHub tracker uses gh CLI auth (API key optional).
	if cfg.Tracker.APIKey != "" {
		apiKey, err := resolveEnv(cfg.Tracker.APIKey)
		if err != nil {
			return nil, fmt.Errorf("tracker.api_key: %w", err)
		}
		cfg.Tracker.APIKey = apiKey
	}

	// Expand ~ in work_dir.
	cfg.WorkDir = ExpandHome(cfg.WorkDir)

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// DefaultConfig returns the default config with sensible defaults.
func DefaultConfig() *Config {
	cfg := defaultConfig()
	return &cfg
}

func defaultConfig() Config {
	return Config{
		Tracker: TrackerConfig{Kind: "linear"},
		Agent:   AgentConfig{Model: "sonnet"},
		Source: SourceConfig{
			States:        []string{"Todo"},
			MaxConcurrent: 3,
			BranchPrefix:  "jiradozer",
		},
		Plan:     StepConfig{PermissionMode: "plan", MaxTurns: 10},
		Build:    StepConfig{PermissionMode: "bypass", MaxTurns: 30},
		Validate: StepConfig{PermissionMode: "bypass", MaxTurns: 10},
		Ship:     StepConfig{PermissionMode: "bypass", MaxTurns: 10},
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
	if c.Tracker.Kind != "local" && c.Tracker.Kind != "github" && c.Tracker.APIKey == "" {
		return fmt.Errorf("tracker.api_key is required (set via config or environment variable)")
	}
	if c.Agent.Model == "" {
		return fmt.Errorf("agent.model is required")
	}
	if err := ValidateWorkDir(c.WorkDir); err != nil {
		return err
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 15 * time.Second
	}
	for name, step := range map[string]StepConfig{
		"plan": c.Plan, "build": c.Build, "validate": c.Validate, "ship": c.Ship,
	} {
		if step.Prompt != "" && len(step.Rounds) > 0 {
			return fmt.Errorf("%s: prompt and rounds are mutually exclusive", name)
		}
		if step.Prompt != "" {
			if _, err := template.New(name).Parse(step.Prompt); err != nil {
				return fmt.Errorf("%s.prompt template: %w", name, err)
			}
		}
		for i, round := range step.Rounds {
			if round.Prompt == "" {
				return fmt.Errorf("%s.rounds[%d]: prompt is required", name, i)
			}
			if _, err := template.New(fmt.Sprintf("%s_round_%d", name, i)).Parse(round.Prompt); err != nil {
				return fmt.Errorf("%s.rounds[%d].prompt template: %w", name, i, err)
			}
		}
	}
	return nil
}

// StepByName returns the StepConfig for a named step.
func (c *Config) StepByName(name string) (StepConfig, bool) {
	switch name {
	case "plan":
		return c.Plan, true
	case "build":
		return c.Build, true
	case "validate":
		return c.Validate, true
	case "ship":
		return c.Ship, true
	default:
		return StepConfig{}, false
	}
}

// ResolveStep fills zero-value fields in a StepConfig from top-level defaults.
func (c *Config) ResolveStep(step StepConfig) StepConfig {
	if step.Model == "" {
		step.Model = c.Agent.Model
	}
	if step.MaxBudgetUSD == 0 {
		step.MaxBudgetUSD = c.MaxBudgetUSD
	}
	return step
}

// ResolveRound converts a RoundConfig into a fully-resolved StepConfig,
// inheriting zero-value fields from the parent step.
func ResolveRound(round RoundConfig, parent StepConfig) StepConfig {
	systemPrompt := round.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = parent.SystemPrompt
	}
	resolved := StepConfig{
		Prompt:         round.Prompt,
		SystemPrompt:   systemPrompt,
		PermissionMode: parent.PermissionMode,
	}
	if round.Model != "" {
		resolved.Model = round.Model
	} else {
		resolved.Model = parent.Model
	}
	if round.MaxTurns > 0 {
		resolved.MaxTurns = round.MaxTurns
	} else {
		resolved.MaxTurns = parent.MaxTurns
	}
	if round.MaxBudgetUSD > 0 {
		resolved.MaxBudgetUSD = round.MaxBudgetUSD
	} else {
		resolved.MaxBudgetUSD = parent.MaxBudgetUSD
	}
	return resolved
}

// ValidateWorkDir checks that a work_dir path exists and is a directory.
func ValidateWorkDir(path string) error {
	if path != "" && path != "." {
		if info, err := os.Stat(path); err != nil {
			return fmt.Errorf("work_dir %q: %w", path, err)
		} else if !info.IsDir() {
			return fmt.Errorf("work_dir %q is not a directory", path)
		}
	}
	return nil
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// resolveEnv expands a value that starts with $ as an environment variable.
// Returns an error if the value references an env var that is not set.
func resolveEnv(value string) (string, error) {
	if strings.HasPrefix(value, "$") {
		envName := strings.TrimPrefix(value, "$")
		if v := os.Getenv(envName); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("environment variable %s is not set", envName)
	}
	return value, nil
}
