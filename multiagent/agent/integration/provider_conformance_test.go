//go:build integration
// +build integration

// Provider conformance tests verify that all agent providers (Claude, Codex, Agy)
// are interchangeable when plugged into the same consumer paths. Tests run with real
// CLI binaries and require network access + valid API keys.
//
// Run with:
//   bazel test //multiagent/agent/integration:integration_test --test_timeout=600
//
// Or build and run the binary directly:
//   bazel build //multiagent/agent/integration:integration_test
//   ./bazel-bin/multiagent/agent/integration/integration_test_/integration_test -test.v -test.run TestConformance_BasicPrompt

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/agy"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Infrastructure
// ============================================================================

// Note: Agy tests do NOT use t.Parallel() to avoid API rate limiting.
// Since shards are separate processes, we can't use a mutex. Instead, we
// conditionally call t.Parallel() only for non-Agy providers.

// agentTurnEvents collects events from a single provider execution.
type agentTurnEvents struct {
	TextEvents     []agent.TextAgentEvent
	ThinkingEvents []agent.ThinkingAgentEvent
	ToolStarts     []agent.ToolStartAgentEvent
	ToolCompletes  []agent.ToolCompleteAgentEvent
	TurnComplete   *agent.TurnCompleteAgentEvent
	Errors         []agent.ErrorAgentEvent
}

// collectAgentEvents reads from provider.Events() until TurnCompleteAgentEvent or context timeout.
func collectAgentEvents(ctx context.Context, events <-chan agent.AgentEvent) (*agentTurnEvents, error) {
	result := &agentTurnEvents{}
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return result, nil
			}
			switch e := ev.(type) {
			case agent.TextAgentEvent:
				result.TextEvents = append(result.TextEvents, e)
			case agent.ThinkingAgentEvent:
				result.ThinkingEvents = append(result.ThinkingEvents, e)
			case agent.ToolStartAgentEvent:
				result.ToolStarts = append(result.ToolStarts, e)
			case agent.ToolCompleteAgentEvent:
				result.ToolCompletes = append(result.ToolCompletes, e)
			case agent.TurnCompleteAgentEvent:
				result.TurnComplete = &e
				return result, nil
			case agent.ErrorAgentEvent:
				result.Errors = append(result.Errors, e)
			}
		}
	}
}

// recordingEventHandler implements agent.EventHandler and records all callbacks.
type recordingEventHandler struct {
	mu            sync.Mutex
	textCalls     []string
	thinkingCalls []string
	toolStarts    []toolStartCall
	toolCompletes []toolCompleteCall
	turnCompletes []turnCompleteCall
	errors        []errorCall
}

type toolStartCall struct {
	Name  string
	ID    string
	Input map[string]interface{}
}

type toolCompleteCall struct {
	Name    string
	ID      string
	Input   map[string]interface{}
	IsError bool
}

type turnCompleteCall struct {
	TurnNumber int
	Success    bool
	DurationMs int64
	CostUSD    float64
}

type errorCall struct {
	Err     error
	Context string
}

func (h *recordingEventHandler) OnText(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.textCalls = append(h.textCalls, text)
}

func (h *recordingEventHandler) OnThinking(thinking string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.thinkingCalls = append(h.thinkingCalls, thinking)
}

func (h *recordingEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toolStarts = append(h.toolStarts, toolStartCall{Name: name, ID: id, Input: input})
}

func (h *recordingEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toolCompletes = append(h.toolCompletes, toolCompleteCall{Name: name, ID: id, Input: input, IsError: isError})
}

func (h *recordingEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turnCompletes = append(h.turnCompletes, turnCompleteCall{
		TurnNumber: turnNumber,
		Success:    success,
		DurationMs: durationMs,
		CostUSD:    costUSD,
	})
}

func (h *recordingEventHandler) OnError(err error, ctx string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errors = append(h.errors, errorCall{Err: err, Context: ctx})
}

// providerFactory creates providers for each backend.
type providerFactory struct {
	name      string
	binary    string // binary to check in PATH
	hasEvents bool
	// hasToolEvents is true only for providers whose Events()/EventHandler
	// stream ToolStart/ToolComplete. Agy's wrapper (agent-cli-wrapper/agy)
	// is a print-mode CLI wrapper that surfaces only TextEvent,
	// TurnCompleteEvent, and ErrorEvent — no tool-call events exist to
	// bridge, so AgyProvider never emits ToolStartAgentEvent/
	// ToolCompleteAgentEvent. hasEvents alone (text/turn-complete streaming)
	// is still true for agy; this flag is the finer-grained one tool-event
	// assertions must gate on instead.
	hasToolEvents  bool
	newProvider    func(t *testing.T, tmpDir string) agent.Provider
	newLongRunning func(t *testing.T, tmpDir string) agent.LongRunningProvider
}

func allFactories() []providerFactory {
	return []providerFactory{
		{
			name:          "claude",
			binary:        "claude",
			hasEvents:     true,
			hasToolEvents: true,
			newProvider: func(t *testing.T, tmpDir string) agent.Provider {
				return agent.NewClaudeProvider(
					claude.WithModel("haiku"),
					claude.WithPermissionMode(claude.PermissionModeBypass),
				)
			},
			newLongRunning: func(t *testing.T, tmpDir string) agent.LongRunningProvider {
				return agent.NewClaudeLongRunningProvider(
					claude.WithModel("haiku"),
					claude.WithWorkDir(tmpDir),
					claude.WithPermissionMode(claude.PermissionModeBypass),
					claude.WithDisablePlugins(),
				)
			},
		},
		{
			name:      "codex",
			binary:    "codex",
			hasEvents: false,
			newProvider: func(t *testing.T, tmpDir string) agent.Provider {
				return agent.NewCodexProvider()
			},
			newLongRunning: nil, // Codex doesn't implement LongRunningProvider
		},
		{
			name:      "agy",
			binary:    "agy",
			hasEvents: true, // streams TextAgentEvent/TurnCompleteAgentEvent
			// hasToolEvents left false: see the field doc above.
			newProvider: func(t *testing.T, tmpDir string) agent.Provider {
				return agent.NewAgyProvider(
					agy.WithModel("gemini-3.8-flash-low"),
					agy.WithStderrHandler(func(data []byte) {
						t.Logf("[agy stderr] %s", string(data))
					}),
				)
			},
			newLongRunning: nil, // AgyProvider doesn't implement LongRunningProvider
		},
	}
}

// skipIfBinaryMissing skips the test if the provider's binary is not in PATH.
func skipIfBinaryMissing(t *testing.T, binary string) {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("binary %q not found in PATH, skipping", binary)
	}
}

// parallelIfNotAgy enables parallel execution for non-Agy tests.
// Agy tests run sequentially to avoid API rate limiting.
func parallelIfNotAgy(t *testing.T, providerName string) {
	t.Helper()
	if providerName != "agy" {
		t.Parallel()
	}
}

// ============================================================================
// Test 1: BasicPrompt — all 3 providers
// ============================================================================

func TestConformance_BasicPrompt(t *testing.T) {
	for _, f := range allFactories() {
		f := f
		t.Run(f.name, func(t *testing.T) {
			parallelIfNotAgy(t, f.name)
			skipIfBinaryMissing(t, f.binary)

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			tmpDir := t.TempDir()
			provider := f.newProvider(t, tmpDir)
			defer provider.Close()

			result, err := provider.Execute(ctx, "Reply with exactly the text: HELLO WORLD. Do not use any tools.", nil,
				agent.WithProviderWorkDir(tmpDir),
			)
			require.NoError(t, err, "Execute should succeed")
			require.NotNil(t, result, "result should not be nil")

			assert.True(t, result.Success, "result.Success should be true")
			assert.Contains(t, strings.ToUpper(result.Text), "HELLO", "response should contain HELLO")
			assert.Greater(t, result.DurationMs, int64(0), "DurationMs should be positive")

			// Verify usage follows expected per-provider pattern
			switch f.name {
			case "claude":
				assert.Greater(t, result.Usage.CostUSD, float64(0), "Claude should report cost")
				assert.Greater(t, result.Usage.InputTokens, 0, "Claude should report input tokens")
				assert.Greater(t, result.Usage.OutputTokens, 0, "Claude should report output tokens")
			case "codex":
				// Codex may or may not report token usage depending on whether
				// the binary emits token_count events. CostUSD is always 0.
				t.Logf("Codex usage: input=%d, output=%d", result.Usage.InputTokens, result.Usage.OutputTokens)
				assert.Equal(t, float64(0), result.Usage.CostUSD, "Codex should not report cost")
			case "agy":
				// AgyProvider maps agy's JSON usage object onto AgentUsage, so a
				// live turn reports real token counts. CostUSD stays 0: agy's
				// result carries no cost, and inventing one from a pricing table
				// would be worse than reporting none (same call as codex).
				assert.Positive(t, result.Usage.InputTokens, "Agy should report input tokens")
				assert.Positive(t, result.Usage.OutputTokens, "Agy should report output tokens")
				assert.Equal(t, float64(0), result.Usage.CostUSD, "Agy should not report cost")
			}
		})
	}
}

// ============================================================================
// Test 2: EventsStreamDuringExecution — Claude and Agy
// ============================================================================

func TestConformance_EventsStreamDuringExecution(t *testing.T) {
	for _, f := range allFactories() {
		f := f
		if !f.hasEvents {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			parallelIfNotAgy(t, f.name)
			skipIfBinaryMissing(t, f.binary)

			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			tmpDir := t.TempDir()
			provider := f.newProvider(t, tmpDir)
			defer provider.Close()

			handler := &recordingEventHandler{}

			// Use a prompt that triggers tool use (file creation)
			targetFile := filepath.Join(tmpDir, "test_conformance.txt")
			prompt := fmt.Sprintf(
				"Create a file at the exact path %s containing the text 'hello world'. "+
					"Use a file writing tool to create it.", targetFile)

			// Run execute in a goroutine while collecting events
			var execResult *agent.AgentResult
			var execErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				execResult, execErr = provider.Execute(ctx, prompt, nil,
					agent.WithProviderWorkDir(tmpDir),
					agent.WithProviderEventHandler(handler),
				)
			}()

			// Collect events from the channel
			events, evErr := collectAgentEvents(ctx, provider.Events())

			// Wait for execute to finish
			<-done

			require.NoError(t, execErr, "Execute should succeed")
			require.NotNil(t, execResult, "result should not be nil")
			assert.True(t, execResult.Success, "result.Success should be true")

			// Verify events from channel
			if evErr == nil {
				assert.NotNil(t, events.TurnComplete, "should receive TurnComplete event")
				assert.True(t, len(events.TextEvents) > 0 || events.TurnComplete != nil,
					"should receive at least text events or turn complete")
			}

			// Verify EventHandler callbacks
			handler.mu.Lock()
			defer handler.mu.Unlock()
			assert.Greater(t, len(handler.textCalls), 0, "OnText should have been called")
			assert.Greater(t, len(handler.turnCompletes), 0, "OnTurnComplete should have been called")
			if len(handler.turnCompletes) > 0 {
				assert.True(t, handler.turnCompletes[0].Success, "turn should be successful")
			}

			// Verify tool events were emitted (providers whose wrapper
			// exposes tool-call events only — see hasToolEvents doc).
			if f.hasToolEvents {
				assert.Greater(t, len(handler.toolStarts), 0, "OnToolStart should have been called (file write tool)")
			}

			// Verify file was created
			_, err := os.Stat(targetFile)
			assert.NoError(t, err, "file should have been created at %s", targetFile)
		})
	}
}

// ============================================================================
// Test 3: LongRunningMultiTurn — Claude and Agy
// ============================================================================

func TestConformance_LongRunningMultiTurn(t *testing.T) {
	for _, f := range allFactories() {
		f := f
		if f.newLongRunning == nil {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			parallelIfNotAgy(t, f.name)
			skipIfBinaryMissing(t, f.binary)

			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			tmpDir := t.TempDir()
			provider := f.newLongRunning(t, tmpDir)
			defer provider.Close()

			err := provider.Start(ctx)
			require.NoError(t, err, "Start should succeed")

			// Turn 1: establish context
			result1, err := provider.SendMessage(ctx, "Remember this: the magic word is BANANA. Just acknowledge that you remember it.")
			require.NoError(t, err, "first SendMessage should succeed")
			require.NotNil(t, result1)
			assert.True(t, result1.Success, "first turn should succeed")

			// Turn 2: verify context is maintained
			result2, err := provider.SendMessage(ctx, "What is the magic word I told you? Reply with just the word.")
			require.NoError(t, err, "second SendMessage should succeed")
			require.NotNil(t, result2)
			assert.True(t, result2.Success, "second turn should succeed")
			assert.Contains(t, strings.ToUpper(result2.Text), "BANANA",
				"response should contain the magic word from the first turn")

			err = provider.Stop()
			assert.NoError(t, err, "Stop should succeed")
		})
	}
}

// ============================================================================
// Test 4: PermissionCallback — Claude only (Agy has no permission-callback API;
// it only exposes coarse --dangerously-skip-permissions / --sandbox flags via
// agy.WithDangerouslySkipPermissions / agy.WithSandbox, no per-tool callback)
// ============================================================================

func TestConformance_PermissionCallback(t *testing.T) {
	t.Parallel()
	skipIfBinaryMissing(t, "claude")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	var mu sync.Mutex
	var claudeReqs []claude.PermissionRequest
	handler := claude.PermissionHandlerFunc(func(ctx context.Context, req *claude.PermissionRequest) (*claude.PermissionResponse, error) {
		mu.Lock()
		claudeReqs = append(claudeReqs, *req)
		mu.Unlock()
		t.Logf("Claude permission requested: tool=%s", req.ToolName)
		return &claude.PermissionResponse{Behavior: claude.PermissionAllow}, nil
	})
	provider := agent.NewClaudeProvider(
		claude.WithModel("haiku"),
		claude.WithPermissionHandler(handler),
		claude.WithPermissionPromptToolStdio(),
	)
	defer provider.Close()

	targetFile := filepath.Join(tmpDir, "permission_test.txt")
	prompt := fmt.Sprintf(
		"Create a file at the exact path %s containing the text 'test content'. "+
			"Use a file writing tool to create it.", targetFile)

	// Don't pass bypass permission mode — we want default so permissions fire.
	result, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmpDir),
		agent.WithProviderPermissionMode("default"),
	)
	require.NoError(t, err, "Execute should succeed")
	require.NotNil(t, result)
	assert.True(t, result.Success, "result.Success should be true")

	// At least one request should have a tool name
	hasToolName := false
	for _, req := range claudeReqs {
		if req.ToolName != "" {
			hasToolName = true
			t.Logf("Claude permission: tool=%s, input keys=%v", req.ToolName, mapKeys(req.Input))
		}
	}
	assert.True(t, hasToolName, "at least one permission request should have a tool name")

	// Verify file was created (permission was granted)
	_, err = os.Stat(targetFile)
	assert.NoError(t, err, "file should have been created (permission was granted)")
}

// ============================================================================
// Test 5: ContextCancellation — all 3 providers
// ============================================================================

func TestConformance_ContextCancellation(t *testing.T) {
	for _, f := range allFactories() {
		f := f
		t.Run(f.name, func(t *testing.T) {
			parallelIfNotAgy(t, f.name)
			skipIfBinaryMissing(t, f.binary)

			tmpDir := t.TempDir()
			provider := f.newProvider(t, tmpDir)

			// Use a very short timeout
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			prompt := "Write a very long essay about the history of computing from the 1800s to today. " +
				"Cover every decade in detail with at least 500 words per decade."
			_, err := provider.Execute(ctx, prompt, nil,
				agent.WithProviderWorkDir(tmpDir),
			)

			// Should either error or the result may have completed very quickly.
			// The key assertion is that Close() doesn't hang.
			if err != nil {
				t.Logf("Execute returned error as expected: %v", err)
			}

			closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer closeCancel()

			closeDone := make(chan error, 1)
			go func() {
				closeDone <- provider.Close()
			}()

			select {
			case err := <-closeDone:
				assert.NoError(t, err, "Close should not error")
			case <-closeCtx.Done():
				t.Fatal("Close() hung after context cancellation")
			}
		})
	}
}

// ============================================================================
// Test 6: ErrorOnInvalidWorkDir — all 3 providers
// ============================================================================

func TestConformance_ErrorOnInvalidWorkDir(t *testing.T) {
	// Test that providers handle invalid work directories gracefully.
	// Claude and Agy return errors; Codex may not (it handles work dirs differently).
	for _, f := range allFactories() {
		f := f
		if f.name == "codex" {
			continue // Codex does not error on invalid work dir
		}
		t.Run(f.name, func(t *testing.T) {
			parallelIfNotAgy(t, f.name)
			skipIfBinaryMissing(t, f.binary)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			tmpDir := t.TempDir()
			provider := f.newProvider(t, tmpDir)
			defer provider.Close()

			_, err := provider.Execute(ctx, "hello", nil,
				agent.WithProviderWorkDir("/nonexistent/path/that/should/not/exist"),
			)
			assert.Error(t, err, "Execute with invalid work dir should error")
		})
	}
}

// ============================================================================
// Test 7: FileToolTracking — providers with tool-call events (Claude only;
// Agy's wrapper has no tool events, see hasToolEvents doc)
// ============================================================================

func TestConformance_FileToolTracking(t *testing.T) {
	for _, f := range allFactories() {
		f := f
		if !f.hasToolEvents {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			parallelIfNotAgy(t, f.name)
			skipIfBinaryMissing(t, f.binary)

			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			tmpDir := t.TempDir()
			provider := f.newProvider(t, tmpDir)
			defer provider.Close()

			handler := &recordingEventHandler{}
			targetFile := filepath.Join(tmpDir, "tracked_file.txt")
			prompt := fmt.Sprintf(
				"Create a file at the exact path %s containing 'tracked content'. "+
					"Use a file writing tool.", targetFile)

			var execResult *agent.AgentResult
			var execErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				execResult, execErr = provider.Execute(ctx, prompt, nil,
					agent.WithProviderWorkDir(tmpDir),
					agent.WithProviderEventHandler(handler),
				)
			}()

			// Collect events
			collectAgentEvents(ctx, provider.Events())
			<-done

			require.NoError(t, execErr, "Execute should succeed")
			require.NotNil(t, execResult)
			assert.True(t, execResult.Success, "result.Success should be true")

			// Check tool events for file_path in Input.
			// For Claude, this shows up in tool complete events.
			handler.mu.Lock()
			defer handler.mu.Unlock()

			foundFilePath := false
			for _, tc := range handler.toolCompletes {
				if tc.Input != nil {
					for key, val := range tc.Input {
						if (key == "file_path" || key == "path") && val != nil {
							foundFilePath = true
							t.Logf("ToolComplete %s has %s=%v", tc.Name, key, val)
						}
					}
				}
			}
			for _, ts := range handler.toolStarts {
				if ts.Input != nil {
					for key, val := range ts.Input {
						if (key == "file_path" || key == "path") && val != nil {
							foundFilePath = true
							t.Logf("ToolStart %s has %s=%v", ts.Name, key, val)
						}
					}
				}
			}
			assert.True(t, foundFilePath,
				"at least one tool event should have file_path or path in Input")

			// Verify file on disk
			_, err := os.Stat(targetFile)
			assert.NoError(t, err, "file should have been created at %s", targetFile)
		})
	}
}

// ============================================================================
// Helpers
// ============================================================================

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
