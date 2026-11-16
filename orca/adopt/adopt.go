// Package adopt hands a host-run swarm (a subagent-swarm-nebula or nebula-swarm
// run directory) over to orca chat and back, attaching its lanes to the
// controller.
//
// The handover is one protocol, recorded write-ahead in a lease journal inside
// the run directory so every step is idempotent and a rerun after a crash
// finishes it:
//
//	adopt:   1 lease  2 import  3 stand-down line  4 Nebula label  5 stop watcher or step loop  6 drive
//	release: 6 stop and checkpoint  5 restart watcher or step loop  4 restore label  3 drop line  2 drop import  1 drop lease
//
// A failed adopt step undoes the completed steps in reverse and drops the
// lease; a failed release step re-applies the release steps it completed and
// restores the journal, so either way the prior state is left as it was.
package adopt

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	leaseFile    = ".orca-lease.json"
	markerPrefix = "ORCHESTRATOR: orca since "
	orcaLabel    = "orca"

	stateAdopting  = "adopting"
	stateAdopted   = "adopted"
	stateReleasing = "releasing"
)

// handoverFiles carry the stand-down line Claude orchestrators read first.
var handoverFiles = []string{"OBJECTIVE.md", "PROGRESS.md"}

var (
	ErrLeaseHeld    = errors.New("run is leased by another orca")
	ErrLeaseNotHeld = errors.New("run is not leased by this orca")
	ErrRunNotFound  = errors.New("swarm run not found")
)

// ChatHolder is the lease holder id of the chat attached to chatRunID. It
// depends only on the chat's run id, so it survives a chat restart.
func ChatHolder(chatRunID string) string { return "orca-chat:" + chatRunID }

// HeldByOrca reports whether runDir is currently held by an active orca lease
// journal or stand-down marker, returning the holder description when held.
func HeldByOrca(runDir string) (string, bool) {
	if j, err := readJournal(filepath.Join(runDir, leaseFile)); err == nil {
		if j.State == stateReleasing {
			return "", false
		}
		if holder := strings.TrimSpace(j.Holder); holder != "" {
			return holder, true
		}
	}
	for _, name := range handoverFiles {
		first, _, _ := strings.Cut(readText(filepath.Join(runDir, name)), "\n")
		if strings.HasPrefix(first, markerPrefix) {
			if open := strings.IndexByte(first, '('); open >= 0 {
				if close := strings.IndexByte(first[open+1:], ')'); close >= 0 {
					if h := strings.TrimSpace(first[open+1 : open+1+close]); h != "" {
						return h, true
					}
				}
			}
			return strings.TrimSpace(strings.TrimPrefix(first, markerPrefix)), true
		}
	}
	return "", false
}

// Lane is one ledger lane with the ownership of its current round.
//
//nolint:govet // fields mirror the two host ledger formats.
type Lane struct {
	ID, Title, Branch, Phase, Status string
	// Session, Terminal and ReportPath belong to the lane's current round
	// (subagent-swarm-nebula: its newest <lane>.<phase>.spawn.json;
	// nebula-swarm: the ledger's terminal for the phase and round and
	// reports/<lane>.<phase>.r<round>.md). ReportPath is empty until that
	// round reports.
	Session, Terminal, ReportPath, PR string
	// HoldAdvance is a host contract marker. Orca must wait for judgment before
	// advancing this lane, even when its report says done.
	HoldAdvance bool
	// HostHold is a host-owned hold/block reason imported from the ledger or
	// lane notes. It is deliberately textual so the dispatcher can name the
	// evidence when it asks for a judgment.
	HostHold string
	// PRDraft and Integrated are host facts. A draft PR is held even when the
	// report says done; an integrated/merged/closed lane is terminal and must
	// never be revisited by adopt.
	PRDraft, Integrated             bool
	PRState, MergeSHA, Head, Checks string
	// TerminalState is the ledger's last recorded terminal state (nebula-swarm).
	TerminalState string
	// Report is the latest report's Outcome line; LastEvent the lane's newest
	// EVENTS line; Next what the run files say comes next for the lane.
	Report, LastEvent, Next string
	// ReportBlocker is the text from an Outcome: blocked report. It is a
	// host-owned hold so an adopted controller can carry it to orca judgment.
	ReportBlocker string
	// IntegrationReason records local-git evidence or the evidence still
	// missing for this lane. It is intentionally textual for dry-run output.
	IntegrationReason string
	Round             int
}

// Run is a host swarm run directory.
//
//nolint:govet // fields mirror host discovery and handover metadata.
type Run struct {
	ID, Dir, Goal, Orchestrator, LastActivity string
	// Format is FormatSubagentSwarm or FormatNebulaSwarm.
	Format string
	// Holder is the orca lease holder, empty when no orca holds the run.
	Holder string
	// DrivenBy is the orchestrator driving the run now: Nebula's run
	// orchestrator session when read, else the ledger's
	// orchestrator_session_id, else the tmux pane of its live step loop.
	DrivenBy string
	// State is RunActive, RunFinished or RunSuperseded; SupersededBy names
	// the newer run the same orchestrator session drives.
	State, SupersededBy string
	// Loop describes the live step loop or watcher processes ("" when none).
	Loop string
	// Repo is the run's owner/repo when the run records it.
	Repo string
	// RepoPath is the local checkout or bare repository recorded by the host,
	// when one is available. It is used for local integration evidence only.
	RepoPath string
	// TargetBranch is the host run's merge target (for example main or a
	// feature branch). Integration is judged against this branch, never GitHub.
	TargetBranch string
	// IntegrationError is set when the local checkout or target fetch could not
	// be inspected. Such lanes must be judged, never treated as integrated.
	IntegrationError string
	// RepoID is the run's Nebula repository id when the run records one.
	RepoID string `json:",omitempty"`
	// Objective is OBJECTIVE.md, with the stand-down marker stripped.
	// PlanPath is the plan file path or ref named in OBJECTIVE.md / PROGRESS.md.
	// Next is the stated Next: work from OBJECTIVE.md (or PROGRESS.md).
	Objective, PlanPath, Next string
	Context, IntegrateRule    string
	NoPR                      bool
	Phases                    []string
	// Progress and Escalations are PROGRESS.md and ESCALATIONS.md, clipped.
	Progress, Escalations string
	// Unchecked lists what discovery could not check for this run.
	Unchecked []string
	Lanes     []Lane
}

// FullObjective returns the complete run objective combining Goal, the full
// OBJECTIVE.md content (milestones, open items, plan references), PlanPath,
// and Next: work so the store, planner, and concierge see the whole objective
// rather than a one-line summary.
func (r Run) FullObjective() string {
	goal := strings.TrimSpace(r.Goal)
	obj := strings.TrimSpace(r.Objective)
	if r.Dir != "" && obj != "" {
		obj = sanitizeObjectivePlanLines(r.Dir, obj)
	}
	switch {
	case obj == "":
		obj = goal
	case goal != "" && !strings.Contains(strings.ToLower(obj), strings.ToLower(goal)):
		obj = "Goal: " + goal + "\n\n" + obj
	}
	if plan := strings.TrimSpace(r.PlanPath); plan != "" && !strings.Contains(obj, plan) {
		if obj != "" {
			obj += "\n"
		}
		obj += "Plan: " + plan
	}
	if next := strings.TrimSpace(r.Next); next != "" && !strings.Contains(obj, next) {
		if obj != "" {
			obj += "\n"
		}
		obj += "Next: " + next
	}
	return strings.TrimSpace(obj)
}

func sanitizeObjectivePlanLines(dir, obj string) string {
	lines := strings.Split(obj, "\n")
	keep := make([]string, 0, len(lines))
	for _, line := range lines {
		if m := sanitizePlanLineRE.FindStringSubmatch(line); len(m) == 2 {
			if resolveSafePlanRef(dir, m[1]) == "" {
				continue
			}
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func isEmptyNextText(s string) bool {
	return store.IsDoneOrEmptyNext(s)
}

// IsDoneOrEmptyNext reports whether a Next: value indicates there is no remaining work.
func IsDoneOrEmptyNext(s string) bool {
	return store.IsDoneOrEmptyNext(s)
}

func hasExplicitNextSection(sources ...string) bool {
	return store.HasExplicitNextSection(sources...)
}

// ObjectiveIsDone reports whether the latest Next: or status lines in md
// declare the run done/complete with no remaining work.
func ObjectiveIsDone(md string) bool {
	return store.ObjectiveIsDone(md)
}

// CurrentNextSection extracts the latest Next: line or # Next section from sources.
func CurrentNextSection(sources ...string) string {
	return store.ExtractNextSection(sources...)
}

var uncheckedTaskItemRE = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+[.)])\s+\[\s\]`)

// HasNextWork reports whether the run's objective/progress states open Next:
// work or unchecked items beyond the currently imported lanes.
func (r Run) HasNextWork() bool {
	if ObjectiveIsDone(r.Objective) || (r.Objective == "" && ObjectiveIsDone(r.Progress)) {
		return false
	}
	next := strings.TrimSpace(r.Next)
	hasExplicit := next != "" || hasExplicitNextSection(r.Objective, r.Progress)
	if next == "" && r.Objective != "" {
		next = strings.TrimSpace(extractNextSection(r.Objective))
	}
	if !isEmptyNextText(next) {
		return true
	}
	if hasExplicit {
		return false
	}
	return uncheckedTaskItemRE.MatchString(r.Objective)
}

// Importer finds and reads run directories: <root>/.shared/subagent-swarm/<run>
// (subagent-swarm-nebula) and <root>/<run> (nebula-swarm, as in ~/.swarm).
type Importer struct {
	// Orchestrators, when set, reads which session Nebula records as each
	// run's orchestrator; Loops finds each run's live step loop or watcher.
	Orchestrators RunOrchestrators
	Loops         LoopFinder
	// Roots defaults to the current and sibling checkouts, ~/.swarm,
	// $SW_HOME and $ORCA_SWARM_ROOTS.
	Roots []string
}

func (i Importer) roots() []string {
	if len(i.Roots) > 0 {
		return i.Roots
	}
	var out []string
	seen := map[string]bool{}
	appendRoot := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		if !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}
	scanSiblings := func(parent string) {
		if parent == "" || parent == string(filepath.Separator) {
			return
		}
		ents, err := os.ReadDir(parent)
		if err != nil {
			return
		}
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			root := filepath.Join(parent, ent.Name())
			if fileExists(filepath.Join(root, ".shared", "subagent-swarm")) {
				appendRoot(root)
			}
			// Host checkouts commonly live at <repo>/worktrees/<repo>;
			// include those sibling worktree roots as well.
			worktrees := filepath.Join(root, "worktrees")
			worktreeEnts, _ := os.ReadDir(worktrees)
			for _, worktree := range worktreeEnts {
				if worktree.IsDir() {
					candidate := filepath.Join(worktrees, worktree.Name())
					if fileExists(filepath.Join(candidate, ".shared", "subagent-swarm")) {
						appendRoot(candidate)
					}
				}
			}
		}
	}
	if b, err := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output(); err == nil {
		current := filepath.Dir(strings.TrimSpace(string(b)))
		appendRoot(current)
		parent := filepath.Dir(current)
		scanSiblings(parent)
		if filepath.Base(parent) == "worktrees" {
			scanSiblings(filepath.Dir(filepath.Dir(parent)))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		appendRoot(filepath.Join(home, ".swarm"))
	}
	if v := os.Getenv("SW_HOME"); v != "" {
		appendRoot(v)
		appendRoot(filepath.Join(v, ".swarm"))
	}
	if v := os.Getenv("ORCA_SWARM_ROOTS"); v != "" {
		for _, root := range filepath.SplitList(v) {
			appendRoot(root)
		}
	}
	return out
}

// runDirs lists each root's subagent-swarm-nebula runs, its nebula-swarm
// runs (direct children) and the root itself when it is a nebula-swarm run.
func (i Importer) runDirs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	for _, root := range i.roots() {
		base := filepath.Join(root, ".shared", "subagent-swarm")
		ents, _ := os.ReadDir(base)
		for _, ent := range ents {
			dir := filepath.Join(base, ent.Name())
			if ent.IsDir() && fileExists(filepath.Join(dir, "state.json")) {
				add(dir)
			}
		}
		if isNebulaSwarmDir(root) {
			add(root)
			continue
		}
		ents, _ = os.ReadDir(root)
		for _, ent := range ents {
			dir := filepath.Join(root, ent.Name())
			if ent.IsDir() && isNebulaSwarmDir(dir) {
				add(dir)
			}
		}
	}
	return out
}

// Discover reads every run under the roots, says which orchestrator session
// drives each now and whether a newer run superseded it. Runs that fail to
// read are reported in the joined error next to the runs that did.
func (i Importer) Discover(ctx context.Context) ([]Run, error) {
	var out []Run
	var errs []error
	for _, dir := range i.runDirs() {
		r, err := Read(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if i.Orchestrators == nil {
			r.Unchecked = append(r.Unchecked, "Nebula run orchestrator (no Nebula client)")
		} else if sid, err := i.Orchestrators.RunOrchestratorSession(ctx, r.ID); err != nil {
			r.Unchecked = append(r.Unchecked, "Nebula run orchestrator: "+err.Error())
		} else if sid != "" {
			r.DrivenBy = sid
		}
		if i.Loops == nil {
			r.Unchecked = append(r.Unchecked, "step loop/watcher processes")
		} else if pids, err := i.Loops.Running(ctx, dir); err != nil {
			r.Unchecked = append(r.Unchecked, "step loop/watcher processes: "+err.Error())
		} else if len(pids) > 0 {
			var pane string
			r.Loop, pane = i.Loops.Describe(ctx, pids)
			// A host orchestrator joins no Nebula session; its tmux pane is
			// what drives the run.
			if r.DrivenBy == "" && pane != "" {
				r.DrivenBy = "tmux " + pane
			}
		}
		out = append(out, r)
	}
	classify(out)
	sortRuns(out)
	return out, errors.Join(errs...)
}

// Find returns the directory of run id.
func (i Importer) Find(id string) (string, error) {
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return "", fmt.Errorf("%w: %q", ErrRunNotFound, id)
	}
	var found []string
	for _, dir := range i.runDirs() {
		if filepath.Base(dir) == id {
			found = append(found, dir)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: %q", ErrRunNotFound, id)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("run %q is ambiguous: %s", id, strings.Join(found, ", "))
}

//nolint:govet // fields mirror both host ledger versions and preserve their JSON names.
type ledgerLane struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Branch         string `json:"branch"`
	Phase          string `json:"phase"`
	Status         string `json:"status"`
	NebulaSession  string `json:"nebula_session"`
	NebulaTerminal string `json:"nebula_terminal"`
	RepoURL        string `json:"repo_url"`
	PR             int    `json:"pr"`
	Round          int    `json:"round"`
	BlockedOn      string `json:"blocked_on"`
	Hold           string `json:"hold"`
	HoldNote       string `json:"hold_note"`
	Park           string `json:"park"`
	Note           string `json:"note"`
	PRDraft        bool   `json:"pr_draft"`
	Draft          bool   `json:"draft"`
	PRState        string `json:"pr_state"`
	MergeSHA       string `json:"merge_sha"`
	Head           string `json:"head"`
	Checks         string `json:"checks"`
}

type ledger struct {
	Config struct {
		Goal      string      `json:"goal"`
		Repo      string      `json:"repo"`
		Target    string      `json:"target"`
		MergeRule string      `json:"merge_rule"`
		Phases    []hostPhase `json:"phases"`
	} `json:"config"`
	Tasks []ledgerLane `json:"tasks"`
}

func hostPhaseNames(phases []hostPhase) []string {
	out := make([]string, 0, len(phases))
	for _, p := range phases {
		if name := strings.TrimSpace(p.Name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Read imports the ledger (state.json) of one run directory and each lane's
// current round from its newest spawn file and report.
func Read(dir string) (Run, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return Run{}, fmt.Errorf("read ledger: %w", err)
	}
	r := Run{ID: filepath.Base(dir), Dir: dir, LastActivity: lastActivity(dir), Format: detectFormat(dir, b)}
	if j, err := readJournal(filepath.Join(dir, leaseFile)); err == nil {
		r.Holder = j.Holder
	}
	r.Orchestrator = orchestrator(dir, r.Holder)
	var localRepoPaths []string
	applyRunEnvRepo(dir, &r, &localRepoPaths)
	if r.Repo == "" {
		r.Repo = repoSlug(dir)
	}
	r.Objective = stripMarker(readText(filepath.Join(dir, "OBJECTIVE.md")))
	r.Context = stripMarker(readText(filepath.Join(dir, "CONTEXT.md")))
	r.Progress = clip(stripMarker(readText(filepath.Join(dir, "PROGRESS.md"))), 1500)
	r.Escalations = clip(stripMarker(readText(filepath.Join(dir, "ESCALATIONS.md"))), 1500)
	if r.Format == FormatNebulaSwarm {
		if err := readNebulaSwarm(dir, b, &r, &localRepoPaths); err != nil {
			return Run{}, err
		}
	} else {
		var st ledger
		if err := json.Unmarshal(b, &st); err != nil {
			return Run{}, fmt.Errorf("parse %s: %w", filepath.Join(dir, "state.json"), err)
		}
		r.Goal = st.Config.Goal
		r.Phases = hostPhaseNames(st.Config.Phases)
		if isLocalRepoPath(st.Config.Repo) {
			localRepoPaths = append(localRepoPaths, st.Config.Repo)
		} else {
			applyRepoValue(&r, st.Config.Repo)
		}
		r.TargetBranch = strings.TrimSpace(st.Config.Target)
		for i := range st.Tasks {
			if isLocalRepoPath(st.Tasks[i].RepoURL) {
				localRepoPaths = append(localRepoPaths, st.Tasks[i].RepoURL)
			} else {
				applyRepoValue(&r, st.Tasks[i].RepoURL)
			}
			r.Lanes = append(r.Lanes, importLane(dir, &st.Tasks[i]))
		}
		annotateLanes(dir, &r, st.Config.Phases)
	}
	if r.Repo == "" && r.RepoID == "" {
		applyOriginRepo(dir, &r, localRepoPaths)
	}
	r.RepoPath = firstLocalRepoPath(dir, localRepoPaths)
	r.PlanPath = extractPlanPath(dir, r.Objective, r.Progress)
	r.Next = extractNextSection(r.Objective)
	r.NoPR = runNoPR(dir, b)
	var contract struct {
		Config struct {
			MergeRule string `json:"merge_rule"`
		} `json:"config"`
	}
	_ = json.Unmarshal(b, &contract)
	r.IntegrateRule = extractMergeRuleText(contract.Config.MergeRule, r.Objective+"\n"+r.Context)
	for i := range r.Lanes {
		r.Lanes[i].HoldAdvance = fileExists(filepath.Join(dir, r.Lanes[i].ID+".hold-advance"))
		r.Lanes[i].HostHold = hostHoldReason(dir, r.Lanes[i].ID, r.Lanes[i].HostHold)
	}
	sort.Slice(r.Lanes, func(a, b int) bool { return r.Lanes[a].ID < r.Lanes[b].ID })
	r.State = RunFinished
	if runLive(&r) {
		r.State = RunActive
	}
	return r, nil
}

func firstLocalRepoPath(runDir string, paths []string) string {
	for _, raw := range paths {
		p := strings.Trim(strings.TrimSpace(raw), `"'`)
		if p == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(p), "file://") {
			p = strings.TrimPrefix(p, "file://")
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			home, err := os.UserHomeDir()
			if err != nil || home == "" {
				continue
			}
			p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(runDir, p)
		}
		p = filepath.Clean(p)
		if info, err := os.Stat(p); err == nil && (info.IsDir() || strings.HasSuffix(p, ".git")) {
			return p
		}
	}
	cur := filepath.Clean(runDir)
	for depth := 0; depth < 8; depth++ {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return ""
}

// applyIntegrationEvidence checks the host's own merge record and local Git
// once per run. It deliberately has no GitHub dependency: GitHub state can be
// stale for a feature-branch target and is not evidence that this lane landed.
func applyIntegrationEvidence(ctx context.Context, r *Run) {
	if r == nil {
		return
	}
	needsGit := false
	for i := range r.Lanes {
		if !integratedLane(r.Lanes[i]) {
			needsGit = true
			break
		}
	}
	if !needsGit {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(r.RepoPath) == "" {
		r.IntegrationError = "no local checkout"
		return
	}
	if _, err := os.Stat(r.RepoPath); err != nil {
		r.IntegrationError = "local checkout unavailable"
		return
	}
	target := strings.TrimSpace(r.TargetBranch)
	if target == "" {
		r.IntegrationError = "run target branch is missing"
		return
	}
	// Fetch exactly once for the run. Lane checks below only inspect the fetched
	// target ref and never call a per-lane remote or GitHub API.
	fetch := exec.CommandContext(ctx, "git", "-C", r.RepoPath, "fetch", "--no-tags", "origin", target)
	if err := fetch.Run(); err != nil {
		r.IntegrationError = "fetch target " + target + " failed"
		return
	}
	targetRef := ""
	for _, ref := range []string{"refs/remotes/origin/" + target, "refs/heads/" + target, target} {
		if out, err := exec.CommandContext(ctx, "git", "-C", r.RepoPath, "rev-parse", "--verify", ref).Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			targetRef = ref
			break
		}
	}
	if targetRef == "" {
		r.IntegrationError = "fetched target " + target + " is unavailable"
		return
	}
	var targetSubjects []string
	if out, err := exec.CommandContext(ctx, "git", "-C", r.RepoPath, "log", "--format=%s", targetRef).Output(); err == nil {
		targetSubjects = strings.Split(string(out), "\n")
	}
	for i := range r.Lanes {
		ln := &r.Lanes[i]
		if integratedLane(*ln) {
			continue
		}
		head := strings.TrimSpace(ln.Head)
		if head == "" && strings.TrimSpace(ln.Branch) != "" {
			for _, ref := range []string{"refs/heads/" + ln.Branch, "refs/remotes/origin/" + ln.Branch, ln.Branch} {
				if out, err := exec.CommandContext(ctx, "git", "-C", r.RepoPath, "rev-parse", "--verify", ref).Output(); err == nil {
					head = strings.TrimSpace(string(out))
					break
				}
			}
		}
		if head == "" {
			ln.IntegrationReason = "integration gate: missing lane head for target " + target
			continue
		}
		if err := exec.CommandContext(ctx, "git", "-C", r.RepoPath, "merge-base", "--is-ancestor", head, targetRef).Run(); err == nil {
			ln.Integrated = true
			ln.IntegrationReason = fmt.Sprintf("integration gate: lane head %s is an ancestor of target %s", shortSHA(head), target)
			continue
		}
		if pr := strings.TrimSpace(ln.PR); pr != "" {
			suffix := "(#" + pr + ")"
			for _, subject := range targetSubjects {
				if strings.HasSuffix(strings.TrimSpace(subject), suffix) {
					ln.Integrated = true
					ln.IntegrationReason = fmt.Sprintf("integration gate: target %s has squash subject ending %s", target, suffix)
					break
				}
			}
		}
		if !ln.Integrated {
			ln.IntegrationReason = fmt.Sprintf("integration gate: lane head %s is not in target %s and no squash subject ending (#%s)", shortSHA(head), target, prNumber(ln.PR))
		}
	}
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func prNumber(pr string) string {
	if strings.TrimSpace(pr) == "" {
		return "PR"
	}
	return strings.TrimSpace(pr)
}

// hostHoldReason reads only scrubbed run-local metadata. It is intentionally
// conservative: a prose line must name the lane and contain an explicit hold,
// park, or block marker before it can veto a lifecycle action.
func hostHoldReason(dir, laneID, current string) string {
	if strings.TrimSpace(current) != "" {
		return clip(strings.TrimSpace(current), 240)
	}
	for _, suffix := range []string{".hold", ".park", ".blocked", ".hold-note"} {
		if text := strings.TrimSpace(readText(filepath.Join(dir, laneID+suffix))); text != "" {
			return clip(text, 240)
		}
	}
	for _, name := range []string{"ESCALATIONS.md", "PROGRESS.md", "CONTEXT.md", "OBJECTIVE.md"} {
		for _, line := range strings.Split(readText(filepath.Join(dir, name)), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && containsWord(trimmed, laneID) && explicitHostHoldLine(trimmed) {
				return clip(trimmed, 240)
			}
		}
	}
	return ""
}

func explicitHostHoldLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if strings.Contains(lower, "hold-advance") || strings.Contains(lower, "hold released") || strings.Contains(lower, "unblocked") {
		return false
	}
	for _, negative := range []string{"hold: none", "hold=none", "blocked: no", "blocked=no", "blocked_on: n/a", "blocked_on=n/a", "park: none", "park=none"} {
		if strings.Contains(lower, negative) {
			return false
		}
	}
	return hostHoldLineRE.MatchString(lower)
}

var hostHoldLineRE = regexp.MustCompile(`(?:^|\s)(?:hold(?:_note)?|park(?:ed)?|blocked(?:_on)?)(?:\s+(?:on|by))?(?:\s*[:=]|\s+)`)

func ledgerHostHold(values ...string) string {
	for i, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if i == len(values)-1 && !explicitHostHoldLine(value) {
			continue
		}
		if meaningfulHostValue(value) {
			return value
		}
	}
	return ""
}

var (
	explicitPlanLineRE = regexp.MustCompile(`(?im)^\s*plan(?:\s+file|\s+path)?\s*[:=]\s*([^\s` + "`" + `"',;()<>]+)`)
	sanitizePlanLineRE = regexp.MustCompile(`(?im)^\s*(?:[-*+]\s*)?plan(?:\s+file|\s+path)?\s*[:=]\s*([^\s` + "`" + `"',;()<>]+)`)
	markdownPathRE     = regexp.MustCompile(`([^\s` + "`" + `"',;()<>]+\.md)\b`)
)

func isIgnoredDocName(p string) bool {
	switch strings.ToUpper(filepath.Base(p)) {
	case "OBJECTIVE.MD", "PROGRESS.MD", "ESCALATIONS.MD", "CLAUDE.MD", "AGENTS.MD", "README.MD", "SKILL.MD":
		return true
	default:
		return false
	}
}

func isNebulaPostPlanRef(ref string) bool {
	trimmed := strings.TrimSpace(ref)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"post:", "nebula://post/", "nebula:"} {
		if strings.HasPrefix(lower, prefix) {
			target := strings.TrimSpace(trimmed[len(prefix):])
			return target != "" && !strings.HasPrefix(target, "-")
		}
	}
	if len(trimmed) != 36 {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

func resolveSafePlanRef(dir, candidate string) string {
	candidate = strings.TrimLeft(strings.TrimSpace(candidate), "`\"'(")
	candidate = strings.TrimRight(candidate, "`\"'.,;:)")
	if candidate == "" || isIgnoredDocName(candidate) {
		return ""
	}
	if isNebulaPostPlanRef(candidate) {
		return candidate
	}
	if strings.HasPrefix(strings.ToLower(candidate), "file://") {
		candidate = strings.TrimSpace(candidate[len("file://"):])
	}
	if dir != "" {
		resolved, err := resolveBriefPath(dir, candidate)
		if err != nil {
			return ""
		}
		return resolved
	}
	if filepath.IsAbs(candidate) {
		return ""
	}
	clean := filepath.Clean(candidate)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	return clean
}

func resolveCandidatePlanPath(dir, candidate string) string {
	resolved := resolveSafePlanRef(dir, candidate)
	if resolved == "" {
		return ""
	}
	if dir != "" && !isNebulaPostPlanRef(resolved) && !fileExists(resolved) {
		return ""
	}
	return resolved
}

func extractPlanPath(dir string, sources ...string) string {
	for _, src := range sources {
		if m := explicitPlanLineRE.FindStringSubmatch(src); len(m) == 2 {
			if p := resolveCandidatePlanPath(dir, m[1]); p != "" {
				return p
			}
		}
	}
	for _, src := range sources {
		if m := explicitPlanLineRE.FindStringSubmatch(src); len(m) == 2 {
			if p := resolveSafePlanRef(dir, m[1]); p != "" {
				return p
			}
		}
	}
	for _, src := range sources {
		for _, m := range markdownPathRE.FindAllStringSubmatch(src, -1) {
			if len(m) == 2 {
				if p := resolveCandidatePlanPath(dir, m[1]); p != "" {
					return p
				}
			}
		}
	}
	if dir != "" && fileExists(filepath.Join(dir, "PLAN.md")) {
		return filepath.Join(dir, "PLAN.md")
	}
	return ""
}

func extractNextSection(sources ...string) string {
	return store.ExtractNextSection(sources...)
}

// stripMarker drops orca's stand-down lines and surrounding blank space.
func stripMarker(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, markerPrefix) {
			keep = append(keep, line)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

type spawnFile struct {
	SessionID  string `json:"session_id"`
	TerminalID string `json:"terminal_id"`
}

func importLane(dir string, l *ledgerLane) Lane {
	x := Lane{ID: l.ID, Title: l.Title, Branch: l.Branch, Phase: l.Phase, Status: l.Status, Round: l.Round,
		HostHold: ledgerHostHold(l.BlockedOn, l.HoldNote, l.Hold, l.Park, l.Note), PRDraft: l.PRDraft || l.Draft,
		PRState: l.PRState, MergeSHA: l.MergeSHA, Head: l.Head, Checks: l.Checks}
	x.PRDraft = x.PRDraft || isDraftPR(x.PRState)
	phase, sp := newestSpawn(dir, l.ID, l.Phase)
	// The ledger records a terminal restart after spawn.json was written, so
	// its session and terminal are the newer ones when set.
	x.Session = firstNonEmpty(l.NebulaSession, sp.SessionID)
	x.Terminal = firstNonEmpty(l.NebulaTerminal, sp.TerminalID)
	if phase == "" {
		phase = l.Phase
	}
	if x.Phase == "" {
		x.Phase = phase
	}
	report := ""
	if p := filepath.Join(dir, l.ID+"."+phase+".md"); phase != "" && fileExists(p) {
		x.ReportPath = p
		report = readText(p)
	}
	if l.PR > 0 {
		x.PR = fmt.Sprint(l.PR)
	} else if m := prLine.FindStringSubmatch(report); m != nil {
		x.PR = m[1]
	}
	if x.MergeSHA != "" {
		x.Integrated = true
	}
	applyReportMetadata(&x, report)
	return x
}

func isDraftPR(state string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(state)), "draft")
}

// newestSpawn returns the phase and contents of the lane's most recently
// written <lane>.<phase>.spawn.json; a rework round rewrites the file of its
// phase, so the newest one is the current round.

func newestSpawn(dir, laneID, currentPhase string) (string, spawnFile) {
	if currentPhase != "" {
		p := filepath.Join(dir, laneID+"."+currentPhase+".spawn.json")
		if b, err := os.ReadFile(p); err == nil {
			var sp spawnFile
			if json.Unmarshal(b, &sp) == nil {
				return currentPhase, sp
			}
		}
	}
	paths, _ := filepath.Glob(filepath.Join(dir, laneID+".*.spawn.json"))
	var phase string
	var newest time.Time
	var out spawnFile
	for _, p := range paths {
		ph := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), laneID+"."), ".spawn.json")
		info, err := os.Stat(p)
		if err != nil || strings.Contains(ph, ".") || (!info.ModTime().After(newest) && phase != "") {
			continue
		}
		var sp spawnFile
		if b, err := os.ReadFile(p); err != nil || json.Unmarshal(b, &sp) != nil {
			continue
		}
		phase, newest, out = ph, info.ModTime(), sp
	}
	return phase, out
}

var prLine = regexp.MustCompile(`(?m)^\s*PR:\s*#?(\d+)\s*$`)

func applyReportMetadata(ln *Lane, report string) {
	if ln == nil {
		return
	}
	if rep, err := lane.ParsePhaseReport(report); err == nil && rep.Outcome == lane.OutcomeBlocked {
		ln.ReportBlocker = clip(strings.TrimSpace(cmp.Or(rep.Reason, rep.Summary, rep.Ask, "blocked report")), 240)
	}
	for _, raw := range strings.Split(report, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(raw), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "head", "sha", "commit":
			if value != "" {
				ln.Head = firstNonEmpty(ln.Head, strings.Fields(value)[0])
			}
		case "checks", "ci":
			ln.Checks = firstNonEmpty(ln.Checks, value)
		case "draft", "pr draft":
			ln.PRDraft = ln.PRDraft || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
		case "pr state":
			normalized := strings.ToLower(value)
			if normalized == "draft" {
				ln.PRDraft = true
			}
			if normalized == "merged" || normalized == "closed" || normalized == "open" {
				ln.PRState = normalized
			}
		case "merged", "integrated":
			if strings.EqualFold(value, "true") || strings.EqualFold(value, "yes") {
				ln.Integrated = true
			}
		case "merge sha", "merged sha":
			if meaningfulHostValue(value) {
				ln.MergeSHA, ln.Integrated = value, true
			}
		}
	}
}

func meaningfulHostValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "n/a", "na", "none", "null", "unknown", "-":
		return false
	default:
		return true
	}
}

func runNoPR(dir string, ledger []byte) bool {
	var probe struct {
		Config struct {
			NoPR bool `json:"no_pr"`
		} `json:"config"`
	}
	if json.Unmarshal(ledger, &probe) == nil && probe.Config.NoPR {
		return true
	}
	if strings.TrimSpace(os.Getenv("SNL_RUN_ID")) == filepath.Base(dir) {
		value := strings.TrimSpace(os.Getenv("SNL_NO_PR"))
		if value != "" && value != "0" && !strings.EqualFold(value, "false") && !strings.EqualFold(value, "no") {
			return true
		}
	}
	// SNL_NO_PR is scoped by SNL_RUN_ID in the host env when present. Reading
	// env.sh keeps dry-run/adopt deterministic without inheriting another run's
	// process environment.
	env := readText(filepath.Join(dir, "env.sh"))
	for _, line := range strings.Split(env, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "SNL_NO_PR" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		return value != "" && value != "0" && !strings.EqualFold(value, "false") && !strings.EqualFold(value, "no")
	}
	return false
}

func orchestrator(dir, holder string) string {
	if holder != "" {
		return orcaLabel
	}
	for _, n := range handoverFiles {
		for _, line := range strings.Split(readText(filepath.Join(dir, n)), "\n") {
			if v, ok := strings.CutPrefix(line, "ORCHESTRATOR:"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return "claude"
}

func lastActivity(dir string) string {
	ents, _ := os.ReadDir(dir)
	var best time.Time
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), leaseFile) {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(best) {
			best = info.ModTime()
		}
	}
	if best.IsZero() {
		return ""
	}
	return best.UTC().Format(time.RFC3339)
}

// Labeler reads and sets the Nebula run's external orchestrator label.
type Labeler interface {
	RunOrchestrator(ctx context.Context, runID string) (string, error)
	SetRunOrchestrator(ctx context.Context, runID, label string) error
}

// Watcher controls the host orchestrator's loop processes: watch_lanes.sh
// for a subagent-swarm-nebula run, the sw_step loop for a nebula-swarm run.
type Watcher interface {
	// Running returns the pids of the run's armed watchers, loops and bridges.
	Running(ctx context.Context, runDir string) ([]int, error)
	Stop(ctx context.Context, pids []int) error
	Start(ctx context.Context, runDir string) error
}

// Service adopts and releases host runs on behalf of one lease holder.
type Service struct {
	Labeler Labeler
	Watcher Watcher
	Driver  RunDriver
	Now     func() time.Time
	// fault, when set by tests, fails the named step before it acts.
	fault  func(step string) error
	Holder string
	// Former lists legacy holder ids still owned by this service.
	Former   []string
	Importer Importer
	// LoopVerifyInterval is the pause before re-checking that no step loop
	// restarted after stopWatcher stops a running loop (or when explicitly set
	// > 0). Zero uses defaultLoopVerifyInterval when WatcherPIDs were stopped.
	LoopVerifyInterval time.Duration
}

// DryRunResult describes the first adoption ticks without taking a lease or
// starting a controller.
//
//nolint:govet // result groups the imported run, lane evidence, and report lines.
type DryRunResult struct {
	Run     Run
	Lanes   []DryRunLane
	Moves   []string
	Verdict string
}

// DryRunLane is the evidence and first-tick action a read-only adoption
// preview derives for one imported lane.
//
//nolint:govet // lane preview fields are intentionally grouped for report output.
type DryRunLane struct {
	ID              string
	Phase           string
	Status          string
	Evidence        []string
	Move            string
	Reason          string
	Gate            string
	EvidencePresent bool
}

// DryRun replays adoption's observe-first boundary from the host files only.
// It is deliberately side-effect free and is safe to run against a copied run
// directory before a live /adopt.
func (s *Service) DryRun(ctx context.Context, id string) (DryRunResult, error) {
	dir, err := s.Importer.Find(id)
	if err != nil {
		return DryRunResult{}, err
	}
	return s.DryRunDir(ctx, dir)
}

// DryRunDir previews adoption from an explicit run directory. It never opens
// a store, acquires a lease, calls a host adapter, or writes to the directory;
// callers can safely point it at a scrubbed copy of a live run.
func (s *Service) DryRunDir(ctx context.Context, dir string) (DryRunResult, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return DryRunResult{}, fmt.Errorf("%w: empty run directory", ErrRunNotFound)
	}
	dir = filepath.Clean(dir)
	r, err := Read(dir)
	if err != nil {
		return DryRunResult{}, err
	}
	applyIntegrationEvidence(ctx, &r)
	result := DryRunResult{Run: r, Moves: []string{"observe-first: read OBJECTIVE.md, CONTEXT.md, hold-advance files, and no-PR configuration"}}
	mutating := make([]string, 0)
	judgments := make([]string, 0)
	for i := range r.Lanes {
		preview := previewLane(r, r.Lanes[i])
		result.Lanes = append(result.Lanes, preview)
		if preview.Move != "none" && preview.Move != "wait" && preview.Move != "judge" {
			mutating = append(mutating, preview.ID+":"+preview.Move)
		} else if preview.Move == "judge" {
			judgments = append(judgments, preview.ID+":"+preview.Move)
		}
	}
	if len(mutating) > 0 {
		result.Verdict = "MOVES: " + strings.Join(mutating, ", ")
	} else if len(judgments) > 0 {
		result.Verdict = "JUDGES: " + strings.Join(judgments, ", ")
	} else {
		result.Verdict = "SAFE"
	}
	result.Moves = append(result.Moves, "adoption: no integrate, complete, release, retry, stop, or host state.json write")
	return result, nil
}

func previewLane(r Run, ln Lane) DryRunLane {
	p := DryRunLane{ID: ln.ID, Phase: cmp.Or(ln.Phase, "unknown"), Status: cmp.Or(ln.Status, "unknown"), Move: "none"}
	p.Evidence = append(p.Evidence, "format="+r.Format, "phase="+p.Phase, "status="+p.Status)
	if ln.TerminalState != "" {
		p.Evidence = append(p.Evidence, "terminal_state="+ln.TerminalState)
	}
	if ln.Report != "" {
		p.Evidence = append(p.Evidence, "report="+ln.Report)
	}
	if ln.LastEvent != "" {
		p.Evidence = append(p.Evidence, "event="+ln.LastEvent)
	}
	if ln.Next != "" {
		p.Evidence = append(p.Evidence, "next="+ln.Next)
	}
	if ln.PR != "" {
		p.Evidence = append(p.Evidence, "pr=#"+ln.PR)
	}
	if ln.HostHold != "" {
		p.Evidence = append(p.Evidence, "host-hold="+clip(ln.HostHold, 160))
	}
	if ln.PRDraft {
		p.Evidence = append(p.Evidence, "draft PR")
	}
	if ln.Integrated || ln.MergeSHA != "" {
		p.Evidence = append(p.Evidence, "integrated")
	}
	if ln.IntegrationReason != "" {
		p.Evidence = append(p.Evidence, ln.IntegrationReason)
	}
	if r.IntegrationError != "" {
		p.Evidence = append(p.Evidence, "integration-unknown="+r.IntegrationError)
	}
	if r.NoPR {
		p.Evidence = append(p.Evidence, "no-PR run")
	}
	if integratedLane(ln) {
		p.Reason = "lane is already integrated, merged, or closed; no lifecycle move"
		return p
	}
	report := previewOutcome(ln.Report)
	if report == string(lane.OutcomeBlocked) {
		p.Move = "judge"
		p.Reason = "orchestrator judgment: lane report blocked: " + cmp.Or(ln.ReportBlocker, "blocked report")
		return p
	}
	if ln.HoldAdvance {
		p.Evidence = append(p.Evidence, "hold-advance file present")
	}
	if hostHold := laneHostHold(ln); hostHold != "" {
		p.Move, p.Reason = "wait", "host-held: "+hostHold
		return p
	}
	if strings.EqualFold(ln.Status, "blocked") {
		p.Move, p.Reason = "wait", "host-held: blocked"
		return p
	}
	if ln.PRDraft {
		p.Move, p.Reason = "wait", "host-held: PR is draft"
		return p
	}
	if strings.EqualFold(ln.Status, "failed") || strings.EqualFold(ln.Status, "needs-human") {
		p.Reason = "run or lane is already terminal; no lifecycle move"
		return p
	}
	if r.State == RunFinished && !reportDone(ln.Report) {
		p.Reason = "run is already terminal; no lifecycle move"
		return p
	}
	if ln.HoldAdvance {
		report := previewOutcome(ln.Report)
		if report != "done" && report != "no-change" {
			p.Move, p.Reason = "wait", "hold-advance requires orchestrator judgment"
			return p
		}
	}
	if strings.EqualFold(ln.TerminalState, "working") || strings.EqualFold(ln.TerminalState, "busy") {
		p.Move, p.Reason = "wait", "terminal is working; adoption must not complete, release, or stop it"
		return p
	}
	switch {
	case report == string(lane.OutcomeNeedsSWE):
		p.Move, p.Reason = "rework", "lane report requests SWE rework"
	case report == string(lane.OutcomeNeedsInput):
		p.Move, p.Reason = "judge", "orchestrator judgment: lane report requires input: "+cmp.Or(ln.ReportBlocker, "lane report requires input")
	case report == "done" || report == "no-change":
		if r.IntegrationError != "" {
			p.Move, p.Reason = "judge", integrationJudgeReason(r, ln)
		} else if ln.IntegrationReason != "" {
			p.Move, p.Reason = "judge", integrationJudgeReason(r, ln)
		} else if ln.HoldAdvance || sweOnly(r, ln) {
			p.Move, p.Gate, p.EvidencePresent = "judge", mergeGate(r), mergeGateEvidence(r, ln)
			p.Reason = fmt.Sprintf("%s evidence=%s", p.Gate, evidenceLabel(p.EvidencePresent, r, ln))
		} else if !r.NoPR && (strings.EqualFold(ln.Phase, "review") || strings.EqualFold(ln.Phase, "clean")) && r.IntegrateRule != "" {
			p.Move, p.Reason = "integrate", "completed report plus the run integrate rule permits the merge path"
		} else if strings.EqualFold(ln.Status, "done") {
			p.Move, p.Reason = "judge", integrationJudgeReason(r, ln)
		} else {
			p.Move, p.Reason = "advance", "completed report permits the next recipe phase"
		}
	default:
		p.Move, p.Reason = "wait", "no terminal handback or actionable report evidence"
	}
	return p
}

func integrationJudgeReason(r Run, ln Lane) string {
	if strings.TrimSpace(r.IntegrationError) != "" {
		return "integration unknown: " + strings.TrimSpace(r.IntegrationError)
	}
	if strings.TrimSpace(ln.IntegrationReason) != "" {
		return strings.TrimSpace(ln.IntegrationReason)
	}
	return "integration gate: missing merge_sha, merged EVENTS record, and local target evidence"
}

func laneHostHold(ln Lane) string {
	if strings.TrimSpace(ln.HostHold) != "" {
		return clip(strings.TrimSpace(ln.HostHold), 240)
	}
	if strings.EqualFold(strings.TrimSpace(ln.Status), "blocked") {
		return "blocked"
	}
	return ""
}

func integratedLane(ln Lane) bool {
	if strings.EqualFold(strings.TrimSpace(ln.Phase), "integrate") && strings.EqualFold(strings.TrimSpace(ln.Status), "done") {
		return true
	}
	if ln.Integrated || strings.TrimSpace(ln.MergeSHA) != "" {
		return true
	}
	state := strings.ToLower(strings.TrimSpace(ln.PRState))
	if state == "merged" || state == "closed" {
		return true
	}
	event := eventKind(ln.LastEvent)
	return event == "merged" || event == "closed"
}

func eventKind(event string) string {
	fields := strings.Fields(event)
	if len(fields) < 2 {
		return ""
	}
	return strings.ToLower(fields[1])
}

func reportDone(report string) bool {
	outcome := previewOutcome(report)
	return outcome == "done" || outcome == "no-change"
}

func sweOnly(r Run, ln Lane) bool {
	if len(r.Phases) == 1 {
		return strings.EqualFold(strings.TrimSpace(r.Phases[0]), "swe")
	}
	return len(r.Phases) == 0 && strings.EqualFold(strings.TrimSpace(ln.Phase), "swe") && strings.TrimSpace(ln.Next) == ""
}

func mergeGate(r Run) string {
	if gate := strings.TrimSpace(r.IntegrateRule); gate != "" {
		if strings.EqualFold(gate, "merge gate evidence") {
			return "run merge gate"
		}
		return clip(gate, 180)
	}
	return "run merge gate"
}

func mergeGateEvidence(r Run, ln Lane) bool {
	gate := strings.ToLower(strings.Join([]string{r.Objective, r.Context, r.IntegrateRule}, "\n"))
	evidence := strings.ToLower(strings.Join([]string{ln.Report, ln.LastEvent, ln.Checks}, "\n"))
	if strings.Contains(gate, "exact head") && strings.TrimSpace(ln.Head) == "" {
		return false
	}
	if strings.Contains(gate, "soak") {
		return containsEvidenceAny(evidence, "soak pass", "soak: pass", "soak passed")
	}
	if strings.Contains(gate, "a/b") || strings.Contains(gate, "ab test") {
		return containsEvidenceAny(evidence, "a/b pass", "a/b: pass", "a/b passed", "ab test pass", "ab test: pass", "ab test passed")
	}
	if strings.Contains(gate, "ci") || strings.Contains(gate, "check") {
		return isGreenEvidence(ln.Checks)
	}
	return false
}

func containsEvidenceAny(text string, phrases ...string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func isGreenEvidence(checks string) bool {
	switch strings.ToLower(strings.TrimSpace(checks)) {
	case "green", "pass", "passed", "success", "ok", "true":
		return true
	default:
		return false
	}
}

func evidenceLabel(present bool, r Run, ln Lane) string {
	if present {
		return "present"
	}
	missing := "merge gate evidence"
	if strings.TrimSpace(r.IntegrateRule) != "" {
		missing = "missing: " + clip(r.IntegrateRule, 120)
	} else if strings.TrimSpace(ln.Head) == "" {
		missing = "missing: exact head and gate result"
	}
	return missing
}

func previewOutcome(report string) string {
	rep, err := lane.ParsePhaseReport(report)
	if err != nil {
		return ""
	}
	return string(rep.Outcome)
}

const defaultLoopVerifyInterval = 50 * time.Millisecond

func (s *Service) loopVerifyInterval() time.Duration {
	if s.LoopVerifyInterval > 0 {
		return s.LoopVerifyInterval
	}
	return defaultLoopVerifyInterval
}

// owns reports whether a lease under holder belongs to this service.
func (s *Service) owns(holder string) bool {
	if holder == s.Holder || slices.Contains(s.Former, holder) {
		return true
	}
	// Legacy empty chat holder ("orca-chat:") migrates to an active chat holder.
	return strings.HasPrefix(s.Holder, ChatHolder("")) && holder == ChatHolder("")
}

// RunDriver drives imported lanes while orca owns the host run.
type RunDriver interface {
	Start(context.Context, Run) error
	Stop(context.Context, Run) error
}

// StoppedLoop records the step-loop process /adopt stopped so /release can
// restart it or tell the owner what to restart.
type StoppedLoop struct {
	Command    string   `json:"command,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	Args       []string `json:"args,omitempty"`
	IsStepLoop bool     `json:"is_step_loop,omitempty"`
}

// journal is the lease file: who holds the run and what each completed
// handover step changed, written before the step acts.
type journal struct {
	PrevLabel   *string      `json:"previous_label,omitempty"`
	Import      *Run         `json:"import,omitempty"`
	StoppedLoop *StoppedLoop `json:"stopped_loop,omitempty"`
	Holder      string       `json:"holder"`
	State       string       `json:"state"`
	Marker      string       `json:"marker,omitempty"`
	Created     []string     `json:"marker_created,omitempty"`
	WatcherPIDs []int        `json:"watcher_pids,omitempty"`
	WatcherStep bool         `json:"watcher_step,omitempty"`
}

// List returns the discoverable host runs.
func (s *Service) List(ctx context.Context) ([]Run, error) { return s.Importer.Discover(ctx) }

// Adopted returns the ids of the runs this holder leases.
func (s *Service) Adopted() ([]string, error) {
	var ids []string
	for _, dir := range s.Importer.runDirs() {
		if j, err := readJournal(filepath.Join(dir, leaseFile)); err == nil && s.owns(j.Holder) {
			ids = append(ids, filepath.Base(dir))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Adopt takes run id over from its host orchestrator.
func (s *Service) Adopt(ctx context.Context, id string) (Run, error) {
	if s.Holder == "" {
		return Run{}, errors.New("adopt: lease holder id is required")
	}
	dir, err := s.Importer.Find(id)
	if err != nil {
		return Run{}, err
	}
	j, err := s.takeLease(dir)
	if err != nil {
		return Run{}, err
	}
	if j.State == stateAdopted && j.Import != nil {
		return s.drive(ctx, dir)
	}
	j.State = stateAdopting
	if err := s.forward(ctx, dir, id, j, 2); err != nil {
		if _, uerr := s.unwind(ctx, dir, id, j); uerr != nil {
			return Run{}, fmt.Errorf("adopt %s: %w; undo incomplete (%v), lease kept: rerun /adopt or /release", id, err, uerr)
		}
		return Run{}, fmt.Errorf("adopt %s: %w (handover undone)", id, err)
	}
	return s.drive(ctx, dir)
}

func (s *Service) drive(ctx context.Context, dir string) (Run, error) {
	r, err := Read(dir)
	if err != nil {
		return Run{}, err
	}
	applyIntegrationEvidence(ctx, &r)
	if s.Driver != nil {
		if err := s.Driver.Start(ctx, r); err != nil {
			return Run{}, fmt.Errorf("start adopted run %s: %w (lease kept; retry /adopt or /release)", r.ID, err)
		}
	}
	return r, nil
}

// Release hands run id back to its host orchestrator.
func (s *Service) Release(ctx context.Context, id string) error {
	dir, err := s.Importer.Find(id)
	if err != nil {
		return err
	}
	p := filepath.Join(dir, leaseFile)
	orig, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrLeaseNotHeld, id)
	}
	if err != nil {
		return err
	}
	j, err := parseJournal(orig)
	if err != nil {
		return err
	}
	if !s.owns(j.Holder) {
		return fmt.Errorf("%w: %s is held by %s", ErrLeaseNotHeld, id, j.Holder)
	}
	if s.Driver != nil {
		r, err := Read(dir)
		if err != nil {
			return err
		}
		if err := s.Driver.Stop(ctx, r); err != nil {
			return fmt.Errorf("checkpoint adopted run %s: %w (adoption kept)", id, errors.Join(err, s.Driver.Start(ctx, r)))
		}
	}
	j.State = stateReleasing
	if err := s.save(dir, j); err != nil {
		if s.Driver != nil {
			_, rerr := s.drive(ctx, dir)
			err = errors.Join(err, rerr)
		}
		return err
	}
	next, err := s.unwind(ctx, dir, id, j)
	if err == nil {
		return nil
	}
	// Re-apply the release steps that completed, newest first, from the
	// original journal (same stand-down line, same previous label), then
	// restore the journal exactly as it was before /release.
	if next < 5 {
		j0, _ := parseJournal(orig)
		if rerr := s.forward(ctx, dir, id, j0, next+1); rerr != nil {
			return fmt.Errorf("release %s: %w; restoring the adoption failed (%v), lease kept: rerun /release", id, err, rerr)
		}
	}
	if werr := writeAtomic(p, orig); werr != nil {
		return fmt.Errorf("release %s: %w; restoring the lease failed: %v", id, err, werr)
	}
	if s.Driver != nil {
		_, rerr := s.drive(ctx, dir)
		if rerr != nil {
			return fmt.Errorf("release %s: %w; restart controller: %v", id, err, rerr)
		}
	}
	return fmt.Errorf("release %s: %w (adoption kept)", id, err)
}

func (s *Service) takeLease(dir string) (*journal, error) {
	p := filepath.Join(dir, leaseFile)
	j := &journal{Holder: s.Holder, State: stateAdopting}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	// Link publishes the complete journal only if no lease exists, so a crash
	// never leaves a half-written lease. Each contender needs its own staging
	// file: a shared name lets concurrent adopters publish one another's holder.
	tmpFile, err := os.CreateTemp(dir, ".orca-lease-*")
	if err != nil {
		return nil, err
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if err := tmpFile.Chmod(0o600); err != nil {
		tmpFile.Close()
		return nil, err
	}
	if _, err := tmpFile.Write(b); err != nil {
		tmpFile.Close()
		return nil, err
	}
	if err := tmpFile.Close(); err != nil {
		return nil, err
	}
	err = os.Link(tmp, p)
	if err == nil {
		return j, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	cur, err := readJournal(p)
	if err != nil {
		return nil, err
	}
	if !s.owns(cur.Holder) {
		return nil, fmt.Errorf("%w: %s", ErrLeaseHeld, cur.Holder)
	}
	if cur.Holder != s.Holder {
		if err := s.migrateJournal(dir, cur); err != nil {
			return nil, err
		}
	}
	return cur, nil
}

func (s *Service) migrateJournal(dir string, cur *journal) error {
	oldHolder := cur.Holder
	oldMarker := cur.Marker
	cur.Holder = s.Holder
	if cur.Import != nil {
		cur.Import.Holder = s.Holder
	}
	if cur.Marker != "" {
		cur.Marker = strings.ReplaceAll(cur.Marker, "("+oldHolder+")", "("+s.Holder+")")
	}
	for _, n := range handoverFiles {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := string(b)
		if oldMarker != "" {
			if rest, ok := strings.CutPrefix(content, oldMarker); ok {
				if err := writeAtomic(p, []byte(cur.Marker+rest)); err != nil {
					return err
				}
				continue
			}
		}
		if strings.HasPrefix(content, markerPrefix) {
			first, rest, hasNewline := strings.Cut(content, "\n")
			updated := strings.ReplaceAll(first, "("+oldHolder+")", "("+s.Holder+")")
			if hasNewline {
				updated += "\n"
			}
			if cur.Marker == "" {
				cur.Marker = updated
			}
			if updated != first+"\n" {
				if err := writeAtomic(p, []byte(updated+rest)); err != nil {
					return err
				}
			}
		}
	}
	return s.save(dir, cur)
}

// forward applies adopt steps from..5 and marks the run adopted.
func (s *Service) forward(ctx context.Context, dir, id string, j *journal, from int) error {
	steps := []func() error{
		2: func() error { return s.importRun(dir, j) },
		3: func() error { return s.mark(dir, j) },
		4: func() error { return s.label(ctx, dir, id, j) },
		5: func() error { return s.stopWatcher(ctx, dir, j) },
	}
	for n := from; n <= 5; n++ {
		if err := steps[n](); err != nil {
			return err
		}
	}
	j.State = stateAdopted
	return s.save(dir, j)
}

// unwind undoes steps 5..1 from the journal. It returns the step that failed
// (0 when all succeeded), leaving the journal to describe what remains.
func (s *Service) unwind(ctx context.Context, dir, id string, j *journal) (int, error) {
	if j.State != stateReleasing {
		j.State = stateReleasing
		_ = s.save(dir, j)
	}
	steps := []func() error{
		1: func() error {
			if err := s.check("release:lease"); err != nil {
				return err
			}
			return removeIfExists(filepath.Join(dir, leaseFile))
		},
		2: func() error { return s.unimport(dir, j) },
		3: func() error { return s.unmark(dir, j) },
		4: func() error { return s.unlabel(ctx, dir, id, j) },
		5: func() error { return s.startWatcher(ctx, dir, j) },
	}
	for n := 5; n >= 1; n-- {
		if err := steps[n](); err != nil {
			return n, err
		}
	}
	return 0, nil
}

func (s *Service) check(step string) error {
	if s.fault == nil {
		return nil
	}
	return s.fault(step)
}

func (s *Service) importRun(dir string, j *journal) error {
	if err := s.check("adopt:import"); err != nil {
		return err
	}
	if j.Import != nil {
		return nil
	}
	r, err := Read(dir)
	if err != nil {
		return err
	}
	r.Holder, r.Orchestrator = s.Holder, orcaLabel
	j.Import = &r
	return s.save(dir, j)
}

func (s *Service) unimport(dir string, j *journal) error {
	if err := s.check("release:import"); err != nil {
		return err
	}
	if j.Import == nil {
		return nil
	}
	j.Import = nil
	return s.save(dir, j)
}

func (s *Service) mark(dir string, j *journal) error {
	if err := s.check("adopt:marker"); err != nil {
		return err
	}
	if j.Marker == "" {
		now := time.Now()
		if s.Now != nil {
			now = s.Now()
		}
		j.Marker = fmt.Sprintf("%s%s (%s). Claude sessions: do not act on this run; run /release in orca to hand back.\n", markerPrefix, now.UTC().Format(time.RFC3339Nano), s.Holder)
		j.Created = nil
		for _, n := range handoverFiles {
			if !fileExists(filepath.Join(dir, n)) {
				j.Created = append(j.Created, n)
			}
		}
		if err := s.save(dir, j); err != nil {
			return err
		}
	}
	for _, n := range handoverFiles {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if strings.HasPrefix(string(b), j.Marker) {
			continue
		}
		body := string(b)
		for strings.HasPrefix(body, markerPrefix) {
			_, rest, ok := strings.Cut(body, "\n")
			if !ok {
				body = ""
				break
			}
			body = rest
		}
		if err := writeAtomic(p, append([]byte(j.Marker), []byte(body)...)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) unmark(dir string, j *journal) error {
	if err := s.check("release:marker"); err != nil {
		return err
	}
	if j.Marker == "" {
		return nil
	}
	for _, n := range handoverFiles {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		rest, ok := strings.CutPrefix(string(b), j.Marker)
		switch {
		case !ok:
		case rest == "" && slices.Contains(j.Created, n):
			err = os.Remove(p)
		default:
			err = writeAtomic(p, []byte(rest))
		}
		if err != nil {
			return err
		}
	}
	j.Marker, j.Created = "", nil
	return s.save(dir, j)
}

func (s *Service) label(ctx context.Context, dir, id string, j *journal) error {
	if err := s.check("adopt:label"); err != nil {
		return err
	}
	if j.PrevLabel == nil {
		cur, err := s.Labeler.RunOrchestrator(ctx, id)
		if err != nil {
			return err
		}
		j.PrevLabel = &cur
		if err := s.save(dir, j); err != nil {
			return err
		}
	}
	return s.Labeler.SetRunOrchestrator(ctx, id, orcaLabel)
}

func (s *Service) unlabel(ctx context.Context, dir, id string, j *journal) error {
	if err := s.check("release:label"); err != nil {
		return err
	}
	if j.PrevLabel == nil {
		return nil
	}
	// A label write that never landed needs no restore.
	if cur, err := s.Labeler.RunOrchestrator(ctx, id); err != nil || cur != *j.PrevLabel {
		if err := s.Labeler.SetRunOrchestrator(ctx, id, *j.PrevLabel); err != nil {
			return err
		}
	}
	j.PrevLabel = nil
	return s.save(dir, j)
}

func (s *Service) stopWatcher(ctx context.Context, dir string, j *journal) error {
	if err := s.check("adopt:watcher"); err != nil {
		return err
	}
	if s.Watcher == nil {
		return errors.New("step loop watcher is not configured")
	}
	if !j.WatcherStep {
		pids, err := s.Watcher.Running(ctx, dir)
		if err != nil {
			return err
		}
		j.WatcherPIDs, j.StoppedLoop, j.WatcherStep = pids, inspectWatcherLoop(s.Watcher, pids), true
		if err := s.save(dir, j); err != nil {
			return err
		}
	}
	// A watcher that exits can leave a bridge behind, so stop until none runs.
	for range 3 {
		pids, err := s.Watcher.Running(ctx, dir)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			break
		}
		if j.StoppedLoop == nil {
			j.StoppedLoop = inspectWatcherLoop(s.Watcher, pids)
			_ = s.save(dir, j)
		}
		if err := s.Watcher.Stop(ctx, pids); err != nil {
			return err
		}
	}
	if pids, err := s.Watcher.Running(ctx, dir); err != nil || len(pids) > 0 {
		return errors.Join(err, fmt.Errorf("watcher still running: %v", pids))
	}
	// Re-check after one loop interval to verify a scheduled tick or bounded
	// step loop did not immediately restart a new loop under our lease.
	if len(j.WatcherPIDs) > 0 || s.LoopVerifyInterval > 0 {
		timer := time.NewTimer(s.loopVerifyInterval())
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if pids, err := s.Watcher.Running(ctx, dir); err != nil || len(pids) > 0 {
		if len(pids) > 0 {
			_ = s.Watcher.Stop(ctx, pids)
		}
		return errors.Join(err, fmt.Errorf("step loop restarted under orca lease: %v", pids))
	}
	return nil
}

func inspectWatcherLoop(w Watcher, pids []int) *StoppedLoop {
	if len(pids) == 0 {
		return nil
	}
	if ins, ok := w.(interface{ Inspect([]int) *StoppedLoop }); ok {
		if l := ins.Inspect(pids); l != nil {
			return l
		}
	}
	return inspectStoppedLoop("", pids)
}

func (s *Service) startWatcher(ctx context.Context, dir string, j *journal) error {
	if err := s.check("release:watcher"); err != nil {
		return err
	}
	if !j.WatcherStep {
		return nil
	}
	if len(j.WatcherPIDs) > 0 {
		pids, err := s.Watcher.Running(ctx, dir)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			var startErr error
			if r, ok := s.Watcher.(interface {
				Restart(context.Context, string, *StoppedLoop) error
			}); ok {
				startErr = r.Restart(ctx, dir, j.StoppedLoop)
			} else {
				startErr = s.Watcher.Start(ctx, dir)
				if startErr != nil && j.StoppedLoop != nil && j.StoppedLoop.Command != "" {
					startErr = fmt.Errorf("%w (restart manually: %s)", startErr, j.StoppedLoop.Command)
				}
			}
			if startErr != nil {
				return startErr
			}
		}
	}
	j.WatcherPIDs, j.StoppedLoop, j.WatcherStep = nil, nil, false
	return s.save(dir, j)
}

func (s *Service) save(dir string, j *journal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicMode(filepath.Join(dir, leaseFile), append(b, '\n'), 0o600)
}

func readJournal(p string) (*journal, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return parseJournal(b)
}

func parseJournal(b []byte) (*journal, error) {
	var j journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("parse %s: %w", leaseFile, err)
	}
	return &j, nil
}

// writeAtomic replaces p, keeping its mode when it exists.
func writeAtomic(p string, b []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode().Perm()
	}
	return writeAtomicMode(p, b, mode)
}

func writeAtomicMode(p string, b []byte, mode os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(p), ".orca-tmp-*")
	if err != nil {
		return err
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if err := tmpFile.Chmod(mode); err != nil {
		tmpFile.Close()
		return err
	}
	if _, err := tmpFile.Write(b); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	return nil
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func readText(p string) string { b, _ := os.ReadFile(p); return string(b) }

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
