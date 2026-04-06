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
	States       StatesConfig  `yaml:"states"`
	Agent        AgentConfig   `yaml:"agent"`
	Source       SourceConfig  `yaml:"source"`
	WorkDir      string        `yaml:"work_dir"`
	BaseBranch   string        `yaml:"base_branch"`
	Plan         StepConfig    `yaml:"plan"`
	Build        StepConfig    `yaml:"build"`
	Validate     StepConfig    `yaml:"validate"`
	Ship         StepConfig    `yaml:"ship"`
	PollInterval time.Duration `yaml:"poll_interval"`
	MaxBudgetUSD float64       `yaml:"max_budget_usd"`
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

// SourceConfig specifies how to discover issues for multi-issue mode.
type SourceConfig struct {
	Team          string   `yaml:"team"`           // Linear team key (e.g. "ENG")
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
	Prompt         string  `yaml:"prompt"`          // Go text/template; empty = built-in default
	SystemPrompt   string  `yaml:"system_prompt"`   // optional system prompt passed to the agent
	Model          string  `yaml:"model"`           // override agent.model; empty = inherit
	PermissionMode string  `yaml:"permission_mode"` // "plan", "bypass", etc.; empty = step default
	MaxTurns       int     `yaml:"max_turns"`
	MaxBudgetUSD   float64 `yaml:"max_budget_usd"` // override top-level; 0 = inherit
	AutoApprove    bool    `yaml:"auto_approve"`   // skip human review after this step
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
	apiKey, err := resolveEnv(cfg.Tracker.APIKey)
	if err != nil {
		return nil, fmt.Errorf("tracker.api_key: %w", err)
	}
	cfg.Tracker.APIKey = apiKey

	// Expand ~ in work_dir.
	cfg.WorkDir = ExpandHome(cfg.WorkDir)

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// DefaultConfigForTest returns the default config with sensible defaults.
// Exported for use in integration tests.
func DefaultConfigForTest() *Config {
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
	if c.Tracker.APIKey == "" {
		return fmt.Errorf("tracker.api_key is required (set via config or $LINEAR_API_KEY)")
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
		if step.Prompt != "" {
			if _, err := template.New(name).Parse(step.Prompt); err != nil {
				return fmt.Errorf("%s.prompt template: %w", name, err)
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
