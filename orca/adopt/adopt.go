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
		if holder := strings.TrimSpace(j.Holder); holder != "" && j.State != stateReleasing {
			return holder, true
		}
	}
	for _, name := range handoverFiles {
		for _, line := range strings.Split(readText(filepath.Join(runDir, name)), "\n") {
			if strings.HasPrefix(line, markerPrefix) {
				if open := strings.IndexByte(line, '('); open >= 0 {
					if close := strings.IndexByte(line[open+1:], ')'); close >= 0 {
						if h := strings.TrimSpace(line[open+1 : open+1+close]); h != "" {
							return h, true
						}
					}
				}
				return strings.TrimSpace(strings.TrimPrefix(line, markerPrefix)), true
			}
		}
	}
	return "", false
}

// Lane is one ledger lane with the ownership of its current round.
type Lane struct {
	ID, Title, Branch, Phase, Status string
	// Session, Terminal and ReportPath belong to the lane's current round
	// (subagent-swarm-nebula: its newest <lane>.<phase>.spawn.json;
	// nebula-swarm: the ledger's terminal for the phase and round and
	// reports/<lane>.<phase>.r<round>.md). ReportPath is empty until that
	// round reports.
	Session, Terminal, ReportPath, PR string
	// TerminalState is the ledger's last recorded terminal state (nebula-swarm).
	TerminalState string
	// Report is the latest report's Outcome line; LastEvent the lane's newest
	// EVENTS line; Next what the run files say comes next for the lane.
	Report, LastEvent, Next string
	Round                   int
}

// Run is a host swarm run directory.
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
	// Progress and Escalations are PROGRESS.md and ESCALATIONS.md, clipped.
	Progress, Escalations string
	// Unchecked lists what discovery could not check for this run.
	Unchecked []string
	Lanes     []Lane
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

type ledgerLane struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Branch         string `json:"branch"`
	Phase          string `json:"phase"`
	Status         string `json:"status"`
	NebulaSession  string `json:"nebula_session"`
	NebulaTerminal string `json:"nebula_terminal"`
	PR             int    `json:"pr"`
	Round          int    `json:"round"`
}

type ledger struct {
	Config struct {
		Goal   string      `json:"goal"`
		Phases []hostPhase `json:"phases"`
	} `json:"config"`
	Tasks []ledgerLane `json:"tasks"`
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
	r.Repo = repoSlug(dir)
	r.Progress = clip(stripMarker(readText(filepath.Join(dir, "PROGRESS.md"))), 1500)
	r.Escalations = clip(stripMarker(readText(filepath.Join(dir, "ESCALATIONS.md"))), 1500)
	if r.Format == FormatNebulaSwarm {
		if err := readNebulaSwarm(dir, b, &r); err != nil {
			return Run{}, err
		}
	} else {
		var st ledger
		if err := json.Unmarshal(b, &st); err != nil {
			return Run{}, fmt.Errorf("parse %s: %w", filepath.Join(dir, "state.json"), err)
		}
		r.Goal = st.Config.Goal
		for i := range st.Tasks {
			r.Lanes = append(r.Lanes, importLane(dir, &st.Tasks[i]))
		}
		annotateLanes(dir, &r, st.Config.Phases)
	}
	sort.Slice(r.Lanes, func(a, b int) bool { return r.Lanes[a].ID < r.Lanes[b].ID })
	r.State = RunFinished
	if runLive(&r) {
		r.State = RunActive
	}
	return r, nil
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
	x := Lane{ID: l.ID, Title: l.Title, Branch: l.Branch, Phase: l.Phase, Status: l.Status, Round: l.Round}
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
	return x
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
	// VerifyInterval is the pause before re-checking that no step loop
	// restarted after stopWatcher. Zero uses a short default (25ms).
	VerifyInterval time.Duration
}

const defaultVerifyInterval = 25 * time.Millisecond

func (s *Service) verifyInterval() time.Duration {
	if s.VerifyInterval > 0 {
		return s.VerifyInterval
	}
	return defaultVerifyInterval
}

// owns reports whether a lease under holder belongs to this service.
func (s *Service) owns(holder string) bool {
	if holder == s.Holder || slices.Contains(s.Former, holder) {
		return true
	}
	// Legacy empty chat holder ("orca-chat:") migrates to the active chat holder.
	return s.Holder != "" && holder == ChatHolder("")
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
			if s.Holder != "" && j.Holder != s.Holder {
				_ = s.migrateJournal(dir, j)
			}
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
				_ = writeAtomic(p, []byte(cur.Marker+rest))
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
				_ = writeAtomic(p, []byte(updated+rest))
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
	timer := time.NewTimer(s.verifyInterval())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
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
