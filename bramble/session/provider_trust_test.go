package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withTestHome points $HOME (and clears $XDG_CONFIG_HOME) at a fresh temp
// directory for the duration of the test, so trustAgyWorkspace's home
// resolution is exercised the same way it runs in production.
func withTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func TestTrustAgyWorkspace_CreatesSettingsWhenAbsent(t *testing.T) {
	home := withTestHome(t)
	workDir := "/work/repo"

	if err := trustAgyWorkspace(workDir); err != nil {
		t.Fatalf("trustAgyWorkspace() error = %v", err)
	}

	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	workspaces, _ := settings["trustedWorkspaces"].([]any)
	if len(workspaces) != 1 || workspaces[0] != workDir {
		t.Fatalf("trustedWorkspaces = %v, want [%q]", workspaces, workDir)
	}
}

func TestTrustAgyWorkspace_PreservesExistingFields(t *testing.T) {
	home := withTestHome(t)
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	seed := `{"enableTelemetry": false, "trustedWorkspaces": ["/already/trusted"]}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := trustAgyWorkspace("/work/repo"); err != nil {
		t.Fatalf("trustAgyWorkspace() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if settings["enableTelemetry"] != false {
		t.Fatalf("enableTelemetry = %v, want false (must survive the merge)", settings["enableTelemetry"])
	}
	workspaces, _ := settings["trustedWorkspaces"].([]any)
	want := map[string]bool{"/already/trusted": true, "/work/repo": true}
	if len(workspaces) != len(want) {
		t.Fatalf("trustedWorkspaces = %v, want two entries: %v", workspaces, want)
	}
	for _, w := range workspaces {
		if !want[w.(string)] {
			t.Fatalf("unexpected workspace %v in %v", w, workspaces)
		}
	}
}

func TestTrustAgyWorkspace_IdempotentNoDuplicate(t *testing.T) {
	withTestHome(t)
	workDir := "/work/repo"

	for i := 0; i < 3; i++ {
		if err := trustAgyWorkspace(workDir); err != nil {
			t.Fatalf("trustAgyWorkspace() call %d error = %v", i, err)
		}
	}

	path, err := agySettingsPath()
	if err != nil {
		t.Fatalf("agySettingsPath() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	workspaces, _ := settings["trustedWorkspaces"].([]any)
	if len(workspaces) != 1 {
		t.Fatalf("trustedWorkspaces = %v, want exactly one entry (idempotent)", workspaces)
	}
}

func TestTrustAgyWorkspace_ConcurrentWritersBothPersist(t *testing.T) {
	withTestHome(t)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = trustAgyWorkspace(filepath.Join("/work", "repo", string(rune('a'+i))))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("trustAgyWorkspace() goroutine %d error = %v", i, err)
		}
	}

	path, err := agySettingsPath()
	if err != nil {
		t.Fatalf("agySettingsPath() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	workspaces, _ := settings["trustedWorkspaces"].([]any)
	if len(workspaces) != n {
		t.Fatalf("trustedWorkspaces has %d entries, want %d — a concurrent writer's addition was lost: %v",
			len(workspaces), n, workspaces)
	}
}

func TestTrustCodexWorkspace_AppendsTrustedTable(t *testing.T) {
	home := withTestHome(t)
	workDir := "/work/repo"

	if err := trustCodexWorkspace(workDir); err != nil {
		t.Fatalf("trustCodexWorkspace() error = %v", err)
	}

	path := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	content := string(data)
	if !strings.Contains(content, `[projects."/work/repo"]`) {
		t.Fatalf("config.toml missing trusted-project header, got:\n%s", content)
	}
	if !strings.Contains(content, `trust_level = "trusted"`) {
		t.Fatalf("config.toml missing trust_level, got:\n%s", content)
	}
}

func TestTrustCodexWorkspace_PreservesExistingConfig(t *testing.T) {
	home := withTestHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	seed := "model = \"gpt-5.6-sol\"\nmodel_reasoning_effort = \"xhigh\"\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := trustCodexWorkspace("/work/repo"); err != nil {
		t.Fatalf("trustCodexWorkspace() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)
	if !strings.HasPrefix(content, seed) {
		t.Fatalf("existing config not preserved verbatim, got:\n%s", content)
	}
	if !strings.Contains(content, `[projects."/work/repo"]`) {
		t.Fatalf("config.toml missing trusted-project header, got:\n%s", content)
	}
}

func TestTrustCodexWorkspace_IdempotentNoDuplicateSection(t *testing.T) {
	home := withTestHome(t)
	workDir := "/work/repo"

	for i := 0; i < 3; i++ {
		if err := trustCodexWorkspace(workDir); err != nil {
			t.Fatalf("trustCodexWorkspace() call %d error = %v", i, err)
		}
	}

	path := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	count := strings.Count(string(data), `[projects."/work/repo"]`)
	if count != 1 {
		t.Fatalf("trusted-project header appears %d times, want 1:\n%s", count, string(data))
	}
}

func TestTrustCodexWorkspace_QuotesSpecialCharacters(t *testing.T) {
	home := withTestHome(t)
	// The path that reproduced a real failure of codex's own `-c` dotted-path
	// override (a literal "." inside a path segment): rejected as the
	// CLI-override direction specifically because of this case. The file-write
	// path must handle it correctly since it is what bramble now relies on.
	workDir := "/work/swarm/agy-A3-trust.branch"

	if err := trustCodexWorkspace(workDir); err != nil {
		t.Fatalf("trustCodexWorkspace() error = %v", err)
	}

	path := filepath.Join(home, ".codex", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	want := `[projects."/work/swarm/agy-A3-trust.branch"]`
	if !strings.Contains(string(data), want) {
		t.Fatalf("config.toml missing exact quoted header %q, got:\n%s", want, string(data))
	}
}

func TestPreTrustWorkspace_NoopForClaudeAndCursor(t *testing.T) {
	home := withTestHome(t)

	preTrustWorkspace(ProviderClaude, "/work/repo")
	preTrustWorkspace(ProviderCursor, "/work/repo")

	// Neither provider owns a trust store this function knows about; nothing
	// should have been created under home for either.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("home directory not empty after no-op providers: %v", entries)
	}
}

func TestPreTrustWorkspace_EmptyWorkDirIsNoop(t *testing.T) {
	home := withTestHome(t)

	preTrustWorkspace(ProviderAgy, "")
	preTrustWorkspace(ProviderCodex, "")

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("home directory not empty after empty workDir: %v", entries)
	}
}
