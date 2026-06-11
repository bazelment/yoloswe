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

const logTailPollInterval = 250 * time.Millisecond

// watchdogTickInterval is the cadence at which runWatchdog evaluates
// the idle gap. The actual cancel latency is up to one tick interval
// LATER than the configured IdleTimeout (worst case: idle just after
// a tick, gap crosses threshold during the next interval). Operators
// who set a tight idle_timeout should account for this slack — for
// the bootstrap defaults (5m+ timeouts) the 30s slack is noise, but
// configs with sub-minute timeouts will see proportionally larger
// effective bounds.
const watchdogTickInterval = 30 * time.Second

// logTailMaxReopenAttempts caps how many times tailSubprocessLog will try
// to reopen the log file after a non-EOF read error before giving up and
// disabling the watchdog. A transient FS hiccup should not permanently
// blind hang detection; a persistent failure should not loop forever.
const logTailMaxReopenAttempts = 3

// logTailOpener opens a log file path. Tests overwrite this to drive
// the non-EOF read-error / reopen branch deterministically; production
// always uses os.Open. Must return a seekable reader so reopen can
// resume from the prior offset rather than replaying the file.
var logTailOpener = func(path string) (io.ReadSeekCloser, error) {
	return os.Open(path)
}

// Example matched line:
//
//	I0504 22:00:54.425221 1350798 workflow.go:339] step: plan issue=...
var klogfmtLineRe = regexp.MustCompile(`^([IWED])\d{4} \d{2}:\d{2}:\d{2}\.\d+ +\d+ +([^\]]+)\] (.*)$`)

var keyValueRe = regexp.MustCompile(`(\w+)=("[^"]*"|\S+)`)

// prURLRe matches PR URLs from any GitHub host: github.com, GitHub
// Enterprise (github.example.com, gh.acme.io), or self-hosted gh.* URLs
// the gh CLI prints. The /pull/<digits> tail is what makes a URL a PR
// link, not the hostname; constraining to github.com would silently
// hide Enterprise PR URLs from the parent log.
var prURLRe = regexp.MustCompile(`https?://[^\s"']+/pull/\d+`)

// tailSubprocessLog watches the per-issue log file, re-emits a narrow set of
// step-transition lines on the parent logger, and updates mw.lastOutputAt on
// every line so the watchdog can distinguish "agent slow but progressing"
// from "agent stuck."
//
// On a non-EOF read error it tries to reopen the log file up to
// logTailMaxReopenAttempts times so a transient FS hiccup does not
// permanently disable hang detection. Only when reopens are exhausted
// (or initial open fails, or stop is signalled) does it clear
// mw.tailerAlive — runWatchdog then skips its idle check, since
// lastOutputAt would otherwise grow unboundedly and kill a healthy
// subprocess.
func (o *Orchestrator) tailSubprocessLog(mw *managedWorkflow, logPath string, stop <-chan struct{}) {
	defer mw.tailerAlive.Store(false)

	f, err := logTailOpener(logPath)
	if err != nil {
		// The tailer is the watchdog's only signal; without it a hung
		// subprocess would never get cancelled. Start() always creates
		// the log file with O_CREATE before the tailer runs, so this
		// branch is reachable only on serious failures (EACCES, EMFILE,
		// FS error). Fail closed: cancel the workflow context so cmd
		// gets SIGINT and Wait() surfaces the failure.
		o.logger.Error("log tailer: open failed, cancelling workflow",
			"issue", mw.issue.Identifier, "path", logPath, "error", err)
		if mw.cancel != nil {
			mw.cancel()
		}
		return
	}

	emittedPRURL := false
	reopens := 0
	// offset tracks bytes consumed so far, including any partial line
	// that ReadString returned with the error (we seek past it on
	// reopen so the next read picks up cleanly at the next newline or
	// EOF). Without this, a transient read error followed by reopen
	// would replay every line from byte 0 — duplicate "step:"
	// transitions, false PR URL emissions, and an inReview flip-flop.
	var offset int64
	reader := bufio.NewReader(f)
	for {
		select {
		case <-stop:
			f.Close()
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if line != "" {
			offset += int64(len(line))
			mw.lastOutputAt.Store(time.Now().UnixNano())
			mw.appendTail(line)
			if o.maybeEmitTransition(mw, line, !emittedPRURL) {
				emittedPRURL = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				select {
				case <-stop:
					f.Close()
					return
				case <-time.After(logTailPollInterval):
					continue
				}
			}
			// Non-EOF read error: try to reopen so transient FS issues
			// don't permanently disable hang detection.
			reopens++
			if reopens > logTailMaxReopenAttempts {
				o.logger.Warn("log tailer: read error after retries, watchdog disabled for this workflow",
					"issue", mw.issue.Identifier, "error", err, "reopens", reopens-1)
				f.Close()
				return
			}
			o.logger.Warn("log tailer: read error, attempting reopen",
				"issue", mw.issue.Identifier, "error", err, "attempt", reopens, "offset", offset)
			f.Close()
			select {
			case <-stop:
				return
			case <-time.After(logTailPollInterval):
			}
			f, err = logTailOpener(logPath)
			if err != nil {
				o.logger.Warn("log tailer: reopen failed, watchdog disabled for this workflow",
					"issue", mw.issue.Identifier, "error", err)
				return
			}
			// Resume from the byte offset we had consumed so the tailer
			// does not replay already-parsed step transitions.
			if _, seekErr := f.Seek(offset, io.SeekStart); seekErr != nil {
				o.logger.Warn("log tailer: seek after reopen failed, watchdog disabled",
					"issue", mw.issue.Identifier, "error", seekErr, "offset", offset)
				f.Close()
				return
			}
			reader = bufio.NewReader(f)
		}
	}
}

// maybeEmitTransition parses one log line and re-emits it on the parent
// logger if it matches the allow-list. Returns true when a PR URL was
// re-emitted so the caller can stop allowing further URL emissions
// (PR URLs appear in several lines around create_pr).
func (o *Orchestrator) maybeEmitTransition(mw *managedWorkflow, rawLine string, allowPRURL bool) bool {
	line := strings.TrimRight(rawLine, "\r\n")
	emittedPRURL := false
	// Cheap Contains() prefilter avoids running the regex on every agent
	// text line (the dominant case before create_pr).
	if allowPRURL && strings.Contains(line, "/pull/") {
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
	msg, kv := splitMsgAndKV(m[3])
	if !isAllowListedMsg(msg) {
		// Most klog lines are agent text/tool calls — skip parseKV entirely.
		return emittedPRURL
	}
	fields := parseKV(kv)

	switch {
	case strings.HasPrefix(msg, "step: "):
		stepName := strings.TrimPrefix(msg, "step: ")
		o.recordStepTransition(mw, stepName)
		// New step starting: review window (if any) is now closed.
		mw.inReview.Store(false)
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
		// Workflow has entered a human-review wait (PollForFeedback);
		// suppress the watchdog so the prior step's idle_timeout does
		// not cancel a legitimate long review.
		mw.inReview.Store(true)
		args := []any{"issue", mw.issue.Identifier}
		if v, ok := fields["step"]; ok {
			args = append(args, "step", v)
		}
		o.logger.Info("subprocess waiting for approval", args...)

	case strings.HasPrefix(msg, "feedback: "):
		// Reviewer responded; subprocess is about to resume real work.
		mw.inReview.Store(false)
		decision := strings.TrimPrefix(msg, "feedback: ")
		args := []any{"issue", mw.issue.Identifier, "decision", decision}
		if v, ok := fields["step"]; ok {
			args = append(args, "step", v)
		}
		o.logger.Info("subprocess feedback", args...)
	}

	return emittedPRURL
}

func isAllowListedMsg(msg string) bool {
	return strings.HasPrefix(msg, "step: ") ||
		msg == "step completed" ||
		msg == "waiting for approval" ||
		strings.HasPrefix(msg, "feedback: ")
}

func (o *Orchestrator) recordStepTransition(mw *managedWorkflow, stepName string) {
	mw.stepMu.Lock()
	mw.currentStep = stepName
	mw.stepMu.Unlock()
}

// idleTimeoutForStep returns the configured idle timeout for stepName, or
// a conservative fallback when the step is unknown.
//
// The startup window — between subprocess Start() and the first parsed
// "step:" line — has an empty currentStep, so a stuck child that never
// emits any log line would otherwise escape detection. We use the FIRST
// configured step's IdleTimeout (plan's, in the bootstrap shape) as the
// startup window: a child that never even reaches its first step is
// presumed stuck within roughly the time plan would take, not the
// loosest cap of any later step. Falling back to the max across all
// steps would let a startup hang sit silent for ~20 minutes under the
// bootstrap defaults — much weaker than the watchdog promises.
//
// Returns 0 when no step has a positive IdleTimeout — that is the
// "watchdog disabled by config" signal runWatchdog already understands.
func (o *Orchestrator) idleTimeoutForStep(stepName string) time.Duration {
	if o.config == nil {
		return 0
	}
	if step, ok := o.config.StepByName(stepName); ok {
		return step.IdleTimeout
	}
	return o.startupIdleTimeout()
}

// startupIdleTimeout returns the IdleTimeout to apply during the
// pre-first-step window. Uses plan's timeout when set; falls back to
// the next configured step in workflow order so this still works for
// configs that disable plan or use an alternative pipeline shape.
func (o *Orchestrator) startupIdleTimeout() time.Duration {
	for _, name := range StepNames() {
		if step, ok := o.config.StepByName(name); ok && step.IdleTimeout > 0 {
			return step.IdleTimeout
		}
	}
	return 0
}

// runWatchdog cancels the workflow context when the gap between now and
// lastOutputAt exceeds the current step's IdleTimeout. The cancel propagates
// to cmd via exec.CommandContext, which cmd.Wait() then surfaces as
// StepCancelled.
//
// Skip conditions:
//   - mw.tailerAlive=false: tailer has exited, lastOutputAt is no longer
//     being updated, so the gap would grow unboundedly and kill a
//     subprocess that may still be healthy.
//   - mw.inReview=true: workflow is blocked in PollForFeedback waiting
//     for human approval; the prior step's idle_timeout must not cancel
//     a legitimate review wait.
//
// The watchdog still drains its ticker until stop closes so the
// cmd.Wait goroutine can join cleanly.
func (o *Orchestrator) runWatchdog(mw *managedWorkflow, tickInterval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if !mw.tailerAlive.Load() {
				continue
			}
			if mw.inReview.Load() {
				continue
			}
			mw.stepMu.Lock()
			step := mw.currentStep
			mw.stepMu.Unlock()
			timeout := o.idleTimeoutForStep(step)
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
			// Mark hung before cancelling so the cmd.Wait goroutine reports
			// this as a failure (stuck agent) rather than an expected
			// cancellation. Set before cancel() to avoid a race where Wait
			// returns and inspects the flag before we write it.
			mw.hung.Store(true)
			if mw.cancel != nil {
				mw.cancel()
			}
			return
		}
	}
}

// releaseLockLabel removes the LockLabel from the issue. Uses a fresh
// background context so an operator's Ctrl+C (which cancelled the workflow
// context) does not also strand this cleanup call.
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

// splitMsgAndKV splits a klog body at the first ` <word>=`. The split is
// reliable for our log sites because none of them put literal `=` after a
// word boundary in the message text.
func splitMsgAndKV(body string) (msg, kv string) {
	loc := keyValueRe.FindStringIndex(body)
	if loc == nil {
		return strings.TrimSpace(body), ""
	}
	end := loc[0]
	for end > 0 && body[end-1] == ' ' {
		end--
	}
	return strings.TrimSpace(body[:end]), body[loc[0]:]
}

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
