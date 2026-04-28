package reviewer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if !strings.Contains(err.Error(), "read scope-hints file") {
		t.Errorf("error should describe the read failure: %v", err)
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

func TestScopeHints_ToPromptOptions_NilSafe(t *testing.T) {
	// LoadScopeHints returns a nil *ScopeHints on error; the CLI layer
	// passes that nil through ToPromptOptions to get a clean fallback. The
	// method must not panic on nil receiver.
	var h *ScopeHints
	opts := h.ToPromptOptions(false)
	if opts.SkipTestExecution {
		t.Error("SkipTestExecution should reflect arg, not panic")
	}
	if opts.TestScopeHints != nil || opts.CrossServicePackages != nil {
		t.Errorf("nil receiver should produce zero PromptOptions, got %+v", opts)
	}
}
