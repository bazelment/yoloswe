package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// RepoSettings holds per-repository Bramble settings.
type RepoSettings struct {
	OnWorktreeCreate []string `json:"on_worktree_create,omitempty"`
	OnWorktreeDelete []string `json:"on_worktree_delete,omitempty"`
}

// Settings holds persistent user preferences.
type Settings struct { //nolint:govet // fieldalignment: keep JSON field order readable
	ThemeName        string                  `json:"theme_name"`
	EnabledProviders []string                `json:"enabled_providers,omitempty"`
	Repos            map[string]RepoSettings `json:"repos,omitempty"`
}

// IsProviderEnabled returns true if the provider is enabled in settings.
// If EnabledProviders is nil/empty, all providers are considered enabled.
func (s Settings) IsProviderEnabled(provider string) bool {
	if len(s.EnabledProviders) == 0 {
		return true
	}
	for _, p := range s.EnabledProviders {
		if p == provider {
			return true
		}
	}
	return false
}

// SetEnabledProviders sets the list of enabled providers.
// A nil or empty list means all providers are enabled.
func (s *Settings) SetEnabledProviders(providers []string) {
	if len(providers) == 0 {
		s.EnabledProviders = nil
		return
	}
	s.EnabledProviders = make([]string, len(providers))
	copy(s.EnabledProviders, providers)
}

// RepoSettingsFor returns settings for one repository.
func (s Settings) RepoSettingsFor(repo string) RepoSettings {
	if s.Repos == nil {
		return RepoSettings{}
	}
	return s.Repos[repo]
}

// SetRepoSettings stores normalized settings for one repository.
func (s *Settings) SetRepoSettings(repo string, cfg RepoSettings) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return
	}
	cfg = normalizeRepoSettings(cfg)
	if len(cfg.OnWorktreeCreate) == 0 && len(cfg.OnWorktreeDelete) == 0 {
		if s.Repos != nil {
			delete(s.Repos, repo)
			if len(s.Repos) == 0 {
				s.Repos = nil
			}
		}
		return
	}
	if s.Repos == nil {
		s.Repos = make(map[string]RepoSettings)
	}
	s.Repos[repo] = cfg
}

func normalizeRepoSettings(cfg RepoSettings) RepoSettings {
	cfg.OnWorktreeCreate = normalizeCommands(cfg.OnWorktreeCreate)
	cfg.OnWorktreeDelete = normalizeCommands(cfg.OnWorktreeDelete)
	return cfg
}

func normalizeCommands(commands []string) []string {
	if len(commands) == 0 {
		return nil
	}
	out := make([]string, 0, len(commands))
	for _, c := range commands {
		c = strings.TrimSpace(c)
		if c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// settingsDir returns the path to ~/.bramble.
func settingsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bramble"), nil
}

// settingsPath returns the path to ~/.bramble/settings.json.
func settingsPath() (string, error) {
	dir, err := settingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// LoadSettings reads settings from ~/.bramble/settings.json.
// Returns default settings if the file is missing or unreadable.
func LoadSettings() Settings {
	p, err := settingsPath()
	if err != nil {
		return Settings{}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return Settings{}
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}
	}
	for repo, cfg := range s.Repos {
		s.Repos[repo] = normalizeRepoSettings(cfg)
	}
	return s
}

// SaveSettings writes settings to ~/.bramble/settings.json.
func SaveSettings(s Settings) error {
	dir, err := settingsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "settings.json")
	return os.WriteFile(p, data, 0o644)
}
