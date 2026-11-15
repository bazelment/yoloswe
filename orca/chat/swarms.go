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

// compactGoalSummary collapses newlines in raw and bounds it to one summary line.
func compactGoalSummary(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "(no goal)"
	}
	summary := oneLineSummary(trimmed)
	if adopt.ObjectiveIsDone(trimmed) && !strings.Contains(strings.ToLower(summary), "done") {
		summary += " [Next: none / DONE]"
	}
	return summary
}

// HostSwarmLine renders the shared one-line host swarm summary.
func HostSwarmLine(r *adopt.Run, now time.Time) string {
	line := fmt.Sprintf("%s: %s | lanes: %s | last activity: %s | orchestrator: %s",
		r.ID, compactGoalSummary(r.Goal), hostPhases(r.Lanes), ago(r.LastActivity, now), cmp.Or(r.Orchestrator, "-"))
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

// hostPhases counts lanes by status and phase.
func hostPhases(lanes []adopt.Lane) string {
	statuses, phases := map[string]int{}, map[string]int{}
	for i := range lanes {
		statuses[cmp.Or(lanes[i].Status, "-")]++
		phases[cmp.Or(lanes[i].Phase, "-")]++
	}
	return laneCounts(len(lanes), statuses, phases)
}

func laneCounts(total int, statuses, phases map[string]int) string {
	if total == 0 {
		return "0"
	}
	if len(phases) == 0 {
		return fmt.Sprintf("%d (%s)", total, countParts(statuses))
	}
	return fmt.Sprintf("%d (%s; %s)", total, countParts(statuses), countParts(phases))
}

// countParts renders sorted per-key counts.
func countParts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s:%d", k, counts[k])
	}
	return strings.Join(parts, ", ")
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
	line := fmt.Sprintf("%s: %s", r.ID, compactGoalSummary(r.Objective))
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
	// Filtered is true when Filter narrowed the list to a named run.
	Filtered bool
}

// compactRow reports whether r renders as a one-line summary without per-lane detail.
func (l SwarmList) compactRow(r *adopt.Run) bool {
	return !l.Filtered && len(l.Host) > 1 && r.State != adopt.RunActive && r.State != ""
}

func (l *SwarmList) sortRecentFirst() {
	sort.SliceStable(l.Host, func(a, b int) bool {
		if l.Host[a].LastActivity != l.Host[b].LastActivity {
			return l.Host[a].LastActivity > l.Host[b].LastActivity
		}
		return l.Host[a].ID > l.Host[b].ID
	})
}

// Filter narrows Orca and Host runs to those whose ID or goal contains run
// (case-insensitive). An empty run returns l sorted recent-first.
func (l SwarmList) Filter(run string) SwarmList {
	l.sortRecentFirst()
	q := strings.ToLower(strings.TrimSpace(run))
	if q == "" {
		return l
	}
	l.Filtered = true
	var orca []store.Run
	for i := range l.Orca {
		r := &l.Orca[i]
		if strings.Contains(strings.ToLower(r.ID), q) || strings.Contains(strings.ToLower(r.Objective), q) {
			orca = append(orca, *r)
		}
	}
	var host []adopt.Run
	for i := range l.Host {
		r := &l.Host[i]
		if strings.Contains(strings.ToLower(r.ID), q) || strings.Contains(strings.ToLower(r.Goal), q) {
			host = append(host, *r)
		}
	}
	l.Orca, l.Host = orca, host
	return l
}

// Render lists both groups; detail adds each host lane's activity, next step
// and PR state, and per run what is running now and what was not checked.
// When unfiltered across multiple host swarms, non-active runs stay as compact
// one-line rows so no run is lost in a long list.
func (l SwarmList) Render(now time.Time, detail bool) string {
	l.sortRecentFirst()
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
			if !detail || l.compactRow(r) {
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

// RenderBounded renders the swarm list within maxBytes, ensuring both the
// orca runs section and the full host swarms summary list are preserved before
// per-lane detail is included or paged.
func (l SwarmList) RenderBounded(now time.Time, detail bool, maxBytes int) string {
	return l.RenderPage(now, detail, maxBytes, 1)
}

// RenderPage renders page (1-based) of the swarm list within maxBytes.
// When the detailed output exceeds maxBytes, page 1 always includes the full
// compact summary of all orca runs and host swarms first so no adoptable run
// is ever truncated out of view.
func (l SwarmList) RenderPage(now time.Time, detail bool, maxBytes, page int) string {
	if page <= 0 {
		page = 1
	}
	full := l.Render(now, detail)
	if maxBytes <= 0 || (page == 1 && len(full) <= maxBytes) {
		return full
	}

	summary := l.Render(now, false)
	const footerReserve = 120
	budget := max(maxBytes-footerReserve, 256)

	var sections []string
	if len(summary) <= budget {
		sections = append(sections, summary)
	} else {
		sections = append(sections, chunkLinesByBudget(strings.Split(summary, "\n"), budget)...)
	}

	if detail {
		l.sortRecentFirst()
		var detailLines []string
		for i := range l.Host {
			r := &l.Host[i]
			if l.compactRow(r) {
				continue
			}
			var db strings.Builder
			fmt.Fprintf(&db, "detail %s:\n", r.ID)
			l.renderHostDetail(&db, r)
			detailLines = append(detailLines, strings.Split(strings.TrimRight(db.String(), "\n"), "\n")...)
		}
		if len(detailLines) > 0 {
			detailChunks := chunkLinesByBudget(detailLines, budget)
			if len(sections) == 1 && len(sections[0])+1+len(detailChunks[0]) <= budget {
				sections[0] = sections[0] + "\n" + detailChunks[0]
				detailChunks = detailChunks[1:]
			}
			sections = append(sections, detailChunks...)
		}
	}

	totalPages := max(len(sections), 1)
	if page > totalPages {
		page = totalPages
	}
	out := sections[page-1]
	if totalPages > 1 {
		out += fmt.Sprintf("\n[swarms page %d/%d; call swarms with page=%d or run=\"<id>\" for more detail]", page, totalPages, min(page+1, totalPages))
	}
	return out
}

func chunkLinesByBudget(lines []string, budget int) []string {
	if budget <= 0 {
		return []string{strings.Join(lines, "\n")}
	}
	var chunks []string
	var cur strings.Builder
	for _, line := range lines {
		if len(line) > budget {
			line = line[:budget]
		}
		if cur.Len() > 0 && cur.Len()+1+len(line) > budget {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

func prRef(pr string) string {
	if pr == "" {
		return "-"
	}
	return "#" + pr
}
