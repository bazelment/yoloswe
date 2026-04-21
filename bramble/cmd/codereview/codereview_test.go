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

func TestEmitEarlyFailure_WritesEnvelopeToStdout(t *testing.T) {
	// emitEarlyFailure must always emit one parseable envelope so automation
	// (e.g. /pr-polish) never has to distinguish "bramble failed early" from
	// "bramble failed late".
	origBackend := backend
	backend = "codex"
	t.Cleanup(func() { backend = origBackend })

	boom := errors.New("backend unreachable")
	var returned error
	stdout, _ := captureStdStreams(t, func() {
		emit := func(env reviewer.ResultEnvelope) {
			_ = reviewer.PrintJSONResult(os.Stdout, env)
		}
		returned = emitEarlyFailure(boom, "gpt-x", emit)
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

func TestFinalizeEnvelope_SuccessDoesNotDoubleEmit(t *testing.T) {
	// When runCodeReview's happy path has already flushed an envelope, the
	// top-level defer must not synthesize a second one — otherwise automation
	// sees two JSON objects and has no way to know which is authoritative.
	written := true
	var retErr error
	emitted := 0
	emit := func(reviewer.ResultEnvelope) {
		emitted++
		written = true
	}
	finalizeEnvelope(envelopeGuardArgs{
		backend:         "codex",
		envelopeWritten: &written,
		retErr:          &retErr,
		panicVal:        nil,
		emit:            emit,
	})
	if emitted != 0 {
		t.Errorf("emit called %d times, want 0 on happy path", emitted)
	}
}

func TestFinalizeEnvelope_UnwrittenReturnSynthesizesEnvelope(t *testing.T) {
	// The PR #162 scenario in the small: the reviewer exited 0 but never
	// wrote an envelope. The defer must detect this and put a parseable
	// error envelope on stdout so /pr-polish can distinguish "ran and found
	// nothing" from "ran and emitted nothing".
	written := false
	var retErr error
	var got reviewer.ResultEnvelope
	emit := func(env reviewer.ResultEnvelope) {
		got = env
		written = true
	}
	finalizeEnvelope(envelopeGuardArgs{
		backend:         "codex",
		envelopeWritten: &written,
		retErr:          &retErr,
		panicVal:        nil,
		emit:            emit,
	})
	if !written {
		t.Fatalf("expected emit to have been called")
	}
	if got.Status != reviewer.StatusError {
		t.Errorf("status = %s, want error", got.Status)
	}
	if !strings.Contains(got.Error, "without producing a review") {
		t.Errorf("error message %q missing sentinel substring", got.Error)
	}
	if got.Backend != "codex" {
		t.Errorf("backend = %s, want codex", got.Backend)
	}
}

func TestFinalizeEnvelope_PanicEmitsEnvelopeThenRepanics(t *testing.T) {
	// Panics anywhere below must not cost us the envelope — but they must
	// still propagate so cobra exits non-zero. Verify both: emit runs once
	// with a panic-flavored message, and the original panic value re-raises.
	written := false
	var retErr error
	var got reviewer.ResultEnvelope
	emit := func(env reviewer.ResultEnvelope) {
		got = env
		written = true
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic to re-raise, got nil")
		}
		if rs, _ := r.(string); rs != "kaboom" {
			t.Errorf("re-raised value = %v, want %q", r, "kaboom")
		}
		if !written {
			t.Fatalf("envelope was not emitted before re-panic")
		}
		if got.Status != reviewer.StatusError {
			t.Errorf("status = %s, want error", got.Status)
		}
		if !strings.Contains(got.Error, "panic in code-review") {
			t.Errorf("error %q missing panic prefix", got.Error)
		}
		if !strings.Contains(got.Error, "kaboom") {
			t.Errorf("error %q missing panic value", got.Error)
		}
		if retErr == nil || !strings.Contains(retErr.Error(), "kaboom") {
			t.Errorf("retErr = %v, want wrapping of panic value", retErr)
		}
	}()

	finalizeEnvelope(envelopeGuardArgs{
		backend:         "codex",
		envelopeWritten: &written,
		retErr:          &retErr,
		panicVal:        "kaboom",
		emit:            emit,
	})
}

func TestFinalizeEnvelope_PanicAlwaysEmitsAndRepanics(t *testing.T) {
	// A panic must always produce an envelope (so automation gets a result)
	// AND re-raise (so the process exits non-zero).
	written := false
	var retErr error
	var got reviewer.ResultEnvelope
	emit := func(env reviewer.ResultEnvelope) {
		got = env
		written = true
	}

	defer func() {
		if r := recover(); r != "boom" {
			t.Errorf("re-raised value = %v, want %q", r, "boom")
		}
		if !written {
			t.Errorf("envelope was not emitted on panic")
		}
		if got.Status != reviewer.StatusError {
			t.Errorf("status = %s, want error", got.Status)
		}
	}()

	finalizeEnvelope(envelopeGuardArgs{
		backend:         "codex",
		envelopeWritten: &written,
		retErr:          &retErr,
		panicVal:        "boom",
		emit:            emit,
	})
}

func TestFinalizeEnvelope_ReturnErrorPropagatesToEnvelopeMessage(t *testing.T) {
	// When the function is returning a regular (non-panic) error and no
	// envelope has been written yet, the synthesized envelope should carry
	// the error's message rather than the generic sentinel — so automation
	// can tell which error code path fired.
	written := false
	retErr := errors.New("reviewer drive-by: auth denied")
	var got reviewer.ResultEnvelope
	emit := func(env reviewer.ResultEnvelope) {
		got = env
		written = true
	}
	finalizeEnvelope(envelopeGuardArgs{
		backend:         "codex",
		envelopeWritten: &written,
		retErr:          &retErr,
		panicVal:        nil,
		emit:            emit,
	})
	if got.Error != "reviewer drive-by: auth denied" {
		t.Errorf("envelope.error = %q, want %q", got.Error, retErr.Error())
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
