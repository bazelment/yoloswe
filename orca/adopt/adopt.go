// Package adopt imports and hands over host-run Nebula swarms without creating work.
package adopt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	swarmstate "github.com/bazelment/yoloswe/swarm-queen/state"
)

//nolint:govet // imported ledger shape prioritizes readable fields
type Lane struct {
	ID, Title, Branch, Phase, Status string
	Round                            int
	Session, Terminal, PR            string
	Sessions                         map[string]string
	Terminals                        map[string]string
	Reports, Notes                   string
}
type Run struct {
	ID, Dir, Goal, Objective, Orchestrator, LastActivity string
	Lanes                                                []Lane
}

//nolint:govet
type Importer struct {
	Roots []string
	Now   func() time.Time
}

func (i Importer) ReadOnly(dir string) (Run, error) { return i.read(dir) }
func (i Importer) read(dir string) (Run, error) {
	st, err := swarmstate.NewStore(dir).Read()
	if err != nil {
		return Run{}, err
	}
	id := filepath.Base(dir)
	r := Run{ID: id, Dir: dir, Goal: st.Config.Goal, Objective: readText(filepath.Join(dir, "OBJECTIVE.md"))}
	r.Orchestrator = orchestrator(r.Objective, readText(filepath.Join(dir, "PROGRESS.md")))
	for _, l := range st.Lanes {
		if l == nil {
			continue
		}
		x := Lane{ID: l.ID, Title: l.Title, Branch: l.Branch, Phase: l.Phase, Status: string(l.Status), Round: l.Round, Notes: strings.Join(l.Notes, "\n"), Sessions: map[string]string{}}
		for k, v := range l.Sessions {
			x.Sessions[k] = v
		}
		x.Session = canonicalSession(l)
		spawn := laneSpawn(dir, l.ID, l.Phase)
		if x.Session == "" {
			x.Session = spawn.Session
		}
		x.Reports, x.Terminal = laneFiles(dir, l)
		if x.Terminal == "" {
			x.Terminal = spawn.Terminal
		}
		x.Terminals = laneTerminals(dir, l.ID)
		if l.PR > 0 {
			x.PR = fmt.Sprint(l.PR)
		} else if spawn.PR != "" {
			x.PR = spawn.PR
		} else {
			x.PR = prNumber(x.Reports + "\n" + readText(filepath.Join(dir, "ledger.md")))
		}
		r.Lanes = append(r.Lanes, x)
	}
	sort.Slice(r.Lanes, func(a, b int) bool { return r.Lanes[a].ID < r.Lanes[b].ID })
	r.LastActivity = lastActivity(dir)
	return r, nil
}

type spawnOwnership struct{ Session, Terminal, PR string }

func laneSpawn(dir, laneID, phase string) spawnOwnership {
	p := filepath.Join(dir, laneID+"."+phase+".spawn.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return spawnOwnership{}
	}
	var v struct {
		SessionID   string          `json:"session_id"`
		TerminalID  string          `json:"terminal_id"`
		TerminalURL string          `json:"terminal_url"`
		PR          json.RawMessage `json:"pr"`
		PRNumber    json.RawMessage `json:"pr_number"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if json.Unmarshal(b, &v) != nil {
		return spawnOwnership{}
	}
	term := v.TerminalID
	if term == "" {
		term = v.TerminalURL
	}
	var pr string
	if len(v.PR) == 0 {
		v.PR = v.PRNumber
	}
	if len(v.PR) == 0 {
		v.PR = v.PullRequest
	}
	if len(v.PR) > 0 {
		var n int
		if json.Unmarshal(v.PR, &n) == nil && n > 0 {
			pr = fmt.Sprint(n)
		}
		if pr == "" {
			_ = json.Unmarshal(v.PR, &pr)
		}
	}
	return spawnOwnership{Session: v.SessionID, Terminal: term, PR: pr}
}

func laneTerminals(dir, laneID string) map[string]string {
	out := map[string]string{}
	paths, _ := filepath.Glob(filepath.Join(dir, laneID+".*.spawn.json"))
	for _, p := range paths {
		var v struct {
			SessionID   string `json:"session_id"`
			TerminalID  string `json:"terminal_id"`
			TerminalURL string `json:"terminal_url"`
		}
		b, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(b, &v) != nil {
			continue
		}
		phase := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), laneID+"."), ".spawn.json")
		if v.TerminalID != "" {
			out[phase] = v.TerminalID
		} else if v.TerminalURL != "" {
			out[phase] = v.TerminalURL
		}
	}
	return out
}
func canonicalSession(l *swarmstate.Lane) string {
	if s, ok := l.SessionFor(l.Phase, l.Round); ok {
		return s
	}
	if s := l.Sessions[l.Phase]; s != "" {
		return s
	}
	var best string
	bestRound := -1
	for _, a := range l.Attempts() {
		if a.Phase == l.Phase && a.Round > bestRound {
			best, bestRound = a.Session, a.Round
		}
	}
	return best
}
func (i Importer) Discover() ([]Run, error) {
	roots := i.Roots
	if len(roots) == 0 {
		if cwd, e := os.Getwd(); e == nil {
			roots = []string{filepath.Dir(cwd)}
		}
		if v := os.Getenv("ORCA_SWARM_ROOTS"); v != "" {
			roots = append(roots, strings.Split(v, string(os.PathListSeparator))...)
		}
	}
	seen := map[string]bool{}
	var out []Run
	for _, root := range roots {
		ents, e := os.ReadDir(filepath.Join(root, ".shared", "subagent-swarm"))
		if e != nil {
			continue
		}
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			d := filepath.Join(root, ".shared", "subagent-swarm", ent.Name())
			if seen[d] {
				continue
			}
			if _, e = os.Stat(filepath.Join(d, "state.json")); e != nil {
				continue
			}
			r, e := i.read(d)
			if e == nil {
				seen[d] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}
func laneFiles(dir string, l *swarmstate.Lane) (string, string) {
	pat := filepath.Join(dir, l.ID+"."+l.Phase+".md")
	b, _ := os.ReadFile(pat)
	report := string(b)
	if report == "" {
		ms, _ := filepath.Glob(filepath.Join(dir, l.ID+"."+l.Phase+".*.md"))
		for _, p := range ms {
			bb, _ := os.ReadFile(p)
			report += string(bb) + "\n"
		}
	}
	sp, _ := filepath.Glob(filepath.Join(dir, l.ID+"."+l.Phase+".spawn.json"))
	term := ""
	if len(sp) > 0 {
		var v struct {
			TerminalID  string `json:"terminal_id"`
			TerminalURL string `json:"terminal_url"`
			SessionID   string `json:"session_id"`
		}
		if b, e := os.ReadFile(sp[0]); e == nil {
			_ = json.Unmarshal(b, &v)
			term = v.TerminalID
			if term == "" {
				term = v.TerminalURL
			}
		}
	}
	return report, strings.TrimSpace(term)
}
func readText(p string) string { b, _ := os.ReadFile(p); return string(b) }
func orchestrator(s ...string) string {
	for _, x := range s {
		for _, line := range strings.Split(x, "\n") {
			if strings.HasPrefix(line, "ORCHESTRATOR:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "ORCHESTRATOR:"))
			}
		}
	}
	return "claude"
}
func lastActivity(dir string) string {
	var best time.Time
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, e error) error {
		if e == nil && info.ModTime().After(best) {
			best = info.ModTime()
		}
		return nil
	})
	if best.IsZero() {
		return ""
	}
	return best.UTC().Format(time.RFC3339)
}

var prRE = regexp.MustCompile(`(?i)(?:PR|pull request)\s*[:#]?\s*(\d+)`)

func prNumber(s string) string {
	m := prRE.FindStringSubmatch(s)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

type Labeler interface{ SetOrchestrator(string, string) error }
type ProcessControl interface {
	StopWatcher(string) error
	StartWatcher(string) error
}

//nolint:govet
type Handover struct {
	Labeler Labeler
	Process ProcessControl
	Host    string
	PID     int
	Now     func() time.Time
	// HolderID is stable across chat process restarts. When unset the host/PID
	// identity preserves the command-line behavior used by older callers.
	HolderID string
}

var ErrLeaseHeld = errors.New("run is leased by another orca")
var ErrLeaseNotHeld = errors.New("run is not leased by this orca")
var ErrAdaptersUnavailable = errors.New("adoption requires labeler and process adapters")

type lease struct {
	Holder               string `json:"holder"`
	PreviousOrchestrator string `json:"previous_orchestrator,omitempty"`
}

func (h Handover) Adopt(r Run) error {
	p := filepath.Join(r.Dir, ".orca-lease.json")
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		if errors.Is(e, os.ErrExist) {
			b, readErr := os.ReadFile(p)
			var current lease
			if readErr == nil && json.Unmarshal(b, &current) == nil && current.Holder == h.holder() {
				return nil
			}
			return ErrLeaseHeld
		}
		return e
	}
	holder := h.holder()
	if e = json.NewEncoder(f).Encode(lease{Holder: holder, PreviousOrchestrator: previousOrchestrator(r.Orchestrator)}); e != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return e
	}
	_ = f.Close()
	original := map[string]fileSnapshot{}
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		original[n] = snapshotFile(filepath.Join(r.Dir, n))
	}
	labelled, stopped := false, false
	rollback := func(cause error) error {
		for n, b := range original {
			_ = restoreFile(filepath.Join(r.Dir, n), b)
		}
		if stopped && h.Process != nil {
			_ = h.Process.StartWatcher(r.Dir)
		}
		if labelled && h.Labeler != nil {
			_ = h.Labeler.SetOrchestrator(r.ID, previousOrchestrator(r.Orchestrator))
		}
		_ = os.Remove(p)
		return cause
	}
	if e = h.setLabel(r.ID, "orca"); e != nil {
		return rollback(e)
	}
	labelled = true
	if e = h.stop(r.Dir); e != nil {
		return rollback(e)
	}
	stopped = true
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	marker := fmt.Sprintf("ORCHESTRATOR: orca since %s (%s). Claude sessions: do not act on this run; run /release in orca to hand back.\n", now.Format(time.RFC3339), holder)
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		if e := prepend(filepath.Join(r.Dir, n), marker); e != nil {
			return rollback(e)
		}
	}
	return nil
}
func (h Handover) Release(r Run) error {
	p := filepath.Join(r.Dir, ".orca-lease.json")
	b, e := os.ReadFile(p)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return ErrLeaseNotHeld
		}
		return e
	}
	var current lease
	if e := json.Unmarshal(b, &current); e != nil {
		return e
	}
	if current.Holder != h.holder() {
		return ErrLeaseNotHeld
	}
	previous := current.PreviousOrchestrator
	if previous == "" {
		previous = previousOrchestrator(r.Orchestrator)
	}
	original := map[string]fileSnapshot{}
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		original[n] = snapshotFile(filepath.Join(r.Dir, n))
	}
	labelled, started := false, false
	rollback := func(cause error) error {
		for n, b := range original {
			_ = restoreFile(filepath.Join(r.Dir, n), b)
		}
		if started && h.Process != nil {
			_ = h.Process.StopWatcher(r.Dir)
		}
		if labelled && h.Labeler != nil {
			_ = h.Labeler.SetOrchestrator(r.ID, "orca")
		}
		return cause
	}
	if e := h.setLabel(r.ID, previous); e != nil {
		return rollback(e)
	}
	labelled = true
	if e := h.start(r.Dir); e != nil {
		return rollback(e)
	}
	started = true
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		if e := removeMarker(filepath.Join(r.Dir, n)); e != nil {
			return rollback(e)
		}
	}
	if e := os.Remove(p); e != nil {
		return rollback(e)
	}
	return nil
}

type fileSnapshot struct {
	exists bool
	data   []byte
}

func snapshotFile(p string) fileSnapshot {
	_, err := os.Stat(p)
	if err != nil {
		return fileSnapshot{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fileSnapshot{exists: true}
	}
	return fileSnapshot{exists: true, data: append([]byte(nil), b...)}
}
func restoreFile(p string, s fileSnapshot) error {
	if !s.exists {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if s.data == nil {
		return nil
	}
	return os.WriteFile(p, s.data, 0644)
}

func previousOrchestrator(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		s = s[:i]
	}
	if s == "" || s == "orca" {
		return "claude"
	}
	return s
}
func prepend(p, s string) error {
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) {
		b = nil
		e = nil
	}
	if e != nil {
		return e
	}
	if strings.HasPrefix(string(b), "ORCHESTRATOR: orca since ") {
		return nil
	}
	return os.WriteFile(p, append([]byte(s), b...), 0644)
}
func removeMarker(p string) error {
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	lines := strings.SplitAfter(string(b), "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "ORCHESTRATOR: orca since ") {
		return os.WriteFile(p, []byte(strings.Join(lines[1:], "")), 0644)
	}
	return nil
}
func hostname() string { h, _ := os.Hostname(); return h }

func (h Handover) holder() string {
	if h.HolderID != "" {
		return h.HolderID
	}
	if h.Host != "" || h.PID != 0 {
		return fmt.Sprintf("%s/%d", h.Host, h.PID)
	}
	return fmt.Sprintf("%s/%d", hostname(), os.Getpid())
}
func (h Handover) setLabel(id, value string) error {
	if h.Labeler == nil {
		return nil
	}
	return h.Labeler.SetOrchestrator(id, value)
}
func (h Handover) stop(dir string) error {
	if h.Process == nil {
		return nil
	}
	return h.Process.StopWatcher(dir)
}
func (h Handover) start(dir string) error {
	if h.Process == nil {
		return nil
	}
	return h.Process.StartWatcher(dir)
}

type Service struct {
	Importer Importer
	Handover Handover
}

func (s Service) List() ([]Run, error) { return s.Importer.Discover() }
func (s Service) Adopt(id string) (Run, error) {
	if s.Handover.Labeler == nil || s.Handover.Process == nil {
		return Run{}, ErrAdaptersUnavailable
	}
	rs, e := s.Importer.Discover()
	if e != nil {
		return Run{}, e
	}
	for _, r := range rs {
		if r.ID == id {
			if adoptErr := s.Handover.Adopt(r); adoptErr != nil {
				return Run{}, adoptErr
			}
			return r, nil
		}
	}
	return Run{}, fmt.Errorf("run %q not found", id)
}
func (s Service) Release(id string) error {
	if s.Handover.Labeler == nil || s.Handover.Process == nil {
		return ErrAdaptersUnavailable
	}
	rs, e := s.Importer.Discover()
	if e != nil {
		return e
	}
	for _, r := range rs {
		if r.ID == id {
			return s.Handover.Release(r)
		}
	}
	return fmt.Errorf("run %q not found", id)
}

// ReleaseOwned hands back the run leased by this stable holder after a chat
// restart. The caller supplies the runs it imported before issuing /release.
func (s Service) ReleaseOwned(runs []Run) error {
	if s.Handover.Labeler == nil || s.Handover.Process == nil {
		return ErrAdaptersUnavailable
	}
	for _, r := range runs {
		b, err := os.ReadFile(filepath.Join(r.Dir, ".orca-lease.json"))
		if err != nil {
			continue
		}
		var l lease
		if json.Unmarshal(b, &l) == nil && l.Holder == s.Handover.holder() {
			return s.Handover.Release(r)
		}
	}
	return ErrLeaseNotHeld
}
