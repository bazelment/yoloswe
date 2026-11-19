package toolbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const ledger = `{"tasks":[
{"id":"unfunded","status":"planned","depends_on":["working"],"brief":"blocked mission"},
{"id":"working","status":"running","phase":"swe","session_id":"s1","round":1,"terminals":{"swe":{"1":"t1"}},"pr":7,"pr_head":"abc123","approval_sha":"abc123"},
{"id":"funded","status":"planned","phase":"swe","brief":"Implement the funded task"},
{"id":"nobrief","status":"planned"},
{"id":"missingfile","status":"planned","brief":"missing.brief"},
{"id":"held","status":"planned","brief":"work","hold":"owner"},
{"id":"done","status":"done","merge_sha":"abc"},
{"id":"answered","status":"running","waiting_on":"host","session_id":"s2","nebula_terminal":"t2"},
{"id":"stale","status":"running","session_id":"s3","nebula_terminal":"t3","pr":8,"pr_head":"new","approval_sha":"old"}]}`

// fixture returns a run directory and a stub of the subagent-swarm-nebula
// scripts that logs each skill call to the returned file.
func fixture(t *testing.T) (dir, skill, calls string) {
	t.Helper()
	dir, skill = t.TempDir(), t.TempDir()
	calls = filepath.Join(skill, "calls")
	stub := `log() { echo "$*" >> '` + calls + `'; echo "$*"; }
snl_spawn() { log spawn "$1"; }
snl_terminal_agent_state() { case $2 in t1) echo working ;; *) echo ended ;; esac; }
snl_nb() { log nb "$@"; }
snl_merge_claims() { log claims "$1" "$2"; }
snl_head_checks() { log head-checks "$1" "$2" "$3"; }
snl_task() { echo 7; }
snl_merge() { log merge "$1"; }
`
	for name, body := range map[string]string{"swarm_nebula_lanes.sh": stub, "lane_ops.sh": "", "step.sh": "", filepath.Join(dir, "state.json"): ledger} {
		if !filepath.IsAbs(name) {
			name = filepath.Join(skill, name)
		}
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, skill, calls
}

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
		{"lane", "start", "held"}, {"lane", "start", "working"}, {"lane", "steer", "done", "go"},
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
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || string(raw) != ledger || skillCalls(t, calls) != "" {
		t.Fatalf("shadow changed the live run: %v", err)
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
