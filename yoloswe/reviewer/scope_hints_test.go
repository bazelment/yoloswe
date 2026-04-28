package reviewer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHintsBytes(t *testing.T, contents []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scope-hints.json")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("write hints file: %v", err)
	}
	return path
}

func writeHintsFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scope-hints.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write hints file: %v", err)
	}
	return path
}

func TestLoadScopeHints_Valid(t *testing.T) {
	path := writeHintsFile(t, `{
		"schema_version": 1,
		"test_paths": ["a/test_x.py", "b/test_y.go"],
		"cross_service_packages": ["svc/a/", "svc/b/"]
	}`)
	h, err := LoadScopeHints(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", h.SchemaVersion)
	}
	if len(h.TestPaths) != 2 || h.TestPaths[0] != "a/test_x.py" {
		t.Errorf("TestPaths = %v, want [a/test_x.py b/test_y.go]", h.TestPaths)
	}
	if len(h.CrossServicePackages) != 2 || h.CrossServicePackages[1] != "svc/b/" {
		t.Errorf("CrossServicePackages = %v", h.CrossServicePackages)
	}
}

func TestLoadScopeHints_EmptyArraysOK(t *testing.T) {
	// Empty arrays are a valid "no clause" signal. Reviewer must accept
	// them so callers can produce a hints file unconditionally without
	// having to skip writing it when there's nothing to add.
	path := writeHintsFile(t, `{
		"schema_version": 1,
		"test_paths": [],
		"cross_service_packages": []
	}`)
	h, err := LoadScopeHints(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(h.TestPaths) != 0 || len(h.CrossServicePackages) != 0 {
		t.Errorf("expected empty arrays, got %+v", h)
	}
}

func TestLoadScopeHints_MissingFile(t *testing.T) {
	_, err := LoadScopeHints(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "open scope-hints file") {
		t.Errorf("error should describe the open failure: %v", err)
	}
}

func TestLoadScopeHints_MalformedJSON(t *testing.T) {
	path := writeHintsFile(t, `{not valid json`)
	_, err := LoadScopeHints(path)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parse scope-hints file") {
		t.Errorf("error should describe the parse failure: %v", err)
	}
}

func TestLoadScopeHints_SchemaVersionMismatch(t *testing.T) {
	// A future caller writing schema_version=2 must fail loudly here, not
	// silently succeed with a degenerate prompt. The CLI layer treats this
	// as a "log warning, fall back to narrow review" — the reviewer never
	// reads a future-shape file.
	path := writeHintsFile(t, `{"schema_version": 2, "test_paths": [], "cross_service_packages": []}`)
	_, err := LoadScopeHints(path)
	if err == nil {
		t.Fatal("expected error for schema_version=2")
	}
	if !strings.Contains(err.Error(), "schema_version=2") {
		t.Errorf("error should include observed version: %v", err)
	}
	if !strings.Contains(err.Error(), "want 1") {
		t.Errorf("error should include expected version: %v", err)
	}
}

func TestLoadScopeHints_MissingSchemaVersionTreatedAsZero(t *testing.T) {
	// JSON unmarshal leaves missing fields at the zero value. A hints file
	// without schema_version is most likely a hand-edited or pre-versioning
	// payload; reject it the same way as an explicit mismatch.
	path := writeHintsFile(t, `{"test_paths": [], "cross_service_packages": []}`)
	_, err := LoadScopeHints(path)
	if err == nil {
		t.Fatal("expected error for missing schema_version")
	}
}

func TestScopeHints_ToPromptOptions(t *testing.T) {
	h := &ScopeHints{
		SchemaVersion:        1,
		TestPaths:            []string{"a/test_x.py"},
		CrossServicePackages: []string{"svc/a/", "svc/b/"},
	}
	opts := h.ToPromptOptions(true)
	if !opts.SkipTestExecution {
		t.Error("SkipTestExecution should be passed through")
	}
	if len(opts.TestScopeHints) != 1 || opts.TestScopeHints[0] != "a/test_x.py" {
		t.Errorf("TestScopeHints = %v", opts.TestScopeHints)
	}
	if len(opts.CrossServicePackages) != 2 {
		t.Errorf("CrossServicePackages len = %d, want 2", len(opts.CrossServicePackages))
	}
}

func TestLoadScopeHints_RejectsOversizeFile(t *testing.T) {
	// A 1 MiB+ file shouldn't be parsed; the size cap defends against
	// hostile or accidental inputs (a /dev/zero symlink, a runaway
	// producer) without changing behavior for the realistic small files
	// scope_gate.py emits.
	big := make([]byte, scopeHintsMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	path := writeHintsBytes(t, big)
	_, err := LoadScopeHints(path)
	if err == nil {
		t.Fatal("expected error for oversized hints file")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size cap: %v", err)
	}
}

func TestLoadScopeHints_AtCapSucceeds(t *testing.T) {
	// Exactly at the cap is valid input — JSON shouldn't round up to
	// "deny." The file content here is structurally valid JSON padded
	// with whitespace (which json.Unmarshal happily ignores) up to the
	// cap, so we exercise both the read and the parse path at the limit.
	pad := make([]byte, scopeHintsMaxBytes)
	for i := range pad {
		pad[i] = ' '
	}
	body := []byte(`{"schema_version":1,"test_paths":[],"cross_service_packages":[]}`)
	if len(body) > len(pad) {
		t.Fatalf("test setup: body %d bytes exceeds pad %d", len(body), len(pad))
	}
	copy(pad, body)
	// Length is now exactly scopeHintsMaxBytes.
	path := writeHintsBytes(t, pad)
	if _, err := LoadScopeHints(path); err != nil {
		t.Errorf("at-cap file should parse, got: %v", err)
	}
}

func TestLoadScopeHints_ErrorsUseBasenameNotFullPath(t *testing.T) {
	// LoadScopeHints embeds the file name in its error text. The CLI
	// surfaces that text in slog warnings on the fallback path, and run
	// logs are routinely shared across machines/PRs, so the embedded name
	// must be just the basename — never the developer's worktree path.
	dir := t.TempDir()
	full := filepath.Join(dir, "scope-hints.json")
	if err := os.WriteFile(full, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write hints file: %v", err)
	}
	_, err := LoadScopeHints(full)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("error leaks tmpdir path %q: %v", dir, err)
	}
	if !strings.Contains(err.Error(), "scope-hints.json") {
		t.Errorf("error should still cite basename for debuggability: %v", err)
	}
}

func TestScopeHints_ToPromptOptions_NilSafe(t *testing.T) {
	// ToPromptOptions must be safe to call on a nil receiver so callers
	// don't have to nil-check first. The codereview CLI doesn't actually
	// pass nil today (it short-circuits to PromptOptions{} on error), but
	// nil-safety is a property of the method that other future callers
	// might rely on, and panicking here would be a footgun.
	var h *ScopeHints
	opts := h.ToPromptOptions(false)
	if opts.SkipTestExecution {
		t.Error("SkipTestExecution should reflect arg, not panic")
	}
	if opts.TestScopeHints != nil || opts.CrossServicePackages != nil {
		t.Errorf("nil receiver should produce zero PromptOptions, got %+v", opts)
	}
}
