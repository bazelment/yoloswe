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
	// Signal sends a signal to pid; nil uses syscall.Kill.
	Signal func(pid int, sig syscall.Signal) error
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
	cwd  string
	args []string
	env  []string
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
		if len(fields) < 2 || fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		var env []string
		if envRaw, err := os.ReadFile(filepath.Join(root, e.Name(), "environ")); err == nil && len(envRaw) > 0 {
			env = strings.Split(strings.TrimRight(string(envRaw), "\x00"), "\x00")
		}
		cwd, _ := os.Readlink(filepath.Join(root, e.Name(), "cwd"))
		out[pid] = proc{
			cwd:  cwd,
			args: strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"),
			env:  env,
			ppid: ppid,
		}
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

func isRunDirCont(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

// mentionsDir reports whether an argument or environment entry names dir or a
// path inside it.
func mentionsDir(args []string, dir string) bool {
	if dir == "" {
		return false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "ORCA_") || strings.HasPrefix(a, "PWD=") || strings.HasPrefix(a, "OLDPWD=") {
			continue
		}
		for i := 0; ; {
			j := strings.Index(a[i:], dir)
			if j < 0 {
				break
			}
			end := i + j + len(dir)
			if end == len(a) || !isRunDirCont(a[end]) {
				return true
			}
			i = end
		}
	}
	return false
}

func runDirCandidates(runDir string) []string {
	dirs := []string{filepath.Clean(runDir)}
	if abs, err := filepath.Abs(runDir); err == nil && !slices.Contains(dirs, abs) {
		dirs = append(dirs, abs)
	}
	if real, err := filepath.EvalSymlinks(runDir); err == nil && !slices.Contains(dirs, real) {
		dirs = append(dirs, real)
	}
	var homes []string
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		homes = append(homes, filepath.Clean(h))
	}
	if h := filepath.Clean(os.Getenv("HOME")); h != "." && h != "" && !slices.Contains(homes, h) {
		homes = append(homes, h)
	}
	for _, d := range slices.Clone(dirs) {
		for _, h := range homes {
			if rel, ok := strings.CutPrefix(d, h+"/"); ok && rel != "" {
				for _, form := range []string{"~/" + rel, "$HOME/" + rel, "${HOME}/" + rel} {
					if !slices.Contains(dirs, form) {
						dirs = append(dirs, form)
					}
				}
			}
		}
	}
	return dirs
}

func mentionsAnyDir(strs []string, dirs []string) bool {
	return slices.ContainsFunc(dirs, func(d string) bool { return mentionsDir(strs, d) })
}

// Running returns the run's step-loop processes, oldest ancestor first.
func (w StepLoopWatcher) Running(_ context.Context, runDir string) ([]int, error) {
	procs, err := procTable(w.ProcRoot)
	if err != nil {
		return nil, err
	}
	dirs := runDirCandidates(runDir)
	// Never this process or the shells that started it.
	self := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !self[pid]; pid = procs[pid].ppid {
		self[pid] = true
	}
	picked := map[int]bool{}
	for pid, p := range procs {
		if self[pid] {
			continue
		}
		if !mentionsAnyDir(p.args, dirs) && !mentionsAnyDir(p.env, dirs) {
			continue
		}
		picked[pid] = true
		// sw_step is a shell function: the shells running it name the run
		// only through $RUN, so take each step-loop ancestor shell too.
		for anc := p.ppid; anc > 1 && !self[anc] && !picked[anc]; anc = procs[anc].ppid {
			ap, ok := procs[anc]
			if !ok || (!hasMarker(ap.args) && !hasMarker(ap.env) && !mentionsAnyDir(ap.args, dirs) && !mentionsAnyDir(ap.env, dirs)) {
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
	return stopPIDs(ctx, pids, w.StopTimeout, w.Signal)
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

// Inspect records the root process of pids before Stop signals it.
func (w StepLoopWatcher) Inspect(pids []int) *StoppedLoop {
	return inspectStoppedLoop(w.ProcRoot, pids)
}

// Restart resumes the run's step loop on /release: it launches the sw_step
// bridge when Script is set and the stopped loop ran sw_step, else it
// restarts the exact command recorded at /adopt.
func (w StepLoopWatcher) Restart(ctx context.Context, runDir string, loop *StoppedLoop) error {
	if w.Script != "" && (loop == nil || len(loop.Args) == 0 || hasMarker(loop.Args) || hasMarker(loop.Env)) {
		return w.Start(ctx, runDir)
	}
	if loop != nil && len(loop.Args) > 0 {
		return startRecordedLoop(runDir, loop)
	}
	err := w.Start(ctx, runDir)
	if err != nil && loop != nil && loop.Command != "" {
		return fmt.Errorf("%w (restart manually: %s)", err, loop.Command)
	}
	return err
}

func inspectStoppedLoop(procRoot string, pids []int) *StoppedLoop {
	if len(pids) == 0 {
		return nil
	}
	procs, err := procTable(procRoot)
	if err != nil {
		return nil
	}
	root := pids[0]
	for _, p := range pids {
		if !slices.Contains(pids, procs[p].ppid) {
			root = p
			break
		}
	}
	rp, ok := procs[root]
	if !ok || len(rp.args) == 0 {
		return nil
	}
	cmd := strings.Join(rp.args, " ")
	for _, kv := range rp.env {
		if v, ok := strings.CutPrefix(kv, "__CLAUDE_CC_COMMAND_TEXT="); ok && v != "" {
			cmd = v
			break
		}
	}
	return &StoppedLoop{
		Command: cmd,
		Dir:     rp.cwd,
		Args:    slices.Clone(rp.args),
		Env:     slices.Clone(rp.env),
	}
}

func startRecordedLoop(runDir string, loop *StoppedLoop) error {
	log, err := os.OpenFile(filepath.Join(runDir, stepBridgeMarker+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("restart step loop (%s): %w", loop.Command, err)
	}
	defer log.Close()
	cmd := exec.Command(loop.Args[0], loop.Args[1:]...)
	if loop.Dir != "" && fileExists(loop.Dir) {
		cmd.Dir = loop.Dir
	}
	if len(loop.Env) > 0 {
		cmd.Env = loop.Env
	}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("restart step loop (%s): %w", loop.Command, err)
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
	// Signal sends a signal to pid; nil uses syscall.Kill.
	Signal func(pid int, sig syscall.Signal) error
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
	sig := w.Signal
	if sig == nil {
		if sw, ok := w.Nebula.(StepLoopWatcher); ok {
			sig = sw.Signal
		}
	}
	return stopPIDs(ctx, pids, w.StopTimeout, sig)
}

func (w FormatWatcher) Start(ctx context.Context, runDir string) error {
	x, err := w.pick(runDir)
	if err != nil {
		return err
	}
	return x.Start(ctx, runDir)
}

// Inspect records the root process of pids before Stop signals it.
func (w FormatWatcher) Inspect(pids []int) *StoppedLoop {
	return inspectStoppedLoop(w.ProcRoot, pids)
}

// Restart resumes the run's watcher or step loop from the recorded loop.
func (w FormatWatcher) Restart(ctx context.Context, runDir string, loop *StoppedLoop) error {
	x, err := w.pick(runDir)
	if err != nil {
		return err
	}
	if r, ok := x.(interface {
		Restart(context.Context, string, *StoppedLoop) error
	}); ok {
		return r.Restart(ctx, runDir, loop)
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
func stopPIDs(ctx context.Context, pids []int, timeout time.Duration, signalFn func(int, syscall.Signal) error) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if signalFn == nil {
		signalFn = syscall.Kill
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, pid := range pids {
			if err := signalFn(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("signal %d: %w", pid, err)
			}
		}
		if waitGone(ctx, pids, timeout) {
			return nil
		}
	}
	return fmt.Errorf("pids %v survived SIGKILL", pids)
}
