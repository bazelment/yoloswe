// Binary agent_review runs a one-shot code review using an agent backend.
//
// Usage:
//
//	agent_review --backend cursor [--verbose] [--goal "..."]
//	agent_review --backend codex  [--verbose] [--goal "..."] [--model gpt-5.2-codex]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

func main() {
	backend := flag.String("backend", "cursor", "Backend: cursor or codex")
	model := flag.String("model", "", "Model override (default: backend-specific)")
	verbose := flag.Bool("verbose", false, "Show tool call details")
	goal := flag.String("goal", "", "Review goal (default: infer from branch)")
	timeout := flag.Duration("timeout", 5*time.Minute, "Review timeout")
	flag.Parse()

	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	config := reviewer.Config{
		BackendType: reviewer.BackendType(*backend),
		WorkDir:     workDir,
		Model:       *model,
		Verbose:     *verbose,
	}

	r := reviewer.New(config)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nInterrupted, shutting down...")
		cancel()
	}()

	if err := r.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start reviewer: %v\n", err)
		os.Exit(1)
	}
	defer r.Stop()

	prompt := reviewer.BuildPrompt(*goal)
	result, err := r.ReviewWithResult(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n\n=== Review Result ===\n")
	fmt.Printf("Success: %v\n", result.Success)
	fmt.Printf("Duration: %dms\n", result.DurationMs)
	fmt.Printf("Response length: %d chars\n", len(result.ResponseText))
}
