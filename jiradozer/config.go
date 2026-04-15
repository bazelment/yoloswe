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
	CreatePR     StepConfig    `yaml:"create_pr"`
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
	Filters       map[string]string `yaml:"filters"`        // Generic key-value filters (see tracker.IssueFilter)
	BranchPrefix  string            `yaml:"branch_prefix"`  // Worktree branch prefix (default: "jiradozer")
	MaxConcurrent int               `yaml:"max_concurrent"` // Max parallel workflows (default: 3)
	DryRun        bool              `yaml:"dry_run"`        // Print equivalent bramble new-session command instead of launching a workflow
}

// ToFilter converts the source config to a tracker.IssueFilter.
func (s SourceConfig) ToFilter() tracker.IssueFilter {
	return tracker.IssueFilter{Filters: s.Filters}
}

// HasSource reports whether the source config specifies enough to enter
// multi-issue mode (at least one filter key must be set).
func (s SourceConfig) HasSource() bool {
	return len(s.Filters) > 0
}

// StepConfig configures a single workflow step (plan or build).
type StepConfig struct {
	Prompt              string        `yaml:"prompt"`          // Go text/template; empty = built-in default
	SystemPrompt        string        `yaml:"system_prompt"`   // optional system prompt passed to the agent
	Model               string        `yaml:"model"`           // override agent.model; empty = inherit
	PermissionMode      string        `yaml:"permission_mode"` // "plan", "bypass", etc.; empty = step default
	Rounds              []RoundConfig `yaml:"rounds"`          // multi-round execution; mutually exclusive with Prompt
	MaxBudgetUSD        float64       `yaml:"max_budget_usd"`  // override top-level; 0 = inherit
	MaxTurns            int           `yaml:"max_turns"`
	MaxToolErrorRetries int           `yaml:"max_tool_error_retries"` // retries when a turn ends with an unresolved tool error; 0 = disabled
	AutoApprove         bool          `yaml:"auto_approve"`           // skip human review after this step
}

// RoundConfig configures a single round within a multi-round step.
// Zero-value fields inherit from the parent StepConfig.
// Exactly one of Prompt or Command must be set.
type RoundConfig struct {
	Prompt              string  `yaml:"prompt"`                 // Go text/template; mutually exclusive with Command
	Command             string  `yaml:"command"`                // Shell command template (sh -c); mutually exclusive with Prompt. WARNING: tracker fields like Title/Description are user-controlled — only interpolate them when the issue source is fully trusted.
	SystemPrompt        string  `yaml:"system_prompt"`          // optional system prompt (agent rounds only)
	Model               string  `yaml:"model"`                  // override; empty = inherit from step (agent rounds only)
	MaxTurns            int     `yaml:"max_turns"`              // override; 0 = inherit from step (agent rounds only)
	MaxToolErrorRetries int     `yaml:"max_tool_error_retries"` // override; 0 = inherit from step (agent rounds only)
	MaxBudgetUSD        float64 `yaml:"max_budget_usd"`         // override; 0 = inherit from step (agent rounds only)
}

// IsCommand reports whether this round runs a shell command instead of an agent session.
func (r RoundConfig) IsCommand() bool {
	return r.Command != ""
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
			MaxConcurrent: 3,
			BranchPrefix:  "jiradozer",
		},
		Plan:     StepConfig{PermissionMode: "plan", MaxTurns: 10},
		Build:    StepConfig{PermissionMode: "bypass", MaxTurns: 30},
		CreatePR: StepConfig{PermissionMode: "bypass", MaxTurns: 5},
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
		"plan": c.Plan, "build": c.Build, "create_pr": c.CreatePR, "validate": c.Validate, "ship": c.Ship,
	} {
		if name == "create_pr" && len(step.Rounds) > 0 {
			return fmt.Errorf("create_pr does not support rounds")
		}
		if step.Prompt != "" && len(step.Rounds) > 0 {
			return fmt.Errorf("%s: prompt and rounds are mutually exclusive", name)
		}
		if step.Prompt != "" {
			if _, err := template.New(name).Parse(step.Prompt); err != nil {
				return fmt.Errorf("%s.prompt template: %w", name, err)
			}
		}
		for i, round := range step.Rounds {
			if round.Prompt == "" && round.Command == "" {
				return fmt.Errorf("%s.rounds[%d]: prompt or command is required", name, i)
			}
			if round.Prompt != "" && round.Command != "" {
				return fmt.Errorf("%s.rounds[%d]: prompt and command are mutually exclusive", name, i)
			}
			if round.Prompt != "" {
				if _, err := template.New(fmt.Sprintf("%s_round_%d", name, i)).Parse(round.Prompt); err != nil {
					return fmt.Errorf("%s.rounds[%d].prompt template: %w", name, i, err)
				}
			}
			if round.Command != "" {
				if _, err := template.New(fmt.Sprintf("%s_round_%d_cmd", name, i)).Parse(round.Command); err != nil {
					return fmt.Errorf("%s.rounds[%d].command template: %w", name, i, err)
				}
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
	case "create_pr":
		return c.CreatePR, true
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
	if round.MaxToolErrorRetries > 0 {
		resolved.MaxToolErrorRetries = round.MaxToolErrorRetries
	} else {
		resolved.MaxToolErrorRetries = parent.MaxToolErrorRetries
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
