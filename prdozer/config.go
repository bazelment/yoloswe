package prdozer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level prdozer configuration.
type Config struct {
	WorkDir      string        `yaml:"work_dir"`
	BaseBranch   string        `yaml:"base_branch"`
	Agent        AgentConfig   `yaml:"agent"`
	Source       SourceConfig  `yaml:"source"`
	Polish       PolishConfig  `yaml:"polish"`
	Backoff      BackoffConfig `yaml:"backoff"`
	MaxBudgetUSD float64       `yaml:"max_budget_usd"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// AgentConfig selects the agent backend.
type AgentConfig struct {
	Model string `yaml:"model"`
}

// SourceConfig controls how PRs are discovered.
type SourceConfig struct {
	Mode          SourceMode   `yaml:"mode"`           // single | list | all
	PRs           []int        `yaml:"prs"`            // explicit PR numbers (single/list mode)
	Filter        SourceFilter `yaml:"filter"`         // discovery filter (all mode)
	MaxConcurrent int          `yaml:"max_concurrent"` // max parallel polish runs
}

// SourceMode is one of "single", "list", or "all".
type SourceMode string

const (
	SourceModeSingle SourceMode = "single"
	SourceModeList   SourceMode = "list"
	SourceModeAll    SourceMode = "all"
)

// SourceFilter narrows the set of PRs in --all mode.
type SourceFilter struct {
	Author        string   `yaml:"author"`         // gh PR query "author:" value (default "@me")
	Labels        []string `yaml:"labels"`         // require ANY of these labels
	ExcludeLabels []string `yaml:"exclude_labels"` // skip PRs carrying ANY of these labels
}

// PolishConfig controls the agent invocation per tick.
type PolishConfig struct {
	// PermissionMode is passed to the agent provider. Prdozer is designed for
	// unattended background operation, so the default is "bypass" (no
	// interactive prompts). This is a trust-boundary setting — the agent can
	// invoke any tool available on the host. Set to "default" to force
	// per-tool approval, or to any other value accepted by the provider.
	PermissionMode string  `yaml:"permission_mode"`
	MaxBudgetUSD   float64 `yaml:"max_budget_usd"` // overrides top-level budget; 0 inherits
	MaxTurns       int     `yaml:"max_turns"`      // cap turns for /pr-polish session
	Local          bool    `yaml:"local"`          // pass --local to /pr-polish
	AutoMerge      bool    `yaml:"auto_merge"`     // run gh pr merge when PR is mergeable
}

// BackoffConfig caps how aggressively prdozer keeps retrying after failures.
type BackoffConfig struct {
	MaxConsecutiveFailures int           `yaml:"max_consecutive_failures"`
	Cooldown               time.Duration `yaml:"cooldown"`
}

// DefaultConfig returns the built-in defaults with validate() applied so the
// no-config path in callers matches the file-backed path: budget inheritance,
// PRDOZER_PERMISSION_MODE env override, etc. all take effect. validate() on the
// built-in defaults cannot realistically fail (model/workdir are preset), but
// treat any failure as a programming error.
func DefaultConfig() *Config {
	c := defaultConfig()
	if err := c.validate(); err != nil {
		panic(fmt.Sprintf("prdozer: DefaultConfig failed to validate: %v", err))
	}
	return &c
}

func defaultConfig() Config {
	return Config{
		Agent:        AgentConfig{Model: "sonnet"},
		WorkDir:      ".",
		BaseBranch:   "main",
		PollInterval: 30 * time.Minute,
		MaxBudgetUSD: 50.0,
		Source: SourceConfig{
			Mode:          SourceModeAll,
			Filter:        SourceFilter{Author: "@me", ExcludeLabels: []string{"wip", "do-not-watch"}},
			MaxConcurrent: 3,
		},
		Polish: PolishConfig{
			Local:          false,
			AutoMerge:      false,
			MaxTurns:       100,
			PermissionMode: "bypass",
			// MaxBudgetUSD left at zero so validate() inherits the top-level value.
		},
		Backoff: BackoffConfig{
			MaxConsecutiveFailures: 3,
			Cooldown:               2 * time.Hour,
		},
	}
}

// LoadConfig reads and parses a prdozer YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.WorkDir = ExpandHome(cfg.WorkDir)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Agent.Model == "" {
		return fmt.Errorf("agent.model is required")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Minute
	}
	if c.Source.MaxConcurrent <= 0 {
		c.Source.MaxConcurrent = 3
	}
	switch c.Source.Mode {
	case SourceModeSingle, SourceModeList, SourceModeAll, "":
	default:
		return fmt.Errorf("source.mode %q is invalid (want single, list, or all)", c.Source.Mode)
	}
	if c.Source.Mode == SourceModeAll && c.Source.Filter.Author == "" {
		c.Source.Filter.Author = "@me"
	}
	// Nested polish budget inherits the top-level value when unset.
	if c.Polish.MaxBudgetUSD <= 0 && c.MaxBudgetUSD > 0 {
		c.Polish.MaxBudgetUSD = c.MaxBudgetUSD
	}
	if c.Polish.PermissionMode == "" {
		c.Polish.PermissionMode = "bypass"
	}
	if envMode := strings.TrimSpace(os.Getenv("PRDOZER_PERMISSION_MODE")); envMode != "" {
		c.Polish.PermissionMode = envMode
	}
	if err := ValidateWorkDir(c.WorkDir); err != nil {
		return err
	}
	return nil
}

// ValidateWorkDir checks that path exists and is a directory (skips "" and ".").
func ValidateWorkDir(path string) error {
	if path != "" && path != "." {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("work_dir %q: %w", path, err)
		}
		if !info.IsDir() {
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
