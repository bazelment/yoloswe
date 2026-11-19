package adopt

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
// running the named fake with the given shell exports. live's env.sh names
// scripts (a stub when "") as its skill's SW.
func superviseFake(t *testing.T, st store.Store, live, scripts, fake, exports string) (d *ShellDispatcher, intent string) {
	t.Helper()
	envSkill(t, live, scripts)
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

// envSkill points live's env.sh SW at scripts, a stub skill in a temp dir
// when "", and returns it. No home skill folder is involved.
func envSkill(t *testing.T, live, scripts string) string {
	t.Helper()
	if scripts == "" {
		scripts = filepath.Join(t.TempDir(), "scripts")
		if err := os.MkdirAll(scripts, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(scripts, "lane_ops.sh"), "")
	}
	f, err := os.OpenFile(filepath.Join(live, "env.sh"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("export SW=" + shq(scripts) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return scripts
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
	d, _ := superviseFake(t, st, t.TempDir(), "", "TestFakeDispatcherClaude", "ORCA_FAKE_DISPATCHER_DIR="+shq(fake))
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
	d, _ := superviseFake(t, st, t.TempDir(), "", "TestFakeResumeClaude", "ORCA_FAKE_RESUME_DIR="+shq(fake))
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
		live := t.TempDir()
		envSkill(t, live, "")
		d := &ShellDispatcher{Run: Run{ID: "r", Dir: live}, Shadow: true, IntentDir: t.TempDir(), Config: model.AgentSessionConfig{Model: set, WorkDir: t.TempDir()}}
		if err := d.Prepare(); err != nil {
			t.Fatal(err)
		}
		if d.Config.Model != want {
			t.Errorf("model %q prepared as %q, want %q", set, d.Config.Model, want)
		}
	}
}

// The run's env.sh SW names the skill, with no home skill folder: the
// adopted prompt names its SKILL.md and references, and the adopted watcher
// and the host watcher run its watch_lanes.sh.
func TestAdoptedSkillFromRunEnvSW(t *testing.T) {
	t.Parallel()
	live, skill := t.TempDir(), t.TempDir()
	scripts, marker := filepath.Join(skill, "scripts"), filepath.Join(skill, "watched")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(scripts, "lane_ops.sh"), "")
	write(t, filepath.Join(scripts, "watch_lanes.sh"), "echo \"$*\" >> "+shq(marker)+"\necho armed: none; echo bridges: 42\n")
	envSkill(t, live, scripts)
	write(t, filepath.Join(live, ".orca-lease.json"), `{"Holder":"h","Marker":"m","State":"adopted"}`)
	d := &ShellDispatcher{Store: store.NewMemoryStore(clock.RealClock{}), Run: Run{ID: "r", Dir: live}}
	if err := d.Prepare(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(skill, "SKILL.md"), filepath.Join(skill, "references", "nebula-lanes.md"), filepath.Join(skill, "references", "standing-rules.md")} {
		if !strings.Contains(d.Config.SystemPrompt, want) {
			t.Errorf("adopted prompt does not name %s", want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.watch(ctx) }()
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })
	cancel()
	<-done
	if pids, err := (ScriptWatcher{}).Running(t.Context(), live); err != nil || !slices.Equal(pids, []int{42}) {
		t.Fatalf("host watcher status = %v, %v", pids, err)
	}
	raw, _ := os.ReadFile(marker)
	if got, want := string(raw), live+"\n--status "+live+"\n"; got != want {
		t.Fatalf("watch_lanes.sh calls = %q, want %q", got, want)
	}
}

// A shadow copy has no env.sh, so the session's toolbox, bound to the copy,
// resolves the skill Prepare found through the live run's SW. Not parallel:
// it sets HOME and the session's environment.
func TestShadowToolboxUsesLiveRunSkill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ORCA_SNL_DIR", "")
	live := t.TempDir()
	scripts := envSkill(t, live, "")
	d := &ShellDispatcher{Run: Run{ID: "r", Dir: live}, Shadow: true, IntentDir: t.TempDir(), Config: model.AgentSessionConfig{WorkDir: t.TempDir()}}
	if err := d.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.workspace, "env.sh")); !os.IsNotExist(err) {
		t.Fatalf("shadow copy has env.sh: %v", err)
	}
	for k, v := range d.Config.Env {
		t.Setenv(k, v)
	}
	// As the toolbox's --skill and --dir defaults read them.
	if got, err := toolbox.SkillDir(t.Context(), os.Getenv("ORCA_SNL_DIR"), os.Getenv("ORCA_TOOLBOX_DIR")); err != nil || got != scripts {
		t.Fatalf("session toolbox skill = %q, %v; want %s", got, err, scripts)
	}
}

// With no SW in the run's env.sh and no home skill folder, adoption refuses
// before launching, naming the paths tried. Not parallel: it sets HOME.
func TestAdoptedSkillRefusalNamesPathsTried(t *testing.T) {
	home, live := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ORCA_SNL_DIR", "")
	d := &ShellDispatcher{Run: Run{ID: "r", Dir: live}, Shadow: true, IntentDir: t.TempDir(), Config: model.AgentSessionConfig{WorkDir: t.TempDir()}}
	err := d.Prepare()
	want := "orca toolbox refused: swarm skill scripts unavailable: " + filepath.Join(live, "env.sh") +
		" exports no SW and no lane_ops.sh in " + filepath.Join(home, ".claude", "skills", "subagent-swarm-nebula", "scripts")
	if err == nil || err.Error() != want || d.Config.SystemPrompt != "" {
		t.Fatalf("Prepare err=%v prompt=%q\nwant %s", err, d.Config.SystemPrompt, want)
	}
	if _, err := (ScriptWatcher{}).Running(t.Context(), live); err == nil || err.Error() != want {
		t.Fatalf("host watcher err=%v\nwant %s", err, want)
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
// head is integrated first, not left waiting on the watcher.
func TestShellDispatcherTickIntegratesReviewPass(t *testing.T) {
	t.Parallel()
	live, fake, skill := t.TempDir(), t.TempDir(), t.TempDir()
	scripts := filepath.Join(skill, "scripts")
	for _, d := range []string{filepath.Join(live, "reports"), scripts} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(skill, "SKILL.md"), "Each tick:\n3. Drain reports; verify each completion claim against commits and artifacts.\n4. Advance, rework, integrate, or mark lanes.\n")
	// Exact-head CI is green only at the approved head, on a fresh base.
	repo, head := handRepo(t, 10*time.Minute)
	handSkill(t, scripts, head)
	write(t, filepath.Join(live, "state.json"), `{"config":{"repo":`+strconv.Quote(repo)+`},"tasks":[{"id":"L1","status":"running","phase":"review","round":1,"sessions":{"review":"nebula:t1"},"nebula_session":"s1","nebula_terminal":"t1","pr":7,"pr_head":"`+head+`","approval_sha":"`+head+`"}]}`)
	write(t, filepath.Join(live, "reports", "L1.review.md"), "PR #7\nVerdict: pass\n")
	_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, scripts, "TestFakeTickClaude", "ORCA_FAKE_TICK_DIR="+shq(fake)+" ORCA_SNL_DIR="+shq(scripts))
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
	write(t, filepath.Join(scripts, "swarm_nebula_lanes.sh"), "snl_task() { echo 7; }\nsnl_merge_claims() { :; }\nsnl_head_checks() { echo checks pending; return 1; }\n")
	for _, name := range []string{"lane_ops.sh", "step.sh"} {
		write(t, filepath.Join(scripts, name), "")
	}
	write(t, filepath.Join(live, "state.json"), `{"config":{"repo":`+strconv.Quote(t.TempDir())+`},"tasks":[{"id":"L1","status":"running","phase":"review","pr":7,"pr_head":"abc123","approval_sha":"abc123"}]}`)
	_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, scripts, "TestFakeNoteClaude", "ORCA_FAKE_NOTE=1 ORCA_SNL_DIR="+shq(scripts))
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
// whose last turn printed a verdict with no report filed, only when its screen
// shows no background shell. It logs each toolbox call and its result.
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
		_ = os.WriteFile(filepath.Join(dir, args[0]+".out"), out.Bytes(), 0o600)
		return out.String()
	}
	fakeClaude(func(int) {
		var lanes []struct{ ID, Status string }
		_ = json.Unmarshal([]byte(act("status")), &lanes)
		for _, l := range lanes {
			check := act("check", l.ID)
			if l.Status == "running" && strings.Contains(prompt, "report_filed=no") && strings.Contains(prompt, "only when background=no") &&
				strings.Contains(check, "agent=idle") && strings.Contains(check, "report_filed=no") && strings.Contains(check, "background=no") &&
				regexp.MustCompile(`last_turn_line=.*(PASS|Verdict)`).MatchString(check) {
				act("lane", "steer", l.ID, "Your last turn printed a verdict but filed no phase report. File it now.", "--action-key", "care-"+l.ID)
			}
		}
		act("wait", "--timeout", "50ms")
	})
}

// A review terminal idle 25 min after printing PASS, with no report filed and
// no backgroundTasks in term list, is steered to file it only when its screen
// shows no background shell. With "1 shell still running" on screen it waits
// on its own gate; with an unreadable screen it is not steered blind.
func TestShellDispatcherTerminalCare(t *testing.T) {
	t.Parallel()
	idle := "Verdict: PASS\n╭──╮\n│ > │\n╰──╯\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
	for _, tc := range []struct {
		name, screen, background string
		steer                    bool
	}{
		{"idle", idle, "no", true},
		{"own shell", strings.Replace(idle, "(shift+tab to cycle)", "· 1 shell still running · ↓ to manage", 1), "yes", false},
		{"screen unreadable", "", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live, fake, scripts := t.TempDir(), t.TempDir(), t.TempDir()
			terms, screen := filepath.Join(scripts, "terms.json"), filepath.Join(scripts, "screen.txt")
			write(t, terms, `{"terminals":[{"id":"t1","status":"running","agentState":"idle","lastOutputAt":"`+
				time.Now().Add(-25*time.Minute).UTC().Format(time.RFC3339Nano)+`","lastTurnText":"Gate green on abc123.\nVerdict: PASS\n"}]}`)
			if tc.screen != "" {
				write(t, screen, tc.screen)
			}
			write(t, filepath.Join(scripts, "swarm_nebula_lanes.sh"), "snl_nb() { if [ \"${2:-}\" = screen ]; then cat "+shq(screen)+"; else cat "+shq(terms)+"; fi; }\n"+
				"snl_terminal_agent_state() { snl_nb | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"terminals\"][0][\"agentState\"])'; }\n"+
				"snl_lane_remote_head() { echo OK; }\nsnl_sandbox_busy() { false; }\n")
			for _, name := range []string{"lane_ops.sh", "step.sh"} {
				write(t, filepath.Join(scripts, name), "")
			}
			write(t, filepath.Join(live, "state.json"), `{"config":{"repo":`+strconv.Quote(t.TempDir())+`},"tasks":[{"id":"L1","status":"running","phase":"review","round":1,"nebula_session":"s1","nebula_terminal":"t1","pr":7}]}`)
			_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, scripts, "TestFakeCareClaude", "ORCA_FAKE_CARE_DIR="+shq(fake)+" ORCA_SNL_DIR="+shq(scripts))
			var actions []string
			waitFor(t, func() bool {
				raw, _ := os.ReadFile(filepath.Join(fake, "actions"))
				actions = strings.Split(strings.TrimSpace(string(raw)), "\n")
				return slices.ContainsFunc(actions, func(a string) bool { return strings.HasPrefix(a, "wait ") })
			})
			wantActions := []string{"status -> ok", "check -> ok"}
			if tc.steer {
				wantActions = append(wantActions, "lane -> ok")
			}
			if i := slices.IndexFunc(actions, func(a string) bool { return strings.HasPrefix(a, "wait ") }); !slices.Equal(actions[:i], wantActions) {
				t.Fatalf("actions = %q, want %q", actions, wantActions)
			}
			check, _ := os.ReadFile(filepath.Join(fake, "check.out"))
			if want := "output_min_ago=25 background=" + tc.background + " report_filed=no\n"; !strings.Contains(string(check), want) {
				t.Fatalf("check = %q, want %q", check, want)
			}
			raw, _ := os.ReadFile(filepath.Join(intent, "actions.jsonl"))
			if steered := strings.Contains(string(raw), `"action":"lane.steer"`); steered != tc.steer {
				t.Fatalf("steer intent = %s, want steered=%v", raw, tc.steer)
			}
		})
	}
}

// handRepo is a host checkout whose branch lane is one commit on root, and
// whose origin/main adds a commit baseAge old. It returns the lane head.
func handRepo(t *testing.T, baseAge time.Duration) (repo, head string) {
	t.Helper()
	repo = t.TempDir()
	git := func(date time.Time, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date.Format(time.RFC3339), "GIT_COMMITTER_DATE="+date.Format(time.RFC3339))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	now := time.Now()
	git(now, "init", "-q", "-b", "lane")
	git(now.Add(-4*time.Hour), "commit", "-q", "--allow-empty", "-m", "root")
	root := git(now, "rev-parse", "HEAD")
	git(now, "commit", "-q", "--allow-empty", "-m", "lane")
	head = git(now, "rev-parse", "HEAD")
	git(now, "update-ref", "refs/remotes/origin/main", git(now.Add(-baseAge), "commit-tree", root+"^{tree}", "-p", root, "-m", "main"))
	return repo, head
}

// handSkill fakes the skill around a host checkout. The live PR head is the
// file pr-head, CI is green only on it, GitHub's update-branch merges
// origin/main into it, and ledger.py set records heads in state.json.
// snl_only_base_merges is the skill's own, copied from lane_ops.sh.
func handSkill(t *testing.T, scripts, head string) (calls string) {
	t.Helper()
	calls = filepath.Join(scripts, "calls")
	prHead := shq(filepath.Join(scripts, "pr-head"))
	write(t, filepath.Join(scripts, "pr-head"), head+"\n")
	write(t, filepath.Join(scripts, "swarm_nebula_lanes.sh"), "log() { echo \"$*\" >> "+shq(calls)+"; }\n"+
		"snl_merge_claims() { :; }\nsnl_head_checks() { [ \"$3\" = \"$(cat "+prHead+")\" ]; }\n"+
		"snl_lane_gh() { cat "+prHead+"; }\nsnl_lane_slug() { echo o/r; }\nsnl_merge() { log merge \"$1\"; }\n"+
		"ghx() { log ghx \"$@\"; git -c user.name=t -c user.email=t@t merge -q --no-ff -m main origin/main && git rev-parse HEAD > "+prHead+"; }\n"+
		"snl_task() { echo 7; }\nsnl_terminal_agent_state() { echo working; }\nsnl_nb() { log nb \"$*\"; }\n"+
		"snl_report_word() { sed -n \"s/^$3:[[:space:]]*//p\" \"$RUN/$1.$2.md\" | awk '{print tolower($1)}' | head -1; }\n")
	write(t, filepath.Join(scripts, "pr_merge.sh"), "#!/bin/sh\necho \"pr_merge $*\" >> "+shq(calls)+"\necho 'merged 7 deadbeef'\n")
	write(t, filepath.Join(scripts, "lane_ops.sh"), `snl_only_base_merges() {
  local c="$2"
  while [ "$c" != "$1" ]; do
    [ "$(git rev-list --parents -n1 "$c" | wc -w)" = 3 ] &&
      git merge-base --is-ancestor "$(git rev-parse "$c^2")" "origin/${BASE:-main}" &&
      [ "$(git merge-tree --write-tree "$c^1" "$c^2" 2>/dev/null)" = "$(git rev-parse "$c^{tree}")" ] || return 1
    c="$(git rev-parse "$c^1")"
  done
}
`)
	write(t, filepath.Join(scripts, "step.sh"), "")
	write(t, filepath.Join(scripts, "ledger.py"), `import json, sys
_, cmd, run, *rest = sys.argv
opts = dict(zip(rest[::2], rest[1::2]))
path = run + "/state.json"
state = json.load(open(path))
for t in state["tasks"]:
    if cmd == "set" and t["id"] == opts["--id"]:
        t["pr_head"], t["approval_sha"] = opts["--pr-head"], opts["--approval-sha"]
json.dump(state, open(path, "w"))
`)
	return calls
}

// handRun is one synthetic run whose OBJECTIVE says integration is by hand:
// the owner conditions PR #7's merge on a readback that a later EVENTS row
// reports. CI is green at L1's exact head, and the oldest base commit missing
// from it is baseAge old. L1's reviewer t1 is stuck in lint; L2 has a hold_note.
func handRun(t *testing.T, baseAge time.Duration) (live, scripts, calls string) {
	t.Helper()
	live, scripts = t.TempDir(), t.TempDir()
	repo, head := handRepo(t, baseAge)
	calls = handSkill(t, scripts, head)
	write(t, filepath.Join(live, "OBJECTIVE.md"), "Integration: no auto-integrate; the orchestrator advances and merges by hand.\n")
	write(t, filepath.Join(live, "EVENTS"), "2026-11-19T01:00:00Z\towner\tmail\tmerge PR #7 only after the code promote carrying A and B is read back clean\n"+
		"2026-11-19T02:00:00Z\tpromote\treadback\tthe code promote carrying A and B read back clean\n")
	write(t, filepath.Join(live, "state.json"), `{"config":{"repo":`+strconv.Quote(repo)+`},"tasks":[`+
		`{"id":"L1","status":"running","phase":"review","round":1,"nebula_session":"s1","nebula_terminal":"t1","pr":7,"pr_head":"`+head+`","approval_sha":"`+head+`"},`+
		`{"id":"L2","status":"running","phase":"review","round":1,"nebula_session":"s1","nebula_terminal":"t2","hold_note":"owner: wait for the promote"}]}`)
	return live, scripts, calls
}

// .hold-advance gates phase advance and integration, not steering: a reviewer
// stuck in lint under it is steered. A hold_note still refuses the steer.
func TestSteerUnderHoldAdvance(t *testing.T) {
	t.Parallel()
	live, scripts, calls := handRun(t, 10*time.Minute)
	for _, lane := range []string{"L1", "L2"} {
		write(t, filepath.Join(live, lane+".hold-advance"), "run rule: hold-advance on every lane\n")
	}
	steer := func(lane string) error {
		return toolbox.Run(t.Context(), []string{"lane", "steer", lane, "Interrupt the lint and run a narrower one.", "--dir", live, "--skill", scripts}, io.Discard)
	}
	if err := steer("L1"); err != nil {
		t.Fatalf("steer under hold-advance: %v", err)
	}
	if err := steer("L2"); !errors.Is(err, toolbox.ErrRefused) {
		t.Fatalf("steer under hold_note: %v", err)
	}
	if raw, _ := os.ReadFile(calls); !regexp.MustCompile(`^nb term send t1 --file \S+\n$`).Match(raw) {
		t.Fatalf("skill calls = %q", raw)
	}
}

// The host's hand merge on a run that holds every lane's advance: a verified
// PASS at a green exact head on a fresh base is readied and merged; a stale
// base is updated from main and the base-only head, green, carries the
// approval. A head with more than base merges, or a base git cannot read, is
// refused.
func TestHandMergeUnderHoldAdvance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		before  func(t *testing.T, repo, scripts string) // runs after a refused stale-base ready
		refusal string
		baseAge time.Duration
	}{
		{name: "fresh base", baseAge: 10 * time.Minute},
		{name: "base-only update", baseAge: 3 * time.Hour},
		{name: "update plus a commit", baseAge: 3 * time.Hour, refusal: "is not approved head", before: func(t *testing.T, repo, scripts string) {
			out, err := exec.Command("bash", "-c", `git -C "$1" -c user.name=t -c user.email=t@t commit -q --allow-empty -m extra && git -C "$1" rev-parse HEAD > "$2"`,
				"-", repo, filepath.Join(scripts, "pr-head")).CombinedOutput()
			if err != nil {
				t.Fatalf("extra commit: %v %s", err, out)
			}
		}},
		{name: "base unreadable", baseAge: 10 * time.Minute, refusal: "base age unknown", before: func(t *testing.T, repo, _ string) {
			if out, err := exec.Command("git", "-C", repo, "update-ref", "-d", "refs/remotes/origin/main").CombinedOutput(); err != nil {
				t.Fatalf("drop origin/main: %v %s", err, out)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live, scripts, calls := handRun(t, tc.baseAge)
			write(t, filepath.Join(live, "L1.hold-advance"), "run rule: hold-advance on every lane\n")
			var s struct{ Config struct{ Repo string } }
			raw, _ := os.ReadFile(filepath.Join(live, "state.json"))
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			pr := func(args ...string) error {
				return toolbox.Run(t.Context(), append([]string{"pr"}, append(args, "7", "--dir", live, "--skill", scripts)...), io.Discard)
			}
			if tc.baseAge > time.Hour {
				if err := pr("ready"); err == nil || !strings.Contains(err.Error(), "stale base") {
					t.Fatalf("ready on a stale base: %v", err)
				}
				if err := pr("update"); err != nil {
					t.Fatalf("update: %v", err)
				}
			}
			if tc.before != nil {
				tc.before(t, s.Config.Repo, scripts)
			}
			ready, merge := pr("ready"), pr("merge")
			got, _ := os.ReadFile(calls)
			prHead, _ := os.ReadFile(filepath.Join(scripts, "pr-head"))
			wantHandMerge := "pr_merge --repo o/r --head " + strings.TrimSpace(string(prHead))
			if tc.refusal != "" {
				if !errors.Is(ready, toolbox.ErrRefused) || !strings.Contains(ready.Error(), tc.refusal) || !errors.Is(merge, toolbox.ErrRefused) || strings.Contains(string(got), "merge L1") {
					t.Fatalf("ready = %v, merge = %v, calls = %q; want both refused with %q", ready, merge, got, tc.refusal)
				}
				return
			}
			if ready != nil || merge != nil || strings.Contains(string(got), "merge L1") || !strings.Contains(string(got), wantHandMerge) {
				t.Fatalf("ready = %v, merge = %v, calls = %q", ready, merge, got)
			}
			// The ledger records the head merged: the base-only update when there was one.
			raw, _ = os.ReadFile(filepath.Join(live, "state.json"))
			var ledger struct {
				Tasks []struct {
					PRHead      string `json:"pr_head"`
					ApprovalSHA string `json:"approval_sha"`
				}
			}
			if err := json.Unmarshal(raw, &ledger); err != nil || ledger.Tasks[0].PRHead != strings.TrimSpace(string(prHead)) || ledger.Tasks[0].ApprovalSHA != ledger.Tasks[0].PRHead {
				t.Fatalf("state.json = %s, want L1 at PR head %s: %v", raw, prHead, err)
			}
		})
	}
}

// The fake integrate session follows its system prompt's integration by hand:
// in a run whose OBJECTIVE says "no auto-integrate", a PR whose owner condition
// an EVENTS row meets is readied, its stale base updated when ready refuses
// only on that, and merged once ready. It logs each toolbox call and result.
func TestFakeIntegrateClaude(t *testing.T) {
	dir := os.Getenv("ORCA_FAKE_INTEGRATE_DIR")
	if dir == "" {
		return
	}
	var prompt string
	if i := slices.Index(os.Args, "--system-prompt"); i > 0 && i+1 < len(os.Args) {
		prompt = os.Args[i+1]
	}
	act := func(args ...string) string {
		result := "ok"
		if err := toolbox.Run(context.Background(), args, io.Discard); err != nil {
			result = err.Error()
		}
		f, _ := os.OpenFile(filepath.Join(dir, "actions"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		_, _ = fmt.Fprintf(f, "%s -> %s\n", strings.Join(args, " "), result)
		_ = f.Close()
		return result
	}
	fakeClaude(func(int) {
		run := os.Getenv("ORCA_TOOLBOX_DIR")
		objective, _ := os.ReadFile(filepath.Join(run, "OBJECTIVE.md"))
		raw, _ := os.ReadFile(filepath.Join(run, "EVENTS"))
		events := string(raw)
		byHand := strings.Contains(string(objective), "no auto-integrate") && strings.Contains(prompt, "A condition the owner set is a condition") && strings.Contains(prompt, "orca pr update <pr>")
		if c := regexp.MustCompile(`merge PR #(\d+) only after (.+) is read back clean`).FindStringSubmatchIndex(events); byHand && c != nil &&
			strings.Contains(events[c[1]:], events[c[4]:c[5]]+" read back clean") {
			pr := events[c[2]:c[3]]
			switch ready := act("pr", "ready", pr); {
			case ready == "ok":
				act("pr", "merge", pr, "--action-key", "merge-"+pr)
			case strings.Contains(ready, "stale base"):
				act("pr", "update", pr, "--action-key", "update-"+pr)
			}
		}
		act("wait", "--timeout", "50ms")
	})
}

// Integration by hand: once the readback row meets the owner's condition at a
// green exact head, the dispatcher readies and merges PR #7; with a 3 h old
// base it updates the branch first and does not merge yet.
func TestShellDispatcherIntegratesByHand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		want    []string
		baseAge time.Duration
	}{
		{"fresh base", []string{"pr ready 7 -> ok", "pr merge 7 --action-key merge-7 -> ok"}, 10 * time.Minute},
		{"stale base", []string{"pr ready 7 -> orca toolbox refused: PR #7 is not ready at", "pr update 7 --action-key update-7 -> ok"}, 3 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live, scripts, _ := handRun(t, tc.baseAge)
			fake := t.TempDir()
			_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, scripts, "TestFakeIntegrateClaude", "ORCA_FAKE_INTEGRATE_DIR="+shq(fake)+" ORCA_SNL_DIR="+shq(scripts))
			var actions []string
			waitFor(t, func() bool {
				raw, _ := os.ReadFile(filepath.Join(fake, "actions"))
				actions = strings.Split(strings.TrimSpace(string(raw)), "\n")
				return slices.ContainsFunc(actions, func(a string) bool { return strings.HasPrefix(a, "wait ") })
			})
			if len(actions) != 3 || !strings.HasPrefix(actions[0], tc.want[0]) || actions[1] != tc.want[1] {
				t.Fatalf("actions = %q, want %q then wait", actions, tc.want)
			}
			raw, _ := os.ReadFile(filepath.Join(intent, "actions.jsonl"))
			if merged, updated := strings.Contains(string(raw), `"action":"pr.merge"`), strings.Contains(string(raw), `"action":"pr.update"`); merged != (tc.baseAge < time.Hour) || updated == merged {
				t.Fatalf("shadow intents = %s", raw)
			}
			if tc.baseAge > time.Hour && !strings.Contains(actions[0], "stale base: the oldest origin/main commit missing from") {
				t.Fatalf("ready refusal = %q, want the stale base", actions[0])
			}
		})
	}
}

// The fake hold session follows its system prompt: on a run whose notes hold
// every lane's advance, it holds each lane that lacks a hold, then waits.
func TestFakeHoldClaude(t *testing.T) {
	dir := os.Getenv("ORCA_FAKE_HOLD_DIR")
	if dir == "" {
		return
	}
	var prompt string
	if i := slices.Index(os.Args, "--system-prompt"); i > 0 && i+1 < len(os.Args) {
		prompt = os.Args[i+1]
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
		run := os.Getenv("ORCA_TOOLBOX_DIR")
		objective, _ := os.ReadFile(filepath.Join(run, "OBJECTIVE.md"))
		var s struct{ Tasks []struct{ ID string } }
		raw, _ := os.ReadFile(filepath.Join(run, "state.json"))
		_ = json.Unmarshal(raw, &s)
		for _, ln := range s.Tasks {
			if _, err := os.Stat(filepath.Join(run, ln.ID+".hold-advance")); err != nil && strings.Contains(string(objective), "hold-advance on every lane") && strings.Contains(prompt, "run orca lane hold <lane>") {
				act("lane", "hold", ln.ID, "--action-key", "hold-"+ln.ID)
			}
		}
		act("wait", "--timeout", "50ms")
	})
}

// A run whose notes hold every lane: the dispatcher holds L1, which lacks a
// hold, and not L2, which has one. In shadow the hold is an intent only.
func TestShellDispatcherHoldsEveryLane(t *testing.T) {
	t.Parallel()
	live, scripts, _ := handRun(t, 10*time.Minute)
	write(t, filepath.Join(live, "OBJECTIVE.md"), "## Notes\n- No auto-integrate: hold-advance on every lane; the orchestrator advances and merges by hand.\n")
	write(t, filepath.Join(live, "L2.hold-advance"), "run rule\n")
	fake := t.TempDir()
	_, intent := superviseFake(t, store.NewMemoryStore(clock.RealClock{}), live, scripts, "TestFakeHoldClaude", "ORCA_FAKE_HOLD_DIR="+shq(fake)+" ORCA_SNL_DIR="+shq(scripts))
	var actions []string
	waitFor(t, func() bool {
		raw, _ := os.ReadFile(filepath.Join(fake, "actions"))
		actions = strings.Split(strings.TrimSpace(string(raw)), "\n")
		return slices.ContainsFunc(actions, func(a string) bool { return strings.HasPrefix(a, "wait ") })
	})
	if len(actions) != 2 || actions[0] != "lane hold L1 --action-key hold-L1 -> ok" {
		t.Fatalf("actions = %q, want lane hold L1 then wait", actions)
	}
	raw, _ := os.ReadFile(filepath.Join(intent, "actions.jsonl"))
	if !strings.Contains(string(raw), `"action":"lane.hold","arguments":`) || !strings.Contains(string(raw), `"lane":"L1"`) || strings.Contains(string(raw), `"lane":"L2"`) {
		t.Fatalf("shadow intents = %s", raw)
	}
	if _, err := os.Stat(filepath.Join(live, "L1.hold-advance")); !os.IsNotExist(err) {
		t.Fatalf("shadow hold reached the live run: %v", err)
	}
}

// A by-hand run holds L1 and no gate recorded its review PASS: approve
// records it when the report passes at the PR head, so ready and merge
// succeed. A report at another head or a verdict that is not pass is refused.
func TestApproveVerifiedPassUnderHoldAdvance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, head, verdict, refusal string }{
		{name: "pass at PR head", verdict: "pass"},
		{name: "report at another head", head: strings.Repeat("a", 40), verdict: "pass", refusal: "is not PR head"},
		{name: "verdict fail", verdict: "fail", refusal: "not pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live, scripts, calls := handRun(t, 10*time.Minute)
			prHead, _ := os.ReadFile(filepath.Join(scripts, "pr-head"))
			head := strings.TrimSpace(string(prHead))
			write(t, filepath.Join(live, "L1.hold-advance"), "run rule: hold-advance on every lane\n")
			write(t, filepath.Join(live, "L1.review.md"), "Outcome: done\nHEAD: "+cmp.Or(tc.head, head)+"\nVerdict: "+tc.verdict+"\n")
			raw, _ := os.ReadFile(filepath.Join(live, "state.json"))
			write(t, filepath.Join(live, "state.json"), strings.Replace(string(raw), `"approval_sha":"`+head+`"`, `"approval_sha":""`, 1))
			pr := func(args ...string) error {
				return toolbox.Run(t.Context(), append([]string{"pr"}, append(args, "7", "--dir", live, "--skill", scripts)...), io.Discard)
			}
			approve, ready, merge := pr("approve"), pr("ready"), pr("merge")
			got, _ := os.ReadFile(calls)
			wantHandMerge := "pr_merge --repo o/r --head " + head
			if tc.refusal != "" {
				if !errors.Is(approve, toolbox.ErrRefused) || !strings.Contains(approve.Error(), tc.refusal) || !errors.Is(ready, toolbox.ErrRefused) || !errors.Is(merge, toolbox.ErrRefused) || len(got) != 0 {
					t.Fatalf("approve = %v, ready = %v, merge = %v, calls = %q; want all refused, approve with %q", approve, ready, merge, got, tc.refusal)
				}
				return
			}
			if approve != nil || ready != nil || merge != nil || strings.Contains(string(got), "merge L1") || !strings.Contains(string(got), wantHandMerge) {
				t.Fatalf("approve = %v, ready = %v, merge = %v, calls = %q", approve, ready, merge, got)
			}
		})
	}
}
