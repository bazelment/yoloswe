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
	"strconv"
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

// The fake resume CLI names its session by process unless --resume names it,
// logs each turn's session, and reports a tick's usage the way the CLI does:
// the result sums eight API calls (240k cache reads) while the context holds 30k.
func TestFakeResumeClaude(t *testing.T) {
	dir := os.Getenv("ORCA_FAKE_RESUME_DIR")
	if dir == "" {
		return
	}
	session := fmt.Sprintf("sess-%d", os.Getpid())
	if i := slices.Index(os.Args, "--resume"); i > 0 && i+1 < len(os.Args) {
		session = os.Args[i+1]
	}
	for scan := bufio.NewScanner(os.Stdin); scan.Scan(); {
		var m struct {
			Type      string
			RequestID string `json:"request_id"`
			Request   struct{ Subtype string }
		}
		_ = json.Unmarshal(scan.Bytes(), &m)
		switch {
		case m.Type == "control_request" && m.Request.Subtype == "get_context_usage":
			fmt.Printf("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":%q,\"response\":{\"totalTokens\":30000}}}\n", m.RequestID)
		case m.Type == "control_request":
			fmt.Printf("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":%q,\"response\":{}}}\n", m.RequestID)
		case m.Type == "user":
			f, err := os.OpenFile(filepath.Join(dir, "turns"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				os.Exit(4)
			}
			_, _ = f.WriteString(session + "\n")
			_ = f.Close()
			fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":%q,\"model\":\"opus\"}\n", session)
			fmt.Printf("{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"done\",\"session_id\":%q,\"num_turns\":8,\"usage\":{\"input_tokens\":80,\"output_tokens\":4000,\"cache_read_input_tokens\":240000,\"cache_creation_input_tokens\":2000}}\n", session)
		}
	}
	os.Exit(0)
}

// Each wake continues the run's one CLI session, so the dispatcher remembers
// what it asked: a tick of many tool calls is not a context overflow.
func TestShellDispatcherWakesContinueOneSession(t *testing.T) {
	t.Parallel()
	fake, st := t.TempDir(), store.NewMemoryStore(clock.RealClock{})
	d, _ := superviseFake(t, st, t.TempDir(), "TestFakeResumeClaude", "ORCA_FAKE_RESUME_DIR="+shq(fake))
	turns := func() []string {
		raw, _ := os.ReadFile(filepath.Join(fake, "turns"))
		return strings.Fields(string(raw))
	}
	waitFor(t, func() bool { return len(turns()) == 1 })
	if _, err := st.Events().Append(t.Context(), store.Event{RunID: "run-fake", Kind: store.EventOwnerMessage, Source: "owner", Summary: "status?"}); err != nil {
		t.Fatal(err)
	}
	if err := d.syncEventFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(turns()) >= 2 })
	if got := turns()[:2]; got[1] != got[0] {
		t.Fatalf("second wake ran in session %q, want the first wake's %q", got[1], got[0])
	}
}

// Unset, the adopted dispatcher runs on the host's model; a set Claude model
// stays, and an OpenAI one falls back to the host's.
func TestShellDispatcherDefaultsToHostModel(t *testing.T) {
	t.Parallel()
	for set, want := range map[string]string{"": HostModel, "claude-sonnet-5-5": "claude-sonnet-5-5", "gpt-6.1-sol": HostModel} {
		d := &ShellDispatcher{Run: Run{ID: "r", Dir: t.TempDir()}, Shadow: true, IntentDir: t.TempDir(), Config: model.AgentSessionConfig{Model: set, WorkDir: t.TempDir()}}
		if err := d.Prepare(); err != nil {
			t.Fatal(err)
		}
		if d.Config.Model != want {
			t.Errorf("model %q prepared as %q, want %q", set, d.Config.Model, want)
		}
	}
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
	write(t, filepath.Join(live, "state.json"), `{"config":{"repo":`+strconv.Quote(t.TempDir())+`},"tasks":[{"id":"L1","status":"running","phase":"review","round":1,"sessions":{"review":"nebula:t1"},"nebula_session":"s1","nebula_terminal":"t1","pr":7,"pr_head":"abc123","approval_sha":"abc123"}]}`)
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

// The fake note session reports a write as its system prompt orders. Batched,
// the note is composed before the write's result is back and claims success;
// told to read the result first, it quotes that result.
func TestFakeNoteClaude(t *testing.T) {
	if os.Getenv("ORCA_FAKE_NOTE") == "" {
		return
	}
	var prompt string
	if i := slices.Index(os.Args, "--system-prompt"); i > 0 && i+1 < len(os.Args) {
		prompt = os.Args[i+1]
	}
	run := func(args ...string) string {
		if err := toolbox.Run(context.Background(), args, io.Discard); err != nil {
			return err.Error()
		}
		return "ok"
	}
	fakeClaude(func(turn int) {
		if turn > 1 {
			return
		}
		if strings.Contains(prompt, "read its result") {
			run("note", "pr ready 7: "+run("pr", "ready", "7"))
			return
		}
		run("note", "Marked #7 ready")
		run("pr", "ready", "7")
	})
}

// A note never runs before the result it reports: pr ready is refused with
// checks pending, and the note that follows quotes the refusal.
func TestShellDispatcherNoteQuotesRefusedWrite(t *testing.T) {
	t.Parallel()
	live, scripts := t.TempDir(), t.TempDir()
	write(t, filepath.Join(scripts, "swarm_nebula_lanes.sh"), "snl_merge_claims() { :; }\nsnl_head_checks() { echo checks pending; return 1; }\n")
	for _, name := range []string{"lane_ops.sh", "step.sh"} {
		write(t, filepath.Join(scripts, name), "")
	}
	write(t, filepath.Join(live, "state.json"), `{"config":{"repo":`+strconv.Quote(t.TempDir())+`},"tasks":[{"id":"L1","status":"running","phase":"review","pr":7,"pr_head":"abc123","approval_sha":"abc123"}]}`)
	_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, "TestFakeNoteClaude", "ORCA_FAKE_NOTE=1 ORCA_SNL_DIR="+shq(scripts))
	var note string
	waitFor(t, func() bool {
		raw, _ := os.ReadFile(filepath.Join(intent, "actions.jsonl"))
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var a struct {
				Arguments map[string]string
				Action    string
			}
			if json.Unmarshal([]byte(line), &a) == nil && a.Action == "note" {
				note = a.Arguments["text"]
				return true
			}
		}
		return false
	})
	if !strings.Contains(note, "refused") || !strings.Contains(note, "checks pending") {
		t.Fatalf("note = %q, want it to quote the pr ready refusal", note)
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

// The fake care session follows its system prompt's terminal care: it reads
// orca status, runs orca check on each running lane, and steers an idle lane
// whose last turn printed a verdict with no report filed, unless a background
// shell still runs. It logs each toolbox call and its result.
func TestFakeCareClaude(t *testing.T) {
	dir := os.Getenv("ORCA_FAKE_CARE_DIR")
	if dir == "" {
		return
	}
	var prompt string
	if i := slices.Index(os.Args, "--system-prompt"); i > 0 && i+1 < len(os.Args) {
		prompt = os.Args[i+1]
	}
	act := func(args ...string) string {
		var out bytes.Buffer
		result := "ok"
		if err := toolbox.Run(context.Background(), args, &out); err != nil {
			result = err.Error()
		}
		f, _ := os.OpenFile(filepath.Join(dir, "actions"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		_, _ = fmt.Fprintf(f, "%s -> %s\n", args[0], result)
		_ = f.Close()
		return out.String()
	}
	fakeClaude(func(int) {
		var lanes []struct{ ID, Status string }
		_ = json.Unmarshal([]byte(act("status")), &lanes)
		for _, l := range lanes {
			check := act("check", l.ID)
			if l.Status == "running" && strings.Contains(prompt, "report_filed=no") && strings.Contains(prompt, "shell still running") &&
				strings.Contains(check, "agent=idle") && strings.Contains(check, "report_filed=no") && strings.Contains(check, "background={}") &&
				regexp.MustCompile(`last_turn_line=.*(PASS|Verdict)`).MatchString(check) {
				act("lane", "steer", l.ID, "Your last turn printed a verdict but filed no phase report. File it now.", "--action-key", "care-"+l.ID)
			}
		}
		act("wait", "--timeout", "50ms")
	})
}

// A review terminal idle 25 min after printing PASS, with no report filed, is
// steered to file it; the same terminal with a background shell still running
// waits on its own gate and is left alone.
func TestShellDispatcherTerminalCare(t *testing.T) {
	t.Parallel()
	for background, want := range map[string]bool{"": true, `"backgroundTasks":{"shells":1},`: false} {
		live, fake, scripts := t.TempDir(), t.TempDir(), t.TempDir()
		terms := filepath.Join(scripts, "terms.json")
		write(t, terms, `{"terminals":[{"id":"t1","status":"running","agentState":"idle",`+background+`"lastOutputAt":"`+
			time.Now().Add(-25*time.Minute).UTC().Format(time.RFC3339)+`","lastTurnText":"Gate green on abc123.\nVerdict: PASS\n"}]}`)
		write(t, filepath.Join(scripts, "swarm_nebula_lanes.sh"), "snl_nb() { cat "+shq(terms)+"; }\n"+
			"snl_terminal_agent_state() { snl_nb | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"terminals\"][0][\"agentState\"])'; }\n"+
			"snl_lane_remote_head() { echo OK; }\nsnl_sandbox_busy() { false; }\n")
		for _, name := range []string{"lane_ops.sh", "step.sh"} {
			write(t, filepath.Join(scripts, name), "")
		}
		write(t, filepath.Join(live, "state.json"), `{"config":{"repo":`+strconv.Quote(t.TempDir())+`},"tasks":[{"id":"L1","status":"running","phase":"review","round":1,"nebula_session":"s1","nebula_terminal":"t1","pr":7}]}`)
		_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, "TestFakeCareClaude", "ORCA_FAKE_CARE_DIR="+shq(fake)+" ORCA_SNL_DIR="+shq(scripts))
		var actions []string
		waitFor(t, func() bool {
			raw, _ := os.ReadFile(filepath.Join(fake, "actions"))
			actions = strings.Split(strings.TrimSpace(string(raw)), "\n")
			return slices.ContainsFunc(actions, func(a string) bool { return strings.HasPrefix(a, "wait ") })
		})
		wantActions := []string{"status -> ok", "check -> ok"}
		if want {
			wantActions = append(wantActions, "lane -> ok")
		}
		if i := slices.IndexFunc(actions, func(a string) bool { return strings.HasPrefix(a, "wait ") }); !slices.Equal(actions[:i], wantActions) {
			t.Fatalf("background %q: actions = %q, want %q", background, actions, wantActions)
		}
		raw, _ := os.ReadFile(filepath.Join(intent, "actions.jsonl"))
		if steered := strings.Contains(string(raw), `"action":"lane.steer"`); steered != want {
			t.Fatalf("background %q: steer intent = %s, want steered=%v", background, raw, want)
		}
	}
}
