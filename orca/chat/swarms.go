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

// HostSwarmLine renders a host swarm on one line: goal, lanes by phase, last
// activity and orchestrator. /runs, /adopt and the concierge's swarms tool
// share it, so they never describe a swarm differently.
func HostSwarmLine(r *adopt.Run, now time.Time) string {
	line := fmt.Sprintf("%s: %s | lanes: %s | last activity: %s | orchestrator: %s",
		r.ID, cmp.Or(strings.TrimSpace(r.Goal), "(no goal)"), hostPhases(r.Lanes), ago(r.LastActivity, now), cmp.Or(r.Orchestrator, "-"))
	if r.Holder != "" {
		line += " | held by " + r.Holder
	}
	return line
}

// hostPhases counts lanes by phase: "3 (build:1, review:2)".
func hostPhases(lanes []adopt.Lane) string {
	counts := map[string]int{}
	for i := range lanes {
		counts[cmp.Or(lanes[i].Phase, "-")]++
	}
	return countLine(len(lanes), counts)
}

// countLine renders a total and its sorted per-key counts.
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

// ago renders an RFC3339 timestamp with its age relative to now.
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

// SwarmList is every run on this machine chat can reach: the orca runs chat
// attaches to and the host swarms /adopt can take over.
//
//nolint:govet // field order follows the doc
type SwarmList struct {
	Orca    []store.Run
	OrcaErr error
	Host    []adopt.Run
	HostErr error
	// HostOff, when set, says why host swarms are not listed.
	HostOff string
}

// Render lists both groups under their own headings, one line per run.
// detail adds each host swarm lane on its own line.
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
			for j := range r.Lanes {
				ln := &r.Lanes[j]
				fmt.Fprintf(&b, "    - %s: phase=%s status=%s round=%d pr=%s %s\n",
					ln.ID, cmp.Or(ln.Phase, "-"), cmp.Or(ln.Status, "-"), ln.Round, prRef(ln.PR), ln.Title)
			}
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
