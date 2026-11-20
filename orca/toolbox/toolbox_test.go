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
	"strconv"
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
snl_lane_gh() { [ "$2" = pr ] && [ "$3" = view ] && echo abc123; }
snl_report_word() { sed -n "s/^$3:[[:space:]]*//p" "$RUN/$1.$2.md" | head -1; }
gh() {
  case "$1 $2" in
  "run list") echo 42 ;;
  "run rerun") echo "gh-run-rerun $3 $4" >> "$CALLS" ;;
  esac
  return 0
}
# snl_spawn takes the real _snl_spawn_locked arguments; a planned lane is
# funded through snl_fund_planned, which writes its swe brief first.
snl_spawn() { : "${1:?lane}" "${2:?phase}" "${3:?brief-file}"; [ -r "$3" ] || return 2; log spawn "$@"; }
snl_brief() { log brief "$@"; }
snl_to_review() { log to-review "$@"; }
snl_to_clean() { log to-clean "$@"; }
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
		filepath.Join(skill, "ledger.py"):                 "import sys\n",
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

func TestActionReceiptArgumentsMustMatchExactly(t *testing.T) {
	t.Parallel()
	dir, skill, _ := fixture(t)
	if _, err := run(t, dir, skill, "note", "long message", "--action-key", "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, dir, skill, "note", "long", "--action-key", "same"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "different arguments") {
		t.Fatalf("reused key with changed text: %v", err)
	}
	for range 2 {
		if out, err := run(t, dir, skill, "note", "line one\nline two", "--action-key", "multiline"); err != nil || (out != "event run note line one\nline two\n" && out != "already recorded\n") {
			t.Fatalf("multiline receipt out=%q err=%v", out, err)
		}
	}
}

func TestPRRerunIsExactHeadOnce(t *testing.T) {
	t.Parallel()
	dir, skill, _ := byHandSkill(t)
	writeRun(t, dir, "working.hold-advance", "run rule\n")
	writeRun(t, dir, "working.review.md", "Verdict: pass\nHEAD: abc123\n")
	if out, err := run(t, dir, skill, "pr", "rerun", "7", "--action-key", "rerun-1"); err != nil || out != "PR #7 rerun requested at abc123\n" {
		t.Fatalf("first rerun out=%q err=%v", out, err)
	}
	if _, err := run(t, dir, skill, "pr", "approve", "7", "--action-key", "approve"); err != nil {
		t.Fatalf("approve after green rerun: %v", err)
	}
	if _, err := run(t, dir, skill, "pr", "ready", "7", "--action-key", "ready"); err != nil {
		t.Fatalf("ready after green rerun: %v", err)
	}
	if _, err := run(t, dir, skill, "pr", "merge", "7", "--action-key", "merge"); err != nil {
		t.Fatalf("merge after green rerun: %v", err)
	}
	secondDir, secondSkill, secondCalls := fixture(t)
	if _, err := run(t, secondDir, secondSkill, "pr", "rerun", "7", "--action-key", "rerun-1"); err != nil {
		t.Fatalf("second fixture first rerun: %v", err)
	}
	if _, err := run(t, secondDir, secondSkill, "pr", "rerun", "7", "--action-key", "rerun-2"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("second rerun err=%v", err)
	}
	if got := skillCalls(t, secondCalls); got != "gh-run-rerun 42 --failed\n" {
		t.Fatalf("rerun calls=%q", got)
	}
}

func TestPRRerunPreflightRefusalCanBeRetried(t *testing.T) {
	t.Parallel()
	dir, skill, _ := byHandSkill(t)
	writeRun(t, dir, "working.hold-advance", "run rule\n")
	writeRun(t, dir, "working.review.md", "Verdict: pass\nHEAD: abc123\n")
	writeRun(t, skill, "step.sh", "gh() { return 0; }\n")
	for _, key := range []string{"rerun-1", "rerun-2"} {
		if _, err := run(t, dir, skill, "pr", "rerun", "7", "--action-key", key); err == nil || !strings.Contains(err.Error(), "no failed exact-head runs") {
			t.Fatalf("rerun %s: %v", key, err)
		}
	}
}

func TestNeedsSWEWithoutBreaksUsesApprovalPath(t *testing.T) {
	t.Parallel()
	dir, skill, calls := byHandSkill(t)
	writeRun(t, dir, "working.hold-advance", "run rule\n")
	writeRun(t, dir, "working.review.md", "Verdict: needs-swe\nBreaks:\nHEAD: abc123\n")
	if out, err := run(t, dir, skill, "pr", "approve", "7", "--action-key", "approve"); err != nil || !strings.Contains(out, "approved at abc123") {
		t.Fatalf("needs-swe approval out=%q err=%v", out, err)
	}
	if _, err := run(t, dir, skill, "pr", "ready", "7", "--action-key", "ready"); err != nil {
		t.Fatalf("needs-swe ready: %v", err)
	}
	if _, err := run(t, dir, skill, "pr", "merge", "7", "--action-key", "merge"); err != nil {
		t.Fatalf("needs-swe merge: %v", err)
	}
	if got := skillCalls(t, calls); !strings.Contains(got, "head-checks working 7 abc123") {
		t.Fatalf("needs-swe did not verify CI: %q", got)
	}
	namedDir, namedSkill, _ := byHandSkill(t)
	writeRun(t, namedDir, "working.hold-advance", "run rule\n")
	writeRun(t, namedDir, "working.review.md", "Verdict: needs-swe\nBreaks: named defect\nHEAD: abc123\n")
	if _, err := run(t, namedDir, namedSkill, "pr", "approve", "7", "--action-key", "approve-2"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "names Breaks") {
		t.Fatalf("named Breaks err=%v", err)
	}
}

func TestNeedsSWEMustStillMatchHeadAndGreenCI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, review, override, want string
	}{
		{"report head differs", "Verdict: needs-swe\nBreaks:\nHEAD: stale\n", "", "review HEAD stale is not PR head abc123"},
		{"exact head CI is red", "Verdict: needs-swe\nBreaks:\nHEAD: abc123\n", "snl_head_checks() { return 3; }\n", "no review PASS at its head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, skill, _ := byHandSkill(t)
			writeRun(t, dir, "working.hold-advance", "run rule\n")
			writeRun(t, dir, "working.review.md", tc.review)
			writeRun(t, skill, "step.sh", tc.override)
			if _, err := run(t, dir, skill, "pr", "approve", "7", "--action-key", "approve"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approval err=%v", err)
			}
		})
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
	if _, err := run(t, dir, skill, "note", "shadow note", "--shadow", "--intent-dir", intent); err != nil {
		t.Fatal(err)
	}
	status, err := run(t, dir, skill, "status", "--shadow", "--intent-dir", intent)
	if err != nil || strings.Contains(status, `"ID":"run"`) {
		t.Fatalf("shadow note created a phantom lane: %q %v", status, err)
	}
}

func TestShadowApprovalOverlayFeedsReady(t *testing.T) {
	t.Parallel()
	dir, skill, _ := fixture(t)
	writeRun(t, dir, "working.hold-advance", "run rule\n")
	writeRun(t, dir, "working.review.md", "Verdict: pass\nHEAD: abc123\n")
	// The copied ledger is stale: approval must project the reviewed head before
	// the following ready read performs its exact-head check.
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"pr_head":"abc123","approval_sha":"abc123"`), []byte(`"pr_head":"old","approval_sha":"old"`), 1)
	writeRun(t, dir, "state.json", string(raw))
	intent := t.TempDir()
	if _, err := run(t, dir, skill, "pr", "approve", "7", "--shadow", "--intent-dir", intent); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out, err := run(t, dir, skill, "pr", "ready", "7", "--shadow", "--intent-dir", intent); err != nil || !strings.Contains(out, "PR #7 ready at abc123") {
		t.Fatalf("ready out=%q err=%v", out, err)
	}
}

func TestShadowFundingOverlayAddsStatusRow(t *testing.T) {
	t.Parallel()
	dir, skill, _ := fixture(t)
	writeRun(t, dir, "new.md", "Cause check: verified\nWork\n")
	intent := t.TempDir()
	if _, err := run(t, dir, skill, "lane", "start", "new", "--phase", "swe", "--brief", filepath.Join(dir, "new.md"), "--shadow", "--intent-dir", intent); err != nil {
		t.Fatalf("fund: %v", err)
	}
	out, err := run(t, dir, skill, "status", "--shadow", "--intent-dir", intent)
	if err != nil || !strings.Contains(out, `"ID":"new"`) || !strings.Contains(out, `"Status":"running"`) {
		t.Fatalf("status out=%q err=%v", out, err)
	}
}

func TestFundAndAdvanceByHand(t *testing.T) {
	t.Parallel()
	dir, skill, calls := fixture(t)
	brief := filepath.Join(dir, "new.md")
	writeRun(t, dir, "new.md", "Cause check: synthetic dependency verified\nWork\n")
	if _, err := run(t, dir, skill, "lane", "start", "new", "--phase", "swe", "--brief", brief); err != nil {
		t.Fatalf("fund: %v", err)
	}
	if !strings.Contains(skillCalls(t, calls), "spawn new swe "+brief) {
		t.Fatalf("fund did not use skill spawn: %q", skillCalls(t, calls))
	}
}

func TestHandAdvance(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		round          int
	}{
		{"a", "clean", "review", 1},
		{"b", "review", "swe", 1},
		{"c", "swe", "clean", 2},
		{"d", "review", "clean", 1},
		{"swe-review", "swe", "review", 1},
		{"rework-round-3", "review", "swe", 2},
		{"rework-default-round", "review", "swe", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modes := []string{"live", "hold", "no-advance-hold", "no-report", "shadow", "no-helper"}
			if tc.to == "swe" {
				modes = append(modes, "no-brief", "no-breaks", "done-report")
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					t.Parallel()
					dir, skill, calls := fixture(t)
					raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
					if err != nil {
						t.Fatal(err)
					}
					raw = bytes.Replace(raw, []byte(`"phase":"swe","round":1`), []byte(`"phase":"`+tc.from+`","round":`+strconv.Itoa(tc.round)), 1)
					writeRun(t, dir, "state.json", string(raw))
					if mode != "no-advance-hold" {
						writeRun(t, dir, "working.hold-advance", "run rule\n")
					}
					if mode == "hold" {
						writeRun(t, dir, "working.hold", "owner hold\n")
					}
					report := "Outcome: done\n"
					if tc.to == "swe" && mode != "done-report" {
						report = "Outcome: needs-swe\n"
						if mode != "no-breaks" {
							report += "Breaks: named defect\n"
						}
					}
					if mode != "no-report" {
						writeRun(t, dir, "working."+tc.from+".md", report)
					}
					writeRun(t, dir, "rework.md", "fix the named defect\n")
					if mode == "no-helper" {
						writeRun(t, skill, "swarm_nebula_lanes.sh", skillStub+"\nunset -f snl_brief snl_to_review snl_to_clean\n")
					}
					args := []string{"lane", "start", "working", "--phase", tc.to}
					if tc.to == "swe" && mode != "no-brief" {
						args = append(args, "--brief", "rework.md")
					}
					if mode == "shadow" {
						args = append(args, "--shadow", "--intent-dir", t.TempDir())
					}
					_, err = run(t, dir, skill, args...)
					refused := tc.name == "d" || mode != "live" && mode != "shadow"
					if refused {
						if err == nil || mode != "no-helper" && !errors.Is(err, ErrRefused) {
							t.Fatalf("expected refusal: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					got := skillCalls(t, calls)
					if refused || mode == "shadow" {
						if got != "" {
							t.Fatalf("unexpected skill write: %q", got)
						}
					} else {
						want := "to-" + tc.to + " working\n"
						if tc.to == "swe" {
							mission := filepath.Join(dir, "rework.md") + " " + strconv.Itoa(max(tc.round, 1)+1) + "\n"
							want = "brief swe working " + mission + "spawn working swe " + mission
						}
						if got != want {
							t.Fatalf("skill calls = %q, want %q", got, want)
						}
					}
				})
			}
		})
	}
}

func TestAdvanceNeedsCurrentRoundDoneReport(t *testing.T) {
	t.Parallel()
	dir, skill, _ := fixture(t)
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"id":"working","status":"running","phase":"swe","round":1`), []byte(`"id":"working","status":"running","phase":"swe","round":2`), 1)
	writeRun(t, dir, "state.json", string(raw))
	writeRun(t, dir, "working.hold-advance", "run rule\n")
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRun(t, dir, "reports/working.swe.r1.md", "Outcome: done\n")
	writeRun(t, dir, "working.swe.md", "The text says Outcome: done but is not a field\n")
	if _, err := run(t, dir, skill, "lane", "start", "working", "--phase", "review"); !errors.Is(err, ErrRefused) {
		t.Fatalf("stale report allowed advance: %v", err)
	}
	writeRun(t, dir, "reports/working.swe.r2.md", "The text says Outcome: done but is not a field\n")
	if _, err := run(t, dir, skill, "lane", "start", "working", "--phase", "review"); !errors.Is(err, ErrRefused) {
		t.Fatalf("malformed report allowed advance: %v", err)
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

// realSnlMerge is the swarm skill's snl_merge, verbatim from
// sycamore-labs/nebula@f623ed57 .claude/skills/subagent-swarm-nebula/scripts/lane_ops.sh.
const realSnlMerge = `
snl_merge() {
  [[ ${SWARM_HOST_GATE_MODE:-required} == required || ${SWARM_HOST_GATE_MODE:-required} == optional ]] || return 1
  local lane="$1" g head pr m sha merr q
  snl_lane_own "$lane" || { echo "$lane: $(_snl_foreign_why "$lane")" >&2; return 1; }
  if _snl_host_gate_optional "$lane"; then
    head=$(_snl_reviewed_head "$lane") || return $?
  else
    g="$(cat "$RUN/$lane.gate" 2>/dev/null)"
    [ "${g%% *}" = PASS ] || { echo "$lane: gate is '$g'" >&2; return 1; }
    head="${g#PASS }"; head="${head%% *}"
  fi
  pr="$(snl_task "$lane" pr)"
  snl_merge_claims "$lane" "$head" || { echo "$lane: #$pr not merged: a status claim is not backed (above)" >&2; return 1; }
  snl_head_checks "$lane" "$pr" "$head" || return $?
  snl_merge_queue "$lane"; q=$?
  [ "$q" != 76 ] || { echo "$lane: #$pr merge not confirmed: $(ghx_limit_active)" >&2; return 76; }
  [ "$q" != 2 ] || { echo "$lane: #$pr not merged: cannot read the merge queue rules of the base branch; run again" >&2; return 75; }
  merr=""
  if [ "$q" = 0 ]; then
    snl_enqueue "$lane" "$pr" "$head" || return $?
  else
    # The queue probe can observe a changing base or check status. Re-run the
    # guard immediately before the direct merge so the mutation is covered by
    # the same exact-head and fresh-base evidence.
    snl_head_checks "$lane" "$pr" "$head" || return $?
    snl_require_ready "$lane" "$pr" || return $?
    merr="$(snl_lane_gh "$lane" pr merge "$pr" --squash --match-head-commit "$head" 2>&1 >/dev/null)"
  fi
  m="$(snl_lane_gh "$lane" pr view "$pr" --json state,mergeCommit --jq '.state+" "+.mergeCommit.oid')"; sha="${m#MERGED }"
  # A limit at the merge or at the read-back is not "not merged": the merge may have landed.
  ! ghx_limit_active >/dev/null || { echo "$lane: #$pr merge not confirmed: $(ghx_limit_active)" >&2; return 76; }
  [ "${m%% *}" = MERGED ] && git fetch -q origin "${BASE:-main}" &&
    git merge-base --is-ancestor "$sha" "origin/${BASE:-main}" ||
    { echo "$lane: #$pr not merged ($m)${merr:+: $merr}" >&2; return 1; }
  # A merge is terminal whatever the phase: a gate repointed at a lane on an earlier phase
  # merges it too, and stepping that phase would leave the merged lane running.
  python3 "$SW/ledger.py" set "$RUN" --id "$lane" --status done --merge-sha "$sha" --pr-head "$head" --approval-sha "$head" --note "merged #$pr" >/dev/null
  snl_reap "$lane" --force
  # Drop stale watcher notices after merging.
  snl_retire_notices "$lane"
  snl_describe
  echo "$lane: merged #$pr -> $sha"
}
`

// byHandSkill is the stub skill with the real snl_merge, a pr_merge.sh that
// logs its call and reports a merge, and a ledger.py that sets row fields.
func byHandSkill(t *testing.T) (dir, skill, calls string) {
	t.Helper()
	dir, skill, calls = fixture(t)
	files := map[string]string{
		"lane_ops.sh": "snl_lane_own() { :; }\n_snl_host_gate_optional() { return 1; }\n" + realSnlMerge,
		"pr_merge.sh": `log() { echo "$*" >> "$CALLS"; }; log pr_merge "$@"; for pr; do :; done; echo "merged $pr feed1234"` + "\n",
		"ledger.py": `import json,sys
a=sys.argv[1:]; kv=dict(zip(a[2::2],a[3::2])); p=a[1]+"/state.json"; s=json.load(open(p))
for t in s["tasks"]:
    if a[0]=="set" and t["id"]==kv["--id"]: t.update({k[2:].replace("-","_"):v for k,v in kv.items() if k!="--id"})
json.dump(s,open(p,"w"))
`,
	}
	for name, body := range files {
		f, err := os.OpenFile(filepath.Join(skill, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o700)
		if err == nil {
			_, err = f.WriteString(body)
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir, skill, calls
}

func ledgerRow(t *testing.T, dir, id string) map[string]any {
	t.Helper()
	var s struct{ Tasks []map[string]any }
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		err = json.Unmarshal(raw, &s)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range s.Tasks {
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("no ledger row %s", id)
	return nil
}

func writeRun(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A held lane has no gate, so snl_merge refuses it: pr merge merges the
// approved head by hand through pr_merge.sh and records the merge; shadow
// records intent only.
func TestHeldLaneMergesByHand(t *testing.T) {
	t.Parallel()
	dir, skill, calls := byHandSkill(t)
	writeRun(t, dir, "working.hold-advance", "run rule\n")
	writeRun(t, dir, "working.review.md", "Verdict: pass\nHEAD: abc123\n")
	intent := t.TempDir()
	if out, err := run(t, dir, skill, "pr", "merge", "7", "--shadow", "--intent-dir", intent); err != nil || out != "shadow: recorded\n" {
		t.Fatalf("shadow out=%q err=%v", out, err)
	}
	if got := skillCalls(t, calls); strings.Contains(got, "pr_merge") || ledgerRow(t, dir, "working")["merge_sha"] != nil {
		t.Fatalf("shadow merged: calls=%q", got)
	}
	if actions, err := os.ReadFile(filepath.Join(intent, "actions.jsonl")); err != nil || !strings.Contains(string(actions), `"action":"pr.merge"`) {
		t.Fatalf("intent: %s %v", actions, err)
	}
	out, err := run(t, dir, skill, "pr", "merge", "7")
	if err != nil || out != "working: merged #7 -> feed1234\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	want := "claims working abc123\nhead-checks working 7 abc123\n" +
		"claims working abc123\nhead-checks working 7 abc123\n" +
		"pr_merge --repo o/r --head abc123 --report " + filepath.Join(dir, "working.review.md") + " 7\n"
	if got := skillCalls(t, calls); got != want {
		t.Fatalf("skill calls=%q, want %q", got, want)
	}
	row := ledgerRow(t, dir, "working")
	if row["status"] != "done" || row["merge_sha"] != "feed1234" || row["pr_head"] != "abc123" {
		t.Fatalf("ledger row=%v", row)
	}
}

// Without an approval at the PR head, a held lane's merge is refused before
// any merge runs.
func TestHeldLaneMergeNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, skill, calls := byHandSkill(t)
	writeRun(t, dir, "working.hold-advance", "run rule\n")
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeRun(t, dir, "state.json", strings.Replace(string(raw), `"approval_sha":"abc123"`, `"approval_sha":""`, 1))
	if _, err := run(t, dir, skill, "pr", "merge", "7"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("merge without approval: %v", err)
	}
	if got := skillCalls(t, calls); got != "" {
		t.Fatalf("unapproved merge reached the skill: %q", got)
	}
}

// A lane that is not held merges through snl_merge, which still requires
// its gate: with an empty one it refuses, and pr_merge.sh is never called.
// (With no gate file, skillOps' set -e exits at snl_merge's cat, silently.)
func TestUnheldLaneMergesThroughSnlMerge(t *testing.T) {
	t.Parallel()
	dir, skill, calls := byHandSkill(t)
	writeRun(t, dir, "working.gate", "")
	if _, err := run(t, dir, skill, "pr", "merge", "7"); err == nil || !strings.Contains(err.Error(), "working: gate is ''") {
		t.Fatalf("merge without a gate: %v", err)
	}
	if got := skillCalls(t, calls); strings.Contains(got, "pr_merge") {
		t.Fatalf("unheld lane merged by hand: %q", got)
	}
}
