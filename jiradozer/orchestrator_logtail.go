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
const watchdogTickInterval = 30 * time.Second

// Example matched line:
//
//	I0504 22:00:54.425221 1350798 workflow.go:339] step: plan issue=...
var klogfmtLineRe = regexp.MustCompile(`^([IWED])\d{4} \d{2}:\d{2}:\d{2}\.\d+ +\d+ +([^\]]+)\] (.*)$`)

var keyValueRe = regexp.MustCompile(`(\w+)=("[^"]*"|\S+)`)
var prURLRe = regexp.MustCompile(`https://github\.com/[^\s"']+/pull/\d+`)

// tailSubprocessLog watches the per-issue log file, re-emits a narrow set of
// step-transition lines on the parent logger, and updates mw.lastOutputAt on
// every line so the watchdog can distinguish "agent slow but progressing"
// from "agent stuck."
//
// On any exit (open failure, non-EOF read error, or stop signal) it clears
// mw.tailerAlive so runWatchdog can skip its idle check — without fresh
// updates to lastOutputAt the gap would grow unboundedly and kill a
// still-healthy subprocess.
func (o *Orchestrator) tailSubprocessLog(mw *managedWorkflow, logPath string, stop <-chan struct{}) {
	defer mw.tailerAlive.Store(false)

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
			o.logger.Warn("log tailer: read error, watchdog disabled for this workflow",
				"issue", mw.issue.Identifier, "error", err)
			return
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

func (o *Orchestrator) idleTimeoutForStep(stepName string) time.Duration {
	if o.config == nil {
		return 0
	}
	if step, ok := o.config.StepByName(stepName); ok {
		return step.IdleTimeout
	}
	return 0
}

// runWatchdog cancels the workflow context when the gap between now and
// lastOutputAt exceeds the current step's IdleTimeout. The cancel propagates
// to cmd via exec.CommandContext, which cmd.Wait() then surfaces as
// StepCancelled.
//
// When the tailer goroutine has exited (mw.tailerAlive == false) the
// idle check is skipped: lastOutputAt is no longer being updated, so the
// gap would grow unboundedly and kill a subprocess that may still be
// healthy. The watchdog still drains its ticker until stop closes so the
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
