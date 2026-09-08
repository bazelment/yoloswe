#!/bin/bash
# Tests for swarm_bramble.sh. The offline cases run against a fixture and need no TUI;
# sw_doctor's live assertions are skipped when no bramble is running.
set -u
SW="$(cd "$(dirname "$0")" && pwd)"
PASS=0; FAIL=0; SKIP=0
ok(){ PASS=$((PASS+1)); echo "  ok   $1"; }
no(){ FAIL=$((FAIL+1)); echo "  FAIL $1"; }
skip(){ SKIP=$((SKIP+1)); echo "  skip $1"; }
chk(){ [ "$2" = "$3" ] && ok "$1 ($2)" || no "$1: expected $3, got $2"; }
TMP=$(mktemp -d /tmp/swb-test-XXXXXX); trap 'rm -rf "$TMP"' EXIT
. "$SW/swarm_bramble.sh"

echo "== sw_socket refuses to guess between two TUIs =="
# Newest-wins would silently attach the swarm to the wrong TUI. Ambiguity must fail
# loudly: "select live evidence by identity, never by list position".
export XDG_RUNTIME_DIR="$TMP/xdg"; mkdir -p "$XDG_RUNTIME_DIR"
U=$(id -u)
python3 - "$XDG_RUNTIME_DIR" "$U" <<'PY'
import socket,sys,os
d,u=sys.argv[1],sys.argv[2]
for pid in ("111","222"):
    s=socket.socket(socket.AF_UNIX); s.bind(os.path.join(d,f"bramble-{u}-{pid}.sock"))
PY
OUT=$(sw_socket 2>&1); RC=$?
chk "two sockets -> non-zero" "$RC" "1"
if echo "$OUT" | grep -q "refusing to guess"; then ok "explains the ambiguity"
else no "unhelpful message: $OUT"; fi

echo "== a -control- socket is not mistaken for the TUI socket =="
rm -f "$XDG_RUNTIME_DIR"/bramble-"$U"-222.sock
python3 - "$XDG_RUNTIME_DIR" "$U" <<'PY'
import socket,sys,os
d,u=sys.argv[1],sys.argv[2]
s=socket.socket(socket.AF_UNIX); s.bind(os.path.join(d,f"bramble-control-{u}-111.sock"))
PY
OUT=$(sw_socket 2>&1); RC=$?
chk "control socket excluded, one match left" "$RC" "0"
case "$OUT" in *control*) no "picked the control socket" ;; *) ok "picked the TUI socket" ;; esac

echo "== no socket at all fails loudly =="
rm -f "$XDG_RUNTIME_DIR"/*.sock
sw_socket >/dev/null 2>&1
chk "no socket -> non-zero" "$?" "1"

echo "== sw_session_field parses the dict wrapper, not a bare list =="
cat > "$TMP/sessions.json" <<'JSON'
{"sessions":[
 {"id":"live-1","model":"opus","prompt":"p","status":"running","type":"builder",
  "worktree_name":"wt-a","tmux_target":"@10"},
 {"id":"dead-1","model":"opus","prompt":"p","status":"failed","type":"builder",
  "worktree_name":"wt-b"}
]}
JSON
sw_sessions(){ cat "$TMP/sessions.json"; }
chk "reads a field"           "$(sw_session_field live-1 tmux_target)" "@10"
chk "absent field is empty"   "$(sw_session_field dead-1 tmux_target)" ""
chk "unknown session is empty" "$(sw_session_field nope tmux_target)"  ""

echo "== liveness keys on tmux_target, not on status =="
# A session with no pane is GONE. That is decidable now, without waiting out a stall.
sw_session_live live-1 && ok "session with a pane is live" || no "live session called gone"
sw_session_live dead-1 && no "gone session called live" || ok "session with no pane is gone"

echo "== sw_nudge refuses a session whose window is gone =="
# Piping an empty tmux_target into capture-pane would error on exactly the session
# most in need of being reported as gone.
OUT=$(sw_nudge dead-1 "hello" 2>&1); RC=$?
chk "nudging a gone session -> non-zero" "$RC" "1"
if echo "$OUT" | grep -q "window is gone"; then ok "says why"
else no "unhelpful message: $OUT"; fi

echo "== live capability self-test =="
# Re-source to drop the sw_sessions stub above: `unset -f` on a REPLACED function
# leaves no original behind, so the stub would otherwise leak into the live check
# and make it look like bramble had drifted.
unset XDG_RUNTIME_DIR
. "$SW/swarm_bramble.sh"
if command -v bramble >/dev/null && sw_socket >/dev/null 2>&1; then
  if sw_doctor >/dev/null 2>&1; then ok "sw_doctor passes against the live TUI"
  else no "sw_doctor failed -- bramble's surface drifted; run it directly"; fi
else
  skip "sw_doctor (no bramble socket; offline checks above still ran)"
fi

echo
echo "passed $PASS, failed $FAIL, skipped $SKIP"
[ "$FAIL" -eq 0 ]
