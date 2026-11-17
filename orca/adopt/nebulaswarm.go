package adopt

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/store"
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

//nolint:govet // fields mirror the nebula-swarm ledger shape.
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
	BlockedOn     string                       `json:"blocked_on"`
	WaitingOn     string                       `json:"waiting_on"`
	Deadline      hostDeadline                 `json:"deadline"`
	Hold          string                       `json:"hold"`
	HoldNote      string                       `json:"hold_note"`
	Park          string                       `json:"park"`
	Note          string                       `json:"note"`
	PRDraft       bool                         `json:"pr_draft"`
	Draft         bool                         `json:"draft"`
	PRState       string                       `json:"pr_state"`
	MergeSHA      string                       `json:"merge_sha"`
	PRHead        string                       `json:"pr_head"`
	Head          string                       `json:"head"`
	Mode          string                       `json:"mode"`
	Checks        string                       `json:"checks"`
}

type nsLedger struct {
	Config struct {
		Goal         string      `json:"goal"`
		Repo         string      `json:"repo"`
		Target       string      `json:"target"`
		Base         string      `json:"base"`
		Orchestrator string      `json:"orchestrator_session_id"`
		Phases       []hostPhase `json:"phases"`
	} `json:"config"`
	Tasks []nsLane `json:"tasks"`
}

// readNebulaSwarm imports a nebula-swarm ledger: each lane's session, the
// terminal of its current phase and round, PR, latest report and EVENTS line.
func readNebulaSwarm(dir string, b []byte, r *Run, localRepoPaths *[]string) error {
	var st nsLedger
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Join(dir, "state.json"), err)
	}
	r.Goal, r.DrivenBy = st.Config.Goal, st.Config.Orchestrator
	r.TargetBranch = strings.TrimSpace(st.Config.Target)
	r.BaseBranch = strings.TrimSpace(st.Config.Base)
	r.Phases = hostPhaseNames(st.Config.Phases)
	if isLocalRepoPath(st.Config.Repo) && localRepoPaths != nil {
		*localRepoPaths = append(*localRepoPaths, st.Config.Repo)
	} else {
		applyRepoValue(r, st.Config.Repo)
	}
	for i := range st.Tasks {
		t := &st.Tasks[i]
		round := max(t.Round, 1)
		x := Lane{ID: t.ID, Title: t.Title, Branch: t.Branch, Phase: t.Phase, Status: t.Status, Round: t.Round, Mode: t.Mode,
			Session: t.SessionID, Terminal: t.Terminals[t.Phase][strconv.Itoa(round)], TerminalState: t.TerminalState,
			HostHold: ledgerHostHold(t.BlockedOn, t.HoldNote, t.Hold, t.Park, t.Note), PRDraft: t.PRDraft || t.Draft,
			PRState: t.PRState, MergeSHA: t.MergeSHA, Head: firstNonEmpty(t.PRHead, t.Head), Checks: t.Checks}
		applySpawnEvidence(dir, &x)
		x.PRDraft = x.PRDraft || isDraftPR(x.PRState)
		if t.PR > 0 {
			x.PR = strconv.Itoa(t.PR)
		}
		x.Integrated = x.MergeSHA != ""
		x.ReportPath = latestReport(dir, t.ID, x.Phase, round)
		x.WaitingOn, x.WaitDeadline = parseHostWait(t.WaitingOn, t.Deadline)
		if x.ReportPath != "" {
			applyAttnAnswer(dir, &x)
		}
		r.Lanes = append(r.Lanes, x)
	}
	annotateLanes(dir, r, st.Config.Phases)
	return nil
}

// annotateLanes adds each lane's latest report headline, newest EVENTS line
// and what comes next from ESCALATIONS.md, PROGRESS.md or the phase order.
func annotateLanes(dir string, r *Run, phaseSpecs []hostPhase) {
	events := laneEvents(filepath.Join(dir, "EVENTS"))
	integratedEvents := laneIntegrationEvents(filepath.Join(dir, "EVENTS"))
	mergeNotePRs := laneMergeNotePRs(filepath.Join(dir, "EVENTS"))
	progress, escalations := readText(filepath.Join(dir, "PROGRESS.md")), readText(filepath.Join(dir, "ESCALATIONS.md"))
	phases := make([]string, 0, len(phaseSpecs))
	for _, p := range phaseSpecs {
		phases = append(phases, p.Name)
	}
	for i := range r.Lanes {
		x := &r.Lanes[i]
		x.LastEvent = events[x.ID]
		eventLower := eventKind(x.LastEvent)
		if integratedEvents[x.ID] != "" || eventLower == "merged" {
			x.Integrated, x.PRState = true, "merged"
		} else if eventLower == "closed" {
			x.Integrated, x.PRState = true, "closed"
		}
		x.MergeNotePRs = mergeNotePRs[x.ID]
		if x.ReportPath != "" {
			report := readText(x.ReportPath)
			x.Report = reportHeadline(report)
			applyReportMetadata(x, report)
		}
		x.Next = laneNext(x, phases, escalations, progress)
	}
}

// laneIntegrationEvents records every merged/integrated EVENTS row. The
// newest row is useful for display, but it is not authoritative for done-ness:
// later progress rows routinely follow the host's merge record.
func laneIntegrationEvents(path string) map[string]string {
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
		if len(parts) < 3 || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(parts[2]))
		if kind != "merged" && kind != "integrated" && kind != "orch-merge" {
			continue
		}
		detail := ""
		if len(parts) == 4 {
			detail = strings.TrimSpace(parts[3])
		}
		out[parts[1]] = clip(strings.Join([]string{parts[0], kind, detail}, " "), 240)
	}
	return out
}

var mergedPRRE = regexp.MustCompile(`(?i)#(\d+)\s+merged\b`)
var prMergedRE = regexp.MustCompile(`(?i)\bPR\s+#(\d+)\b[^\n]{0,120}\bmerged\b`)
var ownLaneMergedRE = regexp.MustCompile(`(?i)\b(?:own\s+)?lane\s+#(\d+)\b[^\n]{0,200}\bmerged\b`)

// laneMergeNotePRs extracts host merge prose from a lane's own EVENTS rows.
// These nominations are deliberately not integration evidence by themselves;
// applyIntegrationEvidence verifies the matching squash subject in the mirror.
func laneMergeNotePRs(path string) map[string][]string {
	out := map[string][]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 4)
		if len(parts) < 4 || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		detail := parts[3]
		lower := strings.ToLower(detail)
		if !strings.Contains(lower, "merged") || strings.Contains(lower, "not merged") || strings.Contains(lower, "unmerged") {
			continue
		}
		matches := mergedPRRE.FindAllStringSubmatch(detail, -1)
		matches = append(matches, prMergedRE.FindAllStringSubmatch(detail, -1)...)
		matches = append(matches, ownLaneMergedRE.FindAllStringSubmatch(detail, -1)...)
		seen := map[string]bool{}
		for _, match := range matches {
			if len(match) == 2 && !seen[match[1]] {
				out[parts[1]] = append(out[parts[1]], match[1])
				seen[match[1]] = true
			}
		}
	}
	return out
}

func latestReport(dir, laneID, phase string, round int) string {
	if phase != "" {
		if p := filepath.Join(dir, "reports", fmt.Sprintf("%s.%s.r%d.md", laneID, phase, round)); fileExists(p) {
			return p
		}
		if p := filepath.Join(dir, fmt.Sprintf("%s.%s.md", laneID, phase)); fileExists(p) {
			return p
		}
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "reports", laneID+".*.md"))
	best, bestMod := "", int64(0)
	for _, p := range paths {
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

// sortRuns orders runs most recent first.
func sortRuns(runs []Run) { sort.Slice(runs, func(a, b int) bool { return newer(&runs[a], &runs[b]) }) }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// repoSlug is the run's owner/repo: nebula-swarm caches it in .repo-slug.
func repoSlug(dir string) string {
	raw := readText(filepath.Join(dir, ".repo-slug"))
	if !isRepoSlug(raw) {
		return ""
	}
	return lane.NormalizeRepositorySlug(raw)
}

func isLocalRepoPath(raw string) bool {
	s := strings.Trim(strings.TrimSpace(raw), `"'`)
	if s == "" {
		return false
	}
	if s == "." || s == ".." || s == "~" {
		return true
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `\`) {
		return true
	}
	if strings.HasPrefix(strings.ToLower(s), "file://") {
		return true
	}
	if len(s) >= 2 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) && s[1] == ':' {
		return true
	}
	if s == ".bare" || s == ".git" ||
		strings.HasSuffix(s, "/.bare") || strings.HasSuffix(s, `\.bare`) ||
		strings.HasSuffix(s, "/.git") || strings.HasSuffix(s, `\.git`) ||
		strings.Contains(s, "/.bare/") || strings.Contains(s, "/.git/") {
		return true
	}
	return false
}

func isRepoSlug(raw string) bool {
	s := strings.Trim(strings.TrimSpace(raw), `"'`)
	return s != "" && !isLocalRepoPath(s) && lane.IsRepositorySlug(s)
}

func isRepoID(raw string) bool {
	s := strings.Trim(strings.TrimSpace(raw), `"'`)
	if s == "" || isLocalRepoPath(s) || lane.IsRepositorySlug(s) {
		return false
	}
	return !strings.ContainsAny(s, "/:\\ \t\r\n")
}

func sanitizeRunRepo(r *Run) {
	if r == nil {
		return
	}
	if isLocalRepoPath(r.Repo) {
		r.Repo = ""
	}
	if !isRepoID(r.RepoID) {
		if isRepoSlug(r.RepoID) && r.Repo == "" {
			r.Repo = lane.NormalizeRepositorySlug(r.RepoID)
		}
		r.RepoID = ""
	}
}

func applyRepoValue(r *Run, raw string) {
	v := strings.Trim(strings.TrimSpace(raw), `"'`)
	if v == "" || isLocalRepoPath(v) {
		return
	}
	if isRepoSlug(v) {
		if r.Repo == "" {
			r.Repo = lane.NormalizeRepositorySlug(v)
		}
		return
	}
	if isRepoID(v) && r.RepoID == "" {
		r.RepoID = v
	}
}

func applyRunEnvRepo(dir string, r *Run, localRepoPaths *[]string) {
	recordRepoVal := func(v string) {
		if isLocalRepoPath(v) {
			if localRepoPaths != nil {
				*localRepoPaths = append(*localRepoPaths, v)
			}
			return
		}
		applyRepoValue(r, v)
	}
	for _, line := range strings.Split(readText(filepath.Join(dir, "env.sh")), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if v == "" {
			continue
		}
		switch k {
		case "SNL_REPO_ID", "NEBULA_REPO_ID":
			if isRepoID(v) && r.RepoID == "" {
				r.RepoID = v
			}
		case "SNL_GH_REPO", "SNL_REPO_URL":
			recordRepoVal(v)
		}
	}
	if r.ID != "" && os.Getenv("SNL_RUN_ID") == r.ID {
		if v := strings.TrimSpace(os.Getenv("SNL_REPO_ID")); isRepoID(v) && r.RepoID == "" {
			r.RepoID = v
		}
		if v := strings.TrimSpace(os.Getenv("NEBULA_REPO_ID")); isRepoID(v) && r.RepoID == "" {
			r.RepoID = v
		}
		if v := strings.TrimSpace(os.Getenv("SNL_GH_REPO")); v != "" {
			recordRepoVal(v)
		}
		if v := strings.TrimSpace(os.Getenv("SNL_REPO_URL")); v != "" {
			recordRepoVal(v)
		}
	}
}

func applyOriginRepo(runDir string, r *Run, localRepoPaths []string) {
	if r == nil || r.Repo != "" || r.RepoID != "" {
		return
	}
	for _, rawPath := range localRepoPaths {
		if slug := gitOriginSlugForPath(runDir, rawPath); slug != "" {
			r.Repo = slug
			return
		}
	}
}

func readTier1AndOriginRepo(dir string) (tier1RepoID, tier1Repo, originRepo string, hasTier1 bool) {
	if strings.TrimSpace(dir) == "" {
		return "", "", "", false
	}
	var t1 Run
	var localPaths []string
	applyRunEnvRepo(dir, &t1, &localPaths)
	if t1.Repo == "" {
		t1.Repo = repoSlug(dir)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		var st struct {
			Config struct {
				Repo string `json:"repo"`
			} `json:"config"`
			Tasks []struct {
				RepoURL string `json:"repo_url"`
			} `json:"tasks"`
		}
		if json.Unmarshal(b, &st) == nil {
			if isLocalRepoPath(st.Config.Repo) {
				localPaths = append(localPaths, st.Config.Repo)
			} else {
				applyRepoValue(&t1, st.Config.Repo)
			}
			for i := range st.Tasks {
				if isLocalRepoPath(st.Tasks[i].RepoURL) {
					localPaths = append(localPaths, st.Tasks[i].RepoURL)
				} else {
					applyRepoValue(&t1, st.Tasks[i].RepoURL)
				}
			}
		}
	}
	hasTier1 = t1.Repo != "" || t1.RepoID != ""
	var t3 Run
	applyOriginRepo(dir, &t3, localPaths)
	return t1.RepoID, t1.Repo, t3.Repo, hasTier1
}

func gitOriginSlugForPath(runDir, rawPath string) string {
	p := strings.Trim(strings.TrimSpace(rawPath), `"'`)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(p), "file://") {
		p = p[len("file://"):]
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	} else if !filepath.IsAbs(p) && runDir != "" {
		p = filepath.Join(runDir, p)
	}
	p = filepath.Clean(p)
	originURL := readGitOriginURL(p)
	if !isRepoSlug(originURL) {
		return ""
	}
	return lane.NormalizeRepositorySlug(originURL)
}

func readGitOriginURL(dirPath string) string {
	cur := filepath.Clean(strings.TrimSpace(dirPath))
	if cur == "" {
		return ""
	}
	parse := func(configPath string) string {
		return parseGitConfigOriginURL(readText(configPath))
	}
	parseGitDir := func(base string) string {
		dotGit := filepath.Join(base, ".git")
		info, err := os.Stat(dotGit)
		if err != nil {
			return ""
		}
		if info.IsDir() {
			return parse(filepath.Join(dotGit, "config"))
		}
		gitDirLine := strings.TrimSpace(readText(dotGit))
		target, ok := strings.CutPrefix(gitDirLine, "gitdir:")
		if !ok {
			return ""
		}
		target = strings.TrimSpace(target)
		if !filepath.IsAbs(target) {
			target = filepath.Join(base, target)
		}
		target = filepath.Clean(target)
		if u := parse(filepath.Join(target, "config")); u != "" {
			return u
		}
		if common := strings.TrimSpace(readText(filepath.Join(target, "commondir"))); common != "" {
			if !filepath.IsAbs(common) {
				common = filepath.Join(target, common)
			}
			return parse(filepath.Join(filepath.Clean(common), "config"))
		}
		return ""
	}
	// A bare repository stores its remotes in <repo>/config. For a checkout,
	// inspect only real .git markers while walking parents; arbitrary config
	// files are never treated as Git metadata.
	if u := parse(filepath.Join(cur, "config")); u != "" {
		return u
	}
	for depth := 0; depth <= 4; depth++ {
		if u := parseGitDir(cur); u != "" {
			return u
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return ""
}

func parseGitConfigOriginURL(text string) string {
	inOrigin := false
	for _, rawLine := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sec := strings.TrimSpace(line[1 : len(line)-1])
			inOrigin = strings.EqualFold(sec, `remote "origin"`)
			continue
		}
		if !inOrigin {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "url") {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// ledgerPhase maps an orca lane phase to a phase valid in a nebula-swarm
// state.json config.phases list (for example keeping "review" when orca runs a
// synthetic "integrate" phase not listed in config.phases).
func ledgerPhase(phase string, cfgPhases []string, fallbackPhase string) string {
	if phase == "" {
		return fallbackPhase
	}
	if len(cfgPhases) == 0 || slices.Contains(cfgPhases, phase) {
		return phase
	}
	if fallbackPhase != "" && slices.Contains(cfgPhases, fallbackPhase) {
		return fallbackPhase
	}
	return cfgPhases[len(cfgPhases)-1]
}

type nsTaskMeta struct {
	Terminals      map[string]map[string]string `json:"terminals"`
	ReviewRoundCap *int                         `json:"review_round_cap"`
	ID             string                       `json:"id"`
	Status         string                       `json:"status"`
	Phase          string                       `json:"phase"`
	SessionID      string                       `json:"session_id"`
	TerminalState  string                       `json:"terminal_state"`
	Verdict        string                       `json:"verdict"`
	Round          int                          `json:"round"`
}

func readNebulaSwarmLedgerMeta(dir string) ([]string, int, map[string]nsTaskMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, 0, nil, err
	}
	var st struct {
		Tasks  []nsTaskMeta `json:"tasks"`
		Config struct {
			Phases    []hostPhase `json:"phases"`
			MaxRounds int         `json:"max_rounds"`
		} `json:"config"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, 0, nil, err
	}
	phases := make([]string, 0, len(st.Config.Phases))
	for _, p := range st.Config.Phases {
		if p.Name != "" {
			phases = append(phases, p.Name)
		}
	}
	tasks := make(map[string]nsTaskMeta, len(st.Tasks))
	for _, t := range st.Tasks {
		tasks[t.ID] = t
	}
	return phases, st.Config.MaxRounds, tasks, nil
}

const ledgerHelperTimeout = 5 * time.Second

// writeNebulaSwarmLaneWithHelper updates one lane in dir/state.json using the
// nebula-swarm skill's ledger.py helper when available.
func writeNebulaSwarmLaneWithHelper(ctx context.Context, dir string, ln lane.Lane, scriptOverride *string) (bool, error) {
	script := ""
	if scriptOverride != nil {
		script = *scriptOverride
	} else {
		script = FindLedgerScript()
	}
	if script == "" {
		return false, nil
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	cfgPhases, cfgMaxRounds, tasks, err := readNebulaSwarmLedgerMeta(absDir)
	if err != nil {
		return false, err
	}
	helperCtx, cancel := context.WithTimeout(ctx, ledgerHelperTimeout)
	defer cancel()
	meta, exists := tasks[ln.ID]
	if !exists {
		branch := cmp.Or(strings.TrimSpace(ln.Branch), fmt.Sprintf("swarm-lane/%s/%s", ln.RunID, ln.ID))
		title := cmp.Or(strings.TrimSpace(ln.Title), ln.ID)
		addArgs := []string{script, "add", absDir, "--id", ln.ID, "--title", title, "--branch", branch}
		if out, err := exec.CommandContext(helperCtx, "python3", addArgs...).CombinedOutput(); err != nil {
			return false, fmt.Errorf("ledger.py add %s: %w (%s)", ln.ID, err, strings.TrimSpace(string(out)))
		}
	}
	status := string(ln.Status)
	if meta.Status == nsNeedsHuman && ln.Status == lane.StatusBlocked {
		status = nsNeedsHuman
	}
	effPhase := ledgerPhase(ln.Phase, cfgPhases, meta.Phase)
	round := max(ln.Round, 1)
	cap := max(cfgMaxRounds, 2)
	if meta.ReviewRoundCap != nil && *meta.ReviewRoundCap > 0 {
		cap = *meta.ReviewRoundCap
	}

	args := []string{script, "set", absDir, "--id", ln.ID}
	if status != "" {
		args = append(args, "--status", status)
	}
	if effPhase != "" && (len(cfgPhases) == 0 || slices.Contains(cfgPhases, effPhase)) {
		args = append(args, "--phase", effPhase)
	}
	if round > cap {
		args = append(args, "--review-round-cap", strconv.Itoa(round))
	}
	args = append(args, "--round", strconv.Itoa(round))
	if ln.SessionID != "" {
		if meta.SessionID == "" {
			args = append(args, "--session-id", ln.SessionID)
		} else if meta.SessionID != ln.SessionID {
			args = append(args, "--adopt-session-id", ln.SessionID)
		}
	}
	if ln.TerminalID != "" && effPhase == ln.Phase && effPhase != "" {
		existingTerm := meta.Terminals[effPhase][strconv.Itoa(round)]
		if existingTerm != "" && existingTerm != ln.TerminalID {
			return false, nil
		}
		if existingTerm == "" {
			args = append(args, "--terminal-id", ln.TerminalID)
		}
	}
	if ln.PR > 0 {
		args = append(args, "--pr", strconv.Itoa(ln.PR))
	}
	if ln.PRHead != "" {
		args = append(args, "--pr-head", ln.PRHead)
	}
	if ln.ApprovalSHA != "" {
		args = append(args, "--approval-sha", ln.ApprovalSHA)
		if meta.Verdict == "" {
			args = append(args, "--verdict", "pass")
		}
	}
	if ln.MergeSHA != "" {
		args = append(args, "--merge-sha", ln.MergeSHA)
	}
	if ln.Status.Terminal() && meta.TerminalState != "" {
		args = append(args, "--terminal-state", "")
	}
	if out, err := exec.CommandContext(helperCtx, "python3", args...).CombinedOutput(); err != nil {
		return false, fmt.Errorf("ledger.py set %s: %w (%s)", ln.ID, err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// appendNebulaSwarmEvents appends one EVENTS line per transition between prev
// and cur in nebula-swarm's tab-separated format (<UTC>\t<lane>\t<kind>\t<detail>).
func appendNebulaSwarmEvents(dir string, now time.Time, prev *lane.Lane, cur lane.Lane, cfgPhases []string) error {
	ts := now.UTC().Format(time.RFC3339)
	type evLine struct {
		kind, detail string
	}
	var lines []evLine

	oldPhase := ""
	if prev != nil {
		oldPhase = ledgerPhase(prev.Phase, cfgPhases, "")
	}
	newPhase := ledgerPhase(cur.Phase, cfgPhases, oldPhase)

	if prev == nil {
		if newPhase != "" && cur.TerminalID != "" && cur.Phase == newPhase {
			lines = append(lines, evLine{kind: "started", detail: newPhase + " " + cur.TerminalID})
		}
	} else {
		if oldPhase != "" && newPhase != "" && oldPhase != newPhase {
			lines = append(lines, evLine{kind: "phase", detail: oldPhase + " -> " + newPhase})
		}
		if cur.TerminalID != "" && cur.TerminalID != prev.TerminalID && cur.Phase == newPhase && newPhase != "" {
			lines = append(lines, evLine{kind: "started", detail: newPhase + " " + cur.TerminalID})
		}
		if cur.ApprovalSHA != "" && cur.ApprovalSHA != prev.ApprovalSHA {
			lines = append(lines, evLine{kind: "verdict", detail: "pass"})
		}
		wasMerged := prev.Status == lane.StatusDone && (prev.PR > 0 || prev.MergeSHA != "")
		isMerged := cur.Status == lane.StatusDone && (cur.PR > 0 || cur.MergeSHA != "")
		if isMerged && (!wasMerged || (cur.MergeSHA != "" && cur.MergeSHA != prev.MergeSHA)) {
			var detail string
			switch {
			case cur.PR > 0 && cur.MergeSHA != "":
				detail = fmt.Sprintf("#%d %s", cur.PR, cur.MergeSHA)
			case cur.PR > 0:
				detail = fmt.Sprintf("#%d", cur.PR)
			default:
				detail = cur.MergeSHA
			}
			lines = append(lines, evLine{kind: "merged", detail: detail})
		} else if cur.Status != prev.Status && prev.Status != "" && cur.Status != "" {
			lines = append(lines, evLine{kind: "status", detail: string(prev.Status) + " -> " + string(cur.Status)})
		}
	}

	if len(lines) == 0 {
		return nil
	}

	lock, err := os.OpenFile(filepath.Join(dir, "EVENTS.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockLedgerFile(lock); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	f, err := os.OpenFile(filepath.Join(dir, "EVENTS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(ts)
		b.WriteByte('\t')
		b.WriteString(cur.ID)
		b.WriteByte('\t')
		b.WriteString(l.kind)
		b.WriteByte('\t')
		b.WriteString(l.detail)
		b.WriteByte('\n')
	}
	_, err = f.WriteString(b.String())
	return err
}

// ValidateCloseoutWrite checks that targetPath resolves inside runDir and is
// either a standard close-out file (FINAL.md, PROGRESS.md, OBJECTIVE.md) or a
// file explicitly named in objective. It returns the resolved path inside runDir.
func ValidateCloseoutWrite(runDir, objective, targetPath string) (string, error) {
	dir := strings.TrimSpace(runDir)
	if dir == "" {
		return "", fmt.Errorf("close-out write %q: run directory is required", targetPath)
	}
	raw := strings.TrimSpace(targetPath)
	if raw == "" {
		return "", fmt.Errorf("close-out file path is empty")
	}
	if _, err := dispatcher.ValidateCloseoutFileRef(objective, raw); err != nil {
		return "", err
	}
	resolved, err := resolveBriefPath(dir, raw)
	if err != nil || resolved == "" {
		return "", fmt.Errorf("close-out write %q is outside the run directory", raw)
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("close-out write %q is outside the run directory", raw)
	}
	cleanDir, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("close-out write %q is outside the run directory", raw)
	}
	rel, err := filepath.Rel(cleanDir, resolved)
	if err != nil {
		return "", fmt.Errorf("close-out write %q is outside the run directory", raw)
	}
	relSlash := filepath.ToSlash(filepath.Clean(rel))
	if relSlash == "." || relSlash == ".." || strings.HasPrefix(relSlash, "../") {
		return "", fmt.Errorf("close-out write %q is outside the run directory", raw)
	}
	if _, err := dispatcher.ValidateCloseoutFileRef(objective, relSlash); err != nil {
		return "", err
	}
	return resolved, nil
}

// WriteCloseoutFile validates targetPath against runDir and objective and
// writes (or appends) content atomically, preserving any ORCHESTRATOR stand-down
// marker header on OBJECTIVE.md and PROGRESS.md.
func WriteCloseoutFile(runDir, objective, targetPath, content string, appendMode bool) (string, error) {
	resolved, err := ValidateCloseoutWrite(runDir, objective, targetPath)
	if err != nil {
		return "", err
	}
	base := filepath.Base(resolved)
	existingRaw := readText(resolved)
	var markerHeader string
	body := existingRaw
	if base == "OBJECTIVE.md" || base == "PROGRESS.md" {
		for strings.HasPrefix(body, markerPrefix) {
			line, rest, ok := strings.Cut(body, "\n")
			if !ok {
				markerHeader += line + "\n"
				body = ""
				break
			}
			markerHeader += line + "\n"
			body = rest
		}
	}
	var nextBody string
	if appendMode && strings.TrimSpace(body) != "" {
		nextBody = strings.TrimRight(body, "\n") + "\n" + strings.TrimLeft(content, "\n")
		if !strings.HasSuffix(nextBody, "\n") {
			nextBody += "\n"
		}
	} else {
		nextBody = content
		if base == "OBJECTIVE.md" || base == "PROGRESS.md" {
			nextBody = stripMarker(nextBody)
		}
		if !strings.HasSuffix(nextBody, "\n") {
			nextBody += "\n"
		}
	}
	if err := writeAtomic(resolved, []byte(markerHeader+nextBody)); err != nil {
		return "", err
	}
	return resolved, nil
}

func hasUncheckedMilestoneItems(objective string) bool {
	for _, line := range strings.Split(objective, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- [ ] ") || strings.HasPrefix(trimmed, "* [ ] ") {
			return true
		}
	}
	return false
}

func shouldAutoCloseoutRun(r Run, lanes []lane.Lane, items []store.BacklogItem) bool {
	if strings.TrimSpace(r.Dir) == "" || dispatcher.CheckRunTerminal(lanes, items, true) != nil {
		return false
	}
	if fileExists(filepath.Join(r.Dir, "FINAL.md")) && ObjectiveIsDone(r.Objective) {
		return false
	}
	if hasUncheckedMilestoneItems(r.Objective) {
		return false
	}
	handoverText := stripMarker(readText(filepath.Join(r.Dir, "HANDOVER.md")))
	triggerText := extractCloseoutTriggerText(r, handoverText)
	if strings.TrimSpace(triggerText) == "" {
		return false
	}
	triggerNorm := adoptNormalizeContractions(strings.ToLower(triggerText))
	if adoptContainsWholeWord(triggerNorm, "staff") ||
		adoptContainsWholeWord(triggerNorm, "spawn") ||
		strings.Contains(triggerNorm, "ask owner") {
		return false
	}
	return strings.Contains(triggerNorm, "final.md") ||
		strings.Contains(triggerNorm, "close out") ||
		strings.Contains(triggerNorm, "close-out") ||
		strings.Contains(triggerNorm, "closeout") ||
		strings.Contains(triggerNorm, "republish")
}

func extractCloseoutTriggerText(r Run, handoverText string) string {
	var parts []string
	next := strings.TrimSpace(r.Next)
	if next == "" && r.Objective != "" {
		next = strings.TrimSpace(extractNextSection(r.Objective))
	}
	if next != "" {
		parts = append(parts, next)
	}
	if hNext := strings.TrimSpace(extractNextSection(handoverText)); hNext != "" {
		parts = append(parts, hNext)
	}
	if hSec := extractHandoverSectionText(handoverText); hSec != "" {
		parts = append(parts, hSec)
	}
	// When OBJECTIVE.md has an active non-done Next: directive, rely only on
	// that Next: directive and explicit remaining-steps sections in HANDOVER.md
	// so historical notes in PROGRESS.md cannot prematurely trigger close-out.
	if next != "" && !IsDoneOrEmptyNext(next) {
		return strings.Join(parts, "\n")
	}
	if progNext := strings.TrimSpace(extractNextSection(r.Progress)); progNext != "" {
		parts = append(parts, progNext)
	}
	if objHandover := extractHandoverSectionText(r.Objective); objHandover != "" {
		parts = append(parts, objHandover)
	}
	if progHandover := extractHandoverSectionText(r.Progress); progHandover != "" {
		parts = append(parts, progHandover)
	}
	return strings.Join(parts, "\n")
}

func extractHandoverSectionText(md string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}
	lines := strings.Split(md, "\n")
	var out []string
	inHandoverHeading := false
	inRemainingBlock := false
	isHistoricalNote := func(lower string) bool {
		clean := strings.TrimLeft(lower, "-* \t")
		return strings.HasPrefix(clean, "previously ") || strings.HasPrefix(clean, "already ")
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(trimmed, "#") {
			heading := strings.TrimSpace(strings.TrimLeft(lower, "#"))
			if strings.HasPrefix(heading, "close-out (") {
				inHandoverHeading = false
				inRemainingBlock = false
				continue
			}
			inHandoverHeading = strings.Contains(heading, "handover") ||
				strings.Contains(heading, "remaining") ||
				strings.Contains(heading, "close-out") ||
				strings.Contains(heading, "close out") ||
				strings.Contains(heading, "closeout")
			inRemainingBlock = false
			continue
		}
		if strings.HasPrefix(lower, "remaining") || strings.HasPrefix(lower, "handover:") {
			inRemainingBlock = true
			if !isHistoricalNote(lower) {
				out = append(out, trimmed)
			}
			continue
		}
		if inRemainingBlock {
			if trimmed == "" {
				inRemainingBlock = false
				continue
			}
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
				if !isHistoricalNote(lower) {
					out = append(out, trimmed)
				}
				continue
			}
			inRemainingBlock = false
		}
		if inHandoverHeading && trimmed != "" && !isHistoricalNote(lower) {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n")
}

func extractCloseoutPlanFile(runDir, objective string) string {
	if p := extractPlanPath(runDir, objective, ""); p != "" {
		if resolved, err := ValidateCloseoutWrite(runDir, objective, p); err == nil {
			base, bErr := filepath.Abs(runDir)
			if bErr == nil {
				if realDir, eErr := filepath.EvalSymlinks(base); eErr == nil {
					base = realDir
				}
				if rel, rErr := filepath.Rel(base, resolved); rErr == nil {
					return filepath.ToSlash(rel)
				}
			}
		}
	}
	if _, err := ValidateCloseoutWrite(runDir, objective, "PLAN.md"); err == nil {
		return "PLAN.md"
	}
	return ""
}

func extractCloseoutPostID(texts ...string) string {
	for _, text := range texts {
		if id := findPostIDInText(text); id != "" {
			return id
		}
	}
	return ""
}

func findPostIDInText(text string) string {
	return dispatcher.FindPostIDInText(text)
}

func replaceObjectiveNextLine(objective, newNextLine string) string {
	trimmedNext := strings.TrimSpace(newNextLine)
	if !strings.HasPrefix(strings.ToLower(trimmedNext), "next:") {
		trimmedNext = "Next: " + trimmedNext
	}
	lines := strings.Split(objective, "\n")
	type span struct {
		start, end int
	}
	var spans []span
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		clean := strings.TrimLeft(trimmed, "-* \t")
		lower := strings.ToLower(clean)
		if strings.HasPrefix(lower, "next:") {
			end := i + 1
			for end < len(lines) {
				nextLine := lines[end]
				nextTrim := strings.TrimSpace(nextLine)
				if nextTrim == "" || strings.HasPrefix(nextTrim, "#") {
					break
				}
				nxtClean := strings.ToLower(strings.TrimLeft(nextTrim, "-* \t"))
				if strings.HasPrefix(nxtClean, "next:") {
					break
				}
				if strings.HasPrefix(nextLine, " ") || strings.HasPrefix(nextLine, "\t") || strings.HasPrefix(nextTrim, "- ") || strings.HasPrefix(nextTrim, "* ") {
					end++
					continue
				}
				break
			}
			spans = append(spans, span{start: i, end: end})
			i = end - 1
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
			if heading == "next" || strings.HasPrefix(heading, "next ") || strings.HasPrefix(heading, "next:") {
				end := i + 1
				for end < len(lines) {
					if strings.HasPrefix(strings.TrimSpace(lines[end]), "#") {
						break
					}
					end++
				}
				for end > i+1 && strings.TrimSpace(lines[end-1]) == "" {
					end--
				}
				spans = append(spans, span{start: i, end: end})
				i = end - 1
			}
		}
	}
	if len(spans) == 0 {
		out := append([]string{}, lines...)
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, trimmedNext)
		return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	}
	last := spans[len(spans)-1]
	var out []string
	out = append(out, lines[:last.start]...)
	out = append(out, trimmedNext)
	out = append(out, lines[last.end:]...)
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}
