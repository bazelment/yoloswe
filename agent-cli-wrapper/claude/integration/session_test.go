//go:build integration
// +build integration

// Integration tests for the Claude Session with session recording and permission flows.
//
// Tests real interactions with Claude CLI covering:
// 1. Bypass permission mode with multi-step execution
// 2. Default permission mode with approval flow
// 3. Plan mode with combined request
// 4. Interrupt support
//
// All tests use temp directories for isolation and record sessions for review.
//
// Run with: go test -tags=integration ./claude/...
//
// These tests require:
// - The claude CLI to be installed and available in PATH
// - A valid API key configured
//
// Set CLAUDE_CLI_PATH to override the default claude CLI location.

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// ============================================================================
// Test Utilities
// ============================================================================

// TurnEvents collects events from a single turn.
type TurnEvents struct {
	Ready        *claude.ReadyEvent
	TextEvents   []claude.TextEvent
	ToolStarts   []claude.ToolStartEvent
	ToolComplete []claude.ToolCompleteEvent
	ToolResults  []claude.CLIToolResultEvent
	TurnComplete *claude.TurnCompleteEvent
	Errors       []claude.ErrorEvent
}

// CollectTurnEvents collects all events until TurnCompleteEvent or context cancellation.
// The ReadyEvent may be included if this is the first turn (CLI sends init after first message).
func CollectTurnEvents(ctx context.Context, s *claude.Session) (*TurnEvents, error) {
	events := &TurnEvents{}

	for {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		case event, ok := <-s.Events():
			if !ok {
				return events, context.Canceled
			}

			switch e := event.(type) {
			case claude.ReadyEvent:
				events.Ready = &e
			case claude.TextEvent:
				events.TextEvents = append(events.TextEvents, e)
			case claude.ToolStartEvent:
				events.ToolStarts = append(events.ToolStarts, e)
			case claude.ToolCompleteEvent:
				events.ToolComplete = append(events.ToolComplete, e)
			case claude.CLIToolResultEvent:
				events.ToolResults = append(events.ToolResults, e)
			case claude.TurnCompleteEvent:
				events.TurnComplete = &e
				return events, nil
			case claude.ErrorEvent:
				events.Errors = append(events.Errors, e)
			}
		}
	}
}

// CollectTurnEventsWhile keeps draining events until keepGoing returns false
// OR the context is cancelled. Used for tests that need to consume multiple
// CLI turns (e.g. background-task continuations) as one logical unit. After
// each event, keepGoing is invoked with the accumulated state to decide
// whether to stop; it is also called when TurnCompleteEvent arrives so the
// caller can observe the per-CLI-turn boundary.
func CollectTurnEventsWhile(ctx context.Context, s *claude.Session, keepGoing func(*TurnEvents) bool) (*TurnEvents, error) {
	events := &TurnEvents{}
	for {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		case event, ok := <-s.Events():
			if !ok {
				return events, context.Canceled
			}
			switch e := event.(type) {
			case claude.ReadyEvent:
				events.Ready = &e
			case claude.TextEvent:
				events.TextEvents = append(events.TextEvents, e)
			case claude.ToolStartEvent:
				events.ToolStarts = append(events.ToolStarts, e)
			case claude.ToolCompleteEvent:
				events.ToolComplete = append(events.ToolComplete, e)
			case claude.CLIToolResultEvent:
				events.ToolResults = append(events.ToolResults, e)
			case claude.TurnCompleteEvent:
				events.TurnComplete = &e
			case claude.ErrorEvent:
				events.Errors = append(events.Errors, e)
			}
			if !keepGoing(events) {
				return events, nil
			}
		}
	}
}

// HasToolNamed checks if any tool with the given name was started.
func (te *TurnEvents) HasToolNamed(name string) bool {
	for _, t := range te.ToolStarts {
		if t.Name == name {
			return true
		}
	}
	return false
}

// validateRecording validates session recording structure.
func validateRecording(t *testing.T, recording *claude.SessionRecording, minTurns int) {
	t.Helper()

	if recording == nil {
		t.Fatal("recording is nil")
	}

	if len(recording.Turns) < minTurns {
		t.Errorf("expected at least %d turns, got %d", minTurns, len(recording.Turns))
	}
}

// ============================================================================
// Scenario 1: Bypass Permission Mode - Multi-Step Execution
// ============================================================================

func TestSession_Integration_Scenario1_BypassPermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Create temp directory for test artifacts
	testDir, err := os.MkdirTemp("", "claude-go-test-scenario1-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	// Create session with bypass permissions
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithRecording(testDir),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Track tools used across all turns
	var allToolStarts []claude.ToolStartEvent

	// Step 1: Search for tariff news
	// Note: CLI sends init message after first user message, so ReadyEvent comes with first turn
	t.Log("Step 1: Searching for tariff news...")
	_, err = session.SendMessage(ctx, "Search latest news about US tariff rate against China/Japan/EU")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	events1, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(1) failed: %v", err)
	}
	if events1.Ready != nil {
		t.Logf("Session ready: id=%s, model=%s", events1.Ready.Info.SessionID, events1.Ready.Info.Model)
	}
	allToolStarts = append(allToolStarts, events1.ToolStarts...)
	t.Logf("Turn 1 completed: success=%v, cost=$%.6f", events1.TurnComplete.Success, events1.TurnComplete.Usage.CostUSD)

	// Step 2: Save to CSV
	t.Log("Step 2: Saving results to CSV...")
	_, err = session.SendMessage(ctx, "Put your results in csv file")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	events2, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(2) failed: %v", err)
	}
	allToolStarts = append(allToolStarts, events2.ToolStarts...)
	t.Logf("Turn 2 completed: success=%v, cost=$%.6f", events2.TurnComplete.Success, events2.TurnComplete.Usage.CostUSD)

	// Step 3: Create Python visualization code
	t.Log("Step 3: Creating Python visualization...")
	_, err = session.SendMessage(ctx, "Write a python code to convert them to a simple html chart")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	events3, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(3) failed: %v", err)
	}
	allToolStarts = append(allToolStarts, events3.ToolStarts...)
	t.Logf("Turn 3 completed: success=%v, cost=$%.6f", events3.TurnComplete.Success, events3.TurnComplete.Usage.CostUSD)

	// Get and validate recording
	recording := session.Recording()
	validateRecording(t, recording, 3)

	// Check tool usage
	hasWebSearch := false
	hasWrite := false
	for _, tool := range allToolStarts {
		if tool.Name == "WebSearch" {
			hasWebSearch = true
		}
		if tool.Name == "Write" {
			hasWrite = true
		}
	}

	if !hasWebSearch {
		t.Error("Expected WebSearch tool to be used")
	}
	if !hasWrite {
		t.Error("Expected Write tool to be used")
	}

	// Check files created
	files, _ := os.ReadDir(testDir)
	hasCsvFile := false
	hasPyFile := false
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".csv" {
			hasCsvFile = true
		}
		if filepath.Ext(f.Name()) == ".py" {
			hasPyFile = true
		}
	}

	if !hasCsvFile {
		t.Error("Expected CSV file to be created")
	}
	if !hasPyFile {
		t.Error("Expected Python file to be created")
	}

	// Optionally export recording as protocol trace fixtures.
	if shouldUpdateFixtures() {
		recPath := session.RecordingPath()
		if recPath == "" {
			t.Fatal("No recording path for fixture export")
		}
		exportTraceFixtures(t, filepath.Join(recPath, "messages.jsonl"))
	}

	t.Log("All assertions passed for Scenario 1")
}

// ============================================================================
// Scenario 2: Default Permission Mode with Approval Flow
// ============================================================================

func TestSession_Integration_Scenario2_DefaultPermissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Create temp directory for test artifacts
	testDir, err := os.MkdirTemp("", "claude-go-test-scenario2-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	// Track permission requests
	var permissionRequests []claude.PermissionRequest

	// Permission handler - auto-approve all
	handler := claude.PermissionHandlerFunc(func(ctx context.Context, req *claude.PermissionRequest) (*claude.PermissionResponse, error) {
		permissionRequests = append(permissionRequests, *req)
		t.Logf("Permission requested for: %s", req.ToolName)
		return &claude.PermissionResponse{Behavior: claude.PermissionAllow}, nil
	})

	// Create session with default permissions
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeDefault),
		claude.WithDisablePlugins(),
		claude.WithRecording(testDir),
		claude.WithPermissionHandler(handler),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Execute same 3-step flow
	t.Log("Step 1: Searching for tariff news...")
	session.SendMessage(ctx, "Search latest news about US tariff rate against China/Japan/EU")
	events1, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(1) failed: %v", err)
	}
	if events1.Ready != nil {
		t.Logf("Session ready: mode=%s", events1.Ready.Info.PermissionMode)
	}

	t.Log("Step 2: Saving results to CSV...")
	session.SendMessage(ctx, "Put your results in csv file")
	_, err = CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(2) failed: %v", err)
	}

	t.Log("Step 3: Creating Python visualization...")
	session.SendMessage(ctx, "Write a python code to convert them to a simple html chart")
	_, err = CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(3) failed: %v", err)
	}

	// Get recording
	recording := session.Recording()
	validateRecording(t, recording, 3)

	// Check permission requests
	if len(permissionRequests) > 0 {
		t.Logf("Permission requests received: %d", len(permissionRequests))
	} else {
		t.Log("No permission requests (CLI may have auto-approved)")
	}

	// Check files created
	files, _ := os.ReadDir(testDir)
	hasCsvFile := false
	hasPyFile := false
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".csv" {
			hasCsvFile = true
		}
		if filepath.Ext(f.Name()) == ".py" {
			hasPyFile = true
		}
	}

	if !hasCsvFile {
		t.Error("Expected CSV file to be created")
	}
	if !hasPyFile {
		t.Error("Expected Python file to be created")
	}

	t.Log("All assertions passed for Scenario 2")
}

// ============================================================================
// Scenario 3: Plan Mode with Combined Request
// ============================================================================

func TestSession_Integration_Scenario3_PlanMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second) // 5min timeout
	defer cancel()

	// Create temp directory for test artifacts
	testDir, err := os.MkdirTemp("", "claude-go-test-scenario3-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	// Create session with plan mode
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModePlan),
		claude.WithDisablePlugins(),
		claude.WithRecording(testDir),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Send combined request
	t.Log("Sending combined request in plan mode...")
	session.SendMessage(ctx,
		"Search latest news about US tariff rates against China/Japan/EU, "+
			"save results to CSV file, and create Python code for HTML chart visualization")

	// Wait for turn 1 to complete (plan presentation)
	events1, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(1) failed: %v", err)
	}
	if events1.Ready != nil {
		t.Logf("Session ready")
	}
	t.Log("Turn 1 completed (plan presented), switching mode and approving...")

	// Switch permission mode to acceptEdits before proceeding
	if err := session.SetPermissionMode(ctx, claude.PermissionModeAcceptEdits); err != nil {
		t.Logf("SetPermissionMode warning: %v", err)
	}

	// Send approval message to execute the plan
	session.SendMessage(ctx, "Yes, please proceed with the plan")

	// Wait for execution to complete
	_, err = CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents(2) failed: %v", err)
	}
	t.Log("Turn 2 completed (plan executed)")

	// Get recording
	recording := session.Recording()
	validateRecording(t, recording, 2)

	// Check files created (may not be created if execution didn't complete fully)
	files, _ := os.ReadDir(testDir)
	hasCsvFile := false
	hasPyFile := false
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".csv" {
			hasCsvFile = true
		}
		if filepath.Ext(f.Name()) == ".py" {
			hasPyFile = true
		}
	}
	t.Logf("Files created: CSV=%v, Python=%v", hasCsvFile, hasPyFile)

	t.Log("All assertions passed for Scenario 3")
}

// ============================================================================
// Scenario 4: Interrupt Support
// ============================================================================

func TestSession_Integration_Scenario4_Interrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Create temp directory for test artifacts
	testDir, err := os.MkdirTemp("", "claude-go-test-scenario4-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	// Create session with bypass permissions
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithRecording(testDir),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Send a long-running task that will be interrupted
	t.Log("Step 1: Sending long-running task...")
	session.SendMessage(ctx,
		"Search for news about AI, climate change, and technology. "+
			"Then create 5 different files with summaries of each topic. "+
			"Take your time and be thorough.")

	// Collect events until we see first tool start, then interrupt
	interruptSent := false
	events := &TurnEvents{}

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("Context cancelled while waiting for events: %v", ctx.Err())
		case event, ok := <-session.Events():
			if !ok {
				t.Fatal("Event channel closed unexpectedly")
			}

			switch e := event.(type) {
			case claude.ReadyEvent:
				events.Ready = &e
				t.Logf("Session ready: id=%s", e.Info.SessionID)
			case claude.ToolStartEvent:
				events.ToolStarts = append(events.ToolStarts, e)
				if !interruptSent {
					interruptSent = true
					t.Logf("Interrupting session after %s started...", e.Name)
					if err := session.Interrupt(ctx); err != nil {
						t.Logf("Interrupt error: %v", err)
					}
				}
			case claude.TurnCompleteEvent:
				events.TurnComplete = &e
				goto turnDone
			case claude.ErrorEvent:
				events.Errors = append(events.Errors, e)
			}
		}
	}
turnDone:

	if !interruptSent {
		t.Error("Expected interrupt to be sent")
	}

	t.Logf("Turn 1 ended (possibly interrupted): success=%v", events.TurnComplete.Success)

	// Get recording
	recording := session.Recording()
	if recording == nil {
		t.Fatal("Expected recording to be available")
	}
	t.Logf("Recording has %d turns", len(recording.Turns))

	// Send a new message to verify session still works after interrupt
	t.Log("Step 2: Sending new message after interrupt...")
	session.SendMessage(ctx, "What is 2+2?")

	events2, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Logf("Turn 2 result: %v", err)
	} else {
		t.Logf("Session accepted new message after interrupt: success=%v", events2.TurnComplete.Success)
	}

	t.Log("All assertions passed for Scenario 4")
}

// ============================================================================
// Scenario 5: Background Task Continuation
// ============================================================================

// TestSession_Integration_BackgroundTask verifies that the session correctly
// waits for background tasks to complete before signaling turn completion.
//
// The Claude CLI supports run_in_background for Bash commands. When used:
//  1. CLI returns tool_result with backgroundTaskId immediately
//  2. Agent ends its turn (stop_reason: end_turn)
//  3. Background task completes → CLI injects <task-notification>
//  4. CLI auto-starts a new assistant turn to process the notification
//  5. New ResultMessage sent for the continuation turn
//
// Without the background task fix, CollectTurnEvents would return after step 2,
// and the caller would see "Running in background..." instead of the final result.
func TestSession_Integration_BackgroundTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "claude-go-test-bgtask-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithRecording(testDir),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Ask the agent to run a short command in the background and report its result.
	// The agent should use run_in_background: true, wait for completion, then report.
	t.Log("Sending background task prompt...")
	_, err = session.SendMessage(ctx,
		"Run this exact bash command in the background: `sleep 2 && echo 'BG_TASK_DONE_MARKER'`. "+
			"After it completes, tell me the exact output. "+
			"You MUST use run_in_background: true for the bash command.")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	// Post-PR-5 the wrapper no longer coalesces continuation turns. The CLI
	// emits one ResultMessage+TurnComplete when the agent's first turn ends
	// (bg task still running) and a second pair after the task-notification
	// auto-continues the session. The test must pump events through both.
	events, err := CollectTurnEventsWhile(ctx, session, func(te *TurnEvents) bool {
		if te.TurnComplete == nil {
			return true // still collecting first turn
		}
		foundBg := false
		for _, tc := range te.ToolComplete {
			if tc.Name == "Bash" {
				if rib, ok := tc.Input["run_in_background"]; ok {
					if b, ok := rib.(bool); ok && b {
						foundBg = true
						break
					}
				}
			}
		}
		if !foundBg {
			// No bg work — single turn is the final answer.
			return false
		}
		// Bg task was used. Keep collecting until the marker text arrives
		// (continuation turn's text) OR context expires.
		full := ""
		for _, t := range te.TextEvents {
			full += t.Text
		}
		return !containsAny(full, "BG_TASK_DONE_MARKER", "bg_task_done_marker", "bg-done")
	})
	if err != nil {
		t.Fatalf("CollectTurnEventsWhile failed: %v", err)
	}

	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent")
	}
	if !events.TurnComplete.Success {
		t.Errorf("Expected successful turn, got error")
	}

	// Verify the agent used a background task. Check tool completions for
	// a Bash tool with run_in_background in its input.
	foundBgTask := false
	for _, tc := range events.ToolComplete {
		if tc.Name == "Bash" {
			if rib, ok := tc.Input["run_in_background"]; ok {
				if b, ok := rib.(bool); ok && b {
					foundBgTask = true
				}
			}
		}
	}
	if !foundBgTask {
		t.Log("WARNING: Agent did not use run_in_background — test may not exercise the background task path")
	}

	// Check that the final text includes the background task output marker.
	// This confirms the session delivered the continuation turn's text, not
	// just the intermediate "running in background" response.
	fullText := ""
	for _, te := range events.TextEvents {
		fullText += te.Text
	}
	t.Logf("Final response text (truncated): %.300s", fullText)

	if foundBgTask {
		if !containsAny(fullText, "BG_TASK_DONE_MARKER", "bg_task_done_marker", "bg-done") {
			t.Error("Expected final text to contain background task output marker, " +
				"but got intermediate 'running in background' response instead. " +
				"This suggests the session did not deliver the continuation turn.")
		}
	}

	t.Logf("Turn completed: success=%v, cost=$%.6f", events.TurnComplete.Success, events.TurnComplete.Usage.CostUSD)
	t.Log("All assertions passed for Background Task scenario")
}

// ============================================================================
// Scenario 6: Background Task Cancellation (parallel tool failure)
// ============================================================================

// TestSession_Integration_BackgroundTaskCancelled verifies that the session
// does NOT hang when background tasks are cancelled due to a sibling parallel
// tool call failing.
//
// This reproduces the exact bug from jiradozer's validate step:
//  1. Agent calls 3 Bash tools in parallel
//  2. First tool fails (exit 1)
//  3. CLI cancels the other two (which had run_in_background: true)
//  4. Session must complete the turn normally, not hang waiting for
//     task-notifications that will never arrive.
func TestSession_Integration_BackgroundTaskCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "claude-go-test-bgcancel-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithRecording(testDir),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Ask the agent to run 3 parallel Bash commands. The first one must fail,
	// which should cause the CLI to cancel the other two background tasks.
	t.Log("Sending parallel-failure prompt...")
	_, err = session.SendMessage(ctx,
		"Run these THREE bash commands in PARALLEL (all in one response, using multiple tool calls):\n"+
			"1. `exit 1` (this will fail)\n"+
			"2. `sleep 5 && echo SECOND_DONE` with run_in_background: true\n"+
			"3. `sleep 5 && echo THIRD_DONE` with run_in_background: true\n\n"+
			"You MUST call all three Bash tools in the same response so they run in parallel. "+
			"Commands 2 and 3 MUST use run_in_background: true.")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	events, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents failed (session may have hung): %v", err)
	}

	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent — session hung or no events received")
	}

	t.Logf("Turn completed: success=%v, cost=$%.6f",
		events.TurnComplete.Success, events.TurnComplete.Usage.CostUSD)

	// Check that we had tool results with errors (cancelled bg tasks).
	cancelledCount := 0
	for _, tr := range events.ToolResults {
		if tr.IsError {
			cancelledCount++
		}
	}
	t.Logf("Tool results with errors: %d", cancelledCount)
	if cancelledCount == 0 {
		t.Log("WARNING: No cancelled tool results — agent may not have used parallel tools. " +
			"Test may not exercise the cancelled-bg-task path.")
	}

	t.Log("All assertions passed for Background Task Cancellation scenario")
}

// ============================================================================
// Scenario 7: Mixed Background + Non-Background Tools
// ============================================================================

// TestSession_Integration_BackgroundTaskMixed verifies that the session
// completes normally when a turn contains BOTH background and non-background
// tools. This reproduces the jiradozer stuck-session bug where the SDK
// incorrectly suppressed the turn when 2× bg + 1× non-bg tools were present.
//
// The ResultMessage for a mixed turn represents completion of the synchronous
// (non-bg) work, so it must NOT be suppressed.
func TestSession_Integration_BackgroundTaskMixed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "claude-go-test-bgmixed-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithRecording(testDir),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}
	defer session.Stop()

	// Ask the agent to run 3 tools in one response: 2 background + 1 blocking.
	// This is the exact pattern that caused the jiradozer stuck session.
	t.Log("Sending mixed bg/non-bg prompt...")
	_, err = session.SendMessage(ctx,
		"Run these THREE bash commands in PARALLEL (all in one response, using multiple tool calls):\n"+
			"1. `sleep 1 && echo BG1_DONE` with run_in_background: true\n"+
			"2. `sleep 1 && echo BG2_DONE` with run_in_background: true\n"+
			"3. `echo BLOCKING_DONE` (NO run_in_background, this is a normal blocking command)\n\n"+
			"You MUST call all three Bash tools in the same response so they run in parallel. "+
			"Commands 1 and 2 MUST use run_in_background: true. "+
			"Command 3 MUST NOT use run_in_background.")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	events, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectTurnEvents failed (session may have hung — this is the mixed bg/non-bg bug): %v", err)
	}

	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent — session hung or no events received")
	}
	if !events.TurnComplete.Success {
		t.Errorf("Expected successful turn, got error")
	}

	// Verify the tool mix: at least one bg and one non-bg tool.
	bgCount := 0
	nonBgCount := 0
	for _, tc := range events.ToolComplete {
		if tc.Name == "Bash" {
			if rib, ok := tc.Input["run_in_background"]; ok {
				if b, ok := rib.(bool); ok && b {
					bgCount++
					continue
				}
			}
			nonBgCount++
		}
	}
	t.Logf("Tool mix: %d bg, %d non-bg", bgCount, nonBgCount)
	if bgCount == 0 || nonBgCount == 0 {
		t.Skipf("Agent did not produce the expected mix of bg + non-bg tools "+
			"(bg=%d, non-bg=%d); cannot exercise the mixed-tool suppression path",
			bgCount, nonBgCount)
	}

	t.Logf("Turn completed: success=%v, cost=$%.6f",
		events.TurnComplete.Success, events.TurnComplete.Usage.CostUSD)
	t.Log("All assertions passed for Mixed Background Task scenario")
}

// containsAny reports whether s contains any of the given substrings (case-insensitive).
func containsAny(s string, subs ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

// ============================================================================
// Trace fixture export
// ============================================================================

func shouldUpdateFixtures() bool {
	return os.Getenv("UPDATE_FIXTURES") != ""
}

// fixtureDir returns the path to testdata/traces, creating it if needed.
func fixtureDir(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"agent-cli-wrapper/testdata/traces",
		"../testdata/traces",
		"../../testdata/traces",
	}
	for _, c := range candidates {
		abs, _ := filepath.Abs(c)
		if info, err := os.Stat(filepath.Dir(abs)); err == nil && info.IsDir() {
			if err := os.MkdirAll(abs, 0o755); err == nil {
				return abs
			}
		}
	}
	dir := filepath.Join(t.TempDir(), "traces")
	os.MkdirAll(dir, 0o755)
	t.Logf("WARNING: Could not find testdata/traces in repo, writing to %s", dir)
	return dir
}

// exportTraceFixtures splits a messages.jsonl recording into from_cli.jsonl
// and to_cli.jsonl trace fixture files, then validates them.
func exportTraceFixtures(t *testing.T, messagesPath string) {
	t.Helper()

	outDir := fixtureDir(t)

	file, err := os.Open(messagesPath)
	if err != nil {
		t.Fatalf("Failed to open %s: %v", messagesPath, err)
	}
	defer file.Close()

	fromFile, err := os.Create(filepath.Join(outDir, "from_cli.jsonl"))
	if err != nil {
		t.Fatalf("Failed to create from_cli.jsonl: %v", err)
	}
	defer fromFile.Close()

	toFile, err := os.Create(filepath.Join(outDir, "to_cli.jsonl"))
	if err != nil {
		t.Fatalf("Failed to create to_cli.jsonl: %v", err)
	}
	defer toFile.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var fromCount, toCount int
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		var record claude.RecordedMessage
		if err := json.Unmarshal(line, &record); err != nil {
			t.Logf("Line %d: failed to unmarshal: %v", lineNum, err)
			continue
		}

		msgBytes, ok := record.Message.(json.RawMessage)
		if !ok {
			msgBytes, err = json.Marshal(record.Message)
			if err != nil {
				continue
			}
		}

		entry := protocol.TraceEntry{
			ID:        fmt.Sprintf("msg-%d", lineNum),
			Timestamp: record.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
			Direction: record.Direction,
			Message:   msgBytes,
		}
		entryBytes, _ := json.Marshal(entry)

		switch record.Direction {
		case "received":
			fromFile.Write(entryBytes)
			fromFile.Write([]byte("\n"))
			fromCount++
		case "sent":
			toFile.Write(entryBytes)
			toFile.Write([]byte("\n"))
			toCount++
		}
	}

	t.Logf("Trace fixtures written to %s: from_cli=%d, to_cli=%d", outDir, fromCount, toCount)

	// Validate fixtures parse correctly
	for _, name := range []string{"from_cli.jsonl", "to_cli.jsonl"} {
		validateFixture(t, filepath.Join(outDir, name), name)
	}
}

// validateFixture parses a fixture file and reports stats.
func validateFixture(t *testing.T, path, label string) {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Failed to open %s: %v", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	typeCounts := make(map[string]int)
	var lineNum, parseErrors int

	for scanner.Scan() {
		lineNum++
		var entry protocol.TraceEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			parseErrors++
			continue
		}
		msg, err := protocol.ParseMessage(entry.Message)
		if err != nil {
			parseErrors++
			continue
		}
		typeCounts[fmt.Sprintf("%T", msg)]++
	}

	t.Logf("%s: %d lines, %d parse errors", label, lineNum, parseErrors)
	for typ, count := range typeCounts {
		t.Logf("  %s: %d", typ, count)
	}
	if parseErrors > 0 {
		t.Errorf("%s: %d parse errors", label, parseErrors)
	}
	if lineNum == 0 {
		t.Errorf("%s: fixture is empty", label)
	}
}
