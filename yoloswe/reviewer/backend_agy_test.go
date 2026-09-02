package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeAgyCLI writes an executable shell script standing in for the agy
// binary, driven purely by exit code and stdout — the same contract
// processManager.Start reads (see agent-cli-wrapper/agy/process.go). This
// exercises RunPrompt end to end (argv construction, event loop, resume
// bookkeeping) without a live agy CLI, deterministically.
func fakeAgyCLI(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\nprintf %s " + shellQuote(stdout) + "\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy CLI: %v", err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestNewAgyBackend_StopBeforeStartIsNoop(t *testing.T) {
	b := newAgyBackend(Config{BackendType: BackendAgy, Model: "gemini-3.8-flash-medium"})
	if b == nil {
		t.Fatal("expected non-nil backend")
	}
	if err := b.Stop(); err != nil {
		t.Errorf("Stop before Start should be no-op, got error: %v", err)
	}
}

func TestAgyBackend_RunPrompt_Success(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	handler := &recordingHandler{}
	result, err := b.RunPrompt(context.Background(), "review this", handler)
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true, got %+v", result)
	}
	if result.ResponseText != "AGYOK" {
		t.Errorf("ResponseText = %q, want %q", result.ResponseText, "AGYOK")
	}
	if len(handler.texts) == 0 || handler.texts[0] != "AGYOK" {
		t.Errorf("handler.texts = %v, want [AGYOK]", handler.texts)
	}
	if len(handler.turns) != 1 || !handler.turns[0] {
		t.Errorf("handler.turns = %v, want [true]", handler.turns)
	}
}

func TestAgyBackend_RunPrompt_ProcessFailure(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "", 1),
	}

	handler := &recordingHandler{}
	result, err := b.RunPrompt(context.Background(), "review this", handler)
	if err == nil {
		t.Fatal("expected error from failing agy process")
	}
	if result == nil {
		t.Fatal("expected non-nil result carrying the error")
	}
	if result.Success {
		t.Errorf("expected success=false, got %+v", result)
	}
	if len(handler.errors) == 0 {
		t.Errorf("expected handler.OnError to be called, got %v", handler.errors)
	}
}

func TestAgyBackend_RunPrompt_ResumeStatusOKOnSuccess(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", ResumeSessionID: "conv-123"},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	result, err := b.RunPrompt(context.Background(), "continue review", &recordingHandler{})
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if result.ResumeStatus != ResumeStatusOK {
		t.Errorf("ResumeStatus = %q, want %q", result.ResumeStatus, ResumeStatusOK)
	}
}

func TestAgyBackend_RunPrompt_ResumeStatusUnverifiedOnFailure(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", ResumeSessionID: "conv-123"},
		cliPath: fakeAgyCLI(t, "", 1),
	}

	result, err := b.RunPrompt(context.Background(), "continue review", &recordingHandler{})
	if err == nil {
		t.Fatal("expected error from failing agy process")
	}
	if result.ResumeStatus != ResumeStatusUnverified {
		t.Errorf("ResumeStatus = %q, want %q", result.ResumeStatus, ResumeStatusUnverified)
	}
}

func TestAgyBackend_RunPrompt_NoResumeLeavesStatusEmpty(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	result, err := b.RunPrompt(context.Background(), "review this", &recordingHandler{})
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if result.ResumeStatus != "" {
		t.Errorf("ResumeStatus = %q, want empty (no resume requested)", result.ResumeStatus)
	}
}

func TestAgyBackend_RunPrompt_NoToolEvents(t *testing.T) {
	// agy's print-mode wire format has no tool-call events at all (see the
	// agyBackend doc comment) — RunPrompt must never call
	// OnToolStart/OnToolComplete, unlike the gemini/ACP backend it replaced.
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	handler := &recordingHandler{}
	if _, err := b.RunPrompt(context.Background(), "review this", handler); err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if len(handler.toolStarts) != 0 || len(handler.toolEnds) != 0 {
		t.Errorf("expected no tool events, got starts=%v ends=%v", handler.toolStarts, handler.toolEnds)
	}
}
