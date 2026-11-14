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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ScriptWatcher controls watch_lanes.sh (subagent-swarm-nebula) processes
// with OS signals. Script is the watch_lanes.sh path.
type ScriptWatcher struct {
	Script string
	// StopTimeout bounds the wait for SIGTERM before SIGKILL (default 10s).
	StopTimeout time.Duration
}

// FindWatchScript returns $ORCA_WATCH_LANES, else the installed
// subagent-swarm-nebula watch_lanes.sh, else "".
func FindWatchScript() string {
	if v := os.Getenv("ORCA_WATCH_LANES"); v != "" {
		if fileExists(v) {
			return v
		}
		return ""
	}
	var cands []string
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".claude", "skills", "subagent-swarm-nebula", "scripts", "watch_lanes.sh"))
	}
	cands = append(cands, "/opt/nebula/skills/subagent-swarm-nebula/scripts/watch_lanes.sh")
	glob, _ := filepath.Glob("/opt/nebula/skills/.nebula-tree-subagent-swarm-nebula.*/scripts/watch_lanes.sh")
	cands = append(cands, glob...)
	for _, c := range cands {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

// Running parses `watch_lanes.sh --status <run>`: its armed and bridge pids.
// Exit 1 (none armed) and 2 (several armed) are answers, not failures.
func (w ScriptWatcher) Running(ctx context.Context, runDir string) ([]int, error) {
	out, err := exec.CommandContext(ctx, "bash", w.Script, "--status", runDir).Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && (exit.ExitCode() == 1 || exit.ExitCode() == 2)) {
		return nil, fmt.Errorf("watch_lanes.sh --status: %w", err)
	}
	var pids []int
	seen := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok || (key != "armed" && key != "bridges") {
			continue
		}
		seen++
		for _, f := range strings.Fields(val) {
			if f == "none" {
				continue
			}
			pid, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("watch_lanes.sh --status: bad pid %q", f)
			}
			pids = append(pids, pid)
		}
	}
	if seen != 2 {
		return nil, fmt.Errorf("watch_lanes.sh --status: unexpected output %q", out)
	}
	return pids, nil
}

// Stop sends SIGTERM, then SIGKILL after StopTimeout, and waits for exit.
func (w ScriptWatcher) Stop(ctx context.Context, pids []int) error {
	timeout := w.StopTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, pid := range pids {
			if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("signal watcher %d: %w", pid, err)
			}
		}
		if waitGone(ctx, pids, timeout) {
			return nil
		}
	}
	return fmt.Errorf("watcher pids %v survived SIGKILL", pids)
}

func waitGone(ctx context.Context, pids []int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		alive := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				alive = true
			}
		}
		if !alive {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

// Start launches a detached bridge (`watch_lanes.sh --bridge <run>`) owned by
// the run's last armed watcher, the way an exiting watcher hands off, so lane
// steps continue until the host orchestrator re-arms its watcher.
func (w ScriptWatcher) Start(_ context.Context, runDir string) error {
	owner, _ := os.ReadFile(filepath.Join(runDir, ".watch-owner"))
	cmd := exec.Command("bash", w.Script, "--bridge", runDir)
	cmd.Env = append(os.Environ(), "WATCH_BRIDGE_OF="+strings.TrimSpace(string(owner)))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start watcher bridge: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
