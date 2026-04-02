// Binary code-review runs a one-shot code review using an agent backend.
//
// Usage:
//
//	code-review --backend cursor [--verbose] [--goal "..."]
//	code-review --backend codex  [--verbose] [--goal "..."] [--model gpt-5.2-codex] [--effort medium]
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
	os.Exit(run())
}

func run() int {
	backend := flag.String("backend", "cursor", "Backend: cursor or codex")
	model := flag.String("model", "", "Model override (default: backend-specific)")
	effort := flag.String("effort", "", "Reasoning effort level for codex (low, medium, high)")
	sandbox := flag.String("sandbox", "", "Codex sandbox mode: read-only, workspace-write, danger-full-access (default: danger-full-access)")
	readOnly := flag.Bool("read-only", true, "Deny file writes via approval handler (Codex only; default true)")
	verbose := flag.Bool("verbose", false, "Show tool call details")
	goal := flag.String("goal", "", "Review goal (default: infer from branch)")
	timeout := flag.Duration("timeout", 5*time.Minute, "Review timeout")
	protocolLogDir := flag.String("protocol-log-dir", "", "Directory for protocol session logs (Codex only; also supports $BRAMBLE_PROTOCOL_LOG_DIR)")
	flag.Parse()

	if err := reviewer.ValidateBackend(*backend); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	workDir, err := reviewer.ResolveWorkDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	config := reviewer.Config{
		BackendType: reviewer.BackendType(*backend),
		WorkDir:     workDir,
		Model:       *model,
		Effort:      *effort,
		Sandbox:     *sandbox,
		ReadOnly:    *readOnly,
		Verbose:     *verbose,
	}

	logPath, err := reviewer.ResolveProtocolLogPath(*protocolLogDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	config.SessionLogPath = logPath

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
		return 1
	}
	defer r.Stop()

	prompt := reviewer.BuildPrompt(*goal)
	result, err := r.ReviewWithResult(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "\n=== Review Result ===\n")
	fmt.Fprintf(os.Stderr, "Success: %v\n", result.Success)
	fmt.Fprintf(os.Stderr, "Duration: %dms\n", result.DurationMs)
	fmt.Fprintf(os.Stderr, "Response length: %d chars\n", len(result.ResponseText))

	// Print the verdict to stdout unless both stdout and stderr are the
	// same interactive terminal (where the streamed output is already visible).
	// This covers: stdout piped/redirected, stderr redirected (e.g. 2>log),
	// both redirected, and any Stat() failure (default to printing).
	stdoutIsTTY := false
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		stdoutIsTTY = true
	}
	stderrIsTTY := false
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		stderrIsTTY = true
	}
	if !(stdoutIsTTY && stderrIsTTY) {
		fmt.Println(result.ResponseText)
	}
	return 0
}
