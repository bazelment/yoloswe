package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

// TestTurnCompletion tests that sending a message and waiting for turn
// completion works reliably. This test is designed to be run multiple times
// to detect flakiness in the turn handling.
func TestTurnCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := codex.NewClient(
		codex.WithClientName("turn-test"),
		codex.WithClientVersion("1.0.0"),
	)

	// Start client
	startCtx, startCancel := context.WithTimeout(ctx, 5*time.Second)
	defer startCancel()

	if err := client.Start(startCtx); err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Stop()

	// Create thread
	createCtx, createCancel := context.WithTimeout(ctx, 10*time.Second)
	defer createCancel()

	thread, err := client.CreateThread(createCtx,
		codex.WithWorkDir(t.TempDir()),
		codex.WithApprovalPolicy(codex.ApprovalPolicyFullAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create thread: %v", err)
	}

	// Wait for thread ready
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()

	start := time.Now()
	if err := thread.WaitReady(readyCtx); err != nil {
		t.Fatalf("Thread not ready after %v: %v", time.Since(start), err)
	}
	t.Logf("Thread ready in %v", time.Since(start))

	// Send a simple message and wait for turn completion
	turnCtx, turnCancel := context.WithTimeout(ctx, 20*time.Second)
	defer turnCancel()

	start = time.Now()
	result, err := thread.Ask(turnCtx, "Reply with just: OK")
	if err != nil {
		t.Fatalf("Turn failed after %v: %v", time.Since(start), err)
	}

	t.Logf("Turn completed in %v, success=%v, response=%q", time.Since(start), result.Success, result.FullText)
}
