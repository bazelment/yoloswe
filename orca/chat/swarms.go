package chat

import (
	"cmp"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/store"
)

// HostSwarmLine renders the shared one-line host swarm summary.
func HostSwarmLine(r *adopt.Run, now time.Time) string {
	line := fmt.Sprintf("%s: %s | lanes: %s | last activity: %s | orchestrator: %s",
		r.ID, cmp.Or(strings.TrimSpace(r.Goal), "(no goal)"), hostPhases(r.Lanes), ago(r.LastActivity, now), cmp.Or(r.Orchestrator, "-"))
	if r.Format != "" {
		line += " | format: " + r.Format
	}
	switch r.State {
	case adopt.RunSuperseded:
		line += " | superseded by " + r.SupersededBy
	case "":
	default:
		line += " | " + r.State
	}
	if r.DrivenBy != "" {
		line += " | driven by " + r.DrivenBy
	}
	if r.Loop != "" {
		line += " | loop: " + r.Loop
	}
	if r.Holder != "" {
		line += " | held by " + r.Holder
	}
	return line
}

// hostPhases counts lanes by phase.
func hostPhases(lanes []adopt.Lane) string {
	counts := map[string]int{}
	for i := range lanes {
		counts[cmp.Or(lanes[i].Phase, "-")]++
	}
	return countLine(len(lanes), counts)
}

// countLine renders a total and sorted per-key counts.
func countLine(total int, counts map[string]int) string {
	if total == 0 {
		return "0"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s:%d", k, counts[k])
	}
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
}

// ago renders a timestamp and its age relative to now.
func ago(stamp string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return cmp.Or(stamp, "unknown")
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s (%s ago)", stamp, d.Round(time.Minute))
}

// orcaRunLine renders an orca run on one line.
func orcaRunLine(r *store.Run, now time.Time) string {
	line := fmt.Sprintf("%s: %s", r.ID, cmp.Or(strings.TrimSpace(r.Objective), "(no goal)"))
	if r.Lease != nil && !r.Lease.Expired(now) {
		line += " | controller running"
	}
	return line
}

// SwarmList contains reachable orca runs and adoptable host swarms.
//
//nolint:govet // field order follows the doc
type SwarmList struct {
	Orca    []store.Run
	OrcaErr error
	Host    []adopt.Run
	HostErr error
	// HostOff, when set, says why host swarms are not listed.
	HostOff string
	// Live holds what Probe read per host lane; nil when not probed.
	// ProbeOff says which live reads were off.
	Live     map[string]LaneLive
	ProbeOff []string
}

// Render lists both groups; detail adds each host lane's activity, next step
// and PR state, and per run what is running now and what was not checked.
func (l SwarmList) Render(now time.Time, detail bool) string {
	var b strings.Builder
	switch {
	case l.OrcaErr != nil:
		fmt.Fprintf(&b, "orca runs: unreadable: %v\n", l.OrcaErr)
	case len(l.Orca) == 0:
		b.WriteString("orca runs: none\n")
	default:
		fmt.Fprintf(&b, "orca runs (%d; /runs <id> attaches):\n", len(l.Orca))
		for i := range l.Orca {
			b.WriteString("  " + orcaRunLine(&l.Orca[i], now) + "\n")
		}
	}
	switch {
	case l.HostOff != "":
		fmt.Fprintf(&b, "host swarms: not listed: %s\n", l.HostOff)
	case len(l.Host) == 0 && l.HostErr == nil:
		b.WriteString("host swarms: none found\n")
	default:
		fmt.Fprintf(&b, "host swarms (%d; /adopt <id> takes one over):\n", len(l.Host))
		for i := range l.Host {
			r := &l.Host[i]
			b.WriteString("  " + HostSwarmLine(r, now) + "\n")
			if !detail {
				continue
			}
			l.renderHostDetail(&b, r)
		}
		if l.HostErr != nil {
			fmt.Fprintf(&b, "  unreadable: %v\n", l.HostErr)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func prRef(pr string) string {
	if pr == "" {
		return "-"
	}
	return "#" + pr
}
