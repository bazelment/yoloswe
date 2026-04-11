//go:build integration
// +build integration

// Integration tests for the Claude Code Monitor tool.
//
// Monitor is a built-in CLI tool that spawns a detached bash subprocess and
// returns immediately with a synchronous tool_result ("Monitor started ...
// keep working, do not poll"). Unlike Bash(run_in_background:true), its
// tool_use.input does NOT carry run_in_background — the CLI treats it as
// background work via task_started/task_updated lifecycle events.
//
// Before the fix, the SDK wrapper's suppression logic keyed only off
// run_in_background, so Monitor turns finalized immediately on end_turn
// while the underlying bash subprocess was still running. Headless harnesses
// like jiradozer then advanced rounds with incomplete results.
//
// These tests exercise the Monitor flow end-to-end against the real Claude
// CLI: the session must block on Ask() until the Monitor task reaches
// terminal state, then return the final TurnResult that reflects the full
// observation window.

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// MonitorTurnEvents collects events from a Monitor-bearing turn. Unlike
// the shared TurnEvents helper in session_test.go, this tracks the task
// lifecycle events that prove suppression/release actually worked.
type MonitorTurnEvents struct {
	TurnComplete      *claude.TurnCompleteEvent
	TaskStarted       []claude.TaskStartedEvent
	TaskUpdated       []claude.TaskUpdatedEvent
	TaskNotifications []claude.TaskNotificationEvent
	ToolStarts        []claude.ToolStartEvent
	ToolComplete      []claude.ToolCompleteEvent
	TextEvents        []claude.TextEvent
	Errors            []claude.ErrorEvent
}

// collectMonitorTurnEvents drains events until TurnCompleteEvent. It records
// task-lifecycle events alongside the usual text/tool events so tests can
// assert on the full Monitor flow.
func collectMonitorTurnEvents(ctx context.Context, s *claude.Session) (*MonitorTurnEvents, error) {
	events := &MonitorTurnEvents{}
	for {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		case event, ok := <-s.Events():
			if !ok {
				return events, context.Canceled
			}
			switch e := event.(type) {
			case claude.TextEvent:
				events.TextEvents = append(events.TextEvents, e)
			case claude.ToolStartEvent:
				events.ToolStarts = append(events.ToolStarts, e)
			case claude.ToolCompleteEvent:
				events.ToolComplete = append(events.ToolComplete, e)
			case claude.TaskStartedEvent:
				events.TaskStarted = append(events.TaskStarted, e)
			case claude.TaskUpdatedEvent:
				events.TaskUpdated = append(events.TaskUpdated, e)
			case claude.TaskNotificationEvent:
				events.TaskNotifications = append(events.TaskNotifications, e)
			case claude.TurnCompleteEvent:
				events.TurnComplete = &e
				return events, nil
			case claude.ErrorEvent:
				events.Errors = append(events.Errors, e)
			}
		}
	}
}

// hasMonitorTool reports whether any Monitor tool_use appeared in the turn.
func (me *MonitorTurnEvents) hasMonitorTool() bool {
	for _, tc := range me.ToolComplete {
		if tc.Name == "Monitor" {
			return true
		}
	}
	return false
}

// fullText joins all streamed text chunks for the turn.
func (me *MonitorTurnEvents) fullText() string {
	var b strings.Builder
	for _, te := range me.TextEvents {
		b.WriteString(te.Text)
	}
	return b.String()
}

// TestSession_Integration_MonitorTool verifies the core fix: when the
// agent uses the Monitor tool, the SDK must block Ask()/CollectResponse()
// until the Monitor task reaches terminal state — NOT finalize the turn
// on the intermediate end_turn stop_reason.
//
// The marker string written by the monitored bash subprocess must appear
// in the final TurnResult text. If it does not, the session finalized
// before the subprocess completed — that's the exact jiradozer bug.
func TestSession_Integration_MonitorTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "claude-go-test-monitor-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	keepArtifacts := os.Getenv("KEEP_ARTIFACTS") != ""
	if !keepArtifacts {
		defer os.RemoveAll(testDir)
	}
	t.Logf("Test artifacts directory: %s (keep=%v)", testDir, keepArtifacts)

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

	// Ask the agent to launch a short Monitor command and report its output.
	// The sentinel file trick guarantees the subprocess runs to completion:
	// we ask the agent to read the file after the Monitor task is done, which
	// only works if the SDK correctly blocked until the subprocess actually
	// finished writing.
	t.Log("Sending Monitor prompt...")
	_, err = session.SendMessage(ctx,
		"Use the Monitor tool (NOT plain Bash, NOT run_in_background) to run this exact command:\n"+
			"`sleep 3 && echo MONITOR_MARKER_XYZ > "+testDir+"/monitor_output.txt`\n\n"+
			"You MUST call the Monitor tool. After calling Monitor, wait for it to finish, "+
			"then read the file `"+testDir+"/monitor_output.txt` and tell me its exact contents. "+
			"Do not guess — read the file.")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	events, err := collectMonitorTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("collectMonitorTurnEvents failed (session may have hung waiting for Monitor): %v", err)
	}

	if events.TurnComplete == nil {
		t.Fatal("Expected TurnCompleteEvent — session hung or no events received")
	}
	if !events.TurnComplete.Success {
		t.Errorf("Expected successful turn, got error")
	}

	t.Logf("Turn completed: success=%v, cost=$%.6f, turn=%d",
		events.TurnComplete.Success, events.TurnComplete.Usage.CostUSD, events.TurnComplete.TurnNumber)
	t.Logf("Task events: started=%d updated=%d notifications=%d",
		len(events.TaskStarted), len(events.TaskUpdated), len(events.TaskNotifications))

	// If the agent didn't use Monitor, the test doesn't exercise the fix —
	// log and skip the behavioral assertions rather than false-failing.
	if !events.hasMonitorTool() {
		t.Skip("Agent did not use the Monitor tool — cannot exercise the fix. " +
			"This can happen with smaller models that pick plain Bash instead.")
	}

	// task_started must have fired at least once — proves the CLI registered
	// the Monitor command as a background task and the SDK received the event.
	if len(events.TaskStarted) == 0 {
		t.Error("Expected at least one TaskStartedEvent when Monitor is used")
	}

	// The terminal release path is either task_updated with a terminal status
	// (completed/failed/killed) OR task_notification. At least one must be
	// present or the turn could not have been released from suppression.
	hasTerminal := false
	for _, tu := range events.TaskUpdated {
		if tu.Status != nil {
			switch *tu.Status {
			case "completed", "failed", "killed":
				hasTerminal = true
			}
		}
	}
	if !hasTerminal && len(events.TaskNotifications) == 0 {
		t.Error("Expected at least one terminal TaskUpdatedEvent or TaskNotificationEvent — " +
			"without one, the turn could not have released from suppression")
	}

	// The actual proof-of-fix: the monitored subprocess wrote a marker file,
	// and the agent read it back. If the SDK had finalized the turn on
	// end_turn (the bug), the subprocess would still be sleeping when Ask
	// returned — the file would be empty and the marker absent from output.
	markerFile := testDir + "/monitor_output.txt"
	if _, statErr := os.Stat(markerFile); statErr != nil {
		t.Errorf("Marker file not created: %v — Monitor subprocess did not complete", statErr)
	} else {
		data, readErr := os.ReadFile(markerFile)
		if readErr != nil {
			t.Errorf("Failed to read marker file: %v", readErr)
		} else if !strings.Contains(string(data), "MONITOR_MARKER_XYZ") {
			t.Errorf("Marker file missing expected content, got: %q", string(data))
		}
	}

	// The final response text should include the marker the agent read from
	// the file. This proves Ask() waited for the full Monitor cycle — the
	// agent had to observe the subprocess's completed output to report it.
	text := events.fullText()
	t.Logf("Final response text (%d bytes):\n%s", len(text), text)
	if !strings.Contains(text, "MONITOR_MARKER_XYZ") {
		t.Error("Expected final response text to contain MONITOR_MARKER_XYZ — " +
			"the agent did not observe the Monitor subprocess's output, " +
			"suggesting the SDK finalized the turn before the subprocess completed")
	}

	t.Log("Monitor tool scenario passed — SDK correctly blocked until task terminal state")
}
