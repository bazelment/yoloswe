//go:build integration
// +build integration

// Integration tests for the Codex Client with real app-server.
//
// These tests require:
// - The codex CLI to be installed and available in PATH
// - A valid API key configured
//
// Run with: go test -tags=integration ./codex/...
// or: go test -tags=integration -v ./codex/... -run Integration

package codex

import (
	"context"
	"os"
	"testing"
	"time"
)

// ============================================================================
// Test Utilities
// ============================================================================

// TurnEvents collects events from a single turn.
type TurnEvents struct {
	ClientReady   *ClientReadyEvent
	ThreadStarted *ThreadStartedEvent
	ThreadReady   *ThreadReadyEvent
	TurnStarted   *TurnStartedEvent
	TurnCompleted *TurnCompletedEvent
	TextDeltas    []TextDeltaEvent
	Errors        []ErrorEvent
}

// CollectTurnEvents collects all events until TurnCompletedEvent or context cancellation.
func CollectTurnEvents(ctx context.Context, c *Client) (*TurnEvents, error) {
	events := &TurnEvents{}

	for {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		case event, ok := <-c.Events():
			if !ok {
				return events, context.Canceled
			}

			switch e := event.(type) {
			case ClientReadyEvent:
				events.ClientReady = &e
			case ThreadStartedEvent:
				events.ThreadStarted = &e
			case ThreadReadyEvent:
				events.ThreadReady = &e
			case TurnStartedEvent:
				events.TurnStarted = &e
			case TurnCompletedEvent:
				events.TurnCompleted = &e
				return events, nil
			case TextDeltaEvent:
				events.TextDeltas = append(events.TextDeltas, e)
			case ErrorEvent:
				events.Errors = append(events.Errors, e)
			}
		}
	}
}

// WaitForThreadReady waits for ThreadReadyEvent for a specific thread.
func WaitForThreadReady(ctx context.Context, c *Client, threadID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-c.Events():
			if !ok {
				return context.Canceled
			}
			if e, ok := event.(ThreadReadyEvent); ok && e.ThreadID == threadID {
				return nil
			}
		}
	}
}

// ============================================================================
// Scenario 1: Basic Ask with Full-Auto Approval
// ============================================================================

func TestClient_Integration_Scenario1_BasicAsk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create temp directory for test artifacts
	testDir, err := os.MkdirTemp("", "codex-go-test-scenario1-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	// Create client
	client := NewClient(
		WithClientName("codex-integration-test"),
		WithClientVersion("1.0.0"),
	)

	// Start client
	t.Log("Starting client...")
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()
	t.Log("Client started successfully")

	// Create thread
	t.Log("Creating thread...")
	thread, err := client.CreateThread(ctx,
		WithWorkDir(testDir),
		WithApprovalPolicy(ApprovalPolicyFullAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create thread: %v", err)
	}
	t.Logf("Thread created: %s", thread.ID())

	// Wait for MCP startup
	t.Log("Waiting for MCP startup...")
	if err := WaitForThreadReady(ctx, client, thread.ID()); err != nil {
		t.Fatalf("Failed waiting for thread ready: %v", err)
	}
	t.Log("Thread ready!")

	// Ask a simple question
	prompt := "What is 2+2? Reply with just the number."
	t.Logf("Asking: %s", prompt)

	result, err := thread.Ask(ctx, prompt)
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}

	t.Logf("Response: %q", result.FullText)
	t.Logf("Success: %v, Duration: %dms", result.Success, result.DurationMs)

	if !result.Success {
		t.Errorf("Expected success, got failure: %v", result.Error)
	}
	if result.FullText == "" {
		t.Error("Expected non-empty response")
	}

	t.Log("Scenario 1 passed!")
}

// ============================================================================
// Scenario 2: Multi-Turn Conversation
// ============================================================================

func TestClient_Integration_Scenario2_MultiTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "codex-go-test-scenario2-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)
	t.Logf("Test artifacts directory: %s", testDir)

	client := NewClient(
		WithClientName("codex-integration-test"),
		WithClientVersion("1.0.0"),
	)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()

	thread, err := client.CreateThread(ctx,
		WithWorkDir(testDir),
		WithApprovalPolicy(ApprovalPolicyFullAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create thread: %v", err)
	}

	if err := WaitForThreadReady(ctx, client, thread.ID()); err != nil {
		t.Fatalf("Failed waiting for thread ready: %v", err)
	}

	// Turn 1
	t.Log("Turn 1: Initial question")
	result1, err := thread.Ask(ctx, "What is the capital of France?")
	if err != nil {
		t.Fatalf("Turn 1 failed: %v", err)
	}
	t.Logf("Turn 1 response: %q", result1.FullText)

	// Turn 2 - Follow up
	t.Log("Turn 2: Follow-up question")
	result2, err := thread.Ask(ctx, "What about Germany?")
	if err != nil {
		t.Fatalf("Turn 2 failed: %v", err)
	}
	t.Logf("Turn 2 response: %q", result2.FullText)

	// Turn 3 - Another follow up
	t.Log("Turn 3: Another follow-up")
	result3, err := thread.Ask(ctx, "And Italy?")
	if err != nil {
		t.Fatalf("Turn 3 failed: %v", err)
	}
	t.Logf("Turn 3 response: %q", result3.FullText)

	if !result1.Success || !result2.Success || !result3.Success {
		t.Error("Expected all turns to succeed")
	}

	t.Log("Scenario 2 passed!")
}

// ============================================================================
// Scenario 3: Streaming Events
// ============================================================================

func TestClient_Integration_Scenario3_Streaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "codex-go-test-scenario3-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	client := NewClient(
		WithClientName("codex-integration-test"),
		WithClientVersion("1.0.0"),
	)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()

	thread, err := client.CreateThread(ctx,
		WithWorkDir(testDir),
		WithApprovalPolicy(ApprovalPolicyFullAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create thread: %v", err)
	}

	if err := WaitForThreadReady(ctx, client, thread.ID()); err != nil {
		t.Fatalf("Failed waiting for thread ready: %v", err)
	}

	// Send message (non-blocking)
	prompt := "Write a short haiku about Go programming."
	t.Logf("Asking: %s", prompt)

	if _, err := thread.SendMessage(ctx, prompt); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	// Collect streaming events
	events, err := CollectTurnEvents(ctx, client)
	if err != nil {
		t.Fatalf("CollectTurnEvents failed: %v", err)
	}

	t.Logf("Received %d text deltas", len(events.TextDeltas))

	if events.TurnCompleted == nil {
		t.Error("Expected TurnCompletedEvent")
	} else {
		t.Logf("Turn completed: success=%v", events.TurnCompleted.Success)
	}

	if len(events.TextDeltas) == 0 {
		t.Error("Expected text deltas for streaming")
	}

	// Verify accumulated text
	var fullText string
	for _, delta := range events.TextDeltas {
		fullText = delta.FullText // Last FullText should have complete response
	}
	t.Logf("Full streamed response: %q", fullText)

	t.Log("Scenario 3 passed!")
}

// ============================================================================
// Scenario 4: Client.Ask Convenience Method
// ============================================================================

func TestClient_Integration_Scenario4_ClientAsk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "codex-go-test-scenario4-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	client := NewClient(
		WithClientName("codex-integration-test"),
		WithClientVersion("1.0.0"),
	)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()

	// Use the simple Ask method
	t.Log("Using client.Ask() convenience method...")
	response, err := client.Ask(ctx, "What is 5+5? Reply with just the number.",
		WithWorkDir(testDir),
		WithApprovalPolicy(ApprovalPolicyFullAuto),
	)
	if err != nil {
		t.Fatalf("client.Ask failed: %v", err)
	}

	t.Logf("Response: %q", response)

	if response == "" {
		t.Error("Expected non-empty response")
	}

	t.Log("Scenario 4 passed!")
}

// ============================================================================
// Scenario 5: Multiple Threads
// ============================================================================

func TestClient_Integration_Scenario5_MultipleThreads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	testDir1, _ := os.MkdirTemp("", "codex-go-test-scenario5-1-")
	testDir2, _ := os.MkdirTemp("", "codex-go-test-scenario5-2-")
	defer os.RemoveAll(testDir1)
	defer os.RemoveAll(testDir2)

	client := NewClient(
		WithClientName("codex-integration-test"),
		WithClientVersion("1.0.0"),
	)

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()

	// Create two threads
	t.Log("Creating thread 1...")
	thread1, err := client.CreateThread(ctx,
		WithWorkDir(testDir1),
		WithApprovalPolicy(ApprovalPolicyFullAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create thread 1: %v", err)
	}

	t.Log("Creating thread 2...")
	thread2, err := client.CreateThread(ctx,
		WithWorkDir(testDir2),
		WithApprovalPolicy(ApprovalPolicyFullAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create thread 2: %v", err)
	}

	// Wait for both threads to be ready
	// Note: This is simplified - in production you'd want to track both events
	t.Log("Waiting for threads to be ready...")
	readyCount := 0
	timeout := time.After(30 * time.Second)
	for readyCount < 2 {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for threads to be ready")
		case event := <-client.Events():
			if e, ok := event.(ThreadReadyEvent); ok {
				if e.ThreadID == thread1.ID() || e.ThreadID == thread2.ID() {
					readyCount++
					t.Logf("Thread ready: %s", e.ThreadID)
				}
			}
		}
	}

	// Use both threads
	t.Log("Using thread 1...")
	result1, err := thread1.Ask(ctx, "What is 1+1?")
	if err != nil {
		t.Fatalf("Thread 1 ask failed: %v", err)
	}
	t.Logf("Thread 1 response: %q", result1.FullText)

	t.Log("Using thread 2...")
	result2, err := thread2.Ask(ctx, "What is 2+2?")
	if err != nil {
		t.Fatalf("Thread 2 ask failed: %v", err)
	}
	t.Logf("Thread 2 response: %q", result2.FullText)

	// Verify both worked
	if !result1.Success || !result2.Success {
		t.Error("Expected both threads to succeed")
	}

	// Verify thread list
	threads := client.ListThreads()
	if len(threads) != 2 {
		t.Errorf("Expected 2 threads, got %d", len(threads))
	}

	t.Log("Scenario 5 passed!")
}
