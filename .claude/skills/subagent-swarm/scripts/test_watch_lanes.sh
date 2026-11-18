#!/bin/bash
# Regression test for watch_lanes.sh signal detection and per-run watcher reaping.
#
# The bug this pins down: for most of this skill's life the watcher globbed only
# "$RUN"/*.done. A review phase that rejects a lane writes <lane>.<phase>.needs-swe
# instead, so a lane bounced back to swe was INVISIBLE to the watcher -- it surfaced
# only via the 15-minute stall path or the 60-minute timeout, with the orchestrator
# idle in between. Live runs do produce these: the 2026-09-08 run held 5 of them.
#
# Runs entirely on fixture directories under a temp dir. It never spawns a session,
# never touches tmux, and never reads a real run dir.
set -u
SW="$(cd "$(dirname "$0")" && pwd)"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/swarm-watch-test-XXXXXX")
PASS=0; FAIL=0; WPIDS=""
ok(){ PASS=$((PASS+1)); echo "  ok   $1"; }
no(){ FAIL=$((FAIL+1)); echo "  FAIL $1"; }

within(){                         # within <seconds> <cmd...>: poll until true or timeout
  local end=$((SECONDS + $1)); shift
  until "$@"; do [ "$SECONDS" -ge "$end" ] && return 1; sleep 0.1; done
}

# Stop only watchers this test armed, and only while their argv still names a run
# dir under $TMP, so a recycled pid is never signalled. A killed watcher's
# in-flight git command can still write under $TMP for a moment, so retry the rm.
cleanup(){
  local p killed=()
  for p in $WPIDS; do
    case "$(ps -ww -o args= -p "$p" 2>/dev/null)" in
      *"watch_lanes.sh $TMP/"*) kill "$p" 2>/dev/null && killed+=("$p") ;;
    esac
  done
  [ "${#killed[@]}" -gt 0 ] && wait "${killed[@]}" 2>/dev/null
  within 5 rm -rf "$TMP" 2>/dev/null || echo "warning: could not remove $TMP" >&2
}
trap cleanup EXIT

# A lane needs a real git worktree: the watcher resolves commits and dirty count
# per lane, and ledger.py lanes --need-worktree drops any lane without one.
make_run(){                       # make_run <name> -> echoes run dir
  local run="$TMP/$1" wt="$TMP/$1-wt"
  mkdir -p "$wt" && git -C "$wt" init -q 2>/dev/null
  git -C "$wt" config user.email t@t && git -C "$wt" config user.name t
  git -C "$wt" commit -q --allow-empty -m base
  /usr/bin/env python3 "$SW/ledger.py" init "$run" --goal g \
    --phases "swe:,local-review:" --base main --target main >/dev/null
  /usr/bin/env python3 "$SW/ledger.py" add "$run" --id lane1 --title t \
    --branch b1 >/dev/null
  /usr/bin/env python3 "$SW/ledger.py" set "$run" --id lane1 --status running \
    --phase swe --worktree "$wt" >/dev/null
  echo "$run"
}

# The watcher snapshots existing signal files at startup as its BASELINE and reports
# only files that appear afterwards -- otherwise every tick would wake instantly on the
# previous tick's signals. So a test signal must be written AFTER the watcher arms.
# watch_then <run> <file-to-create>   (empty second arg = create nothing)
watch_then(){
  local run="$1" mk="${2:-}" out
  out=$(mktemp)
  ( INTERVAL=1 STALL_MIN=99 timeout 20 bash "$SW/watch_lanes.sh" "$run" 1 >"$out" 2>&1 ) &
  local pid=$!
  sleep 2                       # let it snapshot the baseline
  [ -n "$mk" ] && : > "$run/$mk"
  wait $pid 2>/dev/null
  cat "$out"; rm -f "$out"
}

echo "== .needs-swe wakes the watcher =="
RUN=$(make_run needsswe)
OUT=$(watch_then "$RUN" lane1.local-review.needs-swe)
if echo "$OUT" | grep -q 'SIGNAL-FILE'; then ok "signal header printed"
else no "no SIGNAL-FILE header; got: $(echo "$OUT" | head -1)"; fi
if echo "$OUT" | grep -q 'needs-swe'; then ok "signal typed as needs-swe"
else no "signal not labelled needs-swe; got: $(echo "$OUT" | head -3)"; fi
if echo "$OUT" | grep -q 'lane1'; then ok "names the lane"
else no "lane not named"; fi

echo "== .done still wakes the watcher =="
RUN=$(make_run doneonly)
OUT=$(watch_then "$RUN" lane1.swe.done)
if echo "$OUT" | grep -q 'lane1 \[swe\]'; then ok "done signal detected"
else no "done not detected; got: $(echo "$OUT" | head -3)"; fi

echo "== an empty branch is flagged, not merged =="
# A .done is a CLAIM. With zero commits past TARGET the watcher must say so --
# this is the guard that stops an empty branch being integrated.
if echo "$OUT" | grep -q 'EMPTY BRANCH'; then ok "empty branch warned"
else no "no EMPTY BRANCH warning on a zero-commit claim"; fi

echo "== a pre-existing signal is not re-reported =="
# Files present before the watcher arms are the baseline; only NEW ones are a
# decision. Otherwise every tick would wake instantly on last tick's signals.
RUN=$(make_run preexisting)
: > "$RUN/lane1.swe.done"        # present BEFORE the watcher arms
OUT=$(watch_then "$RUN" "")
if echo "$OUT" | grep -q 'SIGNAL-FILE'; then no "re-reported a baseline signal"
else ok "baseline signal ignored (got: $(echo "$OUT" | head -1))"; fi

# Long-lived watchers for the reap tests. arm runs in this shell (not in $(...)) so
# each watcher is this test's own child; ARMED is its pid.
arm(){
  INTERVAL=1 STALL_MIN=99 bash "$SW/watch_lanes.sh" "$1" 1 >/dev/null 2>&1 &
  ARMED=$!; WPIDS="$WPIDS $ARMED"
}
alive(){ local s; s=$(ps -o stat= -p "$1" 2>/dev/null | tr -d ' '); [ -n "$s" ] && [ "${s#Z}" = "$s" ]; }
gone(){ ! alive "$1"; }
recorded(){ [ "$(cat "$1/.watch_lanes.pid" 2>/dev/null)" = "$2" ]; }

echo "== re-arming one run reaps only that run's old watcher =="
# On a shared host every run has its own watcher. The old reap advice,
# pkill -f '[w]atch_lanes[.]sh', matched all of them, so re-arming one run killed
# the others with no wake event. Run B arms AFTER run A, so a host-wide pid file
# or a name-pattern reap would hit B.
RUNA=$(make_run shared); RUNB=$(make_run shared2)
arm "$RUNA"; A1=$ARMED; within 5 recorded "$RUNA" "$A1"
arm "$RUNB"; B1=$ARMED; within 5 recorded "$RUNB" "$B1"
if alive "$A1" && alive "$B1"; then ok "both runs' watchers armed"
else no "a watcher died on startup"; fi
arm "$RUNA"; A2=$ARMED; within 5 recorded "$RUNA" "$A2"
if within 5 gone "$A1"; then ok "run A's old watcher reaped on re-arm"
else no "run A's old watcher ($A1) still running after re-arm"; fi
if alive "$B1"; then ok "run B's watcher survived run A's re-arm"
else no "run A's re-arm killed run B's watcher"; fi
if alive "$A2"; then ok "run A's new watcher running"
else no "run A's new watcher died"; fi

echo "== a recorded pid now owned by another run's watcher is left alone =="
# A recorded pid can outlive its watcher and be reused, even by another run's
# watcher. Only a watch_lanes.sh for the same run dir is stopped; B's dir extends
# A's, so a prefix match on the run dir would kill B here.
echo "$B1" >"$RUNA/.watch_lanes.pid"
arm "$RUNA"; A3=$ARMED; within 5 recorded "$RUNA" "$A3"
if alive "$B1"; then ok "run B's watcher survived a stale pid in run A"
else no "a stale pid in run A killed run B's watcher"; fi

echo
echo "passed $PASS, failed $FAIL"
[ "$FAIL" -eq 0 ]
