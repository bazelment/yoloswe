// Package codereview provides a cobra subcommand for running one-shot code
// reviews using an agent backend.
package codereview

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

var (
	backend        string
	model          string
	effort         string
	sandbox        string
	readOnly       bool
	verbose        bool
	goal           string
	timeout        time.Duration
	protocolLogDir string
)

// Cmd is the cobra command for code review.
var Cmd = &cobra.Command{
	Use:   "code-review",
	Short: "Run a one-shot code review using an agent backend",
	Long: `Run a one-shot code review using an agent backend.

Supported backends: cursor, codex.`,
	Example: `  bramble code-review --backend cursor
  bramble code-review --backend codex --model gpt-5.2-codex --effort medium
  bramble code-review --backend cursor --verbose --goal "review auth changes"`,
	Args: cobra.NoArgs,
	RunE: runCodeReview,
}

func init() {
	Cmd.Flags().StringVar(&backend, "backend", "cursor", "Backend: cursor or codex")
	Cmd.Flags().StringVar(&model, "model", "", "Model override (default: backend-specific)")
	Cmd.Flags().StringVar(&effort, "effort", "", "Reasoning effort level for codex (low, medium, high)")
	Cmd.Flags().StringVar(&sandbox, "sandbox", "", "Codex sandbox mode: read-only, workspace-write, danger-full-access (default: danger-full-access)")
	Cmd.Flags().BoolVar(&readOnly, "read-only", false, "Deny file writes via approval handler (Codex only)")
	Cmd.Flags().BoolVar(&verbose, "verbose", false, "Show tool call details")
	Cmd.Flags().StringVar(&goal, "goal", "", "Review goal (default: infer from branch)")
	Cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Review timeout")
	Cmd.Flags().StringVar(&protocolLogDir, "protocol-log-dir", "", "Directory for protocol session logs (Codex only; also supports $BRAMBLE_PROTOCOL_LOG_DIR)")
}

func runCodeReview(cmd *cobra.Command, args []string) error {
	if err := reviewer.ValidateBackend(backend); err != nil {
		return err
	}

	workDir, err := reviewer.ResolveWorkDir()
	if err != nil {
		return err
	}

	config := reviewer.Config{
		BackendType: reviewer.BackendType(backend),
		WorkDir:     workDir,
		Model:       model,
		Effort:      effort,
		Sandbox:     sandbox,
		ReadOnly:    readOnly,
		Verbose:     verbose,
	}

	logPath, err := reviewer.ResolveProtocolLogPath(protocolLogDir)
	if err != nil {
		return err
	}
	config.SessionLogPath = logPath

	r := reviewer.New(config)

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := r.Start(ctx); err != nil {
		return fmt.Errorf("failed to start reviewer: %w", err)
	}
	defer r.Stop()

	prompt := reviewer.BuildPrompt(goal)
	result, err := r.ReviewWithResult(ctx, prompt)
	if err != nil {
		return fmt.Errorf("review failed: %w", err)
	}

	fmt.Printf("\n\n=== Review Result ===\n")
	fmt.Printf("Success: %v\n", result.Success)
	fmt.Printf("Duration: %dms\n", result.DurationMs)
	fmt.Printf("Response length: %d chars\n", len(result.ResponseText))
	return nil
}
