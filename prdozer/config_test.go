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
