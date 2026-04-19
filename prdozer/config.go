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
	Agent        AgentConfig   `yaml:"agent"`
	WorkDir      string        `yaml:"work_dir"`
	BaseBranch   string        `yaml:"base_branch"`
	Source       SourceConfig  `yaml:"source"`
	Polish       PolishConfig  `yaml:"polish"`
	Backoff      BackoffConfig `yaml:"backoff"`
	PollInterval time.Duration `yaml:"poll_interval"`
	MaxBudgetUSD float64       `yaml:"max_budget_usd"`
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
	Local        bool    `yaml:"local"`          // pass --local to /pr-polish
	AutoMerge    bool    `yaml:"auto_merge"`     // run gh pr merge when PR is mergeable
	MaxTurns     int     `yaml:"max_turns"`      // cap turns for /pr-polish session
	MaxBudgetUSD float64 `yaml:"max_budget_usd"` // override top-level budget
}

// BackoffConfig caps how aggressively prdozer keeps retrying after failures.
type BackoffConfig struct {
	MaxConsecutiveFailures int           `yaml:"max_consecutive_failures"`
	Cooldown               time.Duration `yaml:"cooldown"`
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() *Config {
	c := defaultConfig()
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
			Local:        false,
			AutoMerge:    false,
			MaxTurns:     100,
			MaxBudgetUSD: 20.0,
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
