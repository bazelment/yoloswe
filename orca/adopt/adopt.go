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
		x := Lane{ID: l.ID, Title: l.Title, Branch: l.Branch, Phase: l.Phase, Status: string(l.Status), Round: l.Round, Notes: strings.Join(l.Notes, "\n")}
		x.Session = l.Sessions[l.Phase]
		x.Reports, x.Terminal = laneFiles(dir, l)
		x.PR = prNumber(x.Reports + "\n" + readText(filepath.Join(dir, "ledger.md")))
		r.Lanes = append(r.Lanes, x)
	}
	sort.Slice(r.Lanes, func(a, b int) bool { return r.Lanes[a].ID < r.Lanes[b].ID })
	r.LastActivity = lastActivity(dir)
	return r, nil
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
		}
		if b, e := os.ReadFile(sp[0]); e == nil {
			_ = json.Unmarshal(b, &v)
			term = v.TerminalID + " " + v.TerminalURL
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
}

var ErrLeaseHeld = errors.New("run is leased by another orca")

type lease struct {
	Holder string `json:"holder"`
}

func (h Handover) Adopt(r Run) error {
	p := filepath.Join(r.Dir, ".orca-lease.json")
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		if errors.Is(e, os.ErrExist) {
			return ErrLeaseHeld
		}
		return e
	}
	holder := fmt.Sprintf("%s/%d", h.Host, h.PID)
	if holder == "/0" {
		holder = fmt.Sprintf("%s/%d", hostname(), os.Getpid())
	}
	_ = json.NewEncoder(f).Encode(lease{Holder: holder})
	_ = f.Close()
	if h.Labeler != nil {
		if e = h.Labeler.SetOrchestrator(r.ID, "orca"); e != nil {
			_ = os.Remove(p)
			return e
		}
	}
	if h.Process != nil {
		if stopErr := h.Process.StopWatcher(r.Dir); stopErr != nil {
			return stopErr
		}
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	marker := fmt.Sprintf("ORCHESTRATOR: orca since %s (%s). Claude sessions: do not act on this run; run /release in orca to hand back.\n", now.Format(time.RFC3339), holder)
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		if e := prepend(filepath.Join(r.Dir, n), marker); e != nil {
			return e
		}
	}
	return nil
}
func (h Handover) Release(r Run) error {
	p := filepath.Join(r.Dir, ".orca-lease.json")
	_ = os.Remove(p)
	if h.Labeler != nil {
		if e := h.Labeler.SetOrchestrator(r.ID, "claude"); e != nil {
			return e
		}
	}
	if h.Process != nil {
		if e := h.Process.StartWatcher(r.Dir); e != nil {
			return e
		}
	}
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		if e := removeMarker(filepath.Join(r.Dir, n)); e != nil {
			return e
		}
	}
	return nil
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
	if e != nil {
		return nil
	}
	lines := strings.SplitAfter(string(b), "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "ORCHESTRATOR: orca since ") {
		return os.WriteFile(p, []byte(strings.Join(lines[1:], "")), 0644)
	}
	return nil
}
func hostname() string { h, _ := os.Hostname(); return h }

type Service struct {
	Importer Importer
	Handover Handover
}

func (s Service) List() ([]Run, error) { return s.Importer.Discover() }
func (s Service) Adopt(id string) (Run, error) {
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
