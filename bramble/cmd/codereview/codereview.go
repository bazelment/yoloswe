// Package codereview provides a cobra subcommand for running one-shot code
// reviews using an agent backend.
package codereview

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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
	envelopeFile      string
	skipTestExecution bool
)

// Cmd is the cobra command for code review.
var Cmd = &cobra.Command{
	Use:   "code-review",
	Short: "Run a one-shot code review using an agent backend",
	Long: `Run a one-shot code review using an agent backend.

Supported backends: cursor, codex.

Output:
  Default:         NDJSON progress events on stdout, final envelope also on stdout
                   (last line with "schema_version"). Diagnostics on stderr.
  --envelope-file: Write the final ResultEnvelope to a file instead of stdout.
                   stdout then carries only progress events — ideal for the
                   Monitor tool, which streams stdout line-by-line.

Every run also writes a structured klogfmt log to
~/.bramble/logs/code-review/code-review-{timestamp}-{pid}.log for later
analysis. Set $BRAMBLE_RUN_TAG to tag the log with an external run id.`,
	Example: `  bramble code-review --backend cursor
  bramble code-review --backend codex --model gpt-5.2-codex --effort medium
  bramble code-review --backend codex --envelope-file /tmp/envelope.json --skip-test-execution --goal "review auth changes"`,
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
	Cmd.Flags().StringVar(&envelopeFile, "envelope-file", "", "Write the JSON ResultEnvelope to this file instead of stdout (stdout then carries only progress events)")
	Cmd.Flags().BoolVar(&skipTestExecution, "skip-test-execution", false, "Instruct the reviewer not to run tests/build commands (caller runs them separately)")
}

func runCodeReview(cmd *cobra.Command, args []string) (retErr error) {
	runStart := time.Now()
	// envelopeWritten tracks whether the envelope has already been flushed. A
	// top-level defer uses it to guarantee exactly one envelope is written
	// (to stdout or --envelope-file) even on panic or unexpected return.
	// Without the guard a silent exit leaves automation (e.g. /pr-polish)
	// unable to distinguish "run succeeded with zero findings" from "run
	// produced nothing at all".
	var envelopeWritten bool
	emitEnvelope := func(env reviewer.ResultEnvelope) {
		w, closeW, openErr := openEnvelopeWriter()
		if openErr != nil {
			fmt.Fprintf(os.Stderr, "[code-review] failed to open envelope-file: %v\n", openErr)
			if retErr == nil {
				retErr = fmt.Errorf("failed to open envelope-file: %w", openErr)
			}
			return
		}
		defer closeW()
		if err := reviewer.PrintJSONResult(w, env); err != nil {
			reportEnvelopePrintError(err)
			if retErr == nil {
				retErr = fmt.Errorf("failed to write JSON envelope: %w", err)
			}
			return
		}
		envelopeWritten = true
	}
	defer func() {
		finalizeEnvelope(envelopeGuardArgs{
			backend:         backend,
			envelopeWritten: &envelopeWritten,
			retErr:          &retErr,
			panicVal:        recover(),
			emit:            emitEnvelope,
		})
	}()

	logPath, logClose, logErr := reviewer.SetupRunLog()
	defer logClose()
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "[code-review] run log setup failed: %v\n", logErr)
	} else if logPath != "" {
		fmt.Fprintf(os.Stderr, "[code-review] logging run to %s\n", logPath)
	}

	if err := reviewer.ValidateBackend(backend); err != nil {
		return emitEarlyFailure(err, "", emitEnvelope)
	}

	workDir, err := reviewer.ResolveWorkDir()
	if err != nil {
		return emitEarlyFailure(err, "", emitEnvelope)
	}

	slog.Info("code-review run start",
		"pid", os.Getpid(),
		"cwd", redactPath(workDir),
		"backend", backend,
		"model", model,
		"effort", effort,
		"sandbox", sandbox,
		"read_only", readOnly,
		"timeout", timeout.String(),
		"envelope_file", envelopeFile != "",
		"skip_test_execution", skipTestExecution,
		"goal_len", len(goal))

	config := reviewer.Config{
		BackendType:       reviewer.BackendType(backend),
		WorkDir:           workDir,
		Model:             model,
		Effort:            effort,
		Sandbox:           sandbox,
		ReadOnly:          readOnly,
		Verbose:           verbose,
		SkipTestExecution: skipTestExecution,
	}

	logPath2, err := reviewer.ResolveProtocolLogPath(protocolLogDir)
	if err != nil {
		return emitEarlyFailure(err, "", emitEnvelope)
	}
	config.SessionLogPath = logPath2

	r := reviewer.New(config)
	// Snapshot before Start for early-failure paths. After the backend
	// session begins (OnSessionInfo), call r.EffectiveModel() fresh so the
	// envelope reports the model the backend actually ran (Cursor picks its
	// own default when --model is empty).
	earlyModel := r.EffectiveModel()

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := r.Start(ctx); err != nil {
		slog.Error("reviewer start failed", "error", err.Error())
		return emitEarlyFailure(fmt.Errorf("failed to start reviewer: %w", err), earlyModel, emitEnvelope)
	}
	defer r.Stop()

	prompt := reviewer.BuildJSONPromptWithOptions(goal, skipTestExecution)
	result, err := r.ReviewWithResult(ctx, prompt)
	if err != nil {
		slog.Error("review failed", "error", err.Error())
		// Emit a parseable envelope so the caller can distinguish a
		// bramble-level failure from a reviewer-level "rejected".
		env := reviewer.BuildEnvelope(&reviewer.ReviewResult{
			ErrorMessage: err.Error(),
		}, reviewer.BackendType(backend), r.EffectiveModel(), r.LastSessionID())
		emitEnvelope(env)
		return fmt.Errorf("review failed: %w", err)
	}

	env := reviewer.BuildEnvelope(result, reviewer.BackendType(backend), r.EffectiveModel(), r.LastSessionID())
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
	// Print a plain-text verdict line so the Monitor tool can surface the
	// outcome to Claude before the envelope is written.
	if env.Status == reviewer.StatusOK {
		fmt.Fprintf(os.Stdout, "verdict: %s (%d issues)\n", env.Review.Verdict, len(env.Review.Issues))
	} else {
		fmt.Fprintf(os.Stdout, "error: %s\n", env.Error)
	}
	emitEnvelope(env)
	return retErr
}

// maxSeverity returns the highest severity label in issues, using the order
// critical > high > medium > low. Unknown (non-empty, unrecognized) labels
// rank above "low" so they remain visible in logs instead of being silently
// downgraded to "". Empty when issues is empty.
func maxSeverity(issues []reviewer.ReviewIssue) string {
	rank := map[string]int{"critical": 5, "high": 4, "medium": 3, "low": 1}
	const unknownRank = 2 // above "low" so non-standard labels stay visible
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

// redactPath collapses an absolute working directory to the basename plus a
// length marker. Per-run logs live on disk and are easy to share; persisting
// the full developer path leaks home-directory layout, project locations, and
// branch worktree structure. The basename is enough to disambiguate runs in
// the same project without exposing the rest.
func redactPath(p string) string {
	if p == "" {
		return ""
	}
	return fmt.Sprintf("<redacted:%d>/%s", len(p), filepath.Base(p))
}

// openEnvelopeWriter returns the writer to use for the JSON envelope and a
// close function. When --envelope-file is set, it opens/creates the file;
// otherwise it returns os.Stdout with a no-op close. The caller must always
// invoke close() after writing.
func openEnvelopeWriter() (w *os.File, close func(), err error) {
	if envelopeFile == "" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(envelopeFile)
	if err != nil {
		return nil, func() {}, err
	}
	return f, func() { _ = f.Close() }, nil
}

// envelopeGuardArgs is the input to finalizeEnvelope. Extracted so tests can
// drive the guard without spinning up a real reviewer. All pointer fields are
// read and mutated — the caller owns the storage.
type envelopeGuardArgs struct {
	panicVal        any
	envelopeWritten *bool
	retErr          *error
	emit            func(reviewer.ResultEnvelope)
	backend         string
}

// finalizeEnvelope is the body of the top-level defer in runCodeReview. It
// guarantees exactly one envelope is written before the function returns, and
// re-panics so the process exit code still reflects the original failure.
func finalizeEnvelope(a envelopeGuardArgs) {
	if *a.envelopeWritten && a.panicVal == nil {
		return
	}
	msg := "bramble code-review exited without producing a review"
	switch {
	case a.panicVal != nil:
		msg = fmt.Sprintf("panic in code-review: %v", a.panicVal)
		if *a.retErr == nil {
			*a.retErr = fmt.Errorf("%s", msg)
		}
	case *a.retErr != nil:
		msg = (*a.retErr).Error()
	}
	if !*a.envelopeWritten {
		env := reviewer.BuildEnvelope(&reviewer.ReviewResult{ErrorMessage: msg},
			reviewer.BackendType(a.backend), "", "")
		a.emit(env)
	}
	if a.panicVal != nil {
		// Re-raise so the process still exits non-zero; the envelope is
		// already written for automation to parse.
		panic(a.panicVal)
	}
}

// emitEarlyFailure reports a pre-review failure to the caller. It always
// writes a minimal error envelope so automation sees a single stable output
// shape regardless of where the failure occurred. effectiveModel is the model
// after reviewer.New defaults were applied; pass "" when the reviewer hasn't
// been constructed yet. emit is the envelope emitter from the runCodeReview
// scope; it flips the envelopeWritten flag so the top-level defer guard does
// not double-emit.
func emitEarlyFailure(err error, effectiveModel string, emit func(reviewer.ResultEnvelope)) error {
	env := reviewer.BuildEnvelope(&reviewer.ReviewResult{
		ErrorMessage: err.Error(),
	}, reviewer.BackendType(backend), effectiveModel, "")
	emit(env)
	return err
}

// reportEnvelopePrintError surfaces a write failure to the operator. Once
// SetupRunLog runs, slog.Default() is rebound to a file-only handler; using
// slog here would write the message to disk where the operator won't see it.
// Writing directly to os.Stderr guarantees the message reaches the terminal
// regardless of slog redirection, while a parallel slog.Error keeps the same
// record in the per-run log for forensics.
func reportEnvelopePrintError(printErr error) {
	fmt.Fprintf(os.Stderr, "[code-review] failed to write JSON envelope: %v\n", printErr)
	slog.Error("print json envelope", "error", printErr.Error())
}
