#!/bin/bash
# Wake the orchestrator only for a decision:
#   - a task wrote a new .done file,
#   - a lane's tmux window is gone (it died before it could report), or
#   - a lane went quiet (HEAD and dirty-count both static) for ~STALL_MIN minutes.
#
# This is the delivery path, not an optimization. Bramble may drop a one-line
# hint into a pane when a subagent finishes, but it names no lane, carries no
# result, and is dropped whenever the pane is busy or holds a draft — so it can
# shorten the wait and must never be waited on.
#
#   watch_lanes.sh <run-dir> [max-minutes]
#
# NOT on commits. Lanes are briefed to commit early and often, so a commit-keyed
# watcher fires on every incremental commit — one lane committing five times woke
# the orchestrator five times with nothing to decide.
#
# Lanes come from the ledger (status running), so record
# each worktree with `ledger.py set --worktree` at spawn or it is invisible here.
# Run with run_in_background:true and end the turn.
#
# Reap the PREVIOUS watcher in its own separate foreground call before arming
# this one. Never inside it: the reap pattern then matches this script's own argv
# and it kills itself on startup (exit 144, zero turns of watching).
#     pkill -f '[w]atch_lanes[.]sh'; pgrep -f '[w]atch_lanes[.]sh' | wc -l
set -u

RUN="${1:?usage: watch_lanes.sh <run-dir> [max-minutes]}"
MAXMIN="${2:-60}"
INTERVAL="${INTERVAL:-20}"
STALL_MIN="${STALL_MIN:-15}"
HERE="$(cd "$(dirname "$0")" && pwd)"

lanes() { /usr/bin/env python3 "$HERE/ledger.py" lanes "$RUN" --need-worktree 2>/dev/null; }

TARGET=$(/usr/bin/env python3 "$HERE/ledger.py" lanes "$RUN" --config target 2>/dev/null)
TARGET="${TARGET:-HEAD}"

rounds=$(( MAXMIN * 60 / INTERVAL ))
stall_rounds=$(( STALL_MIN * 60 / INTERVAL ))

before=$(ls "$RUN"/*.done "$RUN"/*.needs-swe 2>/dev/null | sort)
declare -A LASTC LASTD STALL

while IFS=$'\t' read -r id _phase _branch wt wid; do
  LASTC[$id]=$(git -C "$wt" rev-parse HEAD 2>/dev/null)
  LASTD[$id]=$(git -C "$wt" status --porcelain 2>/dev/null | wc -l)
  STALL[$id]=0
done < <(lanes)

for _ in $(seq 1 "$rounds"); do
  now=$(ls "$RUN"/*.done "$RUN"/*.needs-swe 2>/dev/null | sort)
  new=$(comm -13 <(echo "$before") <(echo "$now"))
  if [ -n "$new" ]; then
    echo "SIGNAL-FILE:"
    while read -r f; do
      [ -z "$f" ] && continue
      case "$f" in
        *.done) stem=$(basename "$f" .done); sig="done" ;;
        *.needs-swe) stem=$(basename "$f" .needs-swe); sig="needs-swe" ;;
        *) stem=$(basename "$f"); sig="signal" ;;
      esac
      t="${stem%%.*}"; ph="${stem#*.}"; [ "$ph" = "$t" ] && ph="-"
      wt=$(lanes | awk -F'\t' -v t="$t" '$1==t{print $4}')
      c=$(git -C "$wt" log --oneline "$TARGET..HEAD" 2>/dev/null | wc -l)
      d=$(git -C "$wt" status --porcelain 2>/dev/null | wc -l)
      # A .done or .needs-swe is a CLAIM about work, not the work. Check commits before believing it.
      warn=""
      [ "$c" -eq 0 ] && warn="  <-- EMPTY BRANCH: back up and nudge, do NOT merge"
      echo "  $t [$ph] ($sig, commits=$c dirty=$d)$warn"
    done <<<"$new"
    exit 0
  fi

  while IFS=$'\t' read -r id _phase _branch wt wid; do
    # A dead window is a decision now, not in STALL_MIN minutes. Nothing pushes
    # this: a lane that dies before writing .done has no other way to say so,
    # and its worktree can look perfectly static while it is gone.
    if [ -n "$wid" ] && ! tmux list-panes -t "$wid" >/dev/null 2>&1; then
      echo "GONE: $id — its tmux window ($wid) is closed"
      c=$(git -C "$wt" log --oneline "$TARGET..HEAD" 2>/dev/null | wc -l)
      d=$(git -C "$wt" status --porcelain 2>/dev/null | wc -l)
      echo "  commits=$c dirty=$d — back up uncommitted work before reusing the worktree"
      exit 0
    fi
    c=$(git -C "$wt" rev-parse HEAD 2>/dev/null)
    d=$(git -C "$wt" status --porcelain 2>/dev/null | wc -l)
    if [ "$c" != "${LASTC[$id]:-}" ] || [ "$d" != "${LASTD[$id]:-}" ]; then
      LASTC[$id]=$c; LASTD[$id]=$d; STALL[$id]=0
    else
      STALL[$id]=$(( ${STALL[$id]:-0} + 1 ))
      if [ "${STALL[$id]}" -ge "$stall_rounds" ]; then
        echo "STALLED: $id — no git activity for ~${STALL_MIN}min (commits and dirty count both static)"
        echo "  capture its pane: it is probably blocked on a question, which never notifies"
        exit 0
      fi
    fi
  done < <(lanes)

  sleep "$INTERVAL"
done

echo "TIMEOUT (~${MAXMIN}min) — no lane reported done, none stalled"
while IFS=$'\t' read -r id _phase _branch wt wid; do
  printf '  %-28s commits=%s dirty=%s last=%s\n' "$id" \
    "$(git -C "$wt" log --oneline "$TARGET..HEAD" 2>/dev/null | wc -l)" \
    "$(git -C "$wt" status --porcelain 2>/dev/null | wc -l)" \
    "$(git -C "$wt" log -1 --format=%cr 2>/dev/null)"
done < <(lanes)
