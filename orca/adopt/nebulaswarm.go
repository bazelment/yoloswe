package adopt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Run directory formats.
const (
	// FormatSubagentSwarm is a subagent-swarm-nebula run:
	// <repo>/.shared/subagent-swarm/<run> with spawn files and watch_lanes.sh.
	FormatSubagentSwarm = "subagent-swarm-nebula"
	// FormatNebulaSwarm is a nebula-swarm run: ~/.swarm/<run> with lane
	// terminals in the ledger, reports/, EVENTS and a paced sw_step loop.
	FormatNebulaSwarm = "nebula-swarm"
)

// Run states, from the ledger and the orchestrator's other runs.
const (
	// RunActive has a lane the ledger still runs.
	RunActive = "active"
	// RunFinished has no planned, running or blocked lane.
	RunFinished = "finished"
	// RunSuperseded is a run whose orchestrator session drives a newer run.
	RunSuperseded = "superseded"
)

// RunOrchestrators reads which session Nebula records as a run's orchestrator.
type RunOrchestrators interface {
	RunOrchestratorSession(ctx context.Context, runID string) (string, error)
}

// LoopFinder finds the live processes driving a run directory and says
// which terminal they run in: a description and the tmux pane ("" if none).
type LoopFinder interface {
	Running(ctx context.Context, runDir string) ([]int, error)
	Describe(ctx context.Context, pids []int) (desc, pane string)
}

// DetectFormat names the run format of dir from its ledger.
func DetectFormat(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return FormatSubagentSwarm
	}
	return detectFormat(dir, b)
}

func detectFormat(dir string, ledgerJSON []byte) string {
	var probe struct {
		Config map[string]json.RawMessage   `json:"config"`
		Tasks  []map[string]json.RawMessage `json:"tasks"`
	}
	if json.Unmarshal(ledgerJSON, &probe) == nil {
		if _, ok := probe.Config["orchestrator_session_id"]; ok {
			return FormatNebulaSwarm
		}
		for _, t := range probe.Tasks {
			if _, ok := t["terminals"]; ok {
				return FormatNebulaSwarm
			}
			if _, ok := t["session_id"]; ok {
				return FormatNebulaSwarm
			}
		}
	}
	if filepath.Base(filepath.Dir(dir)) != "subagent-swarm" && fileExists(filepath.Join(dir, "EVENTS")) {
		return FormatNebulaSwarm
	}
	return FormatSubagentSwarm
}

// isNebulaSwarmDir reports whether dir looks like a nebula-swarm run: a
// ledger plus one of the files sw_init writes beside it.
func isNebulaSwarmDir(dir string) bool {
	if !fileExists(filepath.Join(dir, "state.json")) {
		return false
	}
	for _, n := range []string{"EVENTS", "env.sh", "ledger.md", "ESCALATIONS.md"} {
		if fileExists(filepath.Join(dir, n)) {
			return true
		}
	}
	return false
}

type nsLane struct {
	Terminals     map[string]map[string]string `json:"terminals"`
	ID            string                       `json:"id"`
	Title         string                       `json:"title"`
	Branch        string                       `json:"branch"`
	Phase         string                       `json:"phase"`
	Status        string                       `json:"status"`
	SessionID     string                       `json:"session_id"`
	TerminalState string                       `json:"terminal_state"`
	PR            int                          `json:"pr"`
	Round         int                          `json:"round"`
}

type nsLedger struct {
	Config struct {
		Goal         string      `json:"goal"`
		Orchestrator string      `json:"orchestrator_session_id"`
		Phases       []hostPhase `json:"phases"`
	} `json:"config"`
	Tasks []nsLane `json:"tasks"`
}

// readNebulaSwarm imports a nebula-swarm ledger: each lane's session, the
// terminal of its current phase and round, PR, latest report and EVENTS line.
func readNebulaSwarm(dir string, b []byte, r *Run) error {
	var st nsLedger
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Join(dir, "state.json"), err)
	}
	r.Goal, r.DrivenBy = st.Config.Goal, st.Config.Orchestrator
	for i := range st.Tasks {
		t := &st.Tasks[i]
		round := max(t.Round, 1)
		x := Lane{ID: t.ID, Title: t.Title, Branch: t.Branch, Phase: t.Phase, Status: t.Status, Round: t.Round,
			Session: t.SessionID, Terminal: t.Terminals[t.Phase][strconv.Itoa(round)], TerminalState: t.TerminalState}
		if t.PR > 0 {
			x.PR = strconv.Itoa(t.PR)
		}
		x.ReportPath = latestReport(dir, t.ID, t.Phase, round)
		r.Lanes = append(r.Lanes, x)
	}
	annotateLanes(dir, r, st.Config.Phases)
	return nil
}

// annotateLanes adds each lane's latest report headline, newest EVENTS line
// and what comes next from ESCALATIONS.md, PROGRESS.md or the phase order.
func annotateLanes(dir string, r *Run, phaseSpecs []hostPhase) {
	events := laneEvents(filepath.Join(dir, "EVENTS"))
	progress, escalations := readText(filepath.Join(dir, "PROGRESS.md")), readText(filepath.Join(dir, "ESCALATIONS.md"))
	phases := make([]string, 0, len(phaseSpecs))
	for _, p := range phaseSpecs {
		phases = append(phases, p.Name)
	}
	for i := range r.Lanes {
		x := &r.Lanes[i]
		x.LastEvent = events[x.ID]
		if x.ReportPath != "" {
			x.Report = reportHeadline(readText(x.ReportPath))
		}
		x.Next = laneNext(x, phases, escalations, progress)
	}
}

// latestReport is reports/<lane>.<phase>.r<round>.md, else the lane's newest
// accepted report.
func latestReport(dir, laneID, phase string, round int) string {
	if phase != "" {
		if p := filepath.Join(dir, "reports", fmt.Sprintf("%s.%s.r%d.md", laneID, phase, round)); fileExists(p) {
			return p
		}
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "reports", laneID+".*.md"))
	best, bestMod := "", int64(0)
	for _, p := range paths {
		// A glob for lane "a" also matches lane "a.b"; keep <lane>.<phase>.r<n>.md only.
		rest := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), laneID+"."), ".md")
		if strings.Count(rest, ".") != 1 {
			continue
		}
		if info, err := os.Stat(p); err == nil && (best == "" || info.ModTime().UnixNano() > bestMod) {
			best, bestMod = p, info.ModTime().UnixNano()
		}
	}
	return best
}

// reportHeadline is a report's Outcome line, else its first non-empty line.
func reportHeadline(s string) string {
	first := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Outcome:") {
			return clip(line, 200)
		}
		if first == "" && line != "" {
			first = line
		}
	}
	return clip(first, 200)
}

// laneEvents returns each lane's newest EVENTS line as "<time> <event> <detail>".
// EVENTS lines are <UTC> TAB <lane> TAB <event> TAB <detail>.
func laneEvents(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 4)
		if len(parts) < 3 || parts[1] == "" {
			continue
		}
		fields := []string{parts[0], parts[2]}
		if len(parts) == 4 && strings.TrimSpace(parts[3]) != "" {
			fields = append(fields, strings.TrimSpace(parts[3]))
		}
		out[parts[1]] = clip(strings.Join(fields, " "), 240)
	}
	return out
}

// laneNext says what comes next for a lane: its escalation, else the
// PROGRESS.md line naming it, else the run's next phase.
func laneNext(l *Lane, phases []string, escalations, progress string) string {
	if line := lineNaming(escalations, l.ID); line != "" {
		return "escalated: " + line
	}
	if terminalStatus(l.Status) {
		return ""
	}
	if line := lineNaming(progress, l.ID); line != "" {
		return line
	}
	for i, p := range phases {
		if p == l.Phase {
			if i+1 < len(phases) {
				return "next phase: " + phases[i+1]
			}
			return "next: merge"
		}
	}
	return ""
}

func lineNaming(text, laneID string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, markerPrefix) {
			continue
		}
		if containsWord(line, laneID) {
			return clip(line, 240)
		}
	}
	return ""
}

// containsWord reports whether s names id as a whole lane id.
func containsWord(s, id string) bool {
	isID := func(c byte) bool {
		return c == '-' || c == '_' || c == '.' || c == ':' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	for i := 0; ; {
		j := strings.Index(s[i:], id)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(id)
		if (start == 0 || !isID(s[start-1])) && (end == len(s) || !isID(s[end]) || (s[end] == '.' && (end+1 == len(s) || s[end+1] == ' '))) {
			return true
		}
		i = start + 1
	}
}

// terminalStatus is a ledger status no step advances.
func terminalStatus(s string) bool {
	switch s {
	case "done", "failed", "needs-human":
		return true
	}
	return false
}

func runLive(r *Run) bool {
	for i := range r.Lanes {
		switch r.Lanes[i].Status {
		case "planned", "running", "blocked":
			return true
		}
	}
	return false
}

// classify sets each run's State: superseded when its orchestrator session
// drives a newer run, else active while a lane runs, else finished.
func classify(runs []Run) {
	newest := map[string]int{}
	for i := range runs {
		r := &runs[i]
		r.State = RunFinished
		if runLive(r) {
			r.State = RunActive
		}
		if r.DrivenBy == "" {
			continue
		}
		if j, ok := newest[r.DrivenBy]; !ok || newer(r, &runs[j]) {
			newest[r.DrivenBy] = i
		}
	}
	for i := range runs {
		r := &runs[i]
		if j, ok := newest[r.DrivenBy]; ok && j != i {
			r.State, r.SupersededBy = RunSuperseded, runs[j].ID
		}
	}
}

// newer orders runs by last activity, then id (ids start with their date).
func newer(a, b *Run) bool {
	if a.LastActivity != b.LastActivity {
		return a.LastActivity > b.LastActivity
	}
	return a.ID > b.ID
}

// sortRuns keeps Discover's id order.
func sortRuns(runs []Run) { sort.Slice(runs, func(a, b int) bool { return runs[a].ID < runs[b].ID }) }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// repoSlug is the run's owner/repo: nebula-swarm caches it in .repo-slug.
func repoSlug(dir string) string {
	return strings.TrimSpace(readText(filepath.Join(dir, ".repo-slug")))
}
