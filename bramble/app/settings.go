package app

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings holds persistent user preferences.
type Settings struct {
	ThemeName string `json:"theme_name"`
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
