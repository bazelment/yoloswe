#!/bin/bash
# Regression test for ledger.py concurrency and schema.
#
# The bug: save() was open(path,"w") -- truncate in place, then json.dump -- and every
# subcommand is a full read-modify-write of the whole file with no lock. One orchestrator
# owned the ledger, so it never showed. Two writers means a silent lost update: last
# writer wins, no error, and the ledger quietly stops matching reality. That interleaving
# is now the design (a Go tick reconciling while an interactive session runs `set`), so
# it is a correctness bug, not a theoretical one.
#
# Measured on the Go side of the same contract: with the lock removed, 7 of 8 concurrent
# updates vanished. This asserts the shell side the same way.
set -u
SW="$(cd "$(dirname "$0")" && pwd)"
L(){ /usr/bin/env python3 "$SW/ledger.py" "$@"; }
TMP=$(mktemp -d /tmp/swarm-ledger-test-XXXXXX)
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); echo "  ok   $1"; }
no(){ FAIL=$((FAIL+1)); echo "  FAIL $1"; }
chk(){ [ "$2" = "$3" ] && ok "$1 ($2)" || no "$1: expected $3, got $2"; }
trap 'rm -rf "$TMP"' EXIT

echo "== concurrent writers do not lose updates =="
RUN="$TMP/conc"
L init "$RUN" --goal g --phases "swe:,review:" --base main --target main >/dev/null
N=8
for i in $(seq 1 $N); do L add "$RUN" --id "lane$i" --title "t$i" --branch "b$i" >/dev/null; done
# All N fire at once, each a full read-modify-write of the same file.
for i in $(seq 1 $N); do L set "$RUN" --id "lane$i" --note "n$i" >/dev/null 2>&1 & done
wait
SURV=$(/usr/bin/env python3 -c "
import json,sys
d=json.load(open('$RUN/state.json'))
print(sum(1 for t in d['tasks'] if t['notes']))
" 2>/dev/null || echo PARSE_ERROR)
chk "all $N concurrent notes survive" "$SURV" "$N"

echo "== state.json is never observed half-written =="
# A reader must never see a truncated file. With truncate-in-place it can.
RUN2="$TMP/atomic"
L init "$RUN2" --goal g --phases "swe:" --base main --target main >/dev/null
for i in $(seq 1 40); do L add "$RUN2" --id "l$i" --title "$(head -c 200 /dev/zero | tr '\0' 'x')" --branch "b$i" >/dev/null; done
BAD=0
( for i in $(seq 1 60); do L set "$RUN2" --id l1 --note "spin$i" >/dev/null 2>&1; done ) &
WPID=$!
for i in $(seq 1 60); do
  /usr/bin/env python3 -c "import json;json.load(open('$RUN2/state.json'))" 2>/dev/null || BAD=$((BAD+1))
done
wait $WPID
chk "no torn reads during concurrent writes" "$BAD" "0"

echo "== new fields round-trip =="
RUN3="$TMP/fields"
L init "$RUN3" --goal g --phases "swe:" --base main --target main >/dev/null
L add "$RUN3" --id lane1 --title t --branch b >/dev/null
L set "$RUN3" --id lane1 --pr 123 --pr-head abc123 --approval-sha def456 --checks passing >/dev/null 2>&1
V=$(/usr/bin/env python3 -c "
import json
t=json.load(open('$RUN3/state.json'))['tasks'][0]
print(t.get('pr'), t.get('pr_head'), t.get('approval_sha'), t.get('checks'))
" 2>/dev/null || echo ERR)
chk "pr fields persist" "$V" "123 abc123 def456 passing"

echo "== unknown keys written by another tool survive our writes =="
# swarm-queen writes fork_sha/phase_start_sha/round/last_verified_at. ledger.py must not
# drop them: two writers silently deleting each other's fields is the same lost-update
# bug wearing a different hat.
/usr/bin/env python3 -c "
import json
p='$RUN3/state.json'
d=json.load(open(p))
d['tasks'][0]['phase_start_sha']='deadbeef'
d['config']['custom_key']='keepme'
json.dump(d,open(p,'w'),indent=2)
"
L set "$RUN3" --id lane1 --note touched >/dev/null
K=$(/usr/bin/env python3 -c "
import json
d=json.load(open('$RUN3/state.json'))
print(d['tasks'][0].get('phase_start_sha'), d['config'].get('custom_key'))
")
chk "foreign task+config keys preserved" "$K" "deadbeef keepme"

echo "== doctor catches the drift shapes seen in real runs =="
RUN4="$TMP/doc"; WT="$TMP/doc-wt"; mkdir -p "$WT"
L init "$RUN4" --goal g --phases "swe:,review:" --base main --target main >/dev/null
# The 2026-09-04 shape: 8 lanes sat `running` over work that had already merged.
L add "$RUN4" --id merged-running --title t --branch b1 >/dev/null
L set "$RUN4" --id merged-running --status running --phase swe --worktree "$WT" --merge-sha abc >/dev/null
# Approval pinned to an older head. Fired three times in one run; merging on
# reviewDecision alone would have shipped unapproved bytes.
L add "$RUN4" --id stale-appr --title t --branch b2 >/dev/null
L set "$RUN4" --id stale-appr --status running --phase swe --worktree "$WT"      --pr 11968 --pr-head 837c940 --approval-sha fa365c1 >/dev/null
# A `done` lane still holding its worktree -- live right now in the 09-08 run.
L add "$RUN4" --id done-wt --title t --branch b3 >/dev/null
L set "$RUN4" --id done-wt --status done --worktree "$WT" >/dev/null
OUT=$(L doctor "$RUN4" 2>&1); RC=$?
for pat in "merged work hiding as in-flight" "approval is STALE" "status=done but worktree still exists"; do
  if echo "$OUT" | grep -q "$pat"; then ok "detects: $pat"
  else no "missed: $pat"; fi
done
chk "doctor exits non-zero on drift" "$RC" "1"

echo "== doctor is clean on a healthy ledger, and exits 0 =="
RUN5="$TMP/clean"
L init "$RUN5" --goal g --phases "swe:" --base main --target main >/dev/null
L add "$RUN5" --id ok1 --title t --branch b >/dev/null
OUT=$(L doctor "$RUN5" 2>&1); RC=$?
chk "no false positives on a planned lane" "$RC" "0"

echo "== a live session squatting a done lane's worktree is called out =="
# The destructive case. window_id decays to empty (1/12 populated in a live run), so a
# reap plan built from the ledger omits the kill step and removes the worktree out from
# under a running agent. Sessions must be resolved from bramble by worktree_name.
RUN6="$TMP/squat"; SWT="$TMP/squat-wt"; mkdir -p "$SWT"
L init "$RUN6" --goal g --phases "swe:" --base main --target main >/dev/null
L add "$RUN6" --id squatted --title t --branch b >/dev/null
L set "$RUN6" --id squatted --status done --worktree "$SWT" >/dev/null   # window_id stays empty
cat > "$TMP/live.json" <<JSON
{"sessions":[{"id":"squatted-builder-abc","model":"opus","prompt":"p","status":"idle",
 "type":"builder","worktree_name":"$(basename "$SWT")","tmux_target":"@99"}]}
JSON
OUT=$(L doctor "$RUN6" --sessions "$TMP/live.json" 2>&1)
if echo "$OUT" | grep -q "LIVE SESSION still holds its worktree"; then ok "live squatter flagged"
else no "silent on a live session holding a done lane's worktree: $OUT"; fi
if echo "$OUT" | grep -q "@99"; then ok "names the pane to kill"
else no "does not name the pane"; fi
# Without a live session the same lane is only a stale worktree, not a kill-first case.
OUT=$(L doctor "$RUN6" 2>&1)
if echo "$OUT" | grep -q "worktree still exists"; then ok "no session -> plain stale-worktree finding"
else no "lost the plain finding when no sessions given"; fi
if echo "$OUT" | grep -q "LIVE SESSION"; then no "invented a squatter with no session data"
else ok "does not invent a squatter"; fi

echo "== a dangling worktree path is drift whatever the status =="
# The false negative this fixes. A partial teardown -- worktree removed, ledger never
# reconciled, branch left behind -- used to read as CLEAN, because the dangling-path
# check was gated on status=running. A real run had all 12 lanes in this shape and
# doctor reported "no drift detected".
RUN7="$TMP/dangle"
L init "$RUN7" --goal g --phases "swe:" --base main --target main >/dev/null
L add "$RUN7" --id torn-down --title t --branch b7 >/dev/null
L set "$RUN7" --id torn-down --status done --worktree "$TMP/removed-by-hand" >/dev/null
OUT=$(L doctor "$RUN7" 2>&1); RC=$?
if echo "$OUT" | grep -q "recorded worktree is gone"; then ok "done lane with a dangling path flagged"
else no "silent on a done lane whose worktree path does not exist: $OUT"; fi
if echo "$OUT" | grep -q "teardown never reconciled"; then ok "names it as an unreconciled teardown"
else no "does not explain the shape"; fi
chk "dangling path exits non-zero" "$RC" "1"
# Still caught for a running lane, which is where the check started.
L add "$RUN7" --id running-gone --title t --branch b8 >/dev/null
L set "$RUN7" --id running-gone --status running --phase swe --worktree "$TMP/also-gone" >/dev/null
if L doctor "$RUN7" 2>&1 | grep -q "running-gone.*recorded worktree is gone"; then ok "running lane still caught"
else no "regressed the running case"; fi
# A path that EXISTS must not be reported as dangling.
mkdir -p "$TMP/present"
L add "$RUN7" --id present --title t --branch b9 >/dev/null
L set "$RUN7" --id present --status running --phase swe --worktree "$TMP/present" >/dev/null
if L doctor "$RUN7" 2>&1 | grep -q "present.*recorded worktree is gone"; then no "false positive on an existing path"
else ok "no false positive on an existing path"; fi

echo "== a branch probe that cannot run SKIPS rather than reporting zero =="
# `git branch` outside a repo exits 128 with EMPTY stdout and raises nothing, so reading
# stdout alone turns "I could not look" into "no branches survive" -- a clean bill of
# health manufactured by a broken probe, the same shape as audit_cleanup.sh's socket bug.
RUN8="$TMP/probe"
L init "$RUN8" --goal g --phases "swe:" --base main --target main >/dev/null
L add "$RUN8" --id done-lane --title t --branch some-branch >/dev/null
L set "$RUN8" --id done-lane --status done >/dev/null
NOREPO=$(mktemp -d); OUT=$(cd "$NOREPO" && L doctor "$RUN8" 2>&1); rmdir "$NOREPO" 2>/dev/null
if echo "$OUT" | grep -q "branch checks SKIPPED, not passed"; then ok "unusable probe reports SKIPPED"
else no "silently treated an unusable probe as 'no branches survive': $OUT"; fi
if echo "$OUT" | grep -q "exited 128"; then ok "names the exit code"
else no "does not say why it could not look"; fi

echo "== absence is never evidence of approval =="
# A PR with no recorded head/approval must be reported unverifiable, never assumed fine.
L add "$RUN5" --id unverif --title t --branch b2 >/dev/null
L set "$RUN5" --id unverif --status running --phase swe --worktree "$WT" --pr 12000 >/dev/null
if L doctor "$RUN5" 2>&1 | grep -q "approval unverifiable"; then ok "unverifiable approval flagged"
else no "silent on a PR with no approval data"; fi

echo
echo "passed $PASS, failed $FAIL"
[ "$FAIL" -eq 0 ]
