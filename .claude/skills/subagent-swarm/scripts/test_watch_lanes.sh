#!/bin/bash
# Regression test for watch_lanes.sh signal detection.
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
TMP=$(mktemp -d /tmp/swarm-watch-test-XXXXXX)
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); echo "  ok   $1"; }
no(){ FAIL=$((FAIL+1)); echo "  FAIL $1"; }

cleanup(){ rm -rf "$TMP"; }
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

echo
echo "passed $PASS, failed $FAIL"
[ "$FAIL" -eq 0 ]
