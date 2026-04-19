package reviewer

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRunLog_CreatesFileAndLogs(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv(RunLogEnvTag, "test-tag")

	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	logPath, cleanup, err := SetupRunLog()
	if err != nil {
		t.Fatalf("SetupRunLog: %v", err)
	}
	defer cleanup()

	if logPath == "" {
		t.Fatal("expected non-empty log path")
	}
	expectedDir := filepath.Join(tempHome, ".bramble", "logs", "code-review")
	if !strings.HasPrefix(logPath, expectedDir) {
		t.Errorf("log path %q not under %q", logPath, expectedDir)
	}
	if !strings.HasSuffix(logPath, ".log") {
		t.Errorf("log path %q should end in .log", logPath)
	}

	slog.Info("test event", "key", "value")
	// Close the file via cleanup so data is flushed.
	cleanup()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "test event") {
		t.Errorf("log missing message: %q", content)
	}
	if !strings.Contains(content, "run_tag=test-tag") {
		t.Errorf("log missing run_tag: %q", content)
	}
	if !strings.Contains(content, "key=value") {
		t.Errorf("log missing key=value: %q", content)
	}
}

func TestSetupRunLog_CleanupRestoresDefault(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv(RunLogEnvTag, "")

	sentinel := slog.Default()
	t.Cleanup(func() { slog.SetDefault(sentinel) })

	_, cleanup, err := SetupRunLog()
	if err != nil {
		t.Fatalf("SetupRunLog: %v", err)
	}
	if slog.Default() == sentinel {
		t.Fatal("SetupRunLog did not replace slog default")
	}

	cleanup()

	if slog.Default() != sentinel {
		t.Error("cleanup did not restore previous slog default")
	}
}

func TestSetupRunLog_NoEnvTag(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv(RunLogEnvTag, "")

	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	logPath, cleanup, err := SetupRunLog()
	if err != nil {
		t.Fatalf("SetupRunLog: %v", err)
	}
	defer cleanup()

	slog.Info("hello")
	cleanup()

	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "run_tag=") {
		t.Errorf("log should not contain run_tag when env var is empty: %q", string(data))
	}
}
