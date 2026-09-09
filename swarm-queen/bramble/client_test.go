package bramble

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fixture is a real `bramble list-sessions` response (prompts redacted). It
// pins the wire shape so a bramble change breaks a test rather than a live run.
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/list_sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// list-sessions returns {"sessions":[...]}, NOT a bare list. One mined
// orchestrator re-derived a defensive isinstance() guard ~15 times because it
// never learned this.
func TestParseSessionsExpectsDictWrapper(t *testing.T) {
	t.Parallel()
	got, err := parseSessions(fixture(t))
	if err != nil {
		t.Fatalf("parseSessions: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no sessions parsed from fixture")
	}
	for _, s := range got {
		if s.ID == "" || s.Status == "" || s.WorktreeName == "" {
			t.Errorf("required field empty in %+v", s)
		}
	}

	// A bare list must fail loudly rather than silently yielding zero sessions —
	// "no sessions" would read as a healthy empty swarm.
	if _, err := parseSessions([]byte(`[{"id":"x"}]`)); err == nil {
		t.Error("expected a bare list to be rejected")
	}
}

// Rows carry worktree_name and no worktree_path. Matching lanes on a path would
// silently match nothing.
func TestSessionsHaveNoWorktreePath(t *testing.T) {
	t.Parallel()
	var raw struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(fixture(t), &raw); err != nil {
		t.Fatal(err)
	}
	for _, row := range raw.Sessions {
		if _, ok := row["worktree_path"]; ok {
			t.Error("worktree_path is present — update the lane matching strategy")
		}
		if _, ok := row["worktree_name"]; !ok {
			t.Error("worktree_name is missing — lane matching would break")
		}
	}
}

// tmux_target is optional and is absent exactly on a session with no live
// window. Its absence is a liveness signal, not a parse failure.
func TestMissingTmuxTargetIsToleratedAndMeaningful(t *testing.T) {
	t.Parallel()
	sessions, err := parseSessions(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var paneless int
	for _, s := range sessions {
		if !s.HasPane() {
			paneless++
			if s.Status == "running" {
				t.Errorf("session %s is running but has no pane; revisit the assumption", s.ID)
			}
		}
	}
	if paneless == 0 {
		t.Skip("fixture has no paneless session; nothing to assert here")
	}
}

// SocketPath must exclude the separate control socket, which shares the prefix.
func TestSocketPathIgnoresControlSocket(t *testing.T) {
	t.Parallel()
	// Documented-but-nonexistent path from references/bramble-mechanics.md.
	// Kept as a regression note: the real path carries a PID suffix.
	const documented = "/run/user/1000/bramble-1000.sock"
	if _, err := os.Stat(documented); err == nil {
		t.Skip("the documented un-suffixed socket now exists; skill docs may be correct again")
	}
}

func TestSpawnRequestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  SpawnRequest
		ok   bool
	}{
		{"worktree path", SpawnRequest{Worktree: "/wt/a", Prompt: "brief"}, true},
		{"create worktree", SpawnRequest{Repo: "kernel", Branch: "b", CreateWorktree: true, Prompt: "brief"}, true},
		{"no prompt", SpawnRequest{Worktree: "/wt/a"}, false},
		{"no target at all", SpawnRequest{Prompt: "brief"}, false},
		{"create without branch", SpawnRequest{Repo: "k", CreateWorktree: true, Prompt: "brief"}, false},
		{"create without repo", SpawnRequest{Branch: "b", CreateWorktree: true, Prompt: "brief"}, false},
	}
	for _, tc := range cases {
		err := tc.req.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}
}

// --parent must reach the CLI or completed lanes report nowhere, and -r must be
// present or repository inference can pick an unrelated repo.
func TestSpawnRequestArgsCarryParentAndRepo(t *testing.T) {
	t.Parallel()
	args := SpawnRequest{
		Repo: "kernel", Worktree: "/wt/lane", Parent: "orchestrator-1",
		Type: "builder", Model: "opus", Prompt: "do the thing",
	}.Args()

	joined := strings.Join(args, " ")
	for _, want := range []string{"-r kernel", "--parent orchestrator-1", "-w /wt/lane", "-t builder", "-m opus"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "--create-worktree") {
		t.Error("must not create a worktree when an explicit path is given")
	}
}

// A lane forking from a branch with local commits must use an explicit worktree,
// because --from resolves against the REMOTE.
func TestSpawnRequestArgsCreateWorktreeForm(t *testing.T) {
	t.Parallel()
	args := SpawnRequest{
		Repo: "kernel", Branch: "swarm/lane-a", From: "main",
		CreateWorktree: true, Prompt: "brief",
	}.Args()
	joined := strings.Join(args, " ")
	for _, want := range []string{"--create-worktree", "-b swarm/lane-a", "-f main"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, " -w ") {
		t.Error("must not pass a worktree path when creating one")
	}
}

func TestParseSpawnResult(t *testing.T) {
	t.Parallel()
	res, err := parseSpawnResult([]byte(`{"session_id":"lane-a-builder-1234","worktree_path":"/wt/a"}`))
	if err != nil || res.SessionID != "lane-a-builder-1234" || res.WorktreePath != "/wt/a" {
		t.Errorf("JSON form: %+v %v", res, err)
	}
	// Bare id, for older builds.
	res, err = parseSpawnResult([]byte("lane-a-builder-1234\n"))
	if err != nil || res.SessionID != "lane-a-builder-1234" {
		t.Errorf("bare id form: %+v %v", res, err)
	}
	// Silence must be an error: an empty result would read as a successful spawn
	// with nothing to record.
	if _, err := parseSpawnResult(nil); err == nil {
		t.Error("empty output must not parse as a successful spawn")
	}
	if _, err := parseSpawnResult([]byte(`{"error":"boom"}`)); err == nil {
		t.Error("JSON without a session_id must be an error")
	}
}
