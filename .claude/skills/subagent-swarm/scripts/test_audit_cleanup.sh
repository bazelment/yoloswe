#!/bin/bash
# Regression test for audit_cleanup.sh, the five-zeros leak check.
#
# Two defects this pins, both found by review after they shipped:
#
# 1. Probe errors were discarded (`2>/dev/null` with no status check), so a
#    failed `bramble list-sessions` or `tmux list-panes` produced session=0 and
#    tmux=0 for EVERY lane -- a clean bill of health manufactured from
#    observations never made, in the script whose whole job is finding leaks.
#
# 2. A retained backup ref was inferred from "the other four checks are zero".
#    Backup refs are released LAST, so a genuinely leaked ref almost always
#    appears on a lane whose session, worktree, branch and pane are already
#    gone. That inference passed exactly the leak this script exists to catch.
#    Intent is now RECORDED by the reaper in the ledger's backup_retained field.
set -u
SW="$(cd "$(dirname "$0")" && pwd)"
L(){ /usr/bin/env python3 "$SW/ledger.py" "$@"; }
TMP=$(mktemp -d /tmp/swarm-audit-test-XXXXXX)
PASS=0; FAIL=0
ok(){ PASS=$((PASS+1)); echo "  ok   $1"; }
no(){ FAIL=$((FAIL+1)); echo "  FAIL $1"; }
trap 'rm -rf "$TMP"' EXIT

# A git repo to audit from, and a fake bramble/tmux on PATH we can break.
REPO="$TMP/repo"; mkdir -p "$REPO"
git init -q -b main "$REPO"
git -C "$REPO" config user.email t@e.com
git -C "$REPO" config user.name T
git -C "$REPO" commit -q --allow-empty -m base

BIN="$TMP/bin"; mkdir -p "$BIN"
mk_bramble(){ printf '#!/bin/bash\nexit %s\n' "$1" > "$BIN/bramble"; chmod +x "$BIN/bramble"; }
mk_tmux(){ printf '#!/bin/bash\nexit %s\n' "$1" > "$BIN/tmux"; chmod +x "$BIN/tmux"; }
mk_bramble 0; mk_tmux 0

RUN="$TMP/run"
L init "$RUN" --goal g --phases "swe:,clean:" --base main --target main >/dev/null
L add "$RUN" --id lane-a --title t --branch b-a >/dev/null
L set "$RUN" --id lane-a --status done >/dev/null

export BRAMBLE_SOCK="$TMP/sock"
python3 -c "
import socket,sys
s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); s.listen(1)
" "$BRAMBLE_SOCK" 2>/dev/null || : 
run_audit(){ ( cd "$REPO" && PATH="$BIN:$PATH" bash "$SW/audit_cleanup.sh" "$RUN" 2>&1 ); }

echo "== a failed session probe refuses, it does not report zeros =="
mk_bramble 1
OUT=$(run_audit); RC=$?
if [ "$RC" != "0" ] && printf '%s' "$OUT" | grep -q "list-sessions FAILED"; then
  ok "a failed bramble probe aborts rather than counting session=0"
else
  no "a failed bramble probe was treated as a measured empty fleet (rc=$RC)"
fi
mk_bramble 0

echo "== a failed pane probe refuses too =="
mk_tmux 1
OUT=$(run_audit); RC=$?
if [ "$RC" != "0" ] && printf '%s' "$OUT" | grep -q "list-panes FAILED"; then
  ok "a failed tmux probe aborts rather than counting tmux=0"
else
  no "a failed tmux probe was treated as measured (rc=$RC)"
fi
mk_tmux 0

echo "== an UNMARKED backup ref is a leak =="
git -C "$REPO" update-ref refs/backup/lane-a HEAD
OUT=$(run_audit)
if printf '%s' "$OUT" | grep -q "NOT FULLY CLOSED lane-a"; then
  ok "a ref nobody recorded keeping reads as a leak"
else
  no "an unmarked backup ref passed the audit: $OUT"
fi

echo "== a RECORDED retention is reported, not failed =="
python3 - "$RUN" <<'EOF'
import json,sys
p=sys.argv[1]+"/state.json"
d=json.load(open(p))
for t in d["tasks"]:
    if t["id"]=="lane-a": t["backup_retained"]=True
json.dump(d,open(p,"w"),indent=2)
EOF
OUT=$(run_audit)
if printf '%s' "$OUT" | grep -q "FULLY CLOSED lane-a (backup retained on purpose"; then
  ok "a snapshot the reaper recorded keeping is not a failure"
else
  no "a recorded retention was still reported as a leak: $OUT"
fi
if printf '%s' "$OUT" | grep -q "0 not fully closed"; then
  ok "and it does not count against the audit"
else
  no "the recorded retention still counted as a failure: $OUT"
fi

echo
echo "passed $PASS, failed $FAIL"
[ "$FAIL" -eq 0 ]
