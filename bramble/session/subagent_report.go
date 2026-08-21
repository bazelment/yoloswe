package session

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// subagentReportPrefix marks a courier-generated message so a reader can tell
// bramble's own reporting apart from a peer's prose.
const subagentReportPrefix = "[bramble]"

// shouldReport decides whether a child reaching status is worth telling its
// parent about, and records the decision so the same news is not sent twice.
//
// The rule is deliberately quiet. A subagent typically goes idle once, when its
// one-shot prompt is answered, and that is the report the parent is waiting
// for. A later completed/stopped — a tmux window closing, say — carries no new
// information, so it is only reported when nothing has been reported yet.
// A failure is always worth knowing, even after a successful report, because it
// changes what the parent should do next.
func (c *Courier) shouldReport(child SessionID, status SessionStatus) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := c.reportedForLocked(child)
	if seen[status] {
		return false
	}

	switch status {
	case StatusFailed:
		// Always worth a word.
	case StatusIdle:
		// The normal "here is your result" moment.
	case StatusCompleted, StatusStopped:
		// Only if the parent has heard nothing at all so far. Every entry that
		// is present is true — resetIdleReport deletes rather than clears — so
		// a non-empty set means something has been reported.
		if len(seen) > 0 {
			return false
		}
	default:
		return false
	}

	seen[status] = true
	return true
}

// reportedForLocked returns the child's reporting history, creating it on first
// use. The caller must hold c.mu.
func (c *Courier) reportedForLocked(child SessionID) map[SessionStatus]bool {
	seen := c.reported[child]
	if seen == nil {
		seen = make(map[SessionStatus]bool)
		c.reported[child] = seen
	}
	return seen
}

// noteChildSpoke records that a child has messaged its parent directly, so the
// courier does not then repeat itself with a generated report. A subagent that
// writes its own summary always says it better than this file can.
func (c *Courier) noteChildSpoke(child SessionID) {
	if child == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := c.reportedForLocked(child)
	// Mark the states a self-report stands in for. A later failure still
	// reports, because the child's own message predates it.
	seen[StatusIdle] = true
	seen[StatusCompleted] = true
	seen[StatusStopped] = true
}

// resetIdleReport re-arms idle reporting for a session that has just been sent
// a message.
//
// Reporting is per-turn, not per-lifetime. A subagent goes idle once when its
// opening prompt is answered and is reported then; but if its parent replies,
// the child starts another turn and the answer to *that* is news the parent is
// waiting on just as much. Without this, a two-way conversation goes silent
// after the first exchange and the parent is left polling — the thing the
// queue exists to avoid.
func (c *Courier) resetIdleReport(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seen := c.reported[to]; seen != nil {
		delete(seen, StatusIdle)
	}
}

// forgetChild drops a finished child's reporting history so the map does not
// grow for the life of the process.
func (c *Courier) forgetChild(child SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reported, child)
}

// reportToParent tells a child's parent that the child reached status.
//
// This is what makes a non-Claude backend usable as a subagent. Codex has no
// system prompt, no MCP and no tool restrictions in this wrapper, so a codex
// child cannot be reliably instructed to call back — the Reporting section in
// its prompt is a suggestion it may ignore. Generating the report from the
// session's own state means the parent hears back regardless of which CLI ran,
// and regardless of whether the child cooperated.
func (c *Courier) reportToParent(ctx context.Context, child SessionInfo) {
	if child.ParentSessionID == "" {
		return
	}
	// Check the parent can still take a message before composing one:
	// resultPathFor captures two thousand lines of pane and writes them to a
	// file, all of it discarded if the parent has already gone.
	parent, ok := c.target.SessionInfo(child.ParentSessionID)
	if !ok || parent.Status.IsTerminal() {
		return
	}
	if !c.shouldReport(child.ID, child.Status) {
		return
	}
	resultPath := c.resultPathFor(child)
	if _, err := c.Send(ctx, child.ID, child.ParentSessionID, formatSubagentReport(child, resultPath), true); err != nil {
		logDeliveryWarn("failed to report subagent completion", child.ParentSessionID, err)
	}
}

// tmuxCaptureLines is how much scrollback a tmux subagent's result file keeps.
// Generous: it is the only record of what that session did, and a pane that
// scrolled past this is lost either way.
const tmuxCaptureLines = 2000

// resultPathFor picks the file a parent should read for a child's output.
//
// A plan is the most specific artifact, then a transcript. A tmux-mode child
// has neither: that mode returns from runSession as soon as the window is up,
// so no turn loop ever records what the session said. Its pane is captured to a
// file here so the parent still gets a path instead of being told to go and
// look at a window that will scroll away.
func (c *Courier) resultPathFor(child SessionInfo) string {
	if child.PlanFilePath != "" {
		return child.PlanFilePath
	}
	if child.ResearchFilePath != "" {
		return child.ResearchFilePath
	}
	if !isTmuxRunner(child.RunnerType) {
		return ""
	}

	lines, err := c.target.CapturePaneText(child.ID, tmuxCaptureLines)
	if err != nil || len(lines) == 0 {
		// Not worth failing the report over — the parent still learns the
		// child finished, just without a pointer to read.
		return ""
	}
	path, err := ResultFilePath(child.ID)
	if err != nil {
		return ""
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		logDeliveryWarn("failed to write captured pane", child.ID, err)
		return ""
	}
	return path
}

// formatSubagentReport renders the message a parent receives.
//
// It is a pointer, not a payload: a one-line headline plus the path to the
// child's full output. Pasting a transcript into a parent's prompt would burn
// its context on text it may not need, and a pane scrolls away while a file
// does not — the same reason the delegator reads a child's research file
// instead of capturing its screen.
func formatSubagentReport(child SessionInfo, resultPath string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s subagent %s (%s", subagentReportPrefix, child.ID, child.Type)
	if child.Model != "" {
		fmt.Fprintf(&b, ", %s", child.Model)
	}
	fmt.Fprintf(&b, ") is %s", child.Status)
	if child.Progress.TurnCount > 0 {
		fmt.Fprintf(&b, " — %d turn(s)", child.Progress.TurnCount)
		if child.Progress.TotalCostUSD > 0 {
			fmt.Fprintf(&b, ", $%.4f", child.Progress.TotalCostUSD)
		}
	}

	if child.ErrorMsg != "" {
		fmt.Fprintf(&b, "\nerror: %s", child.ErrorMsg)
	}
	if resultPath != "" {
		label := "result"
		if resultPath == child.PlanFilePath {
			label = "plan"
		}
		fmt.Fprintf(&b, "\n%s: %s", label, resultPath)
	}

	return b.String()
}
