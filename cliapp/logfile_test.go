package cliapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLogPath_Override(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logDir, logPath, err := resolveLogPath("mytool", dir)
	if err != nil {
		t.Fatalf("resolveLogPath: %v", err)
	}
	if logDir != dir {
		t.Errorf("logDir = %q, want override %q", logDir, dir)
	}
	if filepath.Dir(logPath) != dir {
		t.Errorf("logPath %q is not inside override dir %q", logPath, dir)
	}
	base := filepath.Base(logPath)
	if !strings.HasPrefix(base, "mytool-") || !strings.HasSuffix(base, ".log") {
		t.Errorf("logPath base = %q, want mytool-<ts>-<pid>.log", base)
	}
}

func TestResolveLogPath_DefaultsToHome(t *testing.T) {
	// Cannot t.Parallel — we set HOME, which is process-global.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	logDir, logPath, err := resolveLogPath("mytool", "")
	if err != nil {
		t.Fatalf("resolveLogPath: %v", err)
	}
	wantDir := filepath.Join(fakeHome, ".mytool", "logs")
	if logDir != wantDir {
		t.Errorf("logDir = %q, want %q", logDir, wantDir)
	}
	if !strings.HasPrefix(logPath, wantDir) {
		t.Errorf("logPath %q not under %q", logPath, wantDir)
	}
}

func TestResolveLogPath_HomeUnset(t *testing.T) {
	// Cannot t.Parallel — we unset HOME, which is process-global.
	t.Setenv("HOME", "")
	if _, _, err := resolveLogPath("mytool", ""); err == nil {
		t.Errorf("resolveLogPath with HOME unset should error")
	}
}

func TestOpenLogFile_CreatesDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "subdir")
	logPath := filepath.Join(dir, "test.log")

	f, err := openLogFile(dir, logPath)
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	if _, err := f.WriteString("hi\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "hi\n" {
		t.Errorf("file content = %q, want %q", string(data), "hi\n")
	}
}

func TestOpenLogFile_FailsOnUnwritablePath(t *testing.T) {
	t.Parallel()
	// Use a path where the parent already exists as a regular file —
	// MkdirAll cannot create a directory under it.
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	dir := filepath.Join(blocker, "subdir")
	logPath := filepath.Join(dir, "test.log")

	if _, err := openLogFile(dir, logPath); err == nil {
		t.Errorf("openLogFile should have failed for path under regular file")
	}
}
