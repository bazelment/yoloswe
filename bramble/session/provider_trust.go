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

// preTrustWorkspace marks workDir as trusted in the launched CLI's own
// on-disk config, before the tmux window is created, so the interactive
// session never renders a first-run "do you trust this directory?" modal.
//
// That modal is otherwise indistinguishable from a healthy session through
// bramble's status API: the pane shows a prompt, the process is alive, and
// nothing reports it as blocked. A human answers it by hand; an unattended
// orchestrator polling list-sessions cannot, and the session simply never
// proceeds. See issue #346 for the same class of problem with claude's
// composer, and its DRIVE-FINDINGS G-C for this one reproduced against a
// real agy CLI.
//
// Only agy and codex are handled:
//   - claude has no such dialog.
//   - cursor already refuses to run in an untrusted directory unless started
//     with --trust, which tmuxRunner.buildCommand always passes (see the
//     ProviderCursor case) — a launch-flag fix already landed, so there is no
//     on-disk state to seed here.
//
// Failure is deliberately non-fatal: this is a best-effort optimization over
// the CLI's own dialog, not a new invariant. A workspace that could not be
// pre-trusted still gets the CLI's dialog exactly as it does today — no
// worse than before this existed — so Start() proceeds either way and only
// logs the miss.
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

// agySettingsRelPath is settings.json's location under the user's home
// directory, per agy's own resolution (confirmed live: it honors $HOME, and
// $XDG_CONFIG_HOME when $HOME is unset).
const agySettingsRelPath = "antigravity-cli/settings.json"

// trustAgyWorkspace adds workDir to agy's trustedWorkspaces list in
// ~/.gemini/antigravity-cli/settings.json, the exact field agy itself writes
// after a human accepts its trust dialog (confirmed by driving the dialog and
// diffing the file). agy has no CLI flag or config-override equivalent to
// cursor's --trust, so the only pre-trust path is seeding this file directly.
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
			// No settings.json yet; agy creates one lazily. Seed the field we
			// need and leave everything else to agy's own defaults.
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
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create dir for %s: %w", path, err)
		}
		return writeFileAtomic(path, out, 0o600)
	})
}

// agySettingsPath resolves ~/.gemini/antigravity-cli/settings.json, matching
// agy's own home resolution (confirmed live): $HOME first, then
// $XDG_CONFIG_HOME as agy itself falls back to it.
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

// codexConfigTrustedTableFmt is the exact table codex itself appends to
// config.toml after a human accepts its trust dialog (confirmed by driving
// the dialog and diffing the file): a `[projects."<path>"]` header followed
// by `trust_level = "trusted"`.
const codexConfigTrustedTableFmt = "[projects.%s]\ntrust_level = \"trusted\"\n"

// trustCodexWorkspace adds workDir to codex's trusted-projects table in
// ~/.codex/config.toml.
//
// codex also accepts an in-process `-c projects."<path>".trust_level=trusted`
// override, which would need no file write at all — but it does not reliably
// suppress the dialog: driven live, a path containing a literal "." (e.g.
// ".../swarm/foo.bar") still rendered the dialog with that override present,
// while a dash/underscore-only path did not. Since bramble worktree paths are
// not guaranteed dot-free, this file write is the one path confirmed
// reliable, matching exactly what accepting the dialog itself persists.
func trustCodexWorkspace(workDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	path := filepath.Join(home, ".codex", "config.toml")
	header := fmt.Sprintf("[projects.%s]", tomlQuoteKey(workDir))

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

		block := fmt.Sprintf(codexConfigTrustedTableFmt, tomlQuoteKey(workDir))
		updated := string(data)
		if updated != "" && !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		updated += block

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create dir for %s: %w", path, err)
		}
		return writeFileAtomic(path, []byte(updated), 0o600)
	})
}

// tomlQuoteKey renders s as a TOML basic string suitable for use as a quoted
// dotted-key segment (codex's `projects."<path>"` table name). TOML basic
// strings escape backslash and double-quote; a worktree path controlled by
// bramble is not expected to carry either, but escaping is cheap and this is
// the one place a raw path is spliced into a config file bramble did not
// create.
func tomlQuoteKey(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// withFileLock serializes concurrent writers to path across processes, using
// a sibling ".lock" file the way sockguard does for tmux sockets: two bramble
// instances (or bramble racing a human's own agy/codex session) can otherwise
// both read the file's old contents, both add their own entry, and one
// writer's addition is silently lost when the other's write lands second.
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
