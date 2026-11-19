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
// and t1's screen, and logs each terminal write.
const fakeNebula = `#!/bin/sh
case "$1 $2" in
"term list") case $3 in
  s1) echo '{"terminals":[{"id":"t1","status":"running","agentState":"working","backgroundTasks":{"shells":1}}]}' ;;
  s2) echo '{"terminals":[{"id":"t2","status":"running","agentState":"waiting"}]}' ;;
  s3) echo '{"terminals":[{"id":"t3","status":"running","agentState":"idle"}]}' ;;
  *) echo '{"terminals":[]}' ;;
  esac ;;
"term screen") [ "$3" = t1 ] && printf '> \n\n  1 shell still running\n' ;;
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
# snl_spawn takes the real _snl_spawn_locked arguments; a planned lane is
# funded through snl_fund_planned, which writes its swe brief first.
snl_spawn() { : "${1:?lane}" "${2:?phase}" "${3:?brief-file}"; [ -r "$3" ] || return 2; log spawn "$@"; }
snl_fund_planned() { log fund-planned "$@"; }
snl_merge_claims() { log claims "$1" "$2"; }
snl_head_checks() { log head-checks "$1" "$2" "$3"; }
snl_merge() { log merge "$1"; }
snl_lane_slug() { echo o/r; }
ghx() { log ghx "$@"; }
snl_event() { log event "$@"; }
`

// fixture returns a run directory whose env.sh exports RUN as that directory,
// as the host's does, and a stub of the subagent-swarm-nebula scripts that
// logs each skill write to the returned file.
func fixture(t *testing.T) (dir, skill, calls string) {
	t.Helper()
	dir, skill, repo := t.TempDir(), t.TempDir(), t.TempDir()
	// The ledger head abc123 is a branch at origin/main, so pr ready reads a fresh base.
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "root"},
		{"branch", "abc123"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
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

// workingCare is orca check's terminal care for t1: its state, background
// from its screen, and backgroundTasks as term list reports it.
const workingCare = "phase=swe agent=working signal= output_min_ago=unknown background=yes backgroundTasks={\"shells\":1} report_filed=no\n" +
	"last_turn_line=\"\"\nscreen_tail=[\"1 shell still running\"]\n"

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
		{"lane", "steer", "answered", "go"}, {"lane", "steer", "expired", "go"}, {"lane", "stop", "funded"},
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
	for _, args := range [][]string{{"lane", "steer", "working", "rebase"}, {"pr", "update", "7"}, {"pr", "merge", "7"}} {
		if _, err := run(t, dir, skill, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	got := skillCalls(t, calls)
	want := "^" + regexp.QuoteMeta("fund-planned funded\nnb term send t1 --file ") + `\S+\n` +
		regexp.QuoteMeta("ghx api -X PUT repos/o/r/pulls/7/update-branch -f expected_head_sha=abc123\nclaims working abc123\nhead-checks working 7 abc123\nmerge working\n") + "$"
	if !regexp.MustCompile(want).MatchString(got) {
		t.Fatalf("skill calls=%q", got)
	}
}

// .hold-advance passes a hand merge, but no other hold on the same lane does:
// a hold file or a set hold field refuses steer, ready, update and merge.
func TestHoldsRefuseUnderHoldAdvance(t *testing.T) {
	t.Parallel()
	for _, hold := range []string{"working.hold", "working.hold-merge", "working.hold-note", "working.park", "working.blocked",
		`"hold_merge":true`, `"blocked_on":"deploy"`, `"park":"owner"`} {
		t.Run(hold, func(t *testing.T) {
			t.Parallel()
			dir, skill, calls := fixture(t)
			files := map[string]string{"working.hold-advance": "run rule\n"}
			if strings.HasPrefix(hold, "working.") {
				files[hold] = "owner: hold the merge\n"
			} else {
				raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				files["state.json"] = strings.Replace(string(raw), `"id":"working",`, `"id":"working",`+hold+`,`, 1)
			}
			for name, body := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"lane", "steer", "working", "go"}, {"pr", "ready", "7"}, {"pr", "update", "7"}, {"pr", "merge", "7"}} {
				if _, err := run(t, dir, skill, args...); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "is held") {
					t.Errorf("%v: %v", args, err)
				}
			}
			if got := skillCalls(t, calls); got != "" {
				t.Fatalf("held actions reached the skill: %q", got)
			}
		})
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
	if want := "lane working status=running terminal=working remote=OK " + repo + "\nbusy=no\n" + workingCare; out != want {
		t.Fatalf("check=%q, want %q", out, want)
	}
}

// When config.repo is a bare repository, as a nebula-swarm host's .bare common
// dir is, skill calls run from its worktree, where lane_ops.sh's
// git rev-parse --show-toplevel resolves.
func TestCheckRunsInWorktreeOfBareRepo(t *testing.T) {
	t.Parallel()
	dir, skill, _ := fixture(t)
	src, root := t.TempDir(), t.TempDir()
	bare, wt := filepath.Join(root, ".bare"), filepath.Join(root, "main")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"clone", "-q", "--bare", src, bare},
		{"-C", bare, "worktree", "add", "-q", wt, "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		raw = regexp.MustCompile(`"repo":"[^"]*"`).ReplaceAll(raw, []byte(`"repo":"`+bare+`"`))
		err = os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, dir, skill, "check", "working")
	if err != nil {
		t.Fatal(err)
	}
	wt, err = filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	if want := "lane working status=running terminal=working remote=OK " + wt + "\nbusy=no\n" + workingCare; out != want {
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

// Without a host checkout in state.json config.repo, skill calls refuse with
// that reason instead of running from the run directory.
func TestSkillCallsRefuseWithoutHostCheckout(t *testing.T) {
	t.Parallel()
	dir, skill, calls := fixture(t)
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		raw = regexp.MustCompile(`"config":\{[^}]*\},`).ReplaceAll(raw, nil)
		err = os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"check", "working"}, {"lane", "steer", "working", "rebase"}} {
		if _, err := run(t, dir, skill, args...); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "config.repo") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if got := skillCalls(t, calls); got != "" {
		t.Fatalf("skill ran without a host checkout: %q", got)
	}
}

// runNoSkill runs the toolbox as an adopted dispatcher does: no --skill, no
// ORCA_SNL_DIR, and a HOME with no .claude/skills.
func runNoSkill(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(t.Context(), append(args, "--dir", dir), &out)
	return out.String(), err
}

// The run's env.sh SW is the skill runtime; the home folder is not needed.
// Not parallel: it sets HOME and ORCA_SNL_DIR.
func TestSkillScriptsFromRunEnvSW(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORCA_SNL_DIR", "")
	dir, skill, calls := fixture(t)
	f, err := os.OpenFile(filepath.Join(dir, "env.sh"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("export SW=" + shq(skill) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runNoSkill(t, dir, "note", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := runNoSkill(t, dir, "lane", "steer", "working", "rebase"); err != nil {
		t.Fatal(err)
	}
	if got := skillCalls(t, calls); !regexp.MustCompile(`^event run note hello\nnb term send t1 --file \S+\n$`).MatchString(got) {
		t.Fatalf("skill calls=%q", got)
	}
}

// With no SW in env.sh and no home skill folder, the refusal names both.
func TestSkillScriptsRefusalNamesPathsTried(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ORCA_SNL_DIR", "")
	dir, _, calls := fixture(t)
	_, err := runNoSkill(t, dir, "note", "hello")
	want := "orca toolbox refused: swarm skill scripts unavailable: " + filepath.Join(dir, "env.sh") +
		" exports no SW and no lane_ops.sh in " + filepath.Join(home, ".claude", "skills", "subagent-swarm-nebula", "scripts")
	if !errors.Is(err, ErrRefused) || err.Error() != want {
		t.Fatalf("err=%v\nwant %s", err, want)
	}
	if got := skillCalls(t, calls); got != "" {
		t.Fatalf("skill calls=%q", got)
	}
}

// lane hold writes the skill's .hold-advance file with its reason, under any
// other hold, once; a done lane is refused.
func TestLaneHoldWritesHoldAdvance(t *testing.T) {
	t.Parallel()
	dir, skill, _ := fixture(t)
	for _, want := range []string{"lane held holds advance\n", "lane held already holds advance\n"} {
		if out, err := run(t, dir, skill, "lane", "hold", "held", "--reason", "run rule: hold-advance on every lane"); err != nil || out != want {
			t.Fatalf("out=%q err=%v, want %q", out, err, want)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "held.hold-advance")); err != nil || string(raw) != "run rule: hold-advance on every lane\n" {
		t.Fatalf("hold file = %q, %v", raw, err)
	}
	if _, err := run(t, dir, skill, "lane", "hold", "done"); !errors.Is(err, ErrRefused) {
		t.Fatalf("hold on a done lane: %v", err)
	}
}
