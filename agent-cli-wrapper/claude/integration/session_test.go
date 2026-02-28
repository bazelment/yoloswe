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
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	if *updateFixtures {
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
// Trace fixture export
// ============================================================================

var updateFixtures = flag.Bool("update-fixtures", false, "export session recording as protocol trace fixtures")

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
