package bramble

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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

// The socket comes in two forms and which one exists FLIPS when the TUI
// restarts. Matching only the suffixed form meant swarm-queen silently could not
// find bramble at all after a restart -- observed live: a TUI came back on the
// un-suffixed path and every probe started reporting "no socket".
func TestSocketPathAcceptsBothForms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	uid := os.Getuid()

	touch := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Un-suffixed alone.
	want := touch(fmt.Sprintf("bramble-%d.sock", uid))
	// The control socket shares the prefix and must never be chosen.
	touch(fmt.Sprintf("bramble-control-%d.sock", uid))
	got, err := SocketPath()
	if err != nil || got != want {
		t.Fatalf("un-suffixed form: got (%q, %v), want %q", got, err, want)
	}

	// Suffixed alone.
	os.Remove(want)
	want = touch(fmt.Sprintf("bramble-%d-2015796.sock", uid))
	touch(fmt.Sprintf("bramble-control-%d-2015796.sock", uid))
	got, err = SocketPath()
	if err != nil || got != want {
		t.Fatalf("suffixed form: got (%q, %v), want %q", got, err, want)
	}

	// Both present: refuse rather than guess. A stale socket outliving its
	// process looks identical to a live one from the filename alone.
	touch(fmt.Sprintf("bramble-%d.sock", uid))
	if _, err := SocketPath(); err == nil {
		t.Error("two candidate sockets must be refused, not silently resolved")
	}
}

// No socket at all is an error, not an empty answer.
func TestSocketPathReportsAbsence(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if got, err := SocketPath(); err == nil {
		t.Errorf("expected an error when no socket exists, got %q", got)
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

// The live failure, end to end: a swarm running against a suffixed socket, the
// TUI dies, and it comes back on the un-suffixed path. Discovery must follow it.
//
// This is not hypothetical -- it happened during a run. Every probe began
// reporting "no socket" while bramble was up and answering pings, because
// discovery was pinned to the form that existed when the run started.
func TestSocketPathFollowsATUIRestartThatChangesForm(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	uid := os.Getuid()

	suffixed := filepath.Join(dir, fmt.Sprintf("bramble-%d-2015796.sock", uid))
	if err := os.WriteFile(suffixed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := SocketPath()
	if err != nil || got != suffixed {
		t.Fatalf("before restart: got (%q, %v), want %q", got, err, suffixed)
	}

	// The TUI dies and comes back on the other form.
	os.Remove(suffixed)
	unsuffixed := filepath.Join(dir, fmt.Sprintf("bramble-%d.sock", uid))
	if err := os.WriteFile(unsuffixed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = SocketPath()
	if err != nil {
		t.Fatalf("after restart the TUI is unreachable: %v", err)
	}
	if got != unsuffixed {
		t.Errorf("after restart: got %q, want %q", got, unsuffixed)
	}
}

// mkSocket creates a real unix socket, which is what SocketPath validates.
func mkSocket(t *testing.T, path string) string {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return path
}

// BRAMBLE_SOCK is the override the multiple-socket error tells the operator to
// set. Nothing read it, so the one documented way out of the ambiguity did not
// exist: the error named a lever that was not connected to anything.
func TestSocketPathHonoursExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	uid := os.Getuid()

	// Two candidates: without the override this is the ambiguity that refuses.
	for _, name := range []string{
		fmt.Sprintf("bramble-%d.sock", uid),
		fmt.Sprintf("bramble-%d-2015796.sock", uid),
	} {
		mkSocket(t, filepath.Join(dir, name))
	}
	if _, err := SocketPath(); err == nil {
		t.Fatal("precondition: two candidates must be ambiguous without the override")
	}

	want := mkSocket(t, filepath.Join(t.TempDir(), "chosen.sock"))
	t.Setenv("BRAMBLE_SOCK", want)
	got, err := SocketPath()
	if err != nil {
		t.Fatalf("the documented override must resolve the ambiguity: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want the overridden socket %q", got, want)
	}
}

// A stale override is reported as a stale override. Trusting it would send every
// probe to a socket nothing listens on, and the failure would read as "bramble
// is down" rather than "this variable is wrong".
func TestSocketPathRejectsStaleOverride(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("BRAMBLE_SOCK", filepath.Join(t.TempDir(), "gone.sock"))

	_, err := SocketPath()
	if err == nil {
		t.Fatal("an override pointing at nothing must be an error")
	}
	if !strings.Contains(err.Error(), "BRAMBLE_SOCK") {
		t.Errorf("the error must name the variable at fault, got %v", err)
	}
}

// A path that exists but is not a socket is also refused: a leftover regular
// file at the expected path would otherwise be handed to every probe.
func TestSocketPathRejectsNonSocketOverride(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	notASocket := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(notASocket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRAMBLE_SOCK", notASocket)

	_, err := SocketPath()
	if err == nil {
		t.Fatal("a regular file must not be accepted as a control socket")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("the error must say what is wrong with it, got %v", err)
	}
}
