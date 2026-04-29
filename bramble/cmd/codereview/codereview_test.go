package codereview

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

func TestReportEnvelopePrintError_WritesToStderr(t *testing.T) {
	// reportEnvelopePrintError uses slog.Error. In production the tee handler
	// (installed by SetupRunLog) routes ERROR records to stderr. Install a
	// temporary slog handler that writes to a buffer so the output is observable
	// without replacing os.Stderr (which slog's default handler doesn't follow
	// after dynamic reassignment).
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	reportEnvelopePrintError(errors.New("broken pipe"))

	got := buf.String()
	if !strings.Contains(got, "broken pipe") {
		t.Errorf("slog output missing wrapped error: %q", got)
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
// duration of fn, returning whatever was written to each. runCodeReview writes
// the envelope to stdout (or --envelope-file) and diagnostics to stderr;
// isolating both lets a test assert on each surface independently.
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

func TestLoadPromptOptions_NoFile(t *testing.T) {
	// Empty path is the legacy/default case: no hints loaded, but
	// SkipTestExecution must still pass through.
	opts := loadPromptOptions("", true)
	if !opts.SkipTestExecution {
		t.Error("SkipTestExecution should be passed through with empty path")
	}
	if len(opts.TestScopeHints) != 0 || len(opts.CrossServicePackages) != 0 {
		t.Errorf("expected empty hints for empty path, got %+v", opts)
	}
}

func TestLoadPromptOptions_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hints.json")
	contents := `{"schema_version":1,"test_paths":["x/test_y.py"],"cross_service_packages":["a/","b/"]}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write hints file: %v", err)
	}
	opts := loadPromptOptions(path, false)
	if len(opts.TestScopeHints) != 1 || opts.TestScopeHints[0] != "x/test_y.py" {
		t.Errorf("TestScopeHints = %v", opts.TestScopeHints)
	}
	if len(opts.CrossServicePackages) != 2 {
		t.Errorf("CrossServicePackages len = %d, want 2", len(opts.CrossServicePackages))
	}
}

func TestLoadPromptOptions_MalformedFallsBack(t *testing.T) {
	// The contract: a malformed/missing hints file must NOT abort the
	// review. Falls back to narrow-review options and logs a warning. This
	// matches the plan's "log-and-fall-back" behavior — automation
	// (/pr-polish) can pass a hints file without worrying that its
	// scope_gate.py emitting garbage will brick the run.
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	path := filepath.Join(dir, "hints.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write hints file: %v", err)
	}
	opts := loadPromptOptions(path, true)
	if !opts.SkipTestExecution {
		t.Error("SkipTestExecution should pass through even on fallback")
	}
	if len(opts.TestScopeHints) != 0 {
		t.Errorf("expected empty TestScopeHints on fallback, got %v", opts.TestScopeHints)
	}
	logged := buf.String()
	if !strings.Contains(logged, "scope-hints file ignored") {
		t.Errorf("expected warning log, got: %q", logged)
	}
	if !strings.Contains(logged, "narrow review") {
		t.Errorf("warning should mention narrow-review fallback, got: %q", logged)
	}
}

func TestLoadPromptOptions_MissingFileFallsBack(t *testing.T) {
	// Same fallback behavior for a path that doesn't exist on disk.
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	opts := loadPromptOptions(filepath.Join(t.TempDir(), "nonexistent.json"), false)
	if len(opts.TestScopeHints) != 0 {
		t.Errorf("expected empty TestScopeHints on missing-file fallback, got %v", opts.TestScopeHints)
	}
	if !strings.Contains(buf.String(), "scope-hints file ignored") {
		t.Errorf("expected warning log, got: %q", buf.String())
	}
}

func TestBuildPromptForRun_WidensWithRealHintsFile(t *testing.T) {
	// End-to-end seam: a real hints file on disk should produce a prompt
	// carrying both the test-quality clause (because test_paths is
	// non-empty) and the cross-service clause (because >=2 packages).
	// This is the test codex flagged as missing: a regression that drops
	// scopeHintsFile from the prompt path, or stops calling
	// loadPromptOptions, would still pass the lower-level tests but fail
	// here.
	dir := t.TempDir()
	path := filepath.Join(dir, "hints.json")
	contents := `{"schema_version":1,"test_paths":["pkg/test_x.py"],"cross_service_packages":["a/","b/"]}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write hints: %v", err)
	}
	got := buildPromptForRun("review goal", path, false)
	if !strings.Contains(got, "## Test quality") {
		t.Errorf("prompt missing test-quality clause; got:\n%s", got)
	}
	if !strings.Contains(got, "## Cross-service contract sweep") {
		t.Errorf("prompt missing cross-service clause; got:\n%s", got)
	}
	if !strings.Contains(got, "pkg/test_x.py") {
		t.Errorf("prompt missing inlined test path; got:\n%s", got)
	}
	if !strings.Contains(got, "review goal") {
		t.Errorf("prompt missing goal text; got:\n%s", got)
	}
}

func TestBuildPromptForRun_V2HintsThreadCallerCalleeFraming(t *testing.T) {
	// End-to-end seam for the v2 scope-hints shape: changed_packages and
	// dependency_packages should reach the prompt builder and select the
	// caller/callee framing instead of the generic flat-list framing.
	// Guards against a regression that drops the new fields somewhere
	// between LoadScopeHints and BuildJSONPromptWithScope.
	dir := t.TempDir()
	path := filepath.Join(dir, "hints.json")
	contents := `{
		"schema_version": 2,
		"test_paths": ["pkg/test_x.py"],
		"cross_service_packages": ["svc/a/", "svc/b/"],
		"changed_packages": ["svc/a/"],
		"dependency_packages": ["svc/b/"]
	}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write hints: %v", err)
	}
	got := buildPromptForRun("review goal", path, false)
	if !strings.Contains(got, "## Cross-service contract sweep") {
		t.Errorf("prompt missing cross-service clause; got:\n%s", got)
	}
	if !strings.Contains(got, "primarily modifies") {
		t.Errorf("prompt missing caller/callee framing; got:\n%s", got)
	}
	if !strings.Contains(got, "callers or dependencies") {
		t.Errorf("prompt missing callers/dependencies line; got:\n%s", got)
	}
	if !strings.Contains(got, "svc/a/") || !strings.Contains(got, "svc/b/") {
		t.Errorf("prompt missing changed/dependency packages; got:\n%s", got)
	}
}

func TestBuildPromptForRun_NoHintsMatchesLegacy(t *testing.T) {
	// Empty hints path must produce today's narrow prompt, byte-equal to
	// the legacy BuildJSONPrompt output. This is the no-regressions
	// guarantee for callers that haven't opted into the wider scope.
	got := buildPromptForRun("g", "", false)
	want := reviewer.BuildJSONPrompt("g")
	if got != want {
		t.Errorf("empty hints path must equal legacy prompt\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildPromptForRun_MalformedHintsFallsBackToLegacy(t *testing.T) {
	// A malformed hints file is the same outcome as no hints file: the
	// legacy narrow prompt. The slog fallback warning is exercised by
	// TestLoadPromptOptions_MalformedFallsBack; this test guards the
	// next layer up.
	dir := t.TempDir()
	path := filepath.Join(dir, "hints.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write hints: %v", err)
	}
	got := buildPromptForRun("g", path, true)
	want := reviewer.BuildJSONPromptWithOptions("g", true)
	if got != want {
		t.Errorf("malformed hints must fall back to legacy prompt\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCmd_ScopeHintsFileFlagIsWired(t *testing.T) {
	// Cobra-level proof that --scope-hints-file is registered, parses,
	// and binds to the global the prompt builder reads. Defends against:
	//   - the flag being removed from init()
	//   - the flag string changing (e.g. typo to --scope-hint-file)
	//   - the StringVar binding being unhooked from &scopeHintsFile
	// All three would still pass the helper-level loadPromptOptions and
	// buildPromptForRun tests, because those bypass Cobra entirely.
	prev := scopeHintsFile
	t.Cleanup(func() { scopeHintsFile = prev })
	scopeHintsFile = ""

	if err := Cmd.ParseFlags([]string{"--scope-hints-file", "/tmp/example-hints.json"}); err != nil {
		t.Fatalf("ParseFlags failed: %v", err)
	}
	if scopeHintsFile != "/tmp/example-hints.json" {
		t.Errorf("scopeHintsFile global = %q, want /tmp/example-hints.json", scopeHintsFile)
	}

	// And once more with a different value, to confirm the binding
	// re-parses on each call rather than freezing on first read.
	if err := Cmd.ParseFlags([]string{"--scope-hints-file", "/other/path.json"}); err != nil {
		t.Fatalf("ParseFlags second pass failed: %v", err)
	}
	if scopeHintsFile != "/other/path.json" {
		t.Errorf("scopeHintsFile global after second parse = %q, want /other/path.json", scopeHintsFile)
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
