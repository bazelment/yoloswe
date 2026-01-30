//go:build integration
// +build integration

// Integration tests for session resume functionality.
//
// These tests verify that:
// 1. A session can be started and its ID captured
// 2. The session can be stopped gracefully
// 3. A new session can resume from the captured session ID
// 4. The resumed session maintains conversation context
//
// Run with: go test -tags=integration ./claude/... -run TestSession_Integration_Resume
//
// These tests require:
// - The claude CLI to be installed and available in PATH
// - A valid API key configured
//
// Set CLAUDE_CLI_PATH to override the default claude CLI location.

package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSession_Integration_Resume tests the resume session functionality.
// This verifies that:
// 1. A session can be started and its ID captured
// 2. The session can be stopped
// 3. A new session can resume from the captured session ID
// 4. The resumed session can continue the conversation with preserved context
func TestSession_Integration_Resume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Create temp directory for test artifacts
	testDir, err := os.MkdirTemp("", "claude-go-test-resume-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	// === Phase 1: Create initial session and establish context ===
	t.Log("Phase 1: Creating initial session...")

	session1 := NewSession(
		WithModel("haiku"),
		WithWorkDir(testDir),
		WithPermissionMode(PermissionModeBypass),
		WithDisablePlugins(),
		WithRecording(testDir),
	)

	if err := session1.Start(ctx); err != nil {
		t.Fatalf("Failed to start session1: %v", err)
	}

	// Send a message with a unique identifier that we can reference later
	_, err = session1.SendMessage(ctx, "Remember this secret code: ALPHA-BRAVO-42. Just acknowledge you received it briefly.")
	if err != nil {
		session1.Stop()
		t.Fatalf("SendMessage failed: %v", err)
	}

	events1, err := CollectTurnEvents(ctx, session1)
	if err != nil {
		session1.Stop()
		t.Fatalf("CollectTurnEvents(1) failed: %v", err)
	}

	if events1.Ready == nil {
		session1.Stop()
		t.Fatal("Expected Ready event with session info")
	}

	sessionID := events1.Ready.Info.SessionID
	t.Logf("Session 1 ID: %s", sessionID)
	t.Logf("Turn 1 completed: success=%v", events1.TurnComplete.Success)

	// Stop the first session
	if err := session1.Stop(); err != nil {
		t.Fatalf("Failed to stop session1: %v", err)
	}
	t.Log("Session 1 stopped")

	// Small delay to ensure session is fully stopped
	time.Sleep(500 * time.Millisecond)

	// === Phase 2: Resume the session ===
	t.Log("Phase 2: Resuming session...")

	session2 := NewSession(
		WithModel("haiku"),
		WithWorkDir(testDir),
		WithPermissionMode(PermissionModeBypass),
		WithDisablePlugins(),
		WithRecording(testDir),
		WithResume(sessionID), // <-- The new option being tested
	)

	// Verify CLI args include --resume
	args, err := session2.CLIArgs()
	if err != nil {
		t.Fatalf("CLIArgs failed: %v", err)
	}
	hasResumeFlag := false
	for i, arg := range args {
		if arg == "--resume" && i+1 < len(args) && args[i+1] == sessionID {
			hasResumeFlag = true
			break
		}
	}
	if !hasResumeFlag {
		t.Errorf("Expected CLI args to contain --resume %s, got: %v", sessionID, args)
	}
	t.Logf("CLI args include --resume flag: %v", hasResumeFlag)

	if err := session2.Start(ctx); err != nil {
		t.Fatalf("Failed to start session2 with resume: %v", err)
	}
	defer session2.Stop()

	// Ask about the previous conversation to verify context was preserved
	_, err = session2.SendMessage(ctx, "What was the secret code I gave you earlier? Just state the code.")
	if err != nil {
		t.Fatalf("SendMessage on resumed session failed: %v", err)
	}

	events2, err := CollectTurnEvents(ctx, session2)
	if err != nil {
		t.Fatalf("CollectTurnEvents(2) failed: %v", err)
	}

	t.Logf("Turn 2 completed: success=%v", events2.TurnComplete.Success)

	// Collect the full response text
	var fullResponse string
	for _, te := range events2.TextEvents {
		if te.FullText != "" {
			fullResponse = te.FullText
		}
	}
	t.Logf("Response text: %s", truncateForLog(fullResponse, 200))

	// Verify the response contains our secret code (proving context was preserved)
	if !strings.Contains(fullResponse, "ALPHA-BRAVO-42") &&
		!strings.Contains(fullResponse, "ALPHA BRAVO 42") &&
		!strings.Contains(strings.ToUpper(fullResponse), "ALPHA") {
		t.Error("Expected response to contain the secret code from previous session")
		t.Logf("Full response was: %s", fullResponse)
	} else {
		t.Log("Context was preserved - secret code found in response!")
	}

	t.Log("Resume test completed successfully")
}

// TestSession_WithResume_CLIArgs verifies that WithResume correctly sets the CLI arguments.
func TestSession_WithResume_CLIArgs(t *testing.T) {
	testSessionID := "test-session-id-12345"

	session := NewSession(
		WithModel("haiku"),
		WithResume(testSessionID),
	)

	args, err := session.CLIArgs()
	if err != nil {
		t.Fatalf("CLIArgs failed: %v", err)
	}

	// Check that --resume flag is present with correct value
	foundResume := false
	for i, arg := range args {
		if arg == "--resume" {
			if i+1 >= len(args) {
				t.Fatal("--resume flag found but no value follows")
			}
			if args[i+1] != testSessionID {
				t.Errorf("--resume value mismatch: expected %s, got %s", testSessionID, args[i+1])
			}
			foundResume = true
			break
		}
	}

	if !foundResume {
		t.Errorf("--resume flag not found in CLI args: %v", args)
	}
}

// truncateForLog truncates a string for logging purposes.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
