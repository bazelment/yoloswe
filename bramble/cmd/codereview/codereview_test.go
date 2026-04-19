package codereview

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

func TestReportEnvelopePrintError_WritesToStderr(t *testing.T) {
	// SetupRunLog rebinds slog.Default() to a file-only handler, so this
	// helper must bypass slog and write directly to stderr — otherwise
	// stdout-serialization failures would only land in the per-run log
	// where the operator never looks.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	reportEnvelopePrintError(errors.New("broken pipe"))
	_ = w.Close()
	got := <-done

	if !strings.Contains(got, "broken pipe") {
		t.Errorf("stderr missing wrapped error: %q", got)
	}
	if !strings.Contains(got, "code-review") {
		t.Errorf("stderr missing source tag: %q", got)
	}
}

func TestRedactPath(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantSuffix  string
		forbidParts []string
	}{
		{
			name:        "absolute home path",
			in:          "/home/alice/work/project-x",
			wantSuffix:  "/project-x",
			forbidParts: []string{"/home/alice", "/work/"},
		},
		{
			name:        "worktree path",
			in:          "/home/bob/worktrees/repo/feature/foo",
			wantSuffix:  "/foo",
			forbidParts: []string{"/home/bob", "worktrees", "repo", "feature"},
		},
		{
			name:       "empty",
			in:         "",
			wantSuffix: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactPath(tt.in)
			if tt.in == "" {
				if got != "" {
					t.Errorf("redactPath(\"\") = %q, want empty", got)
				}
				return
			}
			if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("redactPath(%q) = %q, want suffix %q", tt.in, got, tt.wantSuffix)
			}
			for _, forbidden := range tt.forbidParts {
				if strings.Contains(got, forbidden) {
					t.Errorf("redactPath(%q) = %q leaked %q", tt.in, got, forbidden)
				}
			}
		})
	}
}

// captureStdStreams redirects os.Stdout and os.Stderr to pipes for the
// duration of fn, returning whatever was written to each. Many of the
// --json paths in runCodeReview write the envelope to stdout and
// diagnostics to stderr; isolating both lets a test assert on the
// machine-readable contract and the operator-visible message independently.
func captureStdStreams(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stdout, os.Stderr = outW, errW
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })

	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := io.ReadAll(outR); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errCh <- string(b) }()

	fn()
	_ = outW.Close()
	_ = errW.Close()
	return <-outCh, <-errCh
}

func TestEmitEarlyFailure_JSONMode_WritesEnvelopeToStdout(t *testing.T) {
	// Guards the --json contract: a pre-review failure (validation, cwd
	// resolution, backend start) must still emit one parseable envelope on
	// stdout so /pr-polish and similar automation never have to distinguish
	// "bramble failed early" from "bramble failed late". This is the exact
	// invariant the round-4 slog regression broke.
	origJSON, origBackend := jsonOutput, backend
	jsonOutput = true
	backend = "codex"
	t.Cleanup(func() { jsonOutput, backend = origJSON, origBackend })

	boom := errors.New("backend unreachable")
	var returned error
	stdout, _ := captureStdStreams(t, func() {
		returned = emitEarlyFailure(boom, "gpt-x")
	})

	if returned == nil || !strings.Contains(returned.Error(), "backend unreachable") {
		t.Errorf("returned error = %v, want wrap of original", returned)
	}
	var env reviewer.ResultEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &env); err != nil {
		t.Fatalf("stdout must be a single JSON envelope, got %q: %v", stdout, err)
	}
	if env.Status != reviewer.StatusError {
		t.Errorf("status = %s, want error", env.Status)
	}
	if !strings.Contains(env.Error, "backend unreachable") {
		t.Errorf("envelope.error = %q, want wrapped original", env.Error)
	}
	if env.Backend != "codex" || env.Model != "gpt-x" {
		t.Errorf("envelope backend/model = %s/%s, want codex/gpt-x", env.Backend, env.Model)
	}
	if env.SchemaVersion != reviewer.JSONSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, reviewer.JSONSchemaVersion)
	}
}

func TestEmitEarlyFailure_NonJSONMode_NoStdout(t *testing.T) {
	// The text output contract: prose mode must leave stdout untouched on
	// early failure so humans see the error via stderr and shells that pipe
	// stdout (| tee, | grep) don't pick up stray JSON they can't render.
	origJSON, origBackend := jsonOutput, backend
	jsonOutput = false
	backend = "codex"
	t.Cleanup(func() { jsonOutput, backend = origJSON, origBackend })

	stdout, _ := captureStdStreams(t, func() {
		_ = emitEarlyFailure(errors.New("boom"), "")
	})
	if stdout != "" {
		t.Errorf("stdout should be empty in prose mode, got %q", stdout)
	}
}

func TestMaxSeverity(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		issues []reviewer.ReviewIssue
	}{
		{
			name:   "empty",
			issues: nil,
			want:   "",
		},
		{
			name: "single low",
			issues: []reviewer.ReviewIssue{
				{Severity: "low"},
			},
			want: "low",
		},
		{
			name: "standard ordering picks critical",
			issues: []reviewer.ReviewIssue{
				{Severity: "low"},
				{Severity: "medium"},
				{Severity: "critical"},
				{Severity: "high"},
			},
			want: "critical",
		},
		{
			name: "skips empty severities",
			issues: []reviewer.ReviewIssue{
				{Severity: ""},
				{Severity: "medium"},
			},
			want: "medium",
		},
		{
			name: "unknown label outranks low even when low is seen first",
			issues: []reviewer.ReviewIssue{
				{Severity: "low"},
				{Severity: "blocker"},
			},
			want: "blocker",
		},
		{
			name: "unknown label still below medium",
			issues: []reviewer.ReviewIssue{
				{Severity: "blocker"},
				{Severity: "medium"},
			},
			want: "medium",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maxSeverity(tt.issues)
			if got != tt.want {
				t.Errorf("maxSeverity(%+v) = %q, want %q", tt.issues, got, tt.want)
			}
		})
	}
}
