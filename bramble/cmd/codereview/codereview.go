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
	"strings"
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
	idleTimeout       time.Duration
	protocolLogDir    string
	envelopeFile      string
	skipTestExecution bool
	scopeHintsFile    string
	resumeSessionID   string
	resumePromptStyle string
	reviewMode        string
	rubricFile        string
)

type promptStyle string

const (
	promptStyleFresh    promptStyle = "fresh"
	promptStyleFollowUp promptStyle = "follow-up"
)

// Cmd is the cobra command for code review.
var Cmd = &cobra.Command{
	Use:          "code-review",
	SilenceUsage: true,
	Short:        "Run a one-shot code review using an agent backend",
	Long: `Run a one-shot code review using an agent backend.

Supported backends: cursor, codex, gemini.

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
  bramble code-review --backend codex --model gpt-5.4-mini --effort medium
  bramble code-review --backend codex --envelope-file /tmp/envelope.json --skip-test-execution --goal "review auth changes"`,
	Args: cobra.NoArgs,
	RunE: runCodeReview,
}

func init() {
	Cmd.Flags().StringVar(&backend, "backend", "cursor", "Backend: cursor, codex, or gemini")
	Cmd.Flags().StringVar(&model, "model", "", "Model override (default: backend-specific)")
	Cmd.Flags().StringVar(&effort, "effort", "", "Reasoning effort level for codex (low, medium, high)")
	Cmd.Flags().StringVar(&sandbox, "sandbox", "", "Codex sandbox mode: read-only, workspace-write, danger-full-access (default: danger-full-access)")
	Cmd.Flags().BoolVar(&readOnly, "read-only", true, "Deny file writes via approval handler (Codex only; default true)")
	Cmd.Flags().BoolVar(&verbose, "verbose", false, "Show tool call details")
	Cmd.Flags().StringVar(&goal, "goal", "", "Review goal (default: infer from branch)")
	Cmd.Flags().DurationVar(&timeout, "timeout", 0, "Absolute hard cap on the whole review (0 = none; rely on --idle-timeout). A review making steady progress is bounded only by --idle-timeout.")
	Cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 3*time.Minute, "Kill the review after this much inactivity (no stream events). Resets on every event, so it only trips a stalled backend. 0 disables.")
	Cmd.Flags().StringVar(&protocolLogDir, "protocol-log-dir", "", "Directory for protocol session logs (Codex only; also supports $BRAMBLE_PROTOCOL_LOG_DIR)")
	Cmd.Flags().StringVar(&envelopeFile, "envelope-file", "", "Write the JSON ResultEnvelope to this file instead of stdout (stdout then carries only progress events)")
	Cmd.Flags().BoolVar(&skipTestExecution, "skip-test-execution", false, "Instruct the reviewer not to run tests/build commands (caller runs them separately)")
	Cmd.Flags().StringVar(&scopeHintsFile, "scope-hints-file", "", "JSON file with co-located test paths and cross-service packages to widen review scope; see reviewer.ScopeHints. Missing/malformed files log a warning and fall back to today's narrow review.")
	Cmd.Flags().StringVar(&resumeSessionID, "resume-session-id", "", "Resume an existing backend session/thread id")
	Cmd.Flags().StringVar(&resumePromptStyle, "resume-prompt-style", "fresh", "Prompt style when resuming: follow-up or fresh. Auto-promotes to follow-up when --resume-session-id is set without an explicit style.")
	Cmd.Flags().StringVar(&reviewMode, "review-mode", "code", "Review mode: code (default; reviewer.ReviewModeCode) or design-doc (reviewer.ReviewModeDesignDoc).")
	Cmd.Flags().StringVar(&rubricFile, "review-rubric-file", "", "Path to a rubric file (one grilling question per non-blank line). Required for --review-mode design-doc; rejected for --review-mode code.")
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
			// --envelope-file path is unwritable. Don't return empty —
			// codex round 12 caught that finalizeEnvelope would then call
			// emitEnvelope a second time for the synthesized fallback,
			// which would hit the same broken sink and leave automation
			// with no machine-readable result at all. Last-ditch fallback:
			// dump the envelope to stdout so the orchestrator at least
			// has something parseable on the streamed channel.
			slog.Error("failed to open envelope-file; falling back to stdout", "error", openErr.Error())
			if retErr == nil {
				retErr = fmt.Errorf("failed to open envelope-file: %w", openErr)
			}
			if printErr := reviewer.PrintJSONResult(os.Stdout, env); printErr != nil {
				reportEnvelopePrintError(printErr)
				// stdout itself failed — nothing more we can do.
				return
			}
			envelopeWritten = true
			return
		}
		defer closeW()
		if err := reviewer.PrintJSONResult(w, env); err != nil {
			reportEnvelopePrintError(err)
			if retErr == nil {
				retErr = fmt.Errorf("failed to write JSON envelope: %w", err)
			}
			// Mid-write failure leaves the file in an indeterminate state
			// (partial JSON or empty after O_TRUNC). Same fallback as the
			// open-failure branch: emit the envelope to stdout so the
			// orchestrator's stdout-streaming path still gets the result.
			if printErr := reviewer.PrintJSONResult(os.Stdout, env); printErr != nil {
				reportEnvelopePrintError(printErr)
				return
			}
			envelopeWritten = true
			return
		}
		// Mark written only after a successful flush. A partial write would
		// be detected by PrintJSONResult and surface above; a clean write
		// trips the flag so finalizeEnvelope's idempotency guard fires.
		envelopeWritten = true
	}
	// activeReviewer is observed by the deferred guard so the synthesized
	// panic/error envelope can report the reviewer's authoritative
	// resume_status. It's nil until r := reviewer.New(...) runs below; the
	// guard's callback handles that case by falling back to Unverified
	// whenever --resume-session-id was set.
	var activeReviewer *reviewer.Reviewer
	// resolvedMode is updated once validateModeFlags returns. Captured by
	// reference into the deferred guard so a panic before mode resolution
	// labels its synthesized envelope with the empty (i.e. code-default)
	// mode, while a panic afterwards correctly carries the resolved mode.
	var resolvedMode reviewer.ReviewMode
	defer func() {
		finalizeEnvelope(envelopeGuardArgs{
			backend:         backend,
			envelopeWritten: &envelopeWritten,
			retErr:          &retErr,
			panicVal:        recover(),
			emit:            emitEnvelope,
			mode:            resolvedMode,
			resumeStatus:    func() reviewer.ResumeStatus { return effectiveResumeStatus(activeReviewer, resumeSessionID) },
		})
	}()

	logPath, logClose, logErr := reviewer.SetupRunLog()
	defer logClose()
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "[code-review] run log setup failed: %v\n", logErr)
	} else if logPath != "" {
		fmt.Fprintf(os.Stderr, "[code-review] logging run to %s\n", logPath)
	}

	// requestedMode echoes the operator's --review-mode literal back into
	// every pre-validation early-failure envelope, so an orchestrator
	// triaging with --mode design-doc doesn't reject a backend-validation
	// or workdir-resolution failure as "explicit mode doesn't match
	// envelope". When the literal is not one of the known modes,
	// fall through to ReviewModeCode — the envelope's `error` field
	// will already name the actual problem.
	requestedMode := requestedModeOrCode(reviewMode)

	if err := reviewer.ValidateBackend(backend); err != nil {
		return emitEarlyFailure(err, "", requestedMode, emitEnvelope)
	}

	workDir, err := reviewer.ResolveWorkDir()
	if err != nil {
		return emitEarlyFailure(err, "", requestedMode, emitEnvelope)
	}

	mode, err := validateModeFlags(reviewMode, scopeHintsFile, rubricFile, skipTestExecution)
	if err != nil {
		// Tag the failure envelope with the operator's *requested*
		// mode when it's a known literal. Without this, an orchestrator
		// triaging with --mode design-doc rejects a code-mode-tagged
		// failure as "explicit mode doesn't match envelope" — surfacing
		// a misleading error instead of the actual flag-validation
		// problem (e.g. "design-doc requires --review-rubric-file").
		return emitEarlyFailure(err, model, requestedMode, emitEnvelope)
	}
	resolvedMode = mode

	style, err := normalizePromptStyle(resumeSessionID, resumePromptStyle, cmd.Flags().Changed("resume-prompt-style"))
	if err != nil {
		return emitEarlyFailure(err, model, mode, emitEnvelope)
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
		"scope_hints_file", scopeHintsFile != "",
		"resume_session", resumeSessionID != "",
		"resume_prompt_style", string(style),
		"review_mode", string(mode),
		"rubric_file", rubricFile != "",
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
		ResumeSessionID:   resumeSessionID,
		// Idle (inactivity) timeout is the primary stall-killer, enforced inside
		// the event bridge so a review making steady progress is never cut off.
		// Scoped to this reviewer instance via Config (not a package global) so
		// the CLI's opt-in can't impose a stall policy on other reviewer callers.
		IdleTimeout: idleTimeout,
	}

	logPath2, err := reviewer.ResolveProtocolLogPath(protocolLogDir)
	if err != nil {
		return emitEarlyFailure(err, "", mode, emitEnvelope)
	}
	config.SessionLogPath = logPath2

	r := reviewer.New(config)
	// Expose the reviewer to the deferred guard so a panic between here and
	// the normal envelope emit picks up the latest ResumeStatus instead of
	// dropping it. Setting it before any work that could panic guarantees the
	// guard never observes a stale nil.
	activeReviewer = r
	// Snapshot before Start for early-failure paths. After the backend
	// session begins (OnSessionInfo), call r.EffectiveModel() fresh so the
	// envelope reports the model the backend actually ran (Cursor picks its
	// own default when --model is empty).
	earlyModel := r.EffectiveModel()

	// The absolute --timeout below is an optional belt-and-suspenders hard cap;
	// the primary stall-killer is Config.IdleTimeout (set above), enforced
	// inside the event bridge so a review making steady progress is never cut off.
	ctx := cmd.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := r.Start(ctx); err != nil {
		slog.Error("reviewer start failed", "error", err.Error())
		return emitEarlyFailure(fmt.Errorf("failed to start reviewer: %w", err), earlyModel, mode, emitEnvelope)
	}
	defer r.Stop()

	prompt, err := buildPromptForRun(mode, goal, scopeHintsFile, rubricFile, skipTestExecution, style)
	if err != nil {
		return emitEarlyFailure(err, r.EffectiveModel(), mode, emitEnvelope)
	}
	result, err := r.ReviewWithResult(ctx, prompt)
	if err != nil {
		slog.Error("review failed", "error", err.Error())
		// Emit a parseable envelope so the caller can distinguish a
		// bramble-level failure from a reviewer-level "rejected". Use
		// effectiveResumeStatus so a turn that errored before
		// r.resumeStatus was repopulated still surfaces Unverified
		// when resume was requested — same fallback the deferred
		// guard applies on the panic/silent-exit path.
		env := reviewer.BuildEnvelope(&reviewer.ReviewResult{
			ErrorMessage: err.Error(),
			ResumeStatus: effectiveResumeStatus(activeReviewer, resumeSessionID),
		}, reviewer.BackendType(backend), r.EffectiveModel(), r.LastSessionID(), mode)
		emitVerdictLine(env)
		emitEnvelope(env)
		return fmt.Errorf("review failed: %w", err)
	}

	env := reviewer.BuildEnvelope(result, reviewer.BackendType(backend), r.EffectiveModel(), r.LastSessionID(), mode)
	// Log when the auto-promoted follow-up prompt was sent to a fresh
	// fallback session. The follow-up prompt has an explicit "if no prior
	// context, treat as first-pass" escape hatch so the model still produces
	// usable output, but operators should still see the mismatch in logs
	// because it implies the review's prompt was less specific than intended
	// (the orchestrator asked to resume, the backend cold-started, and we
	// went ahead anyway). Triggers only on the production-default path:
	// follow-up auto-promoted + resume actually fell back.
	if style == promptStyleFollowUp && env.ResumeStatus == reviewer.ResumeStatusFallback {
		slog.Warn("follow-up prompt sent to fallback session; review used the prompt's no-prior-context escape hatch",
			"resume_session", resumeSessionID,
			"backend", backend)
	}
	slog.Info("code-review run exit",
		"status", string(env.Status),
		"verdict", env.Review.Verdict,
		"issue_count", len(env.Review.Issues),
		"max_severity", maxSeverity(env.Review.Issues),
		"total_duration_ms", time.Since(runStart).Milliseconds())
	emitVerdictLine(env)
	emitEnvelope(env)
	return retErr
}

// effectiveResumeStatus returns the ResumeStatus value the caller should
// stamp onto a synthesized envelope, falling back to Unverified whenever
// resume was requested but the reviewer's authoritative status hasn't
// landed yet. Reviewer.ResumeStatus() is cleared at the top of every turn
// and only repopulated from the backend's result, so any path that builds
// an envelope without a fully-completed turn (panic, silent exit, or the
// explicit ReviewWithResult error branch) needs this fallback or
// resume_status drops out of the JSON entirely via omitempty. The
// deferred-guard callback and the explicit error branch share this helper
// so a single rule covers both.
func effectiveResumeStatus(r *reviewer.Reviewer, requestedResumeSessionID string) reviewer.ResumeStatus {
	if r != nil {
		if status := r.ResumeStatus(); status != "" {
			return status
		}
	}
	if requestedResumeSessionID != "" {
		return reviewer.ResumeStatusUnverified
	}
	return ""
}

// emitVerdictLine prints a single human-readable summary to stdout so the
// Monitor tool can surface the outcome to Claude before the envelope file is
// flushed. When --resume-session-id was set, the line ends with a
// [resume=ok|fallback|unverified] suffix so callers streaming stdout can see
// resume health without parsing the envelope. Both the success path
// ("verdict: ...") and the bramble-level failure path ("error: ...") share
// this so resume signal isn't lost on early errors.
func emitVerdictLine(env reviewer.ResultEnvelope) {
	resumeSuffix := ""
	if env.ResumeStatus != "" {
		resumeSuffix = fmt.Sprintf(" [resume=%s]", env.ResumeStatus)
	}
	if env.Status == reviewer.StatusOK {
		fmt.Fprintf(os.Stdout, "verdict: %s (%d issues)%s\n", env.Review.Verdict, len(env.Review.Issues), resumeSuffix)
	} else {
		fmt.Fprintf(os.Stdout, "error: %s%s\n", env.Error, resumeSuffix)
	}
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
	// Index loop avoids the per-iteration value-copy lint (ReviewIssue
	// is ~152 bytes once Sites/Invariant landed).
	for i := range issues {
		sev := issues[i].Severity
		if sev == "" {
			continue
		}
		r, known := rank[sev]
		if !known {
			r = unknownRank
		}
		if r > bestRank {
			best = sev
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
	f, err := os.OpenFile(envelopeFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, func() {}, err
	}
	return f, func() { _ = f.Close() }, nil
}

// envelopeGuardArgs is the input to finalizeEnvelope. Extracted so tests can
// drive the guard without spinning up a real reviewer. All pointer fields are
// read and mutated — the caller owns the storage.
//
// resumeStatus is an optional accessor that the guard invokes lazily so the
// synthesized panic/error envelope carries the same resume_status the normal
// exit paths report. Leave it nil when no resume was requested (or when the
// caller doesn't want to thread one through, e.g. unit tests). When non-nil
// it must be safe to call from the deferred recovery path — typically a
// closure over the effectiveResumeStatus helper (see below), which returns
// r.ResumeStatus() when the reviewer reports a non-empty status, falls back
// to ResumeStatusUnverified when --resume-session-id was set but the
// reviewer's status is still empty (e.g. panic mid-turn before the backend
// repopulated it), and "" otherwise.
type envelopeGuardArgs struct {
	panicVal        any
	envelopeWritten *bool
	retErr          *error
	emit            func(reviewer.ResultEnvelope)
	resumeStatus    func() reviewer.ResumeStatus
	backend         string
	mode            reviewer.ReviewMode
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
		result := &reviewer.ReviewResult{ErrorMessage: msg}
		if a.resumeStatus != nil {
			// Mirror the resume signal the normal exit paths emit. Without
			// this, a resumed run that panics or returns silently drops the
			// new resume_status field — which is exactly the gap the round-2
			// eval flagged on this very file.
			result.ResumeStatus = a.resumeStatus()
		}
		env := reviewer.BuildEnvelope(result,
			reviewer.BackendType(a.backend), "", "", a.mode)
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
// been constructed yet. mode is the resolved review mode (or
// reviewer.ReviewModeCode when the failure happened before mode resolution);
// it labels the envelope so triage layers can dispatch correctly. emit is
// the envelope emitter from the runCodeReview scope; it flips the
// envelopeWritten flag so the top-level defer guard does not double-emit.
//
// When --resume-session-id was set on this run, the synthesized envelope
// reports resume_status=unverified so the orchestrator (and the verdict-line
// suffix) can distinguish "failed before the backend confirmed resume" from
// "no resume requested". Pre-review failures (backend validation, workdir
// resolution, prompt-style normalization, reviewer.Start, etc.) all flow
// through here, so without this every early-failure path would silently
// drop the resume signal.
func emitEarlyFailure(err error, effectiveModel string, mode reviewer.ReviewMode, emit func(reviewer.ResultEnvelope)) error {
	result := &reviewer.ReviewResult{ErrorMessage: err.Error()}
	if resumeSessionID != "" {
		result.ResumeStatus = reviewer.ResumeStatusUnverified
	}
	env := reviewer.BuildEnvelope(result, reviewer.BackendType(backend), effectiveModel, "", mode)
	emit(env)
	return err
}

// reportEnvelopePrintError surfaces a write failure to the operator. slog
// writes to both file and stderr (ERROR level) via the tee handler installed
// by SetupRunLog.
func reportEnvelopePrintError(printErr error) {
	slog.Error("print json envelope", "error", printErr.Error())
}

// buildPromptForRun composes the JSON-output review prompt for one
// runCodeReview turn. Splitting this off (rather than inlining
// loadPromptOptions + BuildJSONPromptWithScope at the call site) gives
// tests a single seam to drive end-to-end: they pass a real hints file
// path and assert the resulting prompt carries the expected scope clauses.
// Without this seam, a regression that quietly stops threading
// scopeHintsFile or rubricPath into the reviewer would slip through
// helper-level tests that only exercise loadPromptOptions in isolation.
//
// Returning an error (rather than logging-and-falling-back like the
// scope-hints path does) matters for design-doc mode: the rubric IS the
// review for that mode, so a missing/malformed rubric file must abort,
// not silently degrade.
func buildPromptForRun(mode reviewer.ReviewMode, goal, hintsPath, rubricPath string, skipTestExecution bool, style promptStyle) (string, error) {
	opts, err := loadPromptOptions(mode, hintsPath, rubricPath, skipTestExecution)
	if err != nil {
		return "", err
	}
	if style == promptStyleFollowUp {
		return reviewer.BuildFollowUpJSONPromptWithScope(goal, opts), nil
	}
	return reviewer.BuildJSONPromptWithScope(goal, opts), nil
}

func normalizePromptStyle(resumeSessionID, rawStyle string, styleExplicit bool) (promptStyle, error) {
	style := promptStyle(rawStyle)
	switch style {
	case promptStyleFresh, promptStyleFollowUp:
	default:
		return "", fmt.Errorf("invalid --resume-prompt-style %q (want fresh or follow-up)", rawStyle)
	}
	if resumeSessionID == "" {
		if style == promptStyleFollowUp {
			return "", fmt.Errorf("--resume-prompt-style=follow-up requires --resume-session-id")
		}
		return style, nil
	}
	if !styleExplicit && style == promptStyleFresh {
		return promptStyleFollowUp, nil
	}
	return style, nil
}

// loadPromptOptions builds the PromptOptions for one runCodeReview turn.
// It dispatches on mode:
//
//   - ReviewModeCode (or empty): reads the scope-hints file when set and
//     converts it to PromptOptions; a missing or malformed file logs a
//     warning and falls back to PromptOptions{SkipTestExecution: ...} —
//     the legacy narrow-review path. SkipTestExecution is honoured.
//
//   - ReviewModeDesignDoc: reads the rubric file (one question per
//     non-blank line, sanitized via SanitizePromptHint, capped at 20
//     entries / 500 chars per line) and builds PromptOptions{Mode,
//     Rubric}. Scope-hints/skip-test-execution are silently dropped at
//     the validation gate (validateModeFlags below) before this is even
//     reached, so we don't have to re-check them here.
//
// The fallback warning records only the basename of the hints file. The
// full path is the operator's own input and run logs are routinely shared
// across machines and PRs, so embedding the developer's worktree layout
// here would weaken the path-redaction hygiene used elsewhere in this file
// (see redactPath). LoadScopeHints itself also identifies the file by
// basename in its error text, so the slog "error" attribute is
// already path-clean.
func loadPromptOptions(mode reviewer.ReviewMode, hintsPath, rubricPath string, skipTestExecution bool) (reviewer.PromptOptions, error) {
	switch mode {
	case reviewer.ReviewModeDesignDoc:
		rubric, err := loadRubricFile(rubricPath)
		if err != nil {
			return reviewer.PromptOptions{}, err
		}
		return reviewer.PromptOptions{
			Mode:   reviewer.ReviewModeDesignDoc,
			Rubric: rubric,
		}, nil
	default:
		if hintsPath == "" {
			return reviewer.PromptOptions{SkipTestExecution: skipTestExecution}, nil
		}
		hints, err := reviewer.LoadScopeHints(hintsPath)
		if err != nil {
			slog.Warn("scope-hints file ignored, using narrow review",
				"file", filepath.Base(hintsPath),
				"error", err.Error())
			return reviewer.PromptOptions{SkipTestExecution: skipTestExecution}, nil
		}
		return hints.ToPromptOptions(skipTestExecution), nil
	}
}

// rubricLineMaxLen bounds the length of one rubric entry. 500 chars is
// enough for a multi-clause grilling question without risking a runaway
// prompt-token bill from a malformed file. Defence-in-depth: the prompt
// builder also caps the list length (rubricCap = 20).
const rubricLineMaxLen = 500

// loadRubricFile reads the rubric file and returns its non-blank lines as a
// list of grilling questions. Each line is trimmed; lines starting with
// '#' (markdown-comment convention) are skipped so authors can leave
// in-file notes. Empty result, an unreadable file, or a sanitization
// rejection all surface as an explicit error — design-doc mode without
// a rubric is unambiguously a misconfiguration, not a "fall back to
// defaults" situation.
func loadRubricFile(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("--review-mode design-doc requires --review-rubric-file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rubric file: %w", err)
	}
	var rubric []string
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(trimmed) > rubricLineMaxLen {
			return nil, fmt.Errorf("rubric line %d exceeds %d chars", i+1, rubricLineMaxLen)
		}
		if !reviewer.SanitizePromptHint(trimmed) {
			return nil, fmt.Errorf("rubric line %d failed sanitization (no leading markdown control chars, no newlines): %q", i+1, trimmed)
		}
		rubric = append(rubric, trimmed)
	}
	if len(rubric) == 0 {
		return nil, fmt.Errorf("rubric file %q has no non-blank, non-comment lines", filepath.Base(path))
	}
	if len(rubric) > 20 {
		return nil, fmt.Errorf("rubric file %q has %d entries; cap is 20", filepath.Base(path), len(rubric))
	}
	return rubric, nil
}

// validateModeFlags checks the --review-mode/--review-rubric-file flag pair
// and emits a warning when ignored flags are passed alongside design-doc
// mode. Returns (mode, error). An unknown --review-mode value is rejected.
//
// Ignored-flag warnings (not errors): --scope-hints-file in design-doc
// mode, --skip-test-execution in design-doc mode. Both are diff-derived
// signals that don't apply to a single doc; the operator probably copied a
// pr-polish-style invocation. Warning-not-error keeps existing automation
// scripts from breaking when they tack the flags on unconditionally.
// requestedModeOrCode parses a --review-mode flag literal into a
// ReviewMode for use in pre-validation early-failure envelopes. It does
// not validate combinations (that's validateModeFlags' job); it just
// echoes the user's intent back into the envelope so an orchestrator's
// auto-detect logic doesn't misroute a code-default failure when the
// user actually invoked design-doc mode.
//
// Unknown literals fall through to ReviewModeCode — the envelope's
// `error` field will already name the problem (e.g.
// "unknown --review-mode 'security-review'") so the wrong mode tag is
// not the load-bearing signal in that case.
func requestedModeOrCode(modeStr string) reviewer.ReviewMode {
	switch reviewer.ReviewMode(modeStr) {
	case reviewer.ReviewModeCode, reviewer.ReviewModeDesignDoc:
		return reviewer.ReviewMode(modeStr)
	}
	return reviewer.ReviewModeCode
}

func validateModeFlags(modeStr, hintsPath, rubricPath string, skipTestExec bool) (reviewer.ReviewMode, error) {
	switch reviewer.ReviewMode(modeStr) {
	case reviewer.ReviewModeCode, "":
		if rubricPath != "" {
			return "", fmt.Errorf("--review-rubric-file requires --review-mode design-doc")
		}
		return reviewer.ReviewModeCode, nil
	case reviewer.ReviewModeDesignDoc:
		if rubricPath == "" {
			return "", fmt.Errorf("--review-mode design-doc requires --review-rubric-file")
		}
		if hintsPath != "" {
			slog.Warn("--scope-hints-file ignored in design-doc mode",
				"file", filepath.Base(hintsPath))
		}
		if skipTestExec {
			slog.Warn("--skip-test-execution ignored in design-doc mode")
		}
		return reviewer.ReviewModeDesignDoc, nil
	default:
		return "", fmt.Errorf("unknown --review-mode %q (want code or design-doc)", modeStr)
	}
}
