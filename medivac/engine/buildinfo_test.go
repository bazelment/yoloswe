package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectBuildInfo_Bazel(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "WORKSPACE"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemBazel {
		t.Errorf("expected bazel, got %s", info.System)
	}
	if info.BuildCmd == "" {
		t.Error("expected non-empty build command")
	}
}

func TestDetectBuildInfo_Nx(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "nx.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNx {
		t.Errorf("expected nx, got %s", info.System)
	}
}

func TestDetectBuildInfo_NxWithPython(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "nx.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNx {
		t.Errorf("expected nx, got %s", info.System)
	}
	// Should have Python rule
	found := false
	for _, rule := range info.ExtraRules {
		if rule == "For Python code, use `uv run ruff check .` for linting and `uv run pytest` for tests" {
			found = true
		}
	}
	if !found {
		t.Error("expected Python-specific rule in ExtraRules")
	}
}

func TestDetectBuildInfo_Make(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemMake {
		t.Errorf("expected make, got %s", info.System)
	}
}

func TestDetectBuildInfo_NPM(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNPM {
		t.Errorf("expected npm, got %s", info.System)
	}
}

func TestDetectBuildInfo_Pnpm(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte(""), 0644)

	info := DetectBuildInfo(dir)
	if info.System != BuildSystemNPM {
		t.Errorf("expected npm (pnpm variant), got %s", info.System)
	}
	if info.LintCmd != "pnpm run lint" {
		t.Errorf("expected pnpm lint cmd, got %s", info.LintCmd)
	}
}

func TestDetectBuildInfo_Unknown(t *testing.T) {
	dir := t.TempDir()
	info := DetectBuildInfo(dir)
	if info.System != BuildSystemUnknown {
		t.Errorf("expected unknown, got %s", info.System)
	}
}

func TestDetectBuildInfo_ClaudeMD(t *testing.T) {
	dir := t.TempDir()
	content := "# Build Instructions\n\nRun `make all`\n"
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(content), 0644)

	info := DetectBuildInfo(dir)
	if info.ClaudeMD != content {
		t.Errorf("CLAUDE.md content not loaded")
	}
}

func TestTruncateClaudeMD(t *testing.T) {
	short := "short content"
	if TruncateClaudeMD(short, 100) != short {
		t.Error("short content should not be truncated")
	}

	long := "a very long string that should be truncated"
	result := TruncateClaudeMD(long, 10)
	if len(result) <= 10 {
		// Result should be 10 chars + the truncation message
		t.Error("truncation did not work")
	}
	if result[:10] != long[:10] {
		t.Errorf("truncated prefix mismatch")
	}
}
