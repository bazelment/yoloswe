package jiradozer

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// logTailPollInterval is the gap between EOF retries when tailing a subprocess
// log. Short enough that operators see step transitions promptly, long enough
// that an idle workflow doesn't burn a CPU.
const logTailPollInterval = 250 * time.Millisecond

// watchdogTickInterval is how often the watchdog re-checks the idle gap.
// The check itself is cheap (one atomic load + duration compare), so erring
// toward responsiveness over efficiency makes sense.
const watchdogTickInterval = 30 * time.Second

// klogfmtLineRe extracts a klog/slog-style log line's leading severity letter
// (I/W/E/D), the source file, and the message body. The body keeps key=value
// pairs intact for downstream extraction.
//
// Example matched line:
//
//	I0504 22:00:54.425221 1350798 workflow.go:339] step: plan issue=...
var klogfmtLineRe = regexp.MustCompile(`^([IWED])\d{4} \d{2}:\d{2}:\d{2}\.\d+ +\d+ +([^\]]+)\] (.*)$`)

// keyValueRe extracts key=value pairs from a klogfmt body. Values may be
// bare tokens or double-quoted strings; we accept both.
var keyValueRe = regexp.MustCompile(`(\w+)=("[^"]*"|\S+)`)

// prURLRe captures GitHub pull-request URLs anywhere in a line.
var prURLRe = regexp.MustCompile(`https://github\.com/[^\s"']+/pull/\d+`)

// tailSubprocessLog watches the per-issue log file and re-emits a narrow set
// of step-transition lines on the parent logger. It also updates
// mw.lastOutputAt on every line (regardless of allow-list match) so the
// watchdog can distinguish "agent slow but progressing" from "agent stuck."
//
// The goroutine exits when stop is closed or the file is removed. Errors
// other than EOF are logged at debug level — the tailer is best-effort and
// must not crash the parent.
func (o *Orchestrator) tailSubprocessLog(mw *managedWorkflow, logPath string, stop <-chan struct{}) {
	f, err := os.Open(logPath)
	if err != nil {
		o.logger.Debug("log tailer: open failed",
			"issue", mw.issue.Identifier, "path", logPath, "error", err)
		return
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	emittedPRURL := false
	for {
		select {
		case <-stop:
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if line != "" {
			mw.lastOutputAt.Store(time.Now().UnixNano())
			if o.maybeEmitTransition(mw, line, !emittedPRURL) {
				emittedPRURL = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				select {
				case <-stop:
					return
				case <-time.After(logTailPollInterval):
					continue
				}
			}
			o.logger.Debug("log tailer: read error",
				"issue", mw.issue.Identifier, "error", err)
			return
		}
	}
}

// maybeEmitTransition parses one log line and re-emits it on the parent
// logger if it matches the allow-list. Returns true when a PR URL was
// re-emitted; the caller uses this to enforce once-per-workflow semantics.
// allowPRURL guards the URL emission so we don't repeat it after the first
// time (PR URLs typically appear in several lines around create_pr).
//
// Allow-list (matched on the slog `msg` body):
//   - "step: <name>" → records currentStep + IdleTimeout, emits "subprocess step"
//   - "step completed" → emits "subprocess step completed" with duration
//   - "waiting for approval" → emits "subprocess waiting for approval"
//   - "feedback: ..." → emits "subprocess feedback"
//   - any line containing a github.com/.../pull/N URL → emits "subprocess pr_url" once
//
// Unknown lines are silently dropped — they still updated lastOutputAt for
// the watchdog, which is the only signal the rest of the orchestrator needs.
func (o *Orchestrator) maybeEmitTransition(mw *managedWorkflow, rawLine string, allowPRURL bool) bool {
	line := strings.TrimRight(rawLine, "\r\n")
	emittedPRURL := false
	if allowPRURL {
		if url := prURLRe.FindString(line); url != "" {
			o.logger.Info("subprocess pr_url",
				"issue", mw.issue.Identifier, "url", url)
			emittedPRURL = true
		}
	}
	m := klogfmtLineRe.FindStringSubmatch(line)
	if m == nil {
		return emittedPRURL
	}
	body := m[3]

	// Extract the slog `msg` field. klog renders the message as the first
	// token after the bracket, followed by key=value pairs. We take
	// everything up to the first ` key=` boundary.
	msg, kv := splitMsgAndKV(body)
	fields := parseKV(kv)

	switch {
	case strings.HasPrefix(msg, "step: "):
		stepName := strings.TrimPrefix(msg, "step: ")
		o.recordStepTransition(mw, stepName)
		args := []any{"issue", mw.issue.Identifier, "step", stepName}
		if v, ok := fields["resume"]; ok {
			args = append(args, "resume", v)
		}
		if v, ok := fields["feedback"]; ok {
			args = append(args, "feedback", v)
		}
		o.logger.Info("subprocess step", args...)

	case msg == "step completed":
		args := []any{"issue", mw.issue.Identifier}
		if v, ok := fields["step"]; ok {
			args = append(args, "step", v)
		}
		if v, ok := fields["duration"]; ok {
			args = append(args, "duration", v)
		}
		o.logger.Info("subprocess step completed", args...)

	case msg == "waiting for approval":
		args := []any{"issue", mw.issue.Identifier}
		if v, ok := fields["step"]; ok {
			args = append(args, "step", v)
		}
		o.logger.Info("subprocess waiting for approval", args...)

	case strings.HasPrefix(msg, "feedback: "):
		decision := strings.TrimPrefix(msg, "feedback: ")
		args := []any{"issue", mw.issue.Identifier, "decision", decision}
		if v, ok := fields["step"]; ok {
			args = append(args, "step", v)
		}
		o.logger.Info("subprocess feedback", args...)
	}

	return emittedPRURL
}

// recordStepTransition stores the current step name and looks up its idle
// timeout from config so the watchdog can use the right threshold.
func (o *Orchestrator) recordStepTransition(mw *managedWorkflow, stepName string) {
	mw.stepMu.Lock()
	mw.currentStep = stepName
	mw.currentStepIdleTimeout = o.idleTimeoutForStep(stepName)
	mw.stepMu.Unlock()
}

// idleTimeoutForStep returns the configured IdleTimeout for a named step,
// or 0 if the step is unknown or has no timeout set (watchdog disabled).
func (o *Orchestrator) idleTimeoutForStep(stepName string) time.Duration {
	if o.config == nil {
		return 0
	}
	if step, ok := o.config.StepByName(stepName); ok {
		return step.IdleTimeout
	}
	return 0
}

// runWatchdog ticks every tickInterval and cancels the workflow's context
// if the gap between now and lastOutputAt exceeds the current step's
// IdleTimeout. The cancel triggers SIGINT via cmd.Cancel, which the
// existing cmd.Wait() goroutine handles as StepCancelled.
//
// Exits when stop is closed (subprocess already exited).
func (o *Orchestrator) runWatchdog(mw *managedWorkflow, tickInterval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			mw.stepMu.Lock()
			step := mw.currentStep
			timeout := mw.currentStepIdleTimeout
			mw.stepMu.Unlock()
			if timeout <= 0 {
				continue
			}
			lastNanos := mw.lastOutputAt.Load()
			if lastNanos == 0 {
				continue
			}
			gap := time.Since(time.Unix(0, lastNanos))
			if gap < timeout {
				continue
			}
			o.logger.Error("subprocess hung — cancelling",
				"issue", mw.issue.Identifier,
				"step", step,
				"idle_for", gap.Round(time.Second),
				"idle_timeout", timeout,
			)
			if mw.cancel != nil {
				mw.cancel()
			}
			return
		}
	}
}

// releaseLockLabel removes the LockLabel from the issue. Called from cleanup
// on every termination path (StepDone, StepCancelled, StepFailed) so the
// label does not leak and block re-discovery. Best-effort: tracker errors
// are logged at warn level (mirrors the AddLabel pattern in claimIssueInProgress).
//
// Uses a fresh background context with a short timeout so a cancelled parent
// context does not also strand the cleanup call (which is the case that
// matters most — operator hits Ctrl+C, we still want the label removed).
func (o *Orchestrator) releaseLockLabel(mw *managedWorkflow) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := o.tracker.RemoveLabel(ctx, mw.issue.ID, LockLabel); err != nil {
		o.logger.Warn("failed to remove lock label",
			"issue", mw.issue.Identifier, "label", LockLabel, "error", err)
		return
	}
	o.logger.Info("released issue: removed lock label",
		"issue", mw.issue.Identifier, "label", LockLabel)
}

// splitMsgAndKV separates a klog message body into its leading message text
// and the trailing key=value cluster. We split at the first occurrence of
// ` <word>=`, which is reliable as long as messages don't contain literal
// `=` after a word boundary — true for every site we care about.
func splitMsgAndKV(body string) (msg, kv string) {
	loc := keyValueRe.FindStringIndex(body)
	if loc == nil {
		return strings.TrimSpace(body), ""
	}
	// Walk back over whitespace so the message doesn't include a trailing space.
	end := loc[0]
	for end > 0 && body[end-1] == ' ' {
		end--
	}
	return strings.TrimSpace(body[:end]), body[loc[0]:]
}

// parseKV extracts a flat key=value map from the trailing portion of a klog
// line. Quoted values have their surrounding quotes stripped.
func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, m := range keyValueRe.FindAllStringSubmatch(s, -1) {
		v := m[2]
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		out[m[1]] = v
	}
	return out
}
