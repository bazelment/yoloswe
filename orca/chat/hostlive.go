package chat

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bazelment/yoloswe/orca/adopt"
)

// HostProbe reads the live state behind a host swarm summary. A nil field is
// reported as not checked, never guessed.
type HostProbe struct {
	// TerminalState returns a Nebula terminal's live agent state.
	TerminalState func(ctx context.Context, terminalID string) (string, error)
	// PR reads a PR from GitHub. repo is owner/name, or "" to resolve it
	// from the git checkout at dir.
	PR func(ctx context.Context, repo, dir, number string) (PRStatus, error)
	// Off says why a nil field is off, e.g. "terminal states: no Nebula client".
	Off []string
}

// PRStatus is a PR as GitHub reports it.
type PRStatus struct {
	// State is OPEN, MERGED or CLOSED.
	State string
	// Checks summarizes the head's checks, e.g. "3 passed, 1 pending".
	Checks string
	Draft  bool
}

func (p PRStatus) String() string {
	s := strings.ToLower(cmp.Or(p.State, "unknown"))
	if p.Draft && p.State == "OPEN" {
		s = "open draft"
	}
	if p.Checks != "" {
		s += ", checks: " + p.Checks
	}
	return s
}

// LaneLive is what a probe read for one host lane; an Err field holds why
// that read failed.
type LaneLive struct {
	PR                      *PRStatus
	Agent, AgentErr, PRErr  string
	agentChecked, prChecked bool
}

func liveKey(run, lane string) string { return run + "\x00" + lane }

// laneActive reports whether a ledger status may still have work running.
func laneActive(status string) bool {
	switch status {
	case "done", "failed", "needs-human":
		return false
	}
	return true
}

// idleAgent reports whether a terminal agent state means no work runs.
func idleAgent(state string) bool {
	switch strings.ToLower(state) {
	case "idle", "exited", "stopped", "released", "ended", "done", "completed", "finished", "sleeping":
		return true
	}
	return false
}

// Probe reads each host lane's terminal agent state (lanes the ledger still
// runs) and PR state (every lane with a PR) into l.Live.
func (l *SwarmList) Probe(ctx context.Context, p HostProbe) {
	l.Live = map[string]LaneLive{}
	l.ProbeOff = p.Off
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range l.Host {
		r := &l.Host[i]
		for j := range r.Lanes {
			ln := r.Lanes[j]
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				var live LaneLive
				if p.TerminalState != nil && ln.Terminal != "" && laneActive(ln.Status) {
					live.agentChecked = true
					st, err := p.TerminalState(ctx, ln.Terminal)
					if err != nil {
						live.AgentErr = err.Error()
					} else {
						live.Agent = cmp.Or(st, "unknown")
					}
				}
				if p.PR != nil && ln.PR != "" {
					live.prChecked = true
					pr, err := p.PR(ctx, r.Repo, r.Dir, ln.PR)
					if err != nil {
						live.PRErr = err.Error()
					} else {
						live.PR = &pr
					}
				}
				mu.Lock()
				l.Live[liveKey(r.ID, ln.ID)] = live
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
}

// renderHostDetail writes a host run's per-lane activity, next step and PR
// state, then what is running now and what was not checked.
func (l SwarmList) renderHostDetail(b *strings.Builder, r *adopt.Run) {
	fmt.Fprintf(b, "    state: %s | driven by: %s | step loop: %s\n", runState(r), cmp.Or(r.DrivenBy, "unknown"), cmp.Or(r.Loop, loopNone(r)))
	var running, unknown, notChecked []string
	notChecked = append(notChecked, r.Unchecked...)
	if l.Live == nil {
		notChecked = append(notChecked, "terminal agent states and PR states (no live probe)")
	} else {
		notChecked = append(notChecked, l.ProbeOff...)
	}
	for j := range r.Lanes {
		ln := &r.Lanes[j]
		live, probed := l.Live[liveKey(r.ID, ln.ID)]
		fmt.Fprintf(b, "    - %s: phase=%s status=%s round=%d pr=%s %s\n",
			ln.ID, cmp.Or(ln.Phase, "-"), cmp.Or(ln.Status, "-"), ln.Round, prRef(ln.PR), ln.Title)
		now := []string{fmt.Sprintf("%s round %d", cmp.Or(ln.Phase, "-"), max(ln.Round, 1))}
		switch {
		case ln.Terminal == "":
			now = append(now, "no terminal recorded")
		case !laneActive(ln.Status):
			now = append(now, "terminal "+ln.Terminal+" (lane "+ln.Status+", not probed)")
		case probed && live.agentChecked && live.AgentErr == "":
			now = append(now, "terminal "+ln.Terminal+" agent="+live.Agent)
		case probed && live.agentChecked:
			now = append(now, "terminal "+ln.Terminal+" agent state unreadable: "+live.AgentErr)
		default:
			now = append(now, "terminal "+ln.Terminal+" agent state not checked")
		}
		if ln.TerminalState != "" {
			now = append(now, "ledger terminal_state="+ln.TerminalState)
		}
		if ln.LastEvent != "" {
			now = append(now, "last event: "+ln.LastEvent)
		}
		if ln.Report != "" {
			now = append(now, "latest report "+filepath.Base(ln.ReportPath)+": "+ln.Report)
		}
		fmt.Fprintf(b, "        now: %s\n", strings.Join(now, "; "))
		fmt.Fprintf(b, "        next: %s\n", cmp.Or(ln.Next, nextDefault(ln)))
		if ln.PR != "" {
			switch {
			case probed && live.PR != nil:
				fmt.Fprintf(b, "        pr #%s (GitHub): %s\n", ln.PR, live.PR)
			case probed && live.prChecked:
				fmt.Fprintf(b, "        pr #%s (GitHub): unreadable: %s\n", ln.PR, live.PRErr)
				notChecked = append(notChecked, "PR #"+ln.PR+" state ("+live.PRErr+")")
			default:
				fmt.Fprintf(b, "        pr #%s: GitHub state not checked\n", ln.PR)
			}
		}
		if !laneActive(ln.Status) {
			continue
		}
		switch {
		case probed && live.agentChecked && live.AgentErr == "" && !idleAgent(live.Agent):
			running = append(running, fmt.Sprintf("%s (%s, agent %s)", ln.ID, cmp.Or(ln.Phase, "-"), live.Agent))
		case probed && live.agentChecked && live.AgentErr == "":
			// Checked idle.
		case ln.Terminal == "":
			unknown = append(unknown, ln.ID+" (status "+ln.Status+", no terminal recorded)")
		case probed && live.agentChecked:
			unknown = append(unknown, ln.ID)
			notChecked = append(notChecked, "terminal "+ln.Terminal+" of "+ln.ID+" ("+live.AgentErr+")")
		default:
			unknown = append(unknown, ln.ID)
		}
	}
	if s := oneLine(r.Escalations); s != "" {
		fmt.Fprintf(b, "    escalations: %s\n", s)
	}
	if s := oneLine(r.Progress); s != "" {
		fmt.Fprintf(b, "    progress: %s\n", s)
	}
	switch {
	case len(running) > 0:
		fmt.Fprintf(b, "    running now: %s\n", strings.Join(running, ", "))
	case r.Loop != "":
		fmt.Fprintf(b, "    running now: the step loop (%s)\n", r.Loop)
	case r.State == adopt.RunSuperseded:
		fmt.Fprintf(b, "    running now: nothing here; orchestrator %s moved on to run %s, ask about that run\n", r.DrivenBy, r.SupersededBy)
	case len(unknown) > 0:
		fmt.Fprintf(b, "    running now: unknown for %s\n", strings.Join(unknown, ", "))
	case len(notChecked) > 0 && r.State == adopt.RunActive:
		fmt.Fprintf(b, "    running now: unknown (see not checked)\n")
	default:
		fmt.Fprintf(b, "    running now: nothing: every lane is finished or its terminal checked idle, and the orchestrator has no newer run\n")
	}
	if len(running) > 0 && len(unknown) > 0 {
		fmt.Fprintf(b, "    also unknown: %s\n", strings.Join(unknown, ", "))
	}
	if len(notChecked) > 0 {
		fmt.Fprintf(b, "    not checked: %s\n", strings.Join(notChecked, "; "))
	}
}

func runState(r *adopt.Run) string {
	switch r.State {
	case adopt.RunSuperseded:
		return fmt.Sprintf("superseded by %s (its orchestrator now drives that run)", r.SupersededBy)
	case "":
		return "unknown"
	}
	return r.State
}

func loopNone(r *adopt.Run) string {
	for _, u := range r.Unchecked {
		if strings.HasPrefix(u, "step loop") {
			return "not checked"
		}
	}
	return "none running"
}

func nextDefault(ln *adopt.Lane) string {
	if !laneActive(ln.Status) {
		return "nothing (lane " + ln.Status + ")"
	}
	return "not recorded"
}

// oneLine joins a file's non-empty lines with " / ", clipped.
func oneLine(s string) string {
	var parts []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	out := strings.Join(parts, " / ")
	if len(out) > 600 {
		out = out[:600] + "…"
	}
	return out
}
