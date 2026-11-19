package toolbox

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
)

func currentTerminal(t task) string {
	if terminal := t.Terminals[t.Phase][fmt.Sprint(t.Round)]; terminal != "" {
		return terminal
	}
	return t.NebulaTerminal
}

func readDriver() (*nebula.Driver, error) {
	return nebula.NewFromEnv(nebula.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}))
}

func liveTerminal(ctx context.Context, t task) (string, error) {
	if t.SessionID == "" || currentTerminal(t) == "" {
		return "", refuse("lane %s has no current terminal", t.ID)
	}
	d, err := readDriver()
	if err != nil {
		return "", refuse("lane %s terminal liveness is unknown", t.ID)
	}
	state, err := d.TerminalAgentState(ctx, currentTerminal(t))
	if err != nil {
		return "", refuse("lane %s terminal liveness is unknown", t.ID)
	}
	return state, nil
}

// The driver supplies one fixed, read-only git/health probe. None of the skill's
// recovery/reaping helpers are safe substitutes for a report-only check.
func inspectLane(ctx context.Context, t task) map[string]any {
	v := map[string]any{"lane": t.ID, "status": t.Status, "terminal": currentTerminal(t), "terminal_state": "unknown", "terminal_live": nil, "dirty": nil, "unpushed": nil, "disk_used_percent": nil, "gate_or_review_busy": nil, "wait": t.WaitingOn}
	d, err := readDriver()
	if err != nil || t.SessionID == "" {
		v["unavailable"] = "no remote read transport or session"
		return v
	}
	if term, err := d.TerminalAgentState(ctx, currentTerminal(t)); err == nil {
		v["terminal_state"] = term
		v["terminal_live"] = term == "working" || term == "waiting"
	}
	ln := lane.Lane{ID: t.ID, SessionID: t.SessionID, TerminalID: currentTerminal(t), Branch: t.Branch, BaseBranch: t.Base}
	if git, err := d.LaneGitState(ctx, ln); err == nil {
		v["dirty"], v["unpushed"] = git.DirtyFiles, git.UnpushedCommits
	} else {
		v["probe"] = "unavailable"
	}
	if health, err := d.LaneSessionHealth(ctx, ln); err == nil {
		v["disk_used_percent"], v["gate_or_review_busy"] = health.DiskUsedPercent, health.BusyProcesses
	}
	return v
}
