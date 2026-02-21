//go:build integration
// +build integration

// Bramble-like Gemini session integration tests.
// These tests exercise the same provider creation path that bramble/session/manager.go
// uses for Gemini sessions, with full protocol message capture for debugging.
//
// Run manually with:
//   bazel build //multiagent/agent/integration:integration_test
//   ./bazel-bin/multiagent/agent/integration/integration_test_/integration_test \
//     -test.v -test.run "TestGemini_" -test.timeout 120s

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const geminiTestModel = "gemini-2.5-flash"

// geminiTestClientOpts returns the standard client options for Gemini session tests,
// mirroring how bramble/session/manager.go constructs its Gemini provider.
// The protocol logger writes to a temp file; its path is returned for dumping on failure.
func geminiTestClientOpts(t *testing.T) ([]acp.ClientOption, string) {
	t.Helper()

	protocolLog := filepath.Join(t.TempDir(), "gemini.protocol.jsonl")
	f, err := os.Create(protocolLog)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })

	opts := []acp.ClientOption{
		acp.WithBinaryArgs("--experimental-acp", "--model", geminiTestModel),
		acp.WithStderrHandler(func(data []byte) {
			t.Logf("[gemini stderr] %s", string(data))
		}),
		acp.WithProtocolLogger(f),
	}

	return opts, protocolLog
}

// dumpProtocolLog reads and logs the protocol log file contents on test failure.
func dumpProtocolLog(t *testing.T, path string) {
	t.Helper()
	if !t.Failed() {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("Failed to read protocol log %s: %v", path, err)
		return
	}
	t.Logf("=== Protocol log (%s) ===\n%s\n=== End protocol log ===", path, string(data))
}

// TestGemini_BuilderSession_BasicPrompt creates a GeminiLongRunningProvider the
// same way manager.go does for builder sessions and verifies basic prompt/response.
func TestGemini_BuilderSession_BasicPrompt(t *testing.T) {
	skipIfBinaryMissing(t, "gemini")

	tmpDir := t.TempDir()
	clientOpts, protocolLog := geminiTestClientOpts(t)
	defer dumpProtocolLog(t, protocolLog)

	// Create provider exactly as manager.go:783-810 does for builder sessions.
	// Builder sessions use the default BypassPermissionHandler (auto-approve all).
	provider := agent.NewGeminiLongRunningProvider(
		clientOpts,
		acp.WithSessionCWD(tmpDir),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	require.NoError(t, err, "Start should succeed")

	handler := &recordingEventHandler{}

	// Collect events from the provider channel
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for ev := range provider.Events() {
			switch e := ev.(type) {
			case agent.TextAgentEvent:
				handler.OnText(e.Text)
			case agent.ThinkingAgentEvent:
				handler.OnThinking(e.Thinking)
			case agent.ToolStartAgentEvent:
				handler.OnToolStart(e.Name, e.ID, e.Input)
			case agent.ToolCompleteAgentEvent:
				handler.OnToolComplete(e.Name, e.ID, e.Input, e.Result, e.IsError)
			case agent.TurnCompleteAgentEvent:
				handler.OnTurnComplete(e.TurnNumber, e.Success, e.DurationMs, e.CostUSD)
			case agent.ErrorAgentEvent:
				handler.OnError(e.Err, e.Context)
			}
		}
	}()

	result, err := provider.SendMessage(ctx, "Reply with exactly the text: HELLO WORLD. Do not use any tools.")
	require.NoError(t, err, "SendMessage should succeed")
	require.NotNil(t, result, "result should not be nil")

	t.Logf("Result text: %q", result.Text)
	t.Logf("Result success: %v", result.Success)
	t.Logf("Result thinking: %q", result.Thinking)

	assert.True(t, result.Success, "result.Success should be true")
	assert.Contains(t, strings.ToUpper(result.Text), "HELLO", "response should contain HELLO")

	err = provider.Stop()
	assert.NoError(t, err, "Stop should succeed")

	err = provider.Close()
	assert.NoError(t, err, "Close should succeed")

	// Wait for event bridge to drain
	select {
	case <-eventsDone:
	case <-time.After(5 * time.Second):
		t.Log("WARNING: event bridge did not drain in time")
	}

	// Verify events were received
	handler.mu.Lock()
	defer handler.mu.Unlock()
	t.Logf("Events: text=%d, thinking=%d, toolStarts=%d, turnCompletes=%d, errors=%d",
		len(handler.textCalls), len(handler.thinkingCalls), len(handler.toolStarts),
		len(handler.turnCompletes), len(handler.errors))

	assert.Greater(t, len(handler.textCalls), 0, "should have received text events")
}

// TestGemini_PlannerSession_ReadOnly creates provider same as manager does for
// planner sessions (with PlanOnlyPermissionHandler) and verifies read-only behavior.
func TestGemini_PlannerSession_ReadOnly(t *testing.T) {
	skipIfBinaryMissing(t, "gemini")

	tmpDir := t.TempDir()
	clientOpts, protocolLog := geminiTestClientOpts(t)
	defer dumpProtocolLog(t, protocolLog)

	// Create a test file to read
	testFile := filepath.Join(tmpDir, "readme.txt")
	err := os.WriteFile(testFile, []byte("This is a test readme file for planning."), 0644)
	require.NoError(t, err)

	// Add PlanOnlyPermissionHandler exactly as manager.go does for planner sessions
	clientOpts = append(clientOpts, acp.WithPermissionHandler(&acp.PlanOnlyPermissionHandler{}))

	provider := agent.NewGeminiLongRunningProvider(
		clientOpts,
		acp.WithSessionCWD(tmpDir),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	err = provider.Start(ctx)
	require.NoError(t, err, "Start should succeed")

	// Turn 1: Read a file (should be allowed)
	result, err := provider.SendMessage(ctx, "Read the file readme.txt and summarize its contents. Do not create or modify any files.")
	require.NoError(t, err, "SendMessage for read should succeed")
	require.NotNil(t, result)
	t.Logf("Read result: %q", result.Text)

	// Turn 2: Try to write a file (should be rejected by PlanOnlyPermissionHandler)
	writeResult, err := provider.SendMessage(ctx, "Create a file called output.txt with the text 'hello world'.")
	require.NoError(t, err, "SendMessage for write should not error")
	require.NotNil(t, writeResult)
	t.Logf("Write result: %q", writeResult.Text)

	err = provider.Stop()
	assert.NoError(t, err)
	err = provider.Close()
	assert.NoError(t, err)

	// Verify that the output file was NOT created
	_, statErr := os.Stat(filepath.Join(tmpDir, "output.txt"))
	assert.True(t, os.IsNotExist(statErr), "output.txt should not have been created by planner session")
}

// TestGemini_BuilderSession_MultiTurn verifies multi-turn context is maintained
// in a long-running session, using the exact bramble session setup path.
func TestGemini_BuilderSession_MultiTurn(t *testing.T) {
	skipIfBinaryMissing(t, "gemini")

	tmpDir := t.TempDir()
	clientOpts, protocolLog := geminiTestClientOpts(t)
	defer dumpProtocolLog(t, protocolLog)

	provider := agent.NewGeminiLongRunningProvider(
		clientOpts,
		acp.WithSessionCWD(tmpDir),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	require.NoError(t, err, "Start should succeed")

	// Turn 1: establish context
	result1, err := provider.SendMessage(ctx, "Remember this secret code: MANGO42. Just acknowledge it.")
	require.NoError(t, err, "Turn 1 should succeed")
	require.NotNil(t, result1)
	t.Logf("Turn 1 text: %q", result1.Text)
	assert.True(t, result1.Success, "Turn 1 should succeed")

	// Turn 2: verify context is maintained
	result2, err := provider.SendMessage(ctx, "What was the secret code I told you? Reply with just the code.")
	require.NoError(t, err, "Turn 2 should succeed")
	require.NotNil(t, result2)
	t.Logf("Turn 2 text: %q", result2.Text)
	assert.True(t, result2.Success, "Turn 2 should succeed")
	assert.Contains(t, strings.ToUpper(result2.Text), "MANGO42",
		"response should contain the secret code from turn 1")

	err = provider.Stop()
	assert.NoError(t, err)
	err = provider.Close()
	assert.NoError(t, err)
}

// TestGemini_BuilderSession_FileWrite creates a builder session with bypass
// permissions that actually writes a file, capturing protocol messages.
func TestGemini_BuilderSession_FileWrite(t *testing.T) {
	skipIfBinaryMissing(t, "gemini")

	tmpDir := t.TempDir()
	clientOpts, protocolLog := geminiTestClientOpts(t)
	defer dumpProtocolLog(t, protocolLog)

	handler := &recordingEventHandler{}

	provider := agent.NewGeminiLongRunningProvider(
		clientOpts,
		acp.WithSessionCWD(tmpDir),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	err := provider.Start(ctx)
	require.NoError(t, err, "Start should succeed")

	// Collect events in background
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for ev := range provider.Events() {
			switch e := ev.(type) {
			case agent.TextAgentEvent:
				handler.OnText(e.Text)
			case agent.ThinkingAgentEvent:
				handler.OnThinking(e.Thinking)
			case agent.ToolStartAgentEvent:
				handler.OnToolStart(e.Name, e.ID, e.Input)
			case agent.ToolCompleteAgentEvent:
				handler.OnToolComplete(e.Name, e.ID, e.Input, e.Result, e.IsError)
			case agent.TurnCompleteAgentEvent:
				handler.OnTurnComplete(e.TurnNumber, e.Success, e.DurationMs, e.CostUSD)
			case agent.ErrorAgentEvent:
				handler.OnError(e.Err, e.Context)
			}
		}
	}()

	targetFile := filepath.Join(tmpDir, "gemini_test_output.txt")
	prompt := "Create a file at the exact path " + targetFile + " containing the text 'bramble test content'. Use a file writing tool to create it."

	result, err := provider.SendMessage(ctx, prompt)
	require.NoError(t, err, "SendMessage should succeed")
	require.NotNil(t, result)

	t.Logf("Result text: %q", result.Text)
	t.Logf("Result success: %v", result.Success)

	assert.True(t, result.Success, "result should be successful")

	err = provider.Stop()
	assert.NoError(t, err)
	err = provider.Close()
	assert.NoError(t, err)

	select {
	case <-eventsDone:
	case <-time.After(5 * time.Second):
	}

	// Verify file was created
	content, err := os.ReadFile(targetFile)
	assert.NoError(t, err, "file should have been created at %s", targetFile)
	if err == nil {
		assert.Contains(t, string(content), "bramble test content",
			"file should contain expected content")
	}

	// Log event summary
	handler.mu.Lock()
	defer handler.mu.Unlock()
	t.Logf("Events: text=%d, thinking=%d, toolStarts=%d, toolCompletes=%d, turnCompletes=%d, errors=%d",
		len(handler.textCalls), len(handler.thinkingCalls), len(handler.toolStarts),
		len(handler.toolCompletes), len(handler.turnCompletes), len(handler.errors))

	for _, ts := range handler.toolStarts {
		t.Logf("  ToolStart: name=%s id=%s input=%v", ts.Name, ts.ID, ts.Input)
	}
	for _, tc := range handler.toolCompletes {
		t.Logf("  ToolComplete: name=%s id=%s isError=%v", tc.Name, tc.ID, tc.IsError)
	}
}
