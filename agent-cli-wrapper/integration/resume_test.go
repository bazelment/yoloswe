//go:build integration
// +build integration

// Generic, table-driven session-resume integration test that exercises the
// "save-and-recall a secret" pattern against every agent-CLI backend.
//
// Each backend has its own native resume API:
//
//   - claude:  claude.WithResume(sessionID) on a new Session
//   - codex:   codex.Client.ResumeThread(ctx, threadID, ...)
//   - cursor:  cursor.WithResume(sessionID) on a new Query
//   - agy:     agy.WithContinue() for the most recent print-mode conversation
//   - acp:     acp.Client.LoadSession(ctx, sessionID, ...) for ACP backends
//
// To keep the contract of "resume actually preserves conversational context"
// uniform across backends, each implementation conforms to the resumeBackend
// interface below. The shared TestResume_Backends test runs the same
// secret-recall scenario against every backend that's available on PATH /
// has the right credentials configured. Backends that aren't installed are
// skipped, not failed — this lets the suite run on developer machines that
// have only some CLIs set up.
//
// Run:
//
//	bazel test //agent-cli-wrapper/integration:integration_test \
//	    --test_tag_filters=integration --test_output=streamed
//
// Or directly:
//
//	go test -tags=integration -v ./agent-cli-wrapper/integration/...
package integration

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
)

// resumeBackend is the minimal contract a backend needs to satisfy so the
// shared resume scenario can drive it. The two phases (StartFresh and Resume)
// each take a single prompt and return the agent's free-text response —
// enough to assert that the secret recorded in phase 1 is recalled in phase 2.
type resumeBackend interface {
	// Name is a short identifier used in subtest names and logs.
	Name() string
	// Available reports whether the backend's CLI is installed and the
	// environment is set up well enough to run the scenario. When false,
	// the subtest is skipped (not failed).
	Available() (bool, string)
	// StartFresh begins a new session and runs a single turn with prompt.
	// Returns the captured session id (for Resume) and the response text.
	StartFresh(ctx context.Context, t *testing.T, workdir, prompt string) (sessionID, response string, err error)
	// Resume re-attaches to a prior session by id and runs a single turn
	// with prompt. Returns the response text plus the session id the
	// backend reports for the resumed session — the scenario asserts that
	// matches the requested id, so a backend that silently re-keys (or
	// fails to actually resume and falls back fresh) is detected even if
	// the model happens to echo a matching secret.
	Resume(ctx context.Context, t *testing.T, workdir, sessionID, prompt string) (resumedSessionID, response string, err error)
}

const (
	// secretToken is unique enough that the resumed session can only know
	// it by recalling phase-1 context — not by guessing or grepping the
	// workdir. Keep it short so token-budgeted models reproduce it cleanly.
	secretToken = "ECHO-7F4A-LOBSTER-92"

	phase1Prompt = "Please remember this code exactly: " + secretToken +
		". Reply with only the word: acknowledged."
	phase2Prompt = "What was the code I asked you to remember? Reply with only the code, nothing else."

	// resumeWindow gates how long we wait between phase 1 and phase 2.
	// Some backends (notably cursor) keep session state in a local cache
	// that needs a moment to flush before --resume can find it.
	resumeWindow = 500 * time.Millisecond
)

func TestResume_Backends(t *testing.T) {
	backends := []resumeBackend{
		&claudeResumeBackend{},
		&codexResumeBackend{},
		&cursorResumeBackend{},
		&agyResumeBackend{},
	}

	for _, backend := range backends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			ok, reason := backend.Available()
			if !ok {
				t.Skipf("backend %q unavailable: %s", backend.Name(), reason)
			}
			runResumeScenario(t, backend)
		})
	}
}

type agyResumeBackend struct{}

func (a *agyResumeBackend) Name() string { return "agy" }

func (a *agyResumeBackend) Available() (bool, string) {
	if _, err := exec.LookPath("agy"); err != nil {
		return false, "agy CLI not found on PATH"
	}
	return true, ""
}

func (a *agyResumeBackend) StartFresh(ctx context.Context, t *testing.T, workdir, prompt string) (string, string, error) {
	t.Helper()
	result, err := agy.Query(ctx, prompt,
		agy.WithWorkDir(workdir),
		agy.WithDangerouslySkipPermissions(),
	)
	if err != nil {
		return "", "", fmt.Errorf("agy start fresh: %w", err)
	}
	if !result.Success {
		return "", result.Text, fmt.Errorf("agy turn failed")
	}
	return "latest", result.Text, nil
}

func (a *agyResumeBackend) Resume(ctx context.Context, t *testing.T, workdir, sessionID, prompt string) (string, string, error) {
	t.Helper()
	if sessionID != "latest" {
		return "", "", fmt.Errorf("agy resume only supports latest conversation sentinel, got %q", sessionID)
	}
	result, err := agy.Query(ctx, prompt,
		agy.WithWorkDir(workdir),
		agy.WithDangerouslySkipPermissions(),
		agy.WithContinue(),
	)
	if err != nil {
		return "", "", fmt.Errorf("agy continue: %w", err)
	}
	if !result.Success {
		return sessionID, result.Text, fmt.Errorf("agy resumed turn failed")
	}
	return sessionID, result.Text, nil
}

// runResumeScenario is the shared body that proves resume preserves context.
// It is intentionally agent-agnostic — anything backend-specific is in the
// resumeBackend implementation.
func runResumeScenario(t *testing.T, b resumeBackend) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	workdir := t.TempDir()

	t.Logf("phase 1: starting fresh session and recording secret %q", secretToken)
	sessionID, phase1Resp, err := b.StartFresh(ctx, t, workdir, phase1Prompt)
	if err != nil {
		t.Fatalf("phase 1 StartFresh failed: %v", err)
	}
	if sessionID == "" {
		t.Fatalf("phase 1 returned empty session id; cannot exercise resume")
	}
	t.Logf("phase 1 session id: %s", sessionID)
	t.Logf("phase 1 response (truncated): %s", truncate(phase1Resp, 200))

	t.Logf("phase 2: resuming session %s and asking for the secret back", sessionID)
	resumedID, phase2Resp, err := resumeWithRetry(ctx, t, b, workdir, sessionID, phase2Prompt)
	if err != nil {
		t.Fatalf("phase 2 Resume failed: %v", err)
	}
	t.Logf("phase 2 response (truncated): %s", truncate(phase2Resp, 200))

	// Assert session identity. Without this, a backend that silently
	// re-keys the session on resume (or falls back to fresh and gets a
	// lucky model echo) would pass the secret-recall check but mask a
	// real resume regression — the exact failure mode that motivated this
	// test in the first place.
	if resumedID == "" {
		t.Fatalf("resumed session id is empty; backend cannot prove it actually resumed")
	}
	if resumedID != sessionID {
		t.Fatalf("resumed session id mismatch: requested %q, got %q (silent fallback?)",
			sessionID, resumedID)
	}

	if !containsSecret(phase2Resp, secretToken) {
		t.Fatalf("resumed session did not recall secret %q; got response: %s",
			secretToken, truncate(phase2Resp, 500))
	}
	t.Logf("resume verified: id %q matches and secret recalled across phase boundary",
		resumedID)
}

func resumeWithRetry(
	ctx context.Context,
	t *testing.T,
	b resumeBackend,
	workdir string,
	sessionID string,
	prompt string,
) (string, string, error) {
	t.Helper()

	deadline := time.NewTimer(resumeWindow)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		resumedID, resp, err := b.Resume(ctx, t, workdir, sessionID, prompt)
		if err == nil {
			return resumedID, resp, nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-deadline.C:
			return "", "", err
		case <-ticker.C:
		}
	}
}

// containsSecret matches the secret with light tolerance for whitespace and
// punctuation rewrites that some models apply when echoing tokens
// (e.g. "ECHO 7F4A LOBSTER 92" instead of "ECHO-7F4A-LOBSTER-92").
func containsSecret(resp, secret string) bool {
	normalize := func(s string) string {
		s = strings.ToUpper(s)
		s = strings.NewReplacer(" ", "", "-", "", "_", "", ".", "", ",", "").Replace(s)
		return s
	}
	return strings.Contains(normalize(resp), normalize(secret))
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ---------------------------------------------------------------------------
// claude
// ---------------------------------------------------------------------------

type claudeResumeBackend struct{}

func (c *claudeResumeBackend) Name() string { return "claude" }

func (c *claudeResumeBackend) Available() (bool, string) {
	if _, err := exec.LookPath("claude"); err != nil {
		return false, "claude CLI not found on PATH"
	}
	return true, ""
}

func (c *claudeResumeBackend) StartFresh(ctx context.Context, t *testing.T, workdir, prompt string) (string, string, error) {
	t.Helper()
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(workdir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
	)
	if err := session.Start(ctx); err != nil {
		return "", "", fmt.Errorf("claude session start: %w", err)
	}
	defer session.Stop()

	if _, err := session.SendMessage(ctx, prompt); err != nil {
		return "", "", fmt.Errorf("claude SendMessage: %w", err)
	}
	sessionID, response, err := drainClaudeTurn(ctx, session)
	if err != nil {
		return "", "", err
	}
	if sessionID == "" {
		return "", response, errors.New("claude: never observed ReadyEvent with session id")
	}
	return sessionID, response, nil
}

func (c *claudeResumeBackend) Resume(ctx context.Context, t *testing.T, workdir, sessionID, prompt string) (string, string, error) {
	t.Helper()
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(workdir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithResume(sessionID),
	)
	if err := session.Start(ctx); err != nil {
		return "", "", fmt.Errorf("claude resume start: %w", err)
	}
	defer session.Stop()

	if _, err := session.SendMessage(ctx, prompt); err != nil {
		return "", "", fmt.Errorf("claude resume SendMessage: %w", err)
	}
	resumedID, response, err := drainClaudeTurn(ctx, session)
	return resumedID, response, err
}

func drainClaudeTurn(ctx context.Context, session *claude.Session) (sessionID, response string, err error) {
	for {
		select {
		case <-ctx.Done():
			return sessionID, response, ctx.Err()
		case ev, ok := <-session.Events():
			if !ok {
				return sessionID, response, errors.New("claude event channel closed before turn complete")
			}
			switch e := ev.(type) {
			case claude.ReadyEvent:
				if sessionID == "" {
					sessionID = e.Info.SessionID
				}
			case claude.TextEvent:
				if e.FullText != "" {
					response = e.FullText
				}
			case claude.TurnCompleteEvent:
				if !e.Success {
					return sessionID, response, fmt.Errorf("claude turn failed: success=false")
				}
				return sessionID, response, nil
			}
		}
	}
}

// ---------------------------------------------------------------------------
// codex
// ---------------------------------------------------------------------------

type codexResumeBackend struct{}

func (c *codexResumeBackend) Name() string { return "codex" }

func (c *codexResumeBackend) Available() (bool, string) {
	if _, err := exec.LookPath("codex"); err != nil {
		return false, "codex CLI not found on PATH"
	}
	return true, ""
}

func (c *codexResumeBackend) StartFresh(ctx context.Context, t *testing.T, workdir, prompt string) (string, string, error) {
	t.Helper()
	client := codex.NewClient(
		codex.WithClientName("agent-cli-wrapper-resume-test"),
		codex.WithClientVersion("1.0.0"),
	)
	if err := client.Start(ctx); err != nil {
		return "", "", fmt.Errorf("codex client start: %w", err)
	}
	defer client.Stop()

	thread, err := client.CreateThread(ctx,
		codex.WithWorkDir(workdir),
		codex.WithApprovalPolicy(codex.ApprovalPolicyFullAuto),
	)
	if err != nil {
		return "", "", fmt.Errorf("codex CreateThread: %w", err)
	}
	if err := thread.WaitReady(ctx); err != nil {
		return "", "", fmt.Errorf("codex thread WaitReady: %w", err)
	}

	result, err := thread.Ask(ctx, prompt)
	if err != nil {
		return "", "", fmt.Errorf("codex Ask: %w", err)
	}
	if !result.Success {
		return "", result.FullText, fmt.Errorf("codex turn failed: %v", result.Error)
	}
	return thread.ID(), result.FullText, nil
}

func (c *codexResumeBackend) Resume(ctx context.Context, t *testing.T, workdir, sessionID, prompt string) (string, string, error) {
	t.Helper()
	client := codex.NewClient(
		codex.WithClientName("agent-cli-wrapper-resume-test"),
		codex.WithClientVersion("1.0.0"),
	)
	if err := client.Start(ctx); err != nil {
		return "", "", fmt.Errorf("codex resume client start: %w", err)
	}
	defer client.Stop()

	thread, err := client.ResumeThread(ctx, sessionID,
		codex.WithWorkDir(workdir),
		codex.WithApprovalPolicy(codex.ApprovalPolicyFullAuto),
	)
	if err != nil {
		return "", "", fmt.Errorf("codex ResumeThread: %w", err)
	}
	if err := thread.WaitReady(ctx); err != nil {
		return "", "", fmt.Errorf("codex resumed thread WaitReady: %w", err)
	}

	result, err := thread.Ask(ctx, prompt)
	if err != nil {
		return thread.ID(), "", fmt.Errorf("codex resume Ask: %w", err)
	}
	if !result.Success {
		return thread.ID(), result.FullText, fmt.Errorf("codex resume turn failed: %v", result.Error)
	}
	return thread.ID(), result.FullText, nil
}

// ---------------------------------------------------------------------------
// cursor
// ---------------------------------------------------------------------------

type cursorResumeBackend struct{}

func (c *cursorResumeBackend) Name() string { return "cursor" }

func (c *cursorResumeBackend) Available() (bool, string) {
	if _, err := exec.LookPath("agent"); err != nil {
		return false, "cursor 'agent' CLI not found on PATH"
	}
	return true, ""
}

func (c *cursorResumeBackend) StartFresh(ctx context.Context, t *testing.T, workdir, prompt string) (string, string, error) {
	t.Helper()
	result, err := cursor.Query(ctx, prompt, cursorOpts(workdir, t)...)
	if err != nil {
		return "", "", fmt.Errorf("cursor Query: %w", err)
	}
	return result.SessionID, result.Text, nil
}

func (c *cursorResumeBackend) Resume(ctx context.Context, t *testing.T, workdir, sessionID, prompt string) (string, string, error) {
	t.Helper()
	opts := append(cursorOpts(workdir, t), cursor.WithResume(sessionID))
	result, err := cursor.Query(ctx, prompt, opts...)
	if err != nil {
		return "", "", fmt.Errorf("cursor resume Query: %w", err)
	}
	return result.SessionID, result.Text, nil
}

func cursorOpts(workdir string, t *testing.T) []cursor.SessionOption {
	stderr := func(b []byte) { t.Logf("cursor stderr: %s", strings.TrimRight(string(b), "\n")) }
	return []cursor.SessionOption{
		cursor.WithWorkDir(workdir),
		cursor.WithTrust(),
		cursor.WithForce(),
		cursor.WithStderrHandler(stderr),
	}
}

// ---------------------------------------------------------------------------
// acp (covers the gemini bridge today; future ACP-speaking agents drop in by
// constructing another acpResumeBackend)
// ---------------------------------------------------------------------------

type acpResumeBackend struct {
	name        string
	binary      string
	binaryArgs  []string
	binaryProbe string
}

func (a *acpResumeBackend) Name() string { return a.name }

func (a *acpResumeBackend) Available() (bool, string) {
	if _, err := exec.LookPath(a.binaryProbe); err != nil {
		return false, fmt.Sprintf("%s CLI not found on PATH", a.binaryProbe)
	}
	return true, ""
}

func (a *acpResumeBackend) newClient() *acp.Client {
	opts := []acp.ClientOption{
		acp.WithClientName("agent-cli-wrapper-resume-test"),
		acp.WithClientVersion("1.0.0"),
	}
	if a.binary != "" {
		opts = append(opts, acp.WithBinaryPath(a.binary))
	}
	if len(a.binaryArgs) > 0 {
		opts = append(opts, acp.WithBinaryArgs(a.binaryArgs...))
	}
	return acp.NewClient(opts...)
}

func (a *acpResumeBackend) StartFresh(ctx context.Context, t *testing.T, workdir, prompt string) (string, string, error) {
	t.Helper()
	client := a.newClient()
	if err := client.Start(ctx); err != nil {
		return "", "", fmt.Errorf("%s ACP client start: %w", a.name, err)
	}
	defer client.Stop()

	session, err := client.NewSession(ctx, acp.WithSessionCWD(workdir))
	if err != nil {
		return "", "", fmt.Errorf("%s ACP NewSession: %w", a.name, err)
	}
	result, err := session.Prompt(ctx, prompt)
	if err != nil {
		return "", "", fmt.Errorf("%s ACP Prompt: %w", a.name, err)
	}
	if !result.Success {
		return "", result.FullText, fmt.Errorf("%s ACP turn failed: %v", a.name, result.Error)
	}
	return session.ID(), result.FullText, nil
}

func (a *acpResumeBackend) Resume(ctx context.Context, t *testing.T, workdir, sessionID, prompt string) (string, string, error) {
	t.Helper()
	client := a.newClient()
	if err := client.Start(ctx); err != nil {
		return "", "", fmt.Errorf("%s ACP resume client start: %w", a.name, err)
	}
	defer client.Stop()

	session, err := client.LoadSession(ctx, sessionID, acp.WithSessionCWD(workdir))
	if err != nil {
		if errors.Is(err, acp.ErrSessionNotFound) {
			return "", "", fmt.Errorf("%s does not advertise LoadSession capability or session %s expired: %w",
				a.name, sessionID, err)
		}
		return "", "", fmt.Errorf("%s ACP LoadSession: %w", a.name, err)
	}
	result, err := session.Prompt(ctx, prompt)
	if err != nil {
		return session.ID(), "", fmt.Errorf("%s ACP resume Prompt: %w", a.name, err)
	}
	if !result.Success {
		return session.ID(), result.FullText, fmt.Errorf("%s ACP resume turn failed: %v", a.name, result.Error)
	}
	return session.ID(), result.FullText, nil
}
