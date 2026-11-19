package toolbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ledger uses the nebula-swarm lane shape: nebula_session, nebula_terminal
// and a sessions map keyed by phase and round. REPO is the host checkout.
const ledger = `{"config":{"repo":"REPO/.git","base":"main"},"tasks":[
{"id":"unfunded","status":"planned","depends_on":["working"],"brief":"blocked mission","sessions":{},"nebula_session":"","nebula_terminal":""},
{"id":"working","status":"running","phase":"swe","round":1,"sessions":{"swe":"nebula:t1"},"nebula_session":"s1","nebula_terminal":"t1","pr":7,"pr_head":"abc123","approval_sha":"abc123"},
{"id":"funded","status":"planned","phase":"","brief":"Implement the funded task","sessions":{},"nebula_session":"","nebula_terminal":""},
{"id":"nobrief","status":"planned"},
{"id":"missingfile","status":"planned","brief":"missing.brief"},
{"id":"held","status":"planned","brief":"work","hold":"owner"},
{"id":"done","status":"done","merge_sha":"abc"},
{"id":"answered","status":"running","phase":"swe","waiting_on":"host","sessions":{"swe":"nebula:t2"},"nebula_session":"s2","nebula_terminal":"t2"},
{"id":"stale","status":"running","phase":"review","round":3,"sessions":{"swe":"nebula:t3a","review3":"nebula:t3"},"nebula_session":"s3","nebula_terminal":"t3","pr":8,"pr_head":"new","approval_sha":"old"},
{"id":"expired","status":"running","phase":"review","round":3,"sessions":{"swe":"nebula:t4a","swe2":"nebula:t4b","review3":"nebula:t4"},"nebula_session":"s4","nebula_terminal":"t4"}]}`

// fakeNebula answers term list per session (t1, t2 alive; t3 idle; t4 gone)
// and logs each terminal write.
const fakeNebula = `#!/bin/sh
case "$1 $2" in
"term list") case $3 in
  s1) echo '{"terminals":[{"id":"t1","status":"running","agentState":"working"}]}' ;;
  s2) echo '{"terminals":[{"id":"t2","status":"running","agentState":"waiting"}]}' ;;
  s3) echo '{"terminals":[{"id":"t3","status":"running","agentState":"idle"}]}' ;;
  *) echo '{"terminals":[]}' ;;
  esac ;;
*) echo "nb $*" >> "$CALLS" ;;
esac
`

// skillStub stands in for swarm_nebula_lanes.sh: snl_task and
// snl_terminal_agent_state are the skill's own; writes are logged.
const skillStub = `log() { echo "$*" >> "$CALLS"; echo "$*"; }
snl_nb() { "$SNL_NEBULA" "$@"; }
snl_task() { /usr/bin/env python3 -c '
import json,sys
for t in json.load(open(sys.argv[1]+"/state.json"))["tasks"]:
    if t["id"]==sys.argv[2]: print(t.get(sys.argv[3],"")); break' "$RUN" "$1" "$2"; }
snl_terminal_agent_state() {
  snl_nb term list "$1" | /usr/bin/env python3 -c '
import json,sys
for t in json.load(sys.stdin).get("terminals", []):
    if t.get("id")==sys.argv[1]: print(t.get("agentState") or ""); break' "$2"
}
snl_lane_remote_head() { : > "$RUN/.snl-lsr.probe"; echo "OK $(git rev-parse --show-toplevel)"; }
snl_sandbox_busy() { false; }
snl_spawn() { log spawn "$@"; }
snl_merge_claims() { log claims "$1" "$2"; }
snl_head_checks() { log head-checks "$1" "$2" "$3"; }
snl_merge() { log merge "$1"; }
`

// fixture returns a run directory whose env.sh exports RUN as that directory,
// as the host's does, and a stub of the subagent-swarm-nebula scripts that
// logs each skill write to the returned file.
func fixture(t *testing.T) (dir, skill, calls string) {
	t.Helper()
	dir, skill, repo := t.TempDir(), t.TempDir(), t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	calls = filepath.Join(skill, "calls")
	env := "export RUN=" + shq(dir) + "\nexport SNL_NEBULA=" + shq(filepath.Join(skill, "nebula")) + "\nexport CALLS=" + shq(calls) + "\n"
	files := map[string]string{
		filepath.Join(skill, "swarm_nebula_lanes.sh"): skillStub,
		// The skill's lane_ops.sh finds its repo config from the cwd checkout.
		filepath.Join(skill, "lane_ops.sh"):               `SNL_REPO_CONFIG="${SNL_REPO_CONFIG:-$(git rev-parse --show-toplevel)/.claude/swarm/repo-config.sh}"` + "\n",
		filepath.Join(skill, "step.sh"):                   "",
		filepath.Join(skill, "nebula"):                    fakeNebula,
		filepath.Join(dir, "state.json"):                  strings.ReplaceAll(ledger, "REPO", repo),
		filepath.Join(dir, "env.sh"):                      env,
		filepath.Join(dir, "briefs", "expired.review.md"): "Review PR #9.\n",
		filepath.Join(dir, "stale.review.brief.txt"):      "Review PR #8.\n",
	}
	for name, body := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir, skill, calls
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func run(t *testing.T, dir, skill string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(t.Context(), append(args, "--dir", dir, "--skill", skill), &out)
	return out.String(), err
}

func skillCalls(t *testing.T, calls string) string {
	t.Helper()
	raw, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRefusalsNeverReachTheSkill(t *testing.T) {
	t.Parallel()
	dir, skill, calls := fixture(t)
	for _, args := range [][]string{
		{"lane", "start", "unfunded"}, {"lane", "start", "nobrief"}, {"lane", "start", "missingfile"},
		{"lane", "start", "held"}, {"lane", "start", "working"}, {"lane", "start", "answered"},
		{"lane", "start", "done"}, {"lane", "steer", "done", "go"},
		{"lane", "steer", "answered", "go"}, {"lane", "steer", "stale", "go"}, {"lane", "stop", "funded"},
		{"pr", "merge", "8"},
	} {
		_, err := run(t, dir, skill, args...)
		t.Logf("%v: %v", args, err)
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if got := skillCalls(t, calls); got != "" {
		t.Fatalf("refused actions reached the skill: %q", got)
	}
}

func TestActionsRunSkillOncePerKey(t *testing.T) {
	t.Parallel()
	dir, skill, calls := fixture(t)
	for range 2 {
		if _, err := run(t, dir, skill, "lane", "start", "funded", "--action-key", "k1"); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"lane", "steer", "working", "rebase"}, {"pr", "merge", "7"}} {
		if _, err := run(t, dir, skill, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	got := skillCalls(t, calls)
	want := "^" + regexp.QuoteMeta("spawn funded\nnb term send t1 --file ") + `\S+\n` + regexp.QuoteMeta("claims working abc123\nhead-checks working 7 abc123\nmerge working\n") + "$"
	if !regexp.MustCompile(want).MatchString(got) {
		t.Fatalf("skill calls=%q", got)
	}
}

func TestShadowNoLiveWrites(t *testing.T) {
	t.Parallel()
	dir, skill, calls := fixture(t)
	intent := t.TempDir()
	out, err := run(t, dir, skill, "lane", "start", "funded", "--shadow", "--intent-dir", intent)
	if err != nil || out != "shadow: recorded\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if skillCalls(t, calls) != "" {
		t.Fatal("shadow reached the skill's writes")
	}
	if _, err := os.Stat(filepath.Join(dir, ".orca-actions")); !os.IsNotExist(err) {
		t.Fatalf("shadow wrote receipts into the run: %v", err)
	}
	actions, err := os.ReadFile(filepath.Join(intent, "actions.jsonl"))
	if err != nil || !strings.Contains(string(actions), `"action":"lane.start"`) {
		t.Fatalf("intent: %s %v", actions, err)
	}
}

func TestWaitEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"EVENTS", "funded.swe.done"} {
		dir, _, _ := fixture(t)
		before, err := Snapshot(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("new synthetic evidence\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, changed, err := WaitSince(ctx, dir, before)
		cancel()
		if err != nil || len(changed) != 1 || changed[0] != name {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
	}
}

// steer and stop find the lane's current terminal from nebula_session and
// nebula_terminal, as the skill's helpers do.
func TestSteerReachesCurrentTerminal(t *testing.T) {
	t.Parallel()
	dir, skill, calls := fixture(t)
	if _, err := run(t, dir, skill, "lane", "steer", "working", "rebase"); err != nil {
		t.Fatal(err)
	}
	if got := skillCalls(t, calls); !regexp.MustCompile(`^nb term send t1 --file \S+\n$`).MatchString(got) {
		t.Fatalf("skill calls=%q", got)
	}
}

// orca check runs the skill's git helpers in the host's checkout, which the
// ledger names in config.repo, not in the run directory.
func TestCheckRunsInHostCheckout(t *testing.T) {
	t.Parallel()
	dir, skill, _ := fixture(t)
	out, err := run(t, dir, skill, "check", "working")
	if err != nil {
		t.Fatal(err)
	}
	var state struct{ Config struct{ Repo string } }
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		err = json.Unmarshal(raw, &state)
	}
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.EvalSymlinks(filepath.Dir(state.Config.Repo))
	if err != nil {
		t.Fatal(err)
	}
	if want := "lane working status=running terminal=working remote=OK " + repo + "\nbusy=no\n"; out != want {
		t.Fatalf("check=%q, want %q", out, want)
	}
}

// In shadow, the copy's env.sh names the live run as RUN; skill calls still
// read and write only in the copy.
func TestShadowSkillCallsStayInCopy(t *testing.T) {
	t.Parallel()
	live, skill, _ := fixture(t)
	copyDir := t.TempDir()
	for _, name := range []string{"state.json", "env.sh"} {
		raw, err := os.ReadFile(filepath.Join(live, name))
		if err == nil {
			err = os.WriteFile(filepath.Join(copyDir, name), raw, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadDir(live)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, copyDir, skill, "check", "working", "--shadow", "--intent-dir", t.TempDir()); err != nil {
		t.Fatalf("check=%q %v", out, err)
	}
	after, err := os.ReadDir(live)
	if err != nil || len(after) != len(before) {
		t.Fatalf("shadow check wrote into the live run: %v %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(copyDir, ".snl-lsr.probe")); err != nil {
		t.Fatalf("check did not run in the copy: %v", err)
	}
}

// lane start gives a running lane whose current terminal is gone or idle a
// fresh terminal for its current phase through the skill's spawn, with the
// phase's stored brief; a lane whose terminal is alive is refused.
func TestLaneStartRestartsDeadPhaseTerminal(t *testing.T) {
	t.Parallel()
	dir, skill, calls := fixture(t)
	for _, lane := range []string{"expired", "stale"} {
		if _, err := run(t, dir, skill, "lane", "start", lane); err != nil {
			t.Fatalf("%s: %v", lane, err)
		}
	}
	if _, err := run(t, dir, skill, "lane", "start", "working"); !errors.Is(err, ErrRefused) {
		t.Fatalf("start on an alive terminal: %v", err)
	}
	want := "spawn expired review " + filepath.Join(dir, "briefs", "expired.review.md") + "\n" +
		"spawn stale review " + filepath.Join(dir, "stale.review.brief.txt") + "\n"
	if skillCalls(t, calls) != want {
		t.Fatalf("skill calls=%q, want %q", skillCalls(t, calls), want)
	}
}
