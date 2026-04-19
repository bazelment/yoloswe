// Package codereview provides a cobra subcommand for running one-shot code
// reviews using an agent backend.
package codereview

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

var (
	backend           string
	model             string
	effort            string
	sandbox           string
	readOnly          bool
	verbose           bool
	goal              string
	timeout           time.Duration
	protocolLogDir    string
	jsonOutput        bool
	skipTestExecution bool
)

// Cmd is the cobra command for code review.
var Cmd = &cobra.Command{
	Use:   "code-review",
	Short: "Run a one-shot code review using an agent backend",
	Long: `Run a one-shot code review using an agent backend.

Supported backends: cursor, codex.

Output:
  Default:      free-form review text on stdout, diagnostics on stderr.
  --json:       a stable JSON envelope on stdout (one object, trailing newline).
                Use this for automated pipelines (e.g. /pr-polish).

Every run also writes a structured klogfmt log to
~/.bramble/logs/code-review/code-review-{timestamp}-{pid}.log for later
analysis. Set $BRAMBLE_RUN_TAG to tag the log with an external run id.`,
	Example: `  bramble code-review --backend cursor
  bramble code-review --backend codex --model gpt-5.2-codex --effort medium
  bramble code-review --backend codex --json --skip-test-execution --goal "review auth changes"`,
	Args: cobra.NoArgs,
	RunE: runCodeReview,
}

func init() {
	Cmd.Flags().StringVar(&backend, "backend", "cursor", "Backend: cursor or codex")
	Cmd.Flags().StringVar(&model, "model", "", "Model override (default: backend-specific)")
	Cmd.Flags().StringVar(&effort, "effort", "", "Reasoning effort level for codex (low, medium, high)")
	Cmd.Flags().StringVar(&sandbox, "sandbox", "", "Codex sandbox mode: read-only, workspace-write, danger-full-access (default: danger-full-access)")
	Cmd.Flags().BoolVar(&readOnly, "read-only", true, "Deny file writes via approval handler (Codex only; default true)")
	Cmd.Flags().BoolVar(&verbose, "verbose", false, "Show tool call details")
	Cmd.Flags().StringVar(&goal, "goal", "", "Review goal (default: infer from branch)")
	Cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Review timeout")
	Cmd.Flags().StringVar(&protocolLogDir, "protocol-log-dir", "", "Directory for protocol session logs (Codex only; also supports $BRAMBLE_PROTOCOL_LOG_DIR)")
	Cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit a machine-readable JSON envelope on stdout instead of free-form prose")
	Cmd.Flags().BoolVar(&skipTestExecution, "skip-test-execution", false, "Instruct the reviewer not to run tests/build commands (caller runs them separately)")
}

func runCodeReview(cmd *cobra.Command, args []string) error {
	runStart := time.Now()
	logPath, logClose, logErr := reviewer.SetupRunLog()
	defer logClose()
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "[code-review] run log setup failed: %v\n", logErr)
	} else if logPath != "" {
		fmt.Fprintf(os.Stderr, "[code-review] logging run to %s\n", logPath)
	}

	if err := reviewer.ValidateBackend(backend); err != nil {
		return emitEarlyFailure(err, "")
	}

	workDir, err := reviewer.ResolveWorkDir()
	if err != nil {
		return emitEarlyFailure(err, "")
	}

	slog.Info("code-review run start",
		"pid", os.Getpid(),
		"cwd", workDir,
		"backend", backend,
		"model", model,
		"effort", effort,
		"sandbox", sandbox,
		"read_only", readOnly,
		"timeout", timeout.String(),
		"json_mode", jsonOutput,
		"skip_test_execution", skipTestExecution,
		"goal", goal)

	config := reviewer.Config{
		BackendType:       reviewer.BackendType(backend),
		WorkDir:           workDir,
		Model:             model,
		Effort:            effort,
		Sandbox:           sandbox,
		ReadOnly:          readOnly,
		Verbose:           verbose,
		JSONOutput:        jsonOutput,
		SkipTestExecution: skipTestExecution,
	}

	logPath2, err := reviewer.ResolveProtocolLogPath(protocolLogDir)
	if err != nil {
		return emitEarlyFailure(err, "")
	}
	config.SessionLogPath = logPath2

	r := reviewer.New(config)
	effectiveModel := r.EffectiveModel()

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := r.Start(ctx); err != nil {
		slog.Error("reviewer start failed", "error", err.Error())
		return emitEarlyFailure(fmt.Errorf("failed to start reviewer: %w", err), effectiveModel)
	}
	defer r.Stop()

	var prompt string
	if jsonOutput {
		prompt = reviewer.BuildJSONPromptWithOptions(goal, skipTestExecution)
	} else {
		prompt = reviewer.BuildPromptWithOptions(goal, skipTestExecution)
	}
	result, err := r.ReviewWithResult(ctx, prompt)
	if err != nil {
		slog.Error("review failed", "error", err.Error())
		if jsonOutput {
			// Still emit a parseable envelope so the caller can distinguish
			// a bramble-level failure from a reviewer-level "rejected".
			env := reviewer.BuildEnvelope(&reviewer.ReviewResult{
				ErrorMessage: err.Error(),
			}, reviewer.BackendType(backend), effectiveModel, r.LastSessionID())
			_ = reviewer.PrintJSONResult(os.Stdout, env)
		}
		return fmt.Errorf("review failed: %w", err)
	}

	if jsonOutput {
		env := reviewer.BuildEnvelope(result, reviewer.BackendType(backend), effectiveModel, r.LastSessionID())
		fmt.Fprintf(os.Stderr, "\n=== Review Result ===\n")
		fmt.Fprintf(os.Stderr, "Status: %s\n", env.Status)
		fmt.Fprintf(os.Stderr, "Duration: %dms\n", env.DurationMs)
		fmt.Fprintf(os.Stderr, "Tokens: %d in / %d out\n", env.InputTokens, env.OutputTokens)
		fmt.Fprintf(os.Stderr, "Response length: %d chars\n", len(result.ResponseText))
		slog.Info("code-review run exit",
			"status", string(env.Status),
			"verdict", env.Review.Verdict,
			"issue_count", len(env.Review.Issues),
			"max_severity", maxSeverity(env.Review.Issues),
			"total_duration_ms", time.Since(runStart).Milliseconds())
		if err := reviewer.PrintJSONResult(os.Stdout, env); err != nil {
			return fmt.Errorf("print json result: %w", err)
		}
		return nil
	}

	reviewer.PrintResultSummary(result)
	slog.Info("code-review run exit",
		"success", result.Success,
		"total_duration_ms", time.Since(runStart).Milliseconds())
	return nil
}

// maxSeverity returns the highest severity label in issues, using the order
// critical > high > medium > low. Unknown (non-empty, unrecognized) labels
// rank above "low" so they remain visible in logs instead of being silently
// downgraded to "". Empty when issues is empty.
func maxSeverity(issues []reviewer.ReviewIssue) string {
	rank := map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}
	const unknownRank = 1 // tied with "low": flagged but no worse than low
	best := ""
	bestRank := 0
	for _, issue := range issues {
		if issue.Severity == "" {
			continue
		}
		r, known := rank[issue.Severity]
		if !known {
			r = unknownRank
		}
		if r > bestRank {
			best = issue.Severity
			bestRank = r
		}
	}
	return best
}

// emitEarlyFailure reports a pre-review failure to the caller. When --json is
// set it also writes a minimal error envelope to stdout so automation sees a
// single stable output shape regardless of where the failure occurred.
// effectiveModel is the model after reviewer.New defaults were applied; pass
// "" when the reviewer hasn't been constructed yet.
func emitEarlyFailure(err error, effectiveModel string) error {
	if jsonOutput {
		env := reviewer.BuildEnvelope(&reviewer.ReviewResult{
			ErrorMessage: err.Error(),
		}, reviewer.BackendType(backend), effectiveModel, "")
		_ = reviewer.PrintJSONResult(os.Stdout, env)
	}
	return err
}
