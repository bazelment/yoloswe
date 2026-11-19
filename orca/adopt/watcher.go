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

	"github.com/bazelment/yoloswe/orca/toolbox"
)

// ScriptWatcher controls watch_lanes.sh (subagent-swarm-nebula) processes
// with OS signals. Script, when set, is the watch_lanes.sh path; empty uses
// each run's WatchScript.
type ScriptWatcher struct {
	Script string
	// StopTimeout bounds the wait for SIGTERM before SIGKILL (default 10s).
	StopTimeout time.Duration
}

// WatchScript is runDir's watch_lanes.sh in the skill scripts directory
// toolbox.SkillDir resolves (ORCA_SNL_DIR, else the run's env.sh SW, else the
// home skill folder); its error names the paths tried.
func WatchScript(ctx context.Context, runDir string) (string, error) {
	scripts, err := toolbox.SkillDir(ctx, os.Getenv("ORCA_SNL_DIR"), runDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(scripts, "watch_lanes.sh"), nil
}

func (w ScriptWatcher) script(ctx context.Context, runDir string) (string, error) {
	if w.Script != "" {
		if !fileExists(w.Script) {
			return "", fmt.Errorf("watch_lanes.sh not found at %s", w.Script)
		}
		return w.Script, nil
	}
	return WatchScript(ctx, runDir)
}

// Running parses `watch_lanes.sh --status <run>`: its armed and bridge pids.
// Exit 1 (none armed) and 2 (several armed) are answers, not failures.
func (w ScriptWatcher) Running(ctx context.Context, runDir string) ([]int, error) {
	script, err := w.script(ctx, runDir)
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "bash", script, "--status", runDir).Output()
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
	return stopPIDs(ctx, pids, w.StopTimeout, nil)
}

func procAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := bytes.LastIndexByte(stat, ')'); i >= 0 {
			if f := strings.Fields(string(stat[i+1:])); len(f) > 0 && (f[0] == "Z" || f[0] == "X") {
				return false
			}
		}
	}
	return true
}

func waitGone(ctx context.Context, pids []int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		alive := false
		for _, pid := range pids {
			if procAlive(pid) {
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
func (w ScriptWatcher) Start(ctx context.Context, runDir string) error {
	if holder, ok := HeldByOrca(runDir); ok {
		return fmt.Errorf("%w: %s", ErrLeaseHeld, holder)
	}
	script, err := w.script(ctx, runDir)
	if err != nil {
		return err
	}
	owner, _ := os.ReadFile(filepath.Join(runDir, ".watch-owner"))
	cmd := exec.Command("bash", script, "--bridge", runDir)
	cmd.Env = append(os.Environ(), "WATCH_BRIDGE_OF="+strings.TrimSpace(string(owner)))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start watcher bridge: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
