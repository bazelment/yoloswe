package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// preTrustWorkspace seeds a provider's trust store before its interactive
// session starts. Failures are non-fatal, preserving the CLI's normal dialog.
func preTrustWorkspace(provider, workDir string) {
	if workDir == "" {
		return
	}
	var err error
	switch provider {
	case ProviderAgy:
		err = trustAgyWorkspace(workDir)
	case ProviderCodex:
		err = trustCodexWorkspace(workDir)
	default:
		return
	}
	if err != nil {
		slog.Warn("failed to pre-trust workspace; the CLI's own trust dialog will still appear",
			"provider", provider, "workdir", workDir, "error", err)
	}
}

const agySettingsRelPath = "antigravity-cli/settings.json"

// trustAgyWorkspace records workDir in agy's trustedWorkspaces list.
func trustAgyWorkspace(workDir string) error {
	path, err := agySettingsPath()
	if err != nil {
		return err
	}
	return withFileLock(path, func() error {
		settings := map[string]any{}
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, &settings); err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
		case os.IsNotExist(err):
		default:
			return fmt.Errorf("read %s: %w", path, err)
		}

		existing, _ := settings["trustedWorkspaces"].([]any)
		for _, w := range existing {
			if s, ok := w.(string); ok && s == workDir {
				return nil // already trusted; nothing to write
			}
		}
		settings["trustedWorkspaces"] = append(existing, workDir)

		out, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", path, err)
		}
		return writeFileAtomic(path, out, 0o600)
	})
}

func agySettingsPath() (string, error) {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".gemini", agySettingsRelPath), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gemini", agySettingsRelPath), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".gemini", agySettingsRelPath), nil
}

const codexConfigTrustedTableFmt = "[projects.%s]\ntrust_level = \"trusted\"\n"

// trustCodexWorkspace records workDir in codex's trusted-projects table.
func trustCodexWorkspace(workDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	path := filepath.Join(home, ".codex", "config.toml")
	quotedWorkDir := fmt.Sprintf("%q", workDir)
	header := fmt.Sprintf("[projects.%s]", quotedWorkDir)

	return withFileLock(path, func() error {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == header {
				return nil // already trusted; nothing to write
			}
		}

		block := fmt.Sprintf(codexConfigTrustedTableFmt, quotedWorkDir)
		updated := string(data)
		if updated != "" && !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		updated += block

		return writeFileAtomic(path, []byte(updated), 0o600)
	})
}

// withFileLock serializes bramble's trust-store updates to one path.
func withFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create dir for %s: %w", path, err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock for %s: %w", path, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn()
}
