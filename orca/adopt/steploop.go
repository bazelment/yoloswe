package adopt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// stepBridgeMarker names the bridge loop StepLoopWatcher.Start launches.
const stepBridgeMarker = "orca-step-bridge"

// stepBridgeScript runs one released nebula-swarm tick.
const stepBridgeScript = `source "$2" || exit
RUN=$1
while [[ -f $RUN/state.json ]]; do
  sw_step "$RUN"; rc=$?
  [[ $rc == 124 ]] || exit "$rc"
done`

// loopMarkers identify nebula-swarm loop processes and their descendants.
var loopMarkers = []string{"sw_step", "sw_wait", "swarm_nebula.sh", "orchestrator_watch.sh", "term-cursors.json", stepBridgeMarker}

// StepLoopWatcher controls a nebula-swarm run's sw_step loop.
type StepLoopWatcher struct {
	// Script is swarm_nebula.sh, which the bridge Start launches sources.
	Script string
	// ProcRoot defaults to /proc.
	ProcRoot string
	// StopTimeout bounds the wait for SIGTERM before SIGKILL (default 10s).
	StopTimeout time.Duration
}

// FindSwarmScript returns $ORCA_NEBULA_SWARM_SH, else the installed
// nebula-swarm swarm_nebula.sh, else "".
func FindSwarmScript() string {
	if v := os.Getenv("ORCA_NEBULA_SWARM_SH"); v != "" {
		if fileExists(v) {
			return v
		}
		return ""
	}
	var cands []string
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".claude", "skills", "nebula-swarm", "scripts", "swarm_nebula.sh"))
	}
	cands = append(cands, "/opt/nebula/skills/nebula-swarm/scripts/swarm_nebula.sh")
	glob, _ := filepath.Glob("/opt/nebula/skills/.nebula-tree-nebula-swarm.*/scripts/swarm_nebula.sh")
	cands = append(cands, glob...)
	for _, c := range cands {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

type proc struct {
	args []string
	ppid int
}

func procTable(root string) (map[int]proc, error) {
	if root == "" {
		root = "/proc"
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	out := map[int]proc{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(root, e.Name(), "stat"))
		if err != nil {
			continue
		}
		// stat is "pid (comm) state ppid ..."; comm may hold spaces and parens.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(stat[i+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		out[pid] = proc{args: strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), ppid: ppid}
	}
	return out, nil
}

func hasMarker(args []string) bool {
	for _, a := range args {
		for _, m := range loopMarkers {
			if strings.Contains(a, m) {
				return true
			}
		}
	}
	return false
}

// mentionsDir reports whether an argument names dir or a path inside it.
func mentionsDir(args []string, dir string) bool {
	for _, a := range args {
		for i := 0; ; {
			j := strings.Index(a[i:], dir)
			if j < 0 {
				break
			}
			end := i + j + len(dir)
			if end == len(a) || strings.ContainsRune("/ \"'\t\n;", rune(a[end])) {
				return true
			}
			i = end
		}
	}
	return false
}

// Running returns the run's step-loop processes, oldest ancestor first.
func (w StepLoopWatcher) Running(_ context.Context, runDir string) ([]int, error) {
	procs, err := procTable(w.ProcRoot)
	if err != nil {
		return nil, err
	}
	dirs := []string{filepath.Clean(runDir)}
	if abs, err := filepath.Abs(runDir); err == nil && abs != dirs[0] {
		dirs = append(dirs, abs)
	}
	if real, err := filepath.EvalSymlinks(runDir); err == nil && !slices.Contains(dirs, real) {
		dirs = append(dirs, real)
	}
	// Never this process or the shells that started it.
	self := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !self[pid]; pid = procs[pid].ppid {
		self[pid] = true
	}
	picked := map[int]bool{}
	for pid, p := range procs {
		if self[pid] || !hasMarker(p.args) {
			continue
		}
		if !slices.ContainsFunc(dirs, func(d string) bool { return mentionsDir(p.args, d) }) {
			continue
		}
		picked[pid] = true
		// sw_step is a shell function: the shells running it name the run
		// only through $RUN, so take each step-loop ancestor shell too.
		for anc := p.ppid; anc > 1 && !self[anc] && !picked[anc]; anc = procs[anc].ppid {
			ap, ok := procs[anc]
			if !ok || !hasMarker(ap.args) {
				break
			}
			picked[anc] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, p := range procs {
			if !picked[pid] && !self[pid] && picked[p.ppid] {
				picked[pid], changed = true, true
			}
		}
	}
	out := make([]int, 0, len(picked))
	for pid := range picked {
		out = append(out, pid)
	}
	slices.Sort(out)
	return out, nil
}

// Stop sends SIGTERM, then SIGKILL after StopTimeout, and waits for exit.
func (w StepLoopWatcher) Stop(ctx context.Context, pids []int) error {
	return stopPIDs(ctx, pids, w.StopTimeout)
}

// Start launches a detached sw_step bridge for the run; it logs to
// <run>/orca-step-bridge.log and exits at the run's first decision.
func (w StepLoopWatcher) Start(_ context.Context, runDir string) error {
	if w.Script == "" {
		return errors.New("swarm_nebula.sh not found (install nebula-swarm or set ORCA_NEBULA_SWARM_SH)")
	}
	log, err := os.OpenFile(filepath.Join(runDir, stepBridgeMarker+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("start step loop bridge: %w", err)
	}
	defer log.Close()
	cmd := exec.Command("bash", "-c", stepBridgeScript, stepBridgeMarker, runDir, w.Script)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start step loop bridge: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// Pane is one tmux pane: its shell pid, target and names.
type Pane struct {
	Target, Window, Title string
	PID                   int
}

// TmuxPanes lists every tmux pane of the user's server; no server is no panes.
func TmuxPanes(ctx context.Context) ([]Pane, error) {
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_pid}\t#{session_name}:#{window_index}.#{pane_index}\t#{window_name}\t#{pane_title}").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) || errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var panes []Pane
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), "\t", 4)
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		panes = append(panes, Pane{PID: pid, Target: f[1], Window: f[2], Title: f[3]})
	}
	return panes, nil
}

// FormatWatcher picks the run's loop controller by its format: watch_lanes.sh
// for subagent-swarm-nebula, the sw_step loop for nebula-swarm. A nil
// controller fails with its Missing reason.
type FormatWatcher struct {
	Subagent, Nebula               Watcher
	SubagentMissing, NebulaMissing string
	// Panes lists tmux panes for Describe; nil skips the pane lookup.
	Panes func(context.Context) ([]Pane, error)
	// ProcRoot defaults to /proc.
	ProcRoot    string
	StopTimeout time.Duration
}

func (w FormatWatcher) pick(runDir string) (Watcher, error) {
	if DetectFormat(runDir) == FormatNebulaSwarm {
		if w.Nebula == nil {
			return nil, errors.New(w.NebulaMissing)
		}
		return w.Nebula, nil
	}
	if w.Subagent == nil {
		return nil, errors.New(w.SubagentMissing)
	}
	return w.Subagent, nil
}

func (w FormatWatcher) Running(ctx context.Context, runDir string) ([]int, error) {
	x, err := w.pick(runDir)
	if err != nil {
		return nil, err
	}
	return x.Running(ctx, runDir)
}

func (w FormatWatcher) Stop(ctx context.Context, pids []int) error {
	return stopPIDs(ctx, pids, w.StopTimeout)
}

func (w FormatWatcher) Start(ctx context.Context, runDir string) error {
	x, err := w.pick(runDir)
	if err != nil {
		return err
	}
	return x.Start(ctx, runDir)
}

// Describe names the loop processes and the tmux pane they run in.
func (w FormatWatcher) Describe(ctx context.Context, pids []int) (desc, pane string) {
	return describeProcs(ctx, w.ProcRoot, w.Panes, pids)
}

func describeProcs(ctx context.Context, procRoot string, panes func(context.Context) ([]Pane, error), pids []int) (string, string) {
	if len(pids) == 0 {
		return "", ""
	}
	ids := make([]string, len(pids))
	for i, p := range pids {
		ids[i] = strconv.Itoa(p)
	}
	desc := "pids " + strings.Join(ids, ",")
	if panes == nil {
		return desc, ""
	}
	list, err := panes(ctx)
	if err != nil {
		return desc + " (tmux panes unreadable: " + err.Error() + ")", ""
	}
	procs, err := procTable(procRoot)
	if err != nil {
		return desc, ""
	}
	byPID := map[int]Pane{}
	for _, p := range list {
		byPID[p.PID] = p
	}
	// Walk up from the oldest loop process: the one whose parent is not a loop process.
	root := pids[0]
	for _, p := range pids {
		if !slices.Contains(pids, procs[p].ppid) {
			root = p
			break
		}
	}
	seen := map[int]bool{}
	for pid := root; pid > 1 && !seen[pid]; pid = procs[pid].ppid {
		seen[pid] = true
		if p, ok := byPID[pid]; ok {
			return fmt.Sprintf("%s in tmux %s (window %q, title %q)", desc, p.Target, p.Window, p.Title), p.Target
		}
		if _, ok := procs[pid]; !ok {
			break
		}
	}
	return desc + " (not in a tmux pane)", ""
}

// stopPIDs sends SIGTERM, then SIGKILL after timeout, and waits for exit.
func stopPIDs(ctx context.Context, pids []int, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, pid := range pids {
			if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("signal %d: %w", pid, err)
			}
		}
		if waitGone(ctx, pids, timeout) {
			return nil
		}
	}
	return fmt.Errorf("pids %v survived SIGKILL", pids)
}
