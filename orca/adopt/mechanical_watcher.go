package adopt

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/orca/toolbox"
)

// supervisedWatcher runs the existing watcher under the current handover's
// authority. Ordinary host invocations retain the skill's stand-down guard.
func supervisedWatcher(ctx context.Context, script, runDir, authority string) error {
	if _, err := os.Stat(script); err != nil {
		return err
	}
	body := `set -euo pipefail
RUN=$1; SW=$2
SNL=${ORCA_SNL_DIR:-$HOME/.claude/skills/subagent-swarm-nebula/scripts}
[ -f "$SNL/step.sh" ] || exit 2
. "$SNL/swarm_nebula_lanes.sh"
. "$SNL/lane_ops.sh"
. "$SNL/step.sh"
` + toolbox.AdoptedLeaseGuard + `
seen=""
while :; do
  snl_orca_lease_check "$RUN" adopted-watcher || exit 0
  for f in "$RUN"/*.done "$RUN"/*.needs-swe; do
    [ -e "$f" ] || continue
    stamp="$f@$(stat -c %Y:%s "$f" 2>/dev/null || true)"
    case $'\n'$seen$'\n' in *$'\n'$stamp$'\n'*) continue;; esac
    seen="$seen$'\n'$stamp"
    snl_step "$f" >/dev/null 2>&1 || true
  done
  sleep 1
done`
	cmd := exec.CommandContext(ctx, "bash", "-c", body, "orca-adopted-watcher", runDir, script)
	cmd.Env = append(os.Environ(), "ORCA_TOOLBOX_DIR="+runDir, "ORCA_ADOPT_AUTH="+authority, "WATCH_NO_REARM=1", "SNL_RUN_ID="+filepath.Base(runDir), "ORCA_SNL_DIR="+filepath.Join(os.Getenv("HOME"), ".claude", "skills", "subagent-swarm-nebula", "scripts"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

func (d *ShellDispatcher) watch(ctx context.Context) {
	if d.Shadow {
		return // the host already owns the live watcher
	}
	script := FindWatchScript()
	if script == "" {
		slog.Error("adopted mechanical watcher unavailable", "run", d.Run.ID)
		return
	}
	for ctx.Err() == nil {
		err := supervisedWatcher(ctx, script, d.workspace, d.Config.Env["ORCA_ADOPT_AUTH"])
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("adopted mechanical watcher restarting", "run", d.Run.ID, "error", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
