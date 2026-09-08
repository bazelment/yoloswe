#!/bin/bash
# Sourceable bramble/run-dir helpers for a swarm orchestrator.
#
#   . ~/.claude/skills/subagent-swarm/scripts/swarm_bramble.sh
#
# WHY THIS EXISTS. Reading `bramble --help` is itself a failure mode. One mined run
# invoked it 17 times and still concluded agy was unsupported (the --backend help
# string is stale; model_registry.go routes gemini-* to the agy provider). Another
# spent 29 hours driving panes with raw `tmux send-keys` without noticing
# `bramble send-key` exists. Every run re-derived the same facts and some got them
# wrong, so they are encoded here once and asserted by sw_doctor.
#
# Every fact below was verified by invocation, not from help text.
set -u

SW_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- socket -----------------------------------------------------------------------
# The skill previously documented "${XDG_RUNTIME_DIR}/bramble-$(id -u).sock". That path
# DOES NOT EXIST: the real socket carries the TUI's pid, e.g. bramble-1000-2015796.sock.
# `test -S` on the documented path therefore fails on step one of every preflight,
# which is why mined runs flagged a stale socket in their first turn.
#
# Globs rather than `pgrep -n` (newest-wins): silently attaching the swarm to the
# wrong TUI is the "select live evidence by identity, never by list position" mistake.
# Identical behaviour with one TUI; fails loudly instead of silently with more.
sw_socket() {
  local d="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" uid; uid=$(id -u)
  local -a m=()
  local f
  for f in "$d"/bramble-"$uid"-*.sock; do
    [ -S "$f" ] || continue
    case "$f" in *bramble-control-*) continue ;; esac
    m+=("$f")
  done
  case ${#m[@]} in
    1) printf '%s\n' "${m[0]}" ;;
    0) echo "sw: no bramble socket in $d -- is the TUI running?" >&2; return 1 ;;
    *) echo "sw: ${#m[@]} bramble sockets in $d; refusing to guess which TUI:" >&2
       printf '  %s\n' "${m[@]}" >&2
       echo "set BRAMBLE_SOCK explicitly to the one you mean" >&2; return 1 ;;
  esac
}

# --- preflight --------------------------------------------------------------------
# sw_preflight <run-dir>  -- resolve coordinates, verify the TUI, write $RUN/env.sh.
#
# env.sh exists because shell state does not persist between an agent's Bash calls:
# mined runs retyped the same BRAMBLE_SOCK/SELF/RUN preamble 37, 31 and 40 times.
# After this, every later command is `. "$RUN/env.sh"`.
sw_preflight() {
  RUN="${1:?usage: sw_preflight <run-dir>}"
  BRAMBLE_SOCK="$(sw_socket)" || return 1
  export BRAMBLE_SOCK RUN
  export SW="$SW_DIR"

  # The orchestrator's own session id, read from the parent CLI's argv.
  SELF="${SELF:-$(ps -o args= -p "$(ps -o ppid= -p $$)" 2>/dev/null |
        sed -n "s/.*session-id '\([^']*\)'.*/\1/p")}"
  export SELF

  TARGET="${TARGET:-$(git rev-parse --abbrev-ref HEAD 2>/dev/null)}"
  BASE="${BASE:-$(git symbolic-ref --quiet refs/remotes/origin/HEAD 2>/dev/null |
        sed 's#refs/remotes/origin/##')}"; BASE="${BASE:-main}"
  export TARGET BASE

  bramble ping >/dev/null 2>&1 || { echo "sw: bramble not answering on $BRAMBLE_SOCK" >&2; return 1; }
  [ -n "$SELF" ] || echo "sw: WARNING could not resolve SELF; spawns cannot set --parent" >&2

  mkdir -p "$RUN"
  # BRAMBLE_SOCK is re-derived on every source, never trusted from the file: it
  # changes on each TUI restart, and a stale path fails closed in confusing ways.
  cat > "$RUN/env.sh" <<EOF
# Written by sw_preflight. Source this instead of retyping a preamble.
export RUN="$RUN"
export SW="$SW_DIR"
export SELF="$SELF"
export TARGET="$TARGET"
export BASE="$BASE"
export BRAMBLE_SOCK="\$(. "$SW_DIR/swarm_bramble.sh" >/dev/null 2>&1; sw_socket)"
EOF
  echo "$RUN/env.sh"
}

# --- sessions ---------------------------------------------------------------------
# list-sessions returns {"sessions":[...]} -- a DICT, not a list. A mined run rewrote
# a defensive isinstance() guard ~15 times for want of this one line.
#
# Row keys: id, model, prompt, status, type, worktree_name are always present;
# tmux_target, backend and parent_session_id are OPTIONAL. There is NO worktree_path,
# so lanes are matched on worktree_name.
sw_sessions() { bramble list-sessions 2>/dev/null; }

# sw_session_field <session-id> <key>  -- empty output means absent, not an error.
sw_session_field() {
  sw_sessions | /usr/bin/env python3 -c '
import json,sys
sid,key=sys.argv[1],sys.argv[2]
try: d=json.load(sys.stdin)
except ValueError: sys.exit(0)
for r in (d.get("sessions",d) if isinstance(d,dict) else d) or []:
    if r.get("id")==sid: print(r.get(key,"")); break
' "$1" "$2"
}

# sw_session_live <session-id>
# A session with NO tmux_target has no pane: it is gone, not merely idle. Absence is
# the liveness signal, available immediately instead of waiting out a stall timeout --
# and it is exactly the session a naive `capture-pane` would error on.
sw_session_live() {
  local t; t="$(sw_session_field "$1" tmux_target)"
  [ -n "$t" ]
}

# --- spawn ------------------------------------------------------------------------
# sw_spawn <lane> <phase> <model> <brief-file> [branch] [worktree]
#
# One call does the spawn AND records it. Recording used to be a separate step, and
# the ledger fields needing that step are precisely the ones that decayed across runs
# (brief 13/13 -> 0/12, window_id 6/13 -> 1/12). Same root cause as the leaked backup
# refs in audit_cleanup.sh: a hand step that costs nothing to skip gets skipped.
sw_spawn() {
  local lane="${1:?lane}" phase="${2:?phase}" model="${3:?model}" brief="${4:?brief-file}"
  local branch="${5:-}" worktree="${6:-}"
  : "${RUN:?run sw_preflight first}"
  [ -r "$brief" ] || { echo "sw_spawn: brief not readable: $brief" >&2; return 1; }
  local repo; repo="$(basename "$(git rev-parse --show-toplevel 2>/dev/null)")"
  [ -n "$repo" ] || { echo "sw_spawn: not in a git repo" >&2; return 1; }

  local -a args=(new-session -r "$repo" -t builder -m "$model"
                 -g "$lane" -p "$(cat "$brief")")
  # --parent is what makes a finished lane report back. Without it the lane completes
  # into the void.
  [ -n "${SELF:-}" ] && args+=(--parent "$SELF")

  if [ -n "$worktree" ]; then
    args+=(-w "$worktree")
  else
    # bramble's -f resolves against the REMOTE. When TARGET carries local commits the
    # lane must fork from local TARGET instead, or it silently starts from stale code.
    branch="${branch:?branch required when no worktree given}"
    if [ -n "$(git log --oneline "origin/$TARGET..$TARGET" 2>/dev/null)" ]; then
      worktree="$(git rev-parse --show-toplevel)/../$branch"
      git worktree add -b "$branch" "$worktree" "$TARGET" >&2 || return 1
      args=(new-session -r "$repo" -w "$worktree" -t builder -m "$model"
            -g "$lane" -p "$(cat "$brief")")
      [ -n "${SELF:-}" ] && args+=(--parent "$SELF")
    else
      args+=(--create-worktree -b "$branch" -f "$BASE")
    fi
  fi

  local out sid
  out="$(bramble "${args[@]}" 2>&1)" || { echo "$out" >&2; return 1; }
  sid="$(printf '%s' "$out" | grep -oE '[A-Za-z0-9_.-]+-(builder|planner|codetalk)-[0-9a-f]+' | head -1)"
  [ -n "$sid" ] || { echo "sw_spawn: could not parse a session id from:" >&2
                     printf '%s\n' "$out" >&2; return 1; }

  [ -z "$worktree" ] && worktree="$(sw_session_field "$sid" worktree_name)"
  local wt_abs; wt_abs="$(realpath "$worktree" 2>/dev/null || echo "$worktree")"

  # Record in the SAME call as the spawn -- ledger row, spawn.json, and the brief.
  /usr/bin/env python3 "$SW_DIR/ledger.py" set "$RUN" --id "$lane" \
    --status running --phase "$phase" --session "$sid" --worktree "$wt_abs" \
    --phase-start-sha "$(git -C "$wt_abs" rev-parse HEAD 2>/dev/null || echo '')" >/dev/null
  cat > "$RUN/$lane.$phase.spawn.json" <<EOF
{
  "session_id": "$sid",
  "worktree_path": "$wt_abs"
}
EOF
  cp "$brief" "$RUN/$lane.$phase.brief.txt" 2>/dev/null || true
  echo "$sid"
}

# --- nudge ------------------------------------------------------------------------
# sw_nudge <session-id> <text>
#
# "Queued for delivery" is NOT delivery. Text can sit unsent in a composer as
# [Pasted Content]; three instructions were lost that way in one run, and one lane was
# found holding 465 stacked pastes. So: count what is already pending, refuse to stack,
# send, then CONFIRM the pane went busy before believing it landed.
sw_nudge() {
  local sid="${1:?session-id}" text="${2:?text}"
  local target; target="$(sw_session_field "$sid" tmux_target)"
  [ -n "$target" ] || { echo "sw_nudge: $sid has no tmux_target -- window is gone; "\
"replace the session rather than nudging it" >&2; return 1; }

  local pending; pending="$(tmux capture-pane -p -t "$target" 2>/dev/null |
                            grep -cE '\[Pasted (text|Content)' || true)"
  if [ "${pending:-0}" -gt 0 ]; then
    echo "sw_nudge: $sid already holds $pending unsent paste(s) -- NOT sending." >&2
    echo "  submit with: bramble send-key --session-id $sid Enter" >&2
    echo "  if they keep stacking, write $RUN/HANDOVER-<lane>.md and replace the session" >&2
    return 1
  fi

  bramble send-input --session-id "$sid" "$text" >/dev/null 2>&1 || return 1
  bramble send-key --session-id "$sid" Enter >/dev/null 2>&1 || true

  # A busy pane renders an ELAPSED TIMER. Key on that, never on the verb: backends
  # animate Working/Brewed/Improvising/... and verb-matching reports busy lanes as idle.
  local i
  for i in 1 2 3 4 5 6; do
    sleep 5
    tmux capture-pane -p -t "$target" 2>/dev/null | tail -20 |
      grep -qE '[0-9]+[ms]( [0-9]+s)? ·|esc to interrupt|[0-9]+m [0-9]+s$' && {
        echo "delivered: $sid is working"; return 0; }
  done
  echo "sw_nudge: sent to $sid but it never went busy -- inspect the pane before resending" >&2
  return 2
}

# --- capability self-test ---------------------------------------------------------
# sw_doctor -- assert the facts this file hard-codes, so bramble drift breaks a check
# here rather than a live run. This is the mechanical form of "verify the tool
# surface", which two mined runs did by hand and got wrong.
sw_doctor() {
  local bad=0
  local sock; sock="$(sw_socket)" || bad=1
  [ -n "${sock:-}" ] && echo "  ok   socket resolves: $sock"

  if BRAMBLE_SOCK="${sock:-}" bramble ping >/dev/null 2>&1; then echo "  ok   bramble ping answers"
  else echo "  FAIL bramble ping"; bad=1; fi

  if BRAMBLE_SOCK="${sock:-}" bramble kill-session --help >/dev/null 2>&1; then
    echo "  NOTE bramble now HAS kill-session -- reaping can be simplified"
  else echo "  ok   no kill-session (reap = wt + tmux kill-window + process kill)"; fi

  if BRAMBLE_SOCK="${sock:-}" bramble send-key --help >/dev/null 2>&1; then
    echo "  ok   send-key exists"
  else echo "  FAIL send-key missing -- sw_nudge cannot submit"; bad=1; fi

  BRAMBLE_SOCK="${sock:-}" sw_sessions | /usr/bin/env python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except ValueError: print("  FAIL list-sessions is not JSON"); sys.exit(1)
if not isinstance(d,dict) or "sessions" not in d:
    print("  FAIL list-sessions is no longer {\"sessions\":[...]}"); sys.exit(1)
print("  ok   list-sessions is a dict with a sessions key")
rows=d["sessions"]
if not rows: print("  ok   (no live sessions to shape-check)"); sys.exit(0)
need={"id","model","prompt","status","type","worktree_name"}
missing=need-set(rows[0])
print("  ok   required row keys present" if not missing else f"  FAIL row missing {missing}")
print("  ok   no worktree_path (match on worktree_name)" if "worktree_path" not in rows[0]
      else "  NOTE worktree_path now exists")
n=sum(1 for r in rows if not r.get("tmux_target"))
print(f"  ok   tmux_target optional ({n}/{len(rows)} rows without one = gone)")
sys.exit(1 if missing else 0)
' || bad=1
  [ "$bad" -eq 0 ] && echo "sw_doctor: OK" || echo "sw_doctor: FAILURES above"
  return "$bad"
}
