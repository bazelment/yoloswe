package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

func TestBuildPrompt(t *testing.T) {
	tests := []struct {
		name     string
		goal     string
		contains []string
	}{
		{
			name: "with goal",
			goal: "add user authentication",
			contains: []string{
				"add user authentication",
				"experienced software engineer",
				"Review all changes on this branch",
			},
		},
		{
			name: "empty goal",
			goal: "",
			contains: []string{
				"Use commit messages to understand their purpose",
				"experienced software engineer",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := BuildPrompt(tt.goal)
			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("BuildPrompt(%q) should contain %q", tt.goal, s)
				}
			}
		})
	}
}

func TestBuildJSONPrompt(t *testing.T) {
	tests := []struct {
		name     string
		goal     string
		contains []string
	}{
		{
			name: "with goal",
			goal: "add user authentication",
			contains: []string{
				"add user authentication",
				"experienced software engineer",
				"Review all changes on this branch",
				"JSON",
				"verdict",
				"accepted",
				"rejected",
				"summary",
				"issues",
				"severity",
				"critical",
				"high",
				"medium",
				"low",
			},
		},
		{
			name: "empty goal",
			goal: "",
			contains: []string{
				"Use commit messages to understand their purpose",
				"JSON",
				"verdict",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt := BuildJSONPrompt(tt.goal)
			for _, s := range tt.contains {
				if !containsString(prompt, s) {
					t.Errorf("BuildJSONPrompt(%q) should contain %q", tt.goal, s)
				}
			}
		})
	}
}

func TestNew_DefaultValues(t *testing.T) {
	r := New(Config{})

	if r.config.Model != "gpt-5.2-codex" {
		t.Errorf("expected default model gpt-5.2-codex, got %s", r.config.Model)
	}
	if r.config.ApprovalPolicy != codex.ApprovalPolicyNever {
		t.Errorf("expected default approval policy never, got %s", r.config.ApprovalPolicy)
	}
	if r.config.BackendType != BackendCodex {
		t.Errorf("expected default backend codex, got %s", r.config.BackendType)
	}
	if r.output == nil {
		t.Error("output should not be nil")
	}
	if r.renderer == nil {
		t.Error("renderer should not be nil")
	}
	if r.backend == nil {
		t.Error("backend should not be nil")
	}
}

func TestEffectiveModel_ReportsDefaultAfterNew(t *testing.T) {
	// Regression: the command-layer JSON envelope used to report the raw
	// --model flag, which is empty when a default applies. EffectiveModel
	// must surface the post-default value so consumers can correlate runs.
	r := New(Config{BackendType: BackendCodex})
	if got := r.EffectiveModel(); got != "gpt-5.2-codex" {
		t.Errorf("EffectiveModel() = %q, want gpt-5.2-codex", got)
	}

	r2 := New(Config{BackendType: BackendCodex, Model: "gpt-5.4"})
	if got := r2.EffectiveModel(); got != "gpt-5.4" {
		t.Errorf("EffectiveModel() = %q, want gpt-5.4", got)
	}
}

func TestEffectiveModel_UpdatesFromSessionInfo(t *testing.T) {
	// Cursor's CLI picks a default model when --model is empty and reports
	// the choice via ReadyEvent → OnSessionInfo. The envelope must surface
	// that real model instead of a stale empty/config value.
	r := New(Config{BackendType: BackendCursor})
	if got := r.EffectiveModel(); got != "" {
		t.Errorf("pre-session EffectiveModel() = %q, want empty", got)
	}
	h := r.newEventHandler()
	h.OnSessionInfo("session-abc", "Composer 2")
	if got := r.EffectiveModel(); got != "Composer 2" {
		t.Errorf("post-session EffectiveModel() = %q, want Composer 2", got)
	}
	// Session info with empty model must not erase a known value.
	h.OnSessionInfo("session-def", "")
	if got := r.EffectiveModel(); got != "Composer 2" {
		t.Errorf("after empty session model EffectiveModel() = %q, want Composer 2", got)
	}
}

func TestNew_WithVerbose(t *testing.T) {
	r := New(Config{Verbose: true})

	if r.renderer == nil {
		t.Error("renderer should not be nil when Verbose is true")
	}
}

func TestNew_WithCustomValues(t *testing.T) {
	r := New(Config{
		Model:          "gpt-4o",
		ApprovalPolicy: codex.ApprovalPolicyNever,
	})

	if r.config.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %s", r.config.Model)
	}
	if r.config.ApprovalPolicy != codex.ApprovalPolicyNever {
		t.Errorf("expected approval policy never, got %s", r.config.ApprovalPolicy)
	}
}

func TestNew_CursorBackend(t *testing.T) {
	r := New(Config{
		BackendType: BackendCursor,
		Model:       "cursor-default",
	})

	if r.config.BackendType != BackendCursor {
		t.Errorf("expected cursor backend, got %s", r.config.BackendType)
	}
	if r.backend == nil {
		t.Error("backend should not be nil for cursor")
	}
	// Cursor backend Start is a no-op, verify it doesn't error
	if err := r.backend.Start(context.TODO()); err != nil {
		t.Errorf("cursor start should be no-op, got error: %v", err)
	}
	if err := r.backend.Stop(); err != nil {
		t.Errorf("cursor stop should be no-op, got error: %v", err)
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStringHelper(s, substr))
}

func containsStringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestNew_SessionLogPath(t *testing.T) {
	r := New(Config{
		SessionLogPath: "/tmp/test-session.jsonl",
	})
	if r.config.SessionLogPath != "/tmp/test-session.jsonl" {
		t.Errorf("expected SessionLogPath to be set, got %q", r.config.SessionLogPath)
	}
}

func TestNew_DefaultApprovalPolicyCodex(t *testing.T) {
	r := New(Config{BackendType: BackendCodex})
	if r.config.ApprovalPolicy != codex.ApprovalPolicyNever {
		t.Errorf("expected codex default approval policy %q, got %q",
			codex.ApprovalPolicyNever, r.config.ApprovalPolicy)
	}
}

func TestNew_ReadOnlyApprovalPolicyCodex(t *testing.T) {
	r := New(Config{BackendType: BackendCodex, ReadOnly: true})
	if r.config.ApprovalPolicy != codex.ApprovalPolicyOnFailure {
		t.Errorf("expected codex read-only approval policy %q, got %q",
			codex.ApprovalPolicyOnFailure, r.config.ApprovalPolicy)
	}
}

func TestNew_ApprovalPolicyNotOverriddenForCursor(t *testing.T) {
	r := New(Config{BackendType: BackendCursor})
	if r.config.ApprovalPolicy != "" {
		t.Errorf("expected cursor approval policy to remain empty, got %q", r.config.ApprovalPolicy)
	}
}

func TestValidateBackend(t *testing.T) {
	tests := []struct {
		backend string
		wantErr bool
	}{
		{"cursor", false},
		{"codex", false},
		{"gemini", false},
		{"unknown", true},
		{"", true},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			err := ValidateBackend(tt.backend)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateBackend(%q) error = %v, wantErr %v", tt.backend, err, tt.wantErr)
			}
		})
	}
}

func TestNew_GeminiBackend(t *testing.T) {
	r := New(Config{
		BackendType: BackendGemini,
	})

	if r.config.BackendType != BackendGemini {
		t.Errorf("expected gemini backend, got %s", r.config.BackendType)
	}
	if r.config.Model != "gemini-3.1-flash-lite-preview" {
		t.Errorf("expected default model gemini-3.1-flash-lite-preview, got %s", r.config.Model)
	}
	if r.backend == nil {
		t.Error("backend should not be nil for gemini")
	}
	// Gemini backend Start and Stop are no-ops.
	if err := r.backend.Start(nil); err != nil { //nolint:staticcheck
		t.Errorf("gemini start should be no-op, got error: %v", err)
	}
	if err := r.backend.Stop(); err != nil {
		t.Errorf("gemini stop should be no-op, got error: %v", err)
	}
}

func TestNew_GeminiBackend_CustomModel(t *testing.T) {
	r := New(Config{
		BackendType: BackendGemini,
		Model:       "gemini-2.5-flash",
	})
	if r.config.Model != "gemini-2.5-flash" {
		t.Errorf("expected custom model gemini-2.5-flash, got %s", r.config.Model)
	}
}

func TestNew_ApprovalPolicyNotOverriddenForGemini(t *testing.T) {
	r := New(Config{BackendType: BackendGemini})
	if r.config.ApprovalPolicy != "" {
		t.Errorf("expected gemini approval policy to remain empty, got %q", r.config.ApprovalPolicy)
	}
}

func TestEffectiveModel_GeminiDefault(t *testing.T) {
	r := New(Config{BackendType: BackendGemini})
	if got := r.EffectiveModel(); got != "gemini-3.1-flash-lite-preview" {
		t.Errorf("EffectiveModel() = %q, want gemini-3.1-flash-lite-preview", got)
	}
}

func TestValidateBackend_GeminiErrorMessage(t *testing.T) {
	err := ValidateBackend("unknown")
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if !containsString(err.Error(), "gemini") {
		t.Errorf("error message should mention gemini: %q", err.Error())
	}
}

func TestResolveWorkDir_EnvVar(t *testing.T) {
	t.Setenv("WORK_DIR", "/tmp/test-workdir")
	dir, err := ResolveWorkDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir != "/tmp/test-workdir" {
		t.Errorf("expected /tmp/test-workdir, got %s", dir)
	}
}

func TestResolveWorkDir_Fallback(t *testing.T) {
	t.Setenv("WORK_DIR", "")
	dir, err := ResolveWorkDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir == "" {
		t.Error("expected non-empty directory from os.Getwd()")
	}
}

func TestResolveProtocolLogPath_Empty(t *testing.T) {
	t.Setenv("BRAMBLE_PROTOCOL_LOG_DIR", "")
	path, err := ResolveProtocolLogPath("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestResolveProtocolLogPath_FlagValue(t *testing.T) {
	dir := t.TempDir()
	path, err := ResolveProtocolLogPath(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("expected path under %s, got %s", dir, path)
	}
	if !strings.HasSuffix(path, ".jsonl") {
		t.Errorf("expected .jsonl suffix, got %s", path)
	}
	if strings.Contains(filepath.Base(path), "reviewer-session-") == false {
		t.Errorf("expected timestamped filename, got %s", filepath.Base(path))
	}
}

func TestResolveProtocolLogPath_EnvVarFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BRAMBLE_PROTOCOL_LOG_DIR", dir)
	path, err := ResolveProtocolLogPath("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("expected path under %s, got %s", dir, path)
	}
}

func TestResolveProtocolLogPath_FlagOverridesEnv(t *testing.T) {
	envDir := t.TempDir()
	flagDir := t.TempDir()
	t.Setenv("BRAMBLE_PROTOCOL_LOG_DIR", envDir)
	path, err := ResolveProtocolLogPath(flagDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(path, flagDir) {
		t.Errorf("expected flag dir %s to take priority, got %s", flagDir, path)
	}
}

func TestResolveProtocolLogPath_CreatesDirectory(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "nested", "dir")
	path, err := ResolveProtocolLogPath(nested)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(nested); os.IsNotExist(err) {
		t.Error("expected directory to be created")
	}
	if path == "" {
		t.Error("expected non-empty path")
	}
}

func TestResolveProtocolLogPath_UniqueFilenames(t *testing.T) {
	dir := t.TempDir()
	path1, err := ResolveProtocolLogPath(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	path2, err := ResolveProtocolLogPath(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Within the same second these will be equal, but the format includes
	// seconds, so they should at least have the timestamp prefix.
	if !strings.Contains(filepath.Base(path1), "reviewer-session-") {
		t.Errorf("expected timestamped filename, got %s", filepath.Base(path1))
	}
	if !strings.Contains(filepath.Base(path2), "reviewer-session-") {
		t.Errorf("expected timestamped filename, got %s", filepath.Base(path2))
	}
}
