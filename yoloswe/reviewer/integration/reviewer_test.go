package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

// TestThreadCreation tests that thread creation and WaitReady completes
// within a reasonable time. This test is designed to be run multiple times
// to detect flakiness in the startup process.
func TestThreadCreation(t *testing.T) {
	config := reviewer.Config{
		Model:   "gpt-5.2-codex",
		WorkDir: t.TempDir(),
		Verbose: true,
	}

	r := reviewer.New(config)

	// Start the client with a short timeout
	startCtx, startCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer startCancel()

	if err := r.Start(startCtx); err != nil {
		t.Fatalf("Failed to start reviewer: %v", err)
	}
	defer r.Stop()

	// Create thread and wait for it to be ready
	createCtx, createCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer createCancel()

	start := time.Now()
	thread, err := r.CreateThread(createCtx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Failed to create thread after %v: %v", elapsed, err)
	}

	t.Logf("Thread created and ready in %v (thread ID: %s)", elapsed, thread.ID())
}
