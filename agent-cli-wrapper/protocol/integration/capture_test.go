//go:build integration
// +build integration

// Trace capture tests run real CLI sessions and save protocol traces as fixtures.
//
// The fixtures are saved to agent-cli-wrapper/testdata/traces/ and are used by
// the trace parsing tests in agent-cli-wrapper/protocol/trace_test.go.
//
// Run with:
//   bazel test //agent-cli-wrapper/protocol/integration:integration_test --test_timeout=300
//
// Or to regenerate fixtures:
//   bazel build //agent-cli-wrapper/protocol/integration:integration_test
//   ./bazel-bin/agent-cli-wrapper/protocol/integration/integration_test_/integration_test \
//       -test.v -test.run TestCapture_Claude -update-fixtures

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

var updateFixtures = flag.Bool("update-fixtures", false, "regenerate trace fixture files")

// fixtureDir returns the path to testdata/traces, creating it if needed.
func fixtureDir(t *testing.T) string {
	t.Helper()
	// When run from bazel, the working directory is the runfiles root.
	// When run directly, we navigate from the integration dir.
	candidates := []string{
		"agent-cli-wrapper/testdata/traces",
		"../testdata/traces",
		"../../testdata/traces",
	}
	for _, c := range candidates {
		abs, _ := filepath.Abs(c)
		if info, err := os.Stat(filepath.Dir(abs)); err == nil && info.IsDir() {
			if err := os.MkdirAll(abs, 0755); err == nil {
				return abs
			}
		}
	}
	// Fallback: create in test temp dir and print the path
	dir := filepath.Join(t.TempDir(), "traces")
	os.MkdirAll(dir, 0755)
	t.Logf("WARNING: Could not find testdata/traces in repo, writing to %s", dir)
	t.Logf("Copy files to agent-cli-wrapper/testdata/traces/ to use as fixtures")
	return dir
}

// splitRecording reads a messages.jsonl file and splits it into from_cli.jsonl
// (direction=received) and to_cli.jsonl (direction=sent).
func splitRecording(t *testing.T, messagesPath, outputDir string) (fromCount, toCount int) {
	t.Helper()

	file, err := os.Open(messagesPath)
	if err != nil {
		t.Fatalf("Failed to open %s: %v", messagesPath, err)
	}
	defer file.Close()

	fromFile, err := os.Create(filepath.Join(outputDir, "from_cli.jsonl"))
	if err != nil {
		t.Fatalf("Failed to create from_cli.jsonl: %v", err)
	}
	defer fromFile.Close()

	toFile, err := os.Create(filepath.Join(outputDir, "to_cli.jsonl"))
	if err != nil {
		t.Fatalf("Failed to create to_cli.jsonl: %v", err)
	}
	defer toFile.Close()

	scanner := bufio.NewScanner(file)
	// Large buffer for messages with tool results
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		var record claude.RecordedMessage
		if err := json.Unmarshal(line, &record); err != nil {
			t.Logf("Line %d: failed to unmarshal RecordedMessage: %v", lineNum, err)
			continue
		}

		// The RecordedMessage.Message is already json.RawMessage after unmarshal.
		// Re-marshal the inner message as a trace entry.
		msgBytes, ok := record.Message.(json.RawMessage)
		if !ok {
			// Try marshaling it back
			var err error
			msgBytes, err = json.Marshal(record.Message)
			if err != nil {
				t.Logf("Line %d: failed to marshal message: %v", lineNum, err)
				continue
			}
		}

		entry := protocol.TraceEntry{
			ID:        fmt.Sprintf("msg-%d", lineNum),
			Timestamp: record.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
			Direction: record.Direction,
			Message:   msgBytes,
		}

		entryBytes, err := json.Marshal(entry)
		if err != nil {
			t.Logf("Line %d: failed to marshal trace entry: %v", lineNum, err)
			continue
		}

		switch record.Direction {
		case "received":
			fromFile.Write(entryBytes)
			fromFile.Write([]byte("\n"))
			fromCount++
		case "sent":
			toFile.Write(entryBytes)
			toFile.Write([]byte("\n"))
			toCount++
		default:
			t.Logf("Line %d: unknown direction %q", lineNum, record.Direction)
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("Scanner error: %v", err)
	}

	return fromCount, toCount
}

// TestCapture_Claude runs a real Claude session that exercises key protocol features
// and saves the trace as fixture files.
func TestCapture_Claude(t *testing.T) {
	if !*updateFixtures {
		t.Skip("Skipping fixture capture (pass -update-fixtures to regenerate)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	testDir := t.TempDir()
	recordDir := filepath.Join(testDir, "recording")
	os.MkdirAll(recordDir, 0755)

	// Create session with recording enabled.
	// This exercises: text streaming, tool use (Write + Read), stream events,
	// system init, result messages.
	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithRecording(recordDir),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("Failed to start session: %v", err)
	}

	// Turn 1: Simple text response (exercises text streaming + stream events)
	t.Log("Turn 1: Simple text response...")
	_, err := session.SendMessage(ctx, "Reply with exactly: PROTOCOL TEST OK")
	if err != nil {
		t.Fatalf("SendMessage(1) failed: %v", err)
	}
	events1, err := collectEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectEvents(1) failed: %v", err)
	}
	if events1.TurnComplete == nil {
		t.Fatal("Turn 1: no TurnComplete event")
	}
	t.Logf("Turn 1 complete: success=%v", events1.TurnComplete.Success)

	// Turn 2: Tool use (exercises tool_use content blocks, tool results)
	targetFile := filepath.Join(testDir, "trace_test.txt")
	t.Log("Turn 2: Tool use (Write + Read)...")
	_, err = session.SendMessage(ctx, fmt.Sprintf(
		"Create a file at %s containing 'trace fixture test'. Then read it back and confirm the contents.", targetFile))
	if err != nil {
		t.Fatalf("SendMessage(2) failed: %v", err)
	}
	events2, err := collectEvents(ctx, session)
	if err != nil {
		t.Fatalf("CollectEvents(2) failed: %v", err)
	}
	if events2.TurnComplete == nil {
		t.Fatal("Turn 2: no TurnComplete event")
	}
	t.Logf("Turn 2 complete: success=%v, tools=%d", events2.TurnComplete.Success, len(events2.ToolStarts))

	// Stop session to flush recording
	session.Stop()

	// Find the recording directory
	recPath := session.RecordingPath()
	if recPath == "" {
		t.Fatal("No recording path returned")
	}
	t.Logf("Recording saved to: %s", recPath)

	messagesPath := filepath.Join(recPath, "messages.jsonl")
	if _, err := os.Stat(messagesPath); err != nil {
		t.Fatalf("messages.jsonl not found at %s", messagesPath)
	}

	// Split into from_cli.jsonl and to_cli.jsonl
	outDir := fixtureDir(t)
	fromCount, toCount := splitRecording(t, messagesPath, outDir)
	t.Logf("Fixture files written to %s", outDir)
	t.Logf("  from_cli.jsonl: %d messages", fromCount)
	t.Logf("  to_cli.jsonl:   %d messages", toCount)

	// Validate the fixture files can be parsed by the trace_test parser
	validateFixture(t, filepath.Join(outDir, "from_cli.jsonl"), "from_cli")
	validateFixture(t, filepath.Join(outDir, "to_cli.jsonl"), "to_cli")
}

// collectEvents collects all events from a session until TurnComplete.
func collectEvents(ctx context.Context, s *claude.Session) (*turnEvents, error) {
	events := &turnEvents{}
	for {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		case ev, ok := <-s.Events():
			if !ok {
				return events, nil
			}
			switch e := ev.(type) {
			case claude.ReadyEvent:
				events.Ready = &e
			case claude.TextEvent:
				events.TextEvents = append(events.TextEvents, e)
			case claude.ToolStartEvent:
				events.ToolStarts = append(events.ToolStarts, e)
			case claude.ToolCompleteEvent:
				events.ToolCompletes = append(events.ToolCompletes, e)
			case claude.TurnCompleteEvent:
				events.TurnComplete = &e
				return events, nil
			case claude.ErrorEvent:
				events.Errors = append(events.Errors, e)
			}
		}
	}
}

type turnEvents struct {
	Ready         *claude.ReadyEvent
	TextEvents    []claude.TextEvent
	ToolStarts    []claude.ToolStartEvent
	ToolCompletes []claude.ToolCompleteEvent
	TurnComplete  *claude.TurnCompleteEvent
	Errors        []claude.ErrorEvent
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
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	typeCounts := make(map[string]int)
	lineNum := 0
	parseErrors := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()

		// Parse as TraceEntry
		var entry protocol.TraceEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			parseErrors++
			t.Logf("%s line %d: trace entry parse error: %v", label, lineNum, err)
			continue
		}

		// Parse the inner message
		msg, err := protocol.ParseMessage(entry.Message)
		if err != nil {
			parseErrors++
			t.Logf("%s line %d: message parse error: %v", label, lineNum, err)
			continue
		}

		typeCounts[fmt.Sprintf("%T", msg)]++
	}

	t.Logf("%s validation: %d lines, %d parse errors", label, lineNum, parseErrors)
	for typ, count := range typeCounts {
		t.Logf("  %s: %d", typ, count)
	}

	if parseErrors > 0 {
		t.Errorf("%s: %d parse errors (see log above)", label, parseErrors)
	}
	if lineNum == 0 {
		t.Errorf("%s: fixture is empty", label)
	}
}
