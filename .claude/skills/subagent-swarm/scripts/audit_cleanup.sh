#!/bin/bash
# Verify every finished task is fully closed. Five things must all be zero: no
# live session, no worktree, no branch, no backup ref, no tmux pane.
#
#   audit_cleanup.sh <run-dir>
#
# Run at every loop tick, not only when asked. Manual per-merge cleanup measured
# 3-for-5 consistent: sessions, worktrees, branches and tmux windows were closed
# reliably, and backup refs leaked repeatedly because releasing them is a separate
# hand step. A leaked ref costs nothing operationally, which is why it is the step
# that survives — and why it needs a mechanical check.
#
# A cleanup routine with five steps drifts on whichever step is least visible.
set -u

RUN="${1:?usage: audit_cleanup.sh <run-dir>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
export BRAMBLE_SOCK="${BRAMBLE_SOCK:-${XDG_RUNTIME_DIR:-/tmp}/bramble-$(id -u).sock}"
# The socket is keyed by UID, not the TUI pid. With the wrong path list-sessions
# fails silently, `sessions` is empty, and every task audits session=0 -- a clean
# bill of health manufactured by a broken probe, in the script whose job is leaks.
[ -S "$BRAMBLE_SOCK" ] || { echo "audit: no bramble socket at $BRAMBLE_SOCK -- session counts would be false" >&2; exit 1; }

# Branch checks run against the current repo, so this must be run from the
# orchestrator's own worktree — the one the task branches merge into.
git rev-parse --git-dir >/dev/null 2>&1 || {
  echo "audit_cleanup.sh: run this from the orchestrator's worktree (cwd is not a git repo)" >&2
  exit 2
}

# Every probe is CHECKED. Discarding exit status made a failed `bramble
# list-sessions` or `tmux list-panes` indistinguishable from a measured empty
# fleet: session=0 and tmux=0 for every lane, and the audit whose whole job is
# finding leaks would print a clean bill of health from observations it never
# made. A probe that could not run is UNKNOWN, and unknown must refuse.
if ! sessions=$(bramble list-sessions 2>/dev/null); then
  echo "audit: bramble list-sessions FAILED -- session counts would be false, not zero" >&2
  exit 3
fi
if ! panes=$(tmux list-panes -a -F '#{window_id} #{pane_current_path}' 2>/dev/null); then
  echo "audit: tmux list-panes FAILED -- pane counts would be false, not zero" >&2
  exit 3
fi
SELFWIN=$(. "$HERE/tmux_safe.sh" 2>/dev/null && resolve_self 2>/dev/null || echo '')

bad=0; n=0
while IFS=$'\t' read -r id _phase branch wt wid; do
  # Retention intent is read STRAIGHT FROM THE LEDGER, not from an extra column
  # on `ledger.py lanes`. That output is a five-field contract shared with
  # poll_panes.sh, watch_lanes.sh and snapshot_at_risk.sh, and bash puts a
  # surplus field into the last variable -- so widening it for this one consumer
  # silently corrupts `wid` in the other three.
  retained=$(/usr/bin/env python3 -c "
import json,sys
try:
    d=json.load(open(sys.argv[1]+'/state.json'))
except Exception:
    sys.exit(2)
print('1' if any(t.get('id')==sys.argv[2] and t.get('backup_retained') for t in d.get('tasks',[])) else '0')
" "$RUN" "$id" 2>/dev/null) || retained=""
  if [ -z "$retained" ]; then
    echo "audit: cannot read backup_retained for $id -- refusing to guess intent" >&2
    exit 3
  fi
  n=$((n + 1))
  # Ownership comes from worktree_name, which is the only key bramble reports
  # (swarm-queen/bramble/client.go: "Note the absence of a worktree PATH"). The
  # old test grepped for a session id beginning with the lane id, so a live
  # session whose id did not embed the lane name counted as absent and passed.
  wtname=""; [ -n "$wt" ] && wtname=$(basename "$wt")
  s=0
  if [ -n "$wtname" ]; then
    s=$(printf '%s' "$sessions" | grep -c "\"worktree_name\"[[:space:]]*:[[:space:]]*\"$wtname\"")
  fi
  w=0; [ -n "$wt" ] && [ -d "$wt" ] && w=1
  b=0; [ -n "$branch" ] && b=$(git branch --list "$branch" | grep -c .)
  r=0; git rev-parse -q --verify "refs/backup/$id" >/dev/null 2>&1 && r=1
  t=0; tw=""
  if [ -n "$wt" ]; then
    tw=$(printf '%s' "$panes" | awk -v w="$wt" 'index($2, w) == 1 {print $1}' | sort -u | tr '\n' ' ')
    t=$(printf '%s' "$tw" | wc -w)
  fi

  # A kept snapshot is only "kept" if the reaper SAID SO. `retained` is the
  # ledger's backup_retained field, written by applyReap in the same transaction
  # that closes the lane.
  #
  # This was briefly inferred from "the other four checks are zero", which is
  # wrong in the most dangerous direction: backup refs are released LAST, so a
  # genuinely leaked ref almost always appears on a lane whose session,
  # worktree, branch and pane are already gone. That inference passed exactly
  # the leak this script's header says leaked repeatedly in real runs.
  if [ "$s$w$b$t" = "0000" ] && [ "$r" = "1" ] && [ "$retained" = "1" ]; then
    echo "FULLY CLOSED $id (backup retained on purpose at refs/backup/$id)"
  elif [ "$s$w$b$r$t" != "00000" ]; then
    echo "NOT FULLY CLOSED $id: session=$s worktree=$w branch=$b backupref=$r tmux=$t${tw:+ [$tw]}"
    for _w in $tw; do
      [ -n "$SELFWIN" ] && [ "$_w" = "$SELFWIN" ] && echo "  ^ $_w IS YOU -- never kill it"
    done
    bad=$((bad + 1))
  fi
done < <(/usr/bin/env python3 "$HERE/ledger.py" lanes "$RUN" --status done 2>/dev/null)

echo "audit: $n done task(s), $bad not fully closed"

# Sixth thing: agent processes whose worktree is gone.
#
# The five checks above are all about resources the ledger KNOWS about. Nothing
# watched the processes. One run accumulated 62 orphaned agent processes -- codex,
# node, claude -- and they surfaced only because a human asked "did you reap" twice;
# a lane's runner can outlive its session, its window and its worktree, holding
# memory and API quota with nothing pointing at it.
#
# Identified by CWD, not by name: matching on "codex" or "node" would sweep up
# unrelated work, and the deleted-worktree marker Linux puts on /proc/<pid>/cwd is
# the unambiguous signal that a process is running somewhere that no longer exists.
orph=0
for pid in $(pgrep -u "$(id -u)" -f 'codex|claude|cursor-agent|agy|node' 2>/dev/null); do
  # Never report ourselves or our own ancestors: this script runs from inside an
  # agent session, and a sweep that lists its own caller trains you to ignore it.
  case " $$ $PPID " in *" $pid "*) continue ;; esac
  cwd=$(readlink "/proc/$pid/cwd" 2>/dev/null) || continue
  case "$cwd" in
    *" (deleted)")
      # argv[0] plus one arg: a shell's full script body is pages long and drowns
      # the finding. The full cmdline still names the lane and round when you need
      # to identify a process before killing it -- read /proc/<pid>/cmdline then.
      cmd=$(tr '\0' '\n' < "/proc/$pid/cmdline" 2>/dev/null | head -2 | tr '\n' ' ' | cut -c1-70)
      # Zombies read as alive to a naive check; escalating to kill -9 on one is futile.
      st=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')
      case "$st" in Z*) note=" (ZOMBIE -- reap its parent, not it)" ;; *) note="" ;; esac
      echo "ORPHAN pid=$pid stat=${st:-?} cwd=${cwd}${note}"
      echo "  $cmd"
      orph=$((orph + 1))
      ;;
  esac
done
if [ "$orph" -gt 0 ]; then
  echo "audit: $orph orphaned agent process(es) in deleted worktrees"
  echo "  identify before killing: /proc/<pid>/cmdline names the lane and round." >&2
  bad=$((bad + orph))
else
  echo "audit: no orphaned agent processes"
fi

if [ "$bad" -gt 0 ]; then
  echo "Teardown order and branch verification: references/bramble-mechanics.md, 'Watch and reap'." >&2
fi
exit $(( bad > 0 ? 1 : 0 ))
