package workspace

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
)

func newTestConfig(t *testing.T) *config.ServiceConfig {
	t.Helper()
	root := t.TempDir()
	return &config.ServiceConfig{
		WorkspaceRoot: root,
		HookTimeoutMs: 5000,
	}
}

func TestCreateForIssue_DeterministicPath(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)

	ws1, err := CreateForIssue(context.Background(), cfg, "ABC-123")
	if err != nil {
		t.Fatalf("CreateForIssue: %v", err)
	}
	ws2, err := CreateForIssue(context.Background(), cfg, "ABC-123")
	if err != nil {
		t.Fatalf("CreateForIssue (second): %v", err)
	}

	if ws1.Path != ws2.Path {
		t.Errorf("paths differ: %q vs %q", ws1.Path, ws2.Path)
	}
	if ws1.WorkspaceKey != ws2.WorkspaceKey {
		t.Errorf("keys differ: %q vs %q", ws1.WorkspaceKey, ws2.WorkspaceKey)
	}
}

func TestCreateForIssue_CreateVsReuse(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)

	ws1, err := CreateForIssue(context.Background(), cfg, "NEW-1")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !ws1.CreatedNow {
		t.Error("first call should have CreatedNow=true")
	}

	ws2, err := CreateForIssue(context.Background(), cfg, "NEW-1")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if ws2.CreatedNow {
		t.Error("second call should have CreatedNow=false")
	}
}

func TestCreateForIssue_SanitizesIdentifier(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)

	ws, err := CreateForIssue(context.Background(), cfg, "feature/special chars@here!")
	if err != nil {
		t.Fatalf("CreateForIssue: %v", err)
	}

	expected := model.SanitizeIdentifier("feature/special chars@here!")
	if ws.WorkspaceKey != expected {
		t.Errorf("workspace key = %q, want %q", ws.WorkspaceKey, expected)
	}

	// Directory should exist.
	info, err := os.Stat(ws.Path)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	if !info.IsDir() {
		t.Error("workspace path is not a directory")
	}
}

func TestCreateForIssue_PathTraversalRejected(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)

	// After sanitization, slashes become underscores so direct traversal via
	// identifier is blocked by sanitization. But let's verify the containment
	// check also works by testing with an identifier that sanitizes cleanly
	// but where root is manipulated.
	_, err := CreateForIssue(context.Background(), cfg, "normal-issue")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Also verify that an all-dots identifier (which passes sanitization) cannot
	// escape. ".." sanitizes to ".." which is valid key chars, but path
	// containment should catch /root/.. == /root's parent.
	// Actually ".." contains only dots and that IS valid chars. Let's test it.
	// filepath.Join(root, "..") resolves to root's parent, which should fail containment.
	_, err = CreateForIssue(context.Background(), cfg, "..")
	if err == nil {
		t.Error("expected error for '..' identifier (path traversal), got nil")
	}
}

func TestCreateForIssue_AfterCreateHookSuccess(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	markerFile := filepath.Join(cfg.WorkspaceRoot, "hook-ran")
	cfg.HookAfterCreate = "touch " + markerFile

	ws, err := CreateForIssue(context.Background(), cfg, "HOOK-1")
	if err != nil {
		t.Fatalf("CreateForIssue: %v", err)
	}
	if !ws.CreatedNow {
		t.Error("expected CreatedNow=true")
	}

	// Hook should have run.
	if _, err := os.Stat(markerFile); err != nil {
		t.Errorf("hook marker file not found: %v", err)
	}
}

func TestCreateForIssue_AfterCreateHookFailure(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	cfg.HookAfterCreate = "exit 1"

	_, err := CreateForIssue(context.Background(), cfg, "HOOKFAIL-1")
	if err == nil {
		t.Fatal("expected error when after_create hook fails")
	}

	// Directory should have been cleaned up.
	wsPath := filepath.Join(cfg.WorkspaceRoot, model.SanitizeIdentifier("HOOKFAIL-1"))
	if _, statErr := os.Stat(wsPath); !os.IsNotExist(statErr) {
		t.Errorf("workspace directory should be removed after hook failure, stat err: %v", statErr)
	}
}

func TestCreateForIssue_AfterCreateHookSkippedOnReuse(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)

	// Create the workspace first without a hook.
	_, err := CreateForIssue(context.Background(), cfg, "REUSE-1")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Now set a hook that would fail. On reuse it should not run.
	cfg.HookAfterCreate = "exit 1"
	ws, err := CreateForIssue(context.Background(), cfg, "REUSE-1")
	if err != nil {
		t.Fatalf("reuse should not run after_create hook: %v", err)
	}
	if ws.CreatedNow {
		t.Error("reuse should have CreatedNow=false")
	}
}

func TestCleanupWorkspace(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create a workspace.
	ws, err := CreateForIssue(context.Background(), cfg, "CLEAN-1")
	if err != nil {
		t.Fatalf("CreateForIssue: %v", err)
	}

	// Write a file inside to verify removal.
	testFile := filepath.Join(ws.Path, "test.txt")
	if err := os.WriteFile(testFile, []byte("data"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	// Cleanup.
	if err := CleanupWorkspace(cfg, "CLEAN-1", logger); err != nil {
		t.Fatalf("CleanupWorkspace: %v", err)
	}

	// Directory should be gone.
	if _, statErr := os.Stat(ws.Path); !os.IsNotExist(statErr) {
		t.Errorf("workspace should be removed, stat err: %v", statErr)
	}
}

func TestCleanupWorkspace_NonExistent(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Cleaning up a non-existent workspace should succeed silently.
	if err := CleanupWorkspace(cfg, "NOEXIST-1", logger); err != nil {
		t.Fatalf("CleanupWorkspace for non-existent dir: %v", err)
	}
}

func TestCleanupWorkspace_BeforeRemoveHookRuns(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	markerFile := filepath.Join(cfg.WorkspaceRoot, "before-remove-ran")
	cfg.HookBeforeRemove = "touch " + markerFile

	// Create workspace.
	_, err := CreateForIssue(context.Background(), cfg, "RMHOOK-1")
	if err != nil {
		t.Fatalf("CreateForIssue: %v", err)
	}

	// Cleanup should run the hook.
	if err := CleanupWorkspace(cfg, "RMHOOK-1", logger); err != nil {
		t.Fatalf("CleanupWorkspace: %v", err)
	}

	if _, statErr := os.Stat(markerFile); statErr != nil {
		t.Errorf("before_remove hook marker not found: %v", statErr)
	}
}

func TestCleanupWorkspace_BeforeRemoveHookFailureIgnored(t *testing.T) {
	t.Parallel()
	cfg := newTestConfig(t)
	cfg.HookBeforeRemove = "exit 1"
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create workspace.
	ws, err := CreateForIssue(context.Background(), cfg, "RMFAIL-1")
	if err != nil {
		t.Fatalf("CreateForIssue: %v", err)
	}

	// Cleanup should succeed even when hook fails.
	if err := CleanupWorkspace(cfg, "RMFAIL-1", logger); err != nil {
		t.Fatalf("CleanupWorkspace should succeed even with hook failure: %v", err)
	}

	// Directory should be gone.
	if _, statErr := os.Stat(ws.Path); !os.IsNotExist(statErr) {
		t.Errorf("workspace should be removed despite hook failure, stat err: %v", statErr)
	}
}
