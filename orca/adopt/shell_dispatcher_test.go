package adopt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
	"github.com/bazelment/yoloswe/orca/toolbox"
)

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// fakeClaude speaks the CLI's stream protocol, running onTurn for each turn.
func fakeClaude(onTurn func(turn int)) {
	for turn, scan := 0, bufio.NewScanner(os.Stdin); scan.Scan(); {
		var m struct {
			Type      string
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(scan.Bytes(), &m)
		switch m.Type {
		case "control_request":
			fmt.Printf("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":%q,\"response\":{}}}\n", m.RequestID)
		case "user":
			turn++
			onTurn(turn)
			fmt.Println(`{"type":"system","subtype":"init","session_id":"synthetic-dispatcher","model":"sonnet"}`)
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"synthetic-dispatcher","usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}
	os.Exit(0)
}

// superviseFake supervises a shadow of live whose CLI is this test binary
// running the named fake with the given shell exports.
func superviseFake(t *testing.T, st store.Store, live, fake, exports string) (d *ShellDispatcher, intent string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	write(t, bin, "#!/bin/sh\nexport "+exports+"\nexec "+shq(exe)+" -test.run=^"+fake+"$ -- \"$@\"\n")
	if err := os.Chmod(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	copyDir, intent := t.TempDir(), t.TempDir()
	d = &ShellDispatcher{
		Store: st, Run: Run{ID: "run-fake", Dir: live}, Shadow: true, IntentDir: intent,
		Config: model.AgentSessionConfig{Model: "sonnet", ClaudeBin: bin, WorkDir: copyDir},
	}
	if err := d.Prepare(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Supervise(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return d, intent
}

// The fake CLI logs each turn and holds its first turn open until the test
// writes the release file, so the test can act while a turn is running.
func TestFakeDispatcherClaude(t *testing.T) {
	dir := os.Getenv("ORCA_FAKE_DISPATCHER_DIR")
	if dir == "" {
		return
	}
	fakeClaude(func(turn int) {
		f, err := os.OpenFile(filepath.Join(dir, "turns"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(4)
		}
		_, _ = f.WriteString("turn\n")
		_ = f.Close()
		for deadline := time.Now().Add(20 * time.Second); turn == 1 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				break
			}
		}
	})
}

// An owner message relayed while a turn runs wakes the next turn; it must not
// become part of the post-turn wait's baseline.
func TestShellDispatcherWakesForOwnerMessageDuringTurn(t *testing.T) {
	t.Parallel()
	fake, st := t.TempDir(), store.NewMemoryStore(clock.RealClock{})
	d, _ := superviseFake(t, st, t.TempDir(), "TestFakeDispatcherClaude", "ORCA_FAKE_DISPATCHER_DIR="+shq(fake))
	turns := func() int {
		raw, _ := os.ReadFile(filepath.Join(fake, "turns"))
		return strings.Count(string(raw), "\n")
	}
	waitFor(t, func() bool { return turns() == 1 })
	// The owner writes during the first turn; the watcher relays it.
	if _, err := st.Events().Append(t.Context(), store.Event{RunID: "run-fake", Kind: store.EventOwnerMessage, Source: "owner", Summary: "answer lane L1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.syncEventFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fake, "release"), "")
	waitFor(t, func() bool { return turns() >= 2 })
}

// The fake tick session follows its system prompt: when the prompt names a
// skill whose tick integrates, it integrates each review PASS through the
// toolbox, then waits. It logs each toolbox call and its result.
func TestFakeTickClaude(t *testing.T) {
	dir := os.Getenv("ORCA_FAKE_TICK_DIR")
	if dir == "" {
		return
	}
	var tick string
	if i := slices.Index(os.Args, "--system-prompt"); i > 0 && i+1 < len(os.Args) {
		raw, _ := os.ReadFile(regexp.MustCompile(`\S+/SKILL\.md`).FindString(os.Args[i+1]))
		_, tick, _ = strings.Cut(string(raw), "Each tick:")
	}
	act := func(args ...string) {
		result := "ok"
		if err := toolbox.Run(context.Background(), args, io.Discard); err != nil {
			result = err.Error()
		}
		f, _ := os.OpenFile(filepath.Join(dir, "actions"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		_, _ = fmt.Fprintf(f, "%s -> %s\n", strings.Join(args, " "), result)
		_ = f.Close()
	}
	fakeClaude(func(int) {
		reports, _ := filepath.Glob(filepath.Join(os.Getenv("ORCA_TOOLBOX_DIR"), "reports", "*.review.md"))
		for _, r := range reports {
			raw, _ := os.ReadFile(r)
			if pr := regexp.MustCompile(`PR #(\d+)`).FindStringSubmatch(string(raw)); pr != nil && strings.Contains(tick, "integrate") && strings.Contains(string(raw), "Verdict: pass") {
				act("pr", "ready", pr[1])
				act("pr", "merge", pr[1], "--action-key", "merge-"+pr[1])
			}
		}
		act("wait", "--timeout", "50ms")
	})
}

// A wake runs the skill's tick: a lane whose review passed at a green exact
// head is integrated first, not left waiting on the watcher. Not parallel:
// the run's skill is found through ORCA_WATCH_LANES.
func TestShellDispatcherTickIntegratesReviewPass(t *testing.T) {
	live, fake, skill := t.TempDir(), t.TempDir(), t.TempDir()
	scripts := filepath.Join(skill, "scripts")
	for _, d := range []string{filepath.Join(live, "reports"), scripts} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(skill, "SKILL.md"), "Each tick:\n3. Drain reports; verify each completion claim against commits and artifacts.\n4. Advance, rework, integrate, or mark lanes.\n")
	// Exact-head CI is green only at the approved head.
	write(t, filepath.Join(scripts, "swarm_nebula_lanes.sh"), "snl_merge_claims() { :; }\nsnl_head_checks() { [ \"$3\" = abc123 ]; }\n")
	for _, name := range []string{"lane_ops.sh", "step.sh", "watch_lanes.sh"} {
		write(t, filepath.Join(scripts, name), "")
	}
	write(t, filepath.Join(live, "state.json"), `{"tasks":[{"id":"L1","status":"running","phase":"review","session_id":"s1","round":1,"terminals":{"review":{"1":"t1"}},"pr":7,"pr_head":"abc123","approval_sha":"abc123"}]}`)
	write(t, filepath.Join(live, "reports", "L1.review.md"), "PR #7\nVerdict: pass\n")
	t.Setenv("ORCA_WATCH_LANES", filepath.Join(scripts, "watch_lanes.sh"))
	_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, "TestFakeTickClaude", "ORCA_FAKE_TICK_DIR="+shq(fake)+" ORCA_SNL_DIR="+shq(scripts))
	var actions []string
	waitFor(t, func() bool {
		raw, _ := os.ReadFile(filepath.Join(fake, "actions"))
		actions = strings.Split(strings.TrimSpace(string(raw)), "\n")
		return slices.ContainsFunc(actions, func(a string) bool { return strings.HasPrefix(a, "wait ") })
	})
	if want := []string{"pr ready 7 -> ok", "pr merge 7 --action-key merge-7 -> ok"}; len(actions) < 2 || !slices.Equal(actions[:2], want) {
		t.Fatalf("first actions after the wake = %q, want %q", actions, want)
	}
	if raw, err := os.ReadFile(filepath.Join(intent, "actions.jsonl")); err != nil || !strings.Contains(string(raw), `"action":"pr.merge"`) {
		t.Fatalf("shadow merge intent = %s, %v", raw, err)
	}
}

// orca wait keeps the caller's baseline across calls: a change between calls
// returns at once, and neither a sync's rewrite nor the session's own edits
// in the copy end a wait with no live change.
func TestOrcaWaitReturnsOnlyOnLiveChange(t *testing.T) {
	t.Parallel()
	live, copyDir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(live, "state.json"), `{"tasks":[]}`)
	write(t, filepath.Join(live, "EVENTS"), "a\n")
	sync := func() {
		t.Helper()
		if err := SyncWorkspace(live, copyDir); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(timeout string) (string, error) {
		var out bytes.Buffer
		err := toolbox.Run(t.Context(), []string{"wait", "--dir", copyDir, "--timeout", timeout}, &out)
		return out.String(), err
	}
	sync()
	if out, err := wait("50ms"); !errors.Is(err, toolbox.ErrTimeout) {
		t.Fatalf("first wait = %q, %v; want a timeout", out, err)
	}
	write(t, filepath.Join(live, "EVENTS"), "a\nb\n")
	sync()
	if out, err := wait("10s"); err != nil || out != "changed: EVENTS\n" {
		t.Fatalf("wait after a live change = %q, %v", out, err)
	}
	write(t, filepath.Join(copyDir, "state.json"), `{"tasks":[{"id":"mine"}]}`)
	write(t, filepath.Join(copyDir, "notes.md"), "session notes\n")
	sync()
	start := time.Now()
	if out, err := wait("300ms"); !errors.Is(err, toolbox.ErrTimeout) || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("wait with no live change = %q, %v after %s; want a block until timeout", out, err, time.Since(start))
	}
}
