// Binary cursor_review runs a one-shot code review using the Cursor Agent CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

func main() {
	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	// First, run a quick test to verify the cursor CLI works
	fmt.Println("=== Testing Cursor Agent CLI ===")
	testCtx, testCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer testCancel()

	result, err := cursor.Query(testCtx, "What is 2+2? Reply with just the number.",
		cursor.WithWorkDir(workDir),
		cursor.WithTrust(),
		cursor.WithStderrHandler(func(b []byte) {
			fmt.Fprintf(os.Stderr, "[cursor stderr] %s", b)
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cursor test failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Cursor test response: %q (success=%v, duration=%dms)\n\n", result.Text, result.Success, result.DurationMs)

	// Now run the actual review
	fmt.Println("=== Running Code Review ===")
	config := reviewer.Config{
		BackendType: reviewer.BackendCursor,
		WorkDir:     workDir,
		Verbose:     true,
	}

	r := reviewer.New(config)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start reviewer: %v\n", err)
		os.Exit(1)
	}
	defer r.Stop()

	prompt := reviewer.BuildPrompt("Add Cursor Agent CLI support across SDK, reviewer, and multiagent provider")
	reviewResult, err := r.ReviewWithResult(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n\n=== Review Result ===\n")
	fmt.Printf("Success: %v\n", reviewResult.Success)
	fmt.Printf("Duration: %dms\n", reviewResult.DurationMs)
	fmt.Printf("Response length: %d chars\n", len(reviewResult.ResponseText))
}
