package prdozer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig_Sane(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	assert.Equal(t, "sonnet", c.Agent.Model)
	assert.Equal(t, SourceModeAll, c.Source.Mode)
	assert.Equal(t, "@me", c.Source.Filter.Author)
	assert.Equal(t, 30*time.Minute, c.PollInterval)
	assert.Equal(t, 3, c.Source.MaxConcurrent)
	assert.False(t, c.Polish.AutoMerge)
}

func TestDefaultConfig_RunsValidateForBudgetInheritance(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	assert.Greater(t, c.MaxBudgetUSD, 0.0, "top-level budget must have a default")
	assert.Equal(t, c.MaxBudgetUSD, c.Polish.MaxBudgetUSD,
		"no-config path must inherit top-level budget into polish.max_budget_usd")
	assert.Equal(t, "bypass", c.Polish.PermissionMode,
		"no-config path must populate the permission mode default")
}

func TestDefaultConfig_AppliesEnvPermissionMode(t *testing.T) {
	// No t.Parallel — mutates process env.
	t.Setenv("PRDOZER_PERMISSION_MODE", "plan")
	c := DefaultConfig()
	assert.Equal(t, "plan", c.Polish.PermissionMode,
		"PRDOZER_PERMISSION_MODE must take effect even when no config file is loaded")
}

func TestLoadConfig_Overrides(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	yaml := `
agent:
  model: opus
poll_interval: 5m
source:
  mode: list
  prs: [101, 102, 103]
  max_concurrent: 1
polish:
  local: true
  auto_merge: true
backoff:
  max_consecutive_failures: 5
  cooldown: 1h
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "opus", c.Agent.Model)
	assert.Equal(t, 5*time.Minute, c.PollInterval)
	assert.Equal(t, SourceModeList, c.Source.Mode)
	assert.Equal(t, []int{101, 102, 103}, c.Source.PRs)
	assert.Equal(t, 1, c.Source.MaxConcurrent)
	assert.True(t, c.Polish.Local)
	assert.True(t, c.Polish.AutoMerge)
	assert.Equal(t, 5, c.Backoff.MaxConsecutiveFailures)
	assert.Equal(t, time.Hour, c.Backoff.Cooldown)
}

func TestLoadConfig_InvalidMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	require.NoError(t, os.WriteFile(path, []byte("source:\n  mode: bogus\n"), 0o600))
	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source.mode")
}

func TestLoadConfig_DefaultsAuthorWhenAllMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	require.NoError(t, os.WriteFile(path, []byte("source:\n  mode: all\n"), 0o600))
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "@me", c.Source.Filter.Author)
}

func TestLoadConfig_TopLevelBudgetInheritedByPolish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	yaml := `
max_budget_usd: 77.5
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 77.5, c.MaxBudgetUSD)
	assert.Equal(t, 77.5, c.Polish.MaxBudgetUSD,
		"nested polish.max_budget_usd should inherit the top-level value when not set explicitly")
}

func TestLoadConfig_PolishBudgetOverridesTopLevel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	yaml := `
max_budget_usd: 99
polish:
  max_budget_usd: 10
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 99.0, c.MaxBudgetUSD)
	assert.Equal(t, 10.0, c.Polish.MaxBudgetUSD,
		"explicit nested value must win over top-level")
}

func TestLoadConfig_PermissionModeDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	t.Setenv("PRDOZER_PERMISSION_MODE", "")
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "bypass", c.Polish.PermissionMode)
}

func TestLoadConfig_PermissionModeYAMLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	yaml := `
polish:
  permission_mode: default
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	t.Setenv("PRDOZER_PERMISSION_MODE", "")
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "default", c.Polish.PermissionMode)
}

func TestLoadConfig_PermissionModeEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prdozer.yaml")
	yaml := `
polish:
  permission_mode: default
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	t.Setenv("PRDOZER_PERMISSION_MODE", "plan")
	c, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "plan", c.Polish.PermissionMode, "env must win over YAML")
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/tmp/prdozer-home")
	assert.Equal(t, "/tmp/prdozer-home/subdir", ExpandHome("~/subdir"))
	assert.Equal(t, "/abs", ExpandHome("/abs"))
	assert.Equal(t, "rel/path", ExpandHome("rel/path"))
}

func TestValidateWorkDir(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateWorkDir(""))
	require.NoError(t, ValidateWorkDir("."))
	dir := t.TempDir()
	require.NoError(t, ValidateWorkDir(dir))
	file := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	require.Error(t, ValidateWorkDir(file))
}
