//go:build integration

// Package integration drives a real bramble binary, in tmux mode, on a real
// (throwaway) git repo, and exercises the subagent path end to end: lineage,
// automatic reporting to a parent, and queued delivery in both directions.
//
// These are the manual reproductions of the bugs that only appear once a real
// CLI is running in a real pane — a paste dropped while the agent's TUI is
// still finalizing, an Enter eaten by tmux copy mode, a session whose status
// never leaves "idle" so its next turn produces no state change. None of them
// are visible from unit tests, and all three silently broke subagent messaging.
//
// Everything is isolated: a private tmux server (`tmux -S`), a private HOME so
// the delivery queue and session store are the test's own, a private
// XDG_RUNTIME_DIR so socket discovery cannot find another bramble, and a
// throwaway worktree repo. Nothing here touches the developer's tmux session,
// their ~/.bramble, or their real repos.
//
// Run them with:
//
//	bazel test //bramble/integration:integration_test --test_output=all
//
// They are tagged manual and never run in CI: they need tmux, a built bramble
// binary, and (for the live-backend cases) a logged-in agent CLI.
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/control"
	"github.com/bazelment/yoloswe/bramble/ipc"
	"github.com/bazelment/yoloswe/bramble/session"
)

const (
	// repoName is the throwaway repo every test operates on.
	repoName = "subagentrepo"
	// settleTimeout bounds waits on an agent CLI reacting. Generous: a real
	// backend has to answer a prompt inside it.
	settleTimeout = 90 * time.Second
	// pollInterval is how often those waits re-check.
	pollInterval = 250 * time.Millisecond
)

// harness is one isolated bramble under test.
type harness struct {
	t            *testing.T
	tmuxSocket   string
	worktreePath string
	ipcSock      string
	controlSock  string
	home         string
	stubLog      string
	// lastWorktreePath is the path returned by the most recent
	// spawnOnNewWorktree, so a test can assert on what bramble actually made.
	lastWorktreePath string
	// answeredDialogs records the screen each dialog was answered on, so one
	// appearance collects one answer.
	answeredDialogs map[string]string
}

// newHarness brings up a private tmux server, a throwaway repo, and a bramble
// running in tmux mode against them, and tears it all down afterwards.
//
// stubAgent selects the backend: true installs a scripted stand-in for the
// `codex` binary so the test is deterministic and needs no credentials; false
// leaves the real CLIs on PATH for the live-backend cases.
func newHarness(t *testing.T, stubAgent bool) *harness {
	t.Helper()
	requireTool(t, "tmux")
	requireTool(t, "git")
	brambleBin := brambleBinary(t)

	root := t.TempDir()
	// A unix socket path is capped at ~107 bytes, and bazel's tmpdir plus a
	// descriptive test name blows straight through that. Both the tmux socket
	// and bramble's own pid-scoped sockets — which land in XDG_RUNTIME_DIR —
	// have to live somewhere shallow. Everything else can use the long path.
	shortRoot := shortTempDir(t)
	h := &harness{
		t:               t,
		tmuxSocket:      filepath.Join(shortRoot, "t.sock"),
		answeredDialogs: map[string]string{},
	}
	wtRoot := filepath.Join(root, "worktrees")
	runtimeDir := filepath.Join(shortRoot, "run")

	// The live-backend cases must keep the developer's real HOME: an agent CLI
	// reads its credentials from there, and a logged-out CLI hangs on an
	// interactive prompt rather than failing. The stubbed cases get a private
	// HOME so the delivery queue and session store are the test's own.
	h.home = os.Getenv("HOME")
	if stubAgent {
		h.home = filepath.Join(root, "home")
	}
	for _, dir := range []string{h.home, wtRoot, runtimeDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	h.worktreePath = seedRepo(t, wtRoot)

	h.stubLog = filepath.Join(root, "stub.log")
	env := []string{
		"HOME=" + h.home,
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"WT_ROOT=" + wtRoot,
		"TERM=xterm-256color",
		// The stand-in records what it parsed and what its notify program
		// returned. Without it a failed notify is invisible: bramble passes
		// --silent, so the hook swallows its own errors by design.
		"BRAMBLE_IT_STUB_LOG=" + h.stubLog,
	}
	pathDirs := os.Getenv("PATH")
	if stubAgent {
		// Prepended, so bramble's provider probe and its tmux windows both
		// find the stand-in rather than a real codex.
		pathDirs = installStubAgent(t, root) + string(os.PathListSeparator) + pathDirs
	}
	env = append(env, "PATH="+pathDirs)

	// PATH cannot ride `tmux -e`: tmux special-cases it and silently drops the
	// override, so the whole environment goes through a shell wrapper instead.
	// (The same trap is documented in fleet/dispatch.go.)
	var exports strings.Builder
	for _, kv := range env {
		exports.WriteString(exportOf(kv))
	}
	cmd := fmt.Sprintf("%sexec %s --repo %s --session-mode tmux --yolo",
		exports.String(), shellQuote(brambleBin), shellQuote(repoName))

	out, err := exec.Command("tmux", "-S", h.tmuxSocket, "new-session", "-d",
		"-x", "200", "-y", "50", "-c", h.worktreePath, cmd).CombinedOutput()
	require.NoError(t, err, "start bramble under tmux: %s", out)

	// A window bramble opens inherits PATH from bramble itself (its tmux
	// client), which is how the stand-in gets found — but not arbitrary
	// variables. Those have to go on the server. PATH deliberately does not:
	// tmux special-cases it here and the override is silently dropped, which
	// is why it rides the shell wrapper above instead.
	for _, kv := range []string{"BRAMBLE_IT_STUB_LOG=" + h.stubLog} {
		name, value, _ := strings.Cut(kv, "=")
		_, _ = h.tmux("set-environment", "-g", name, value)
	}

	t.Cleanup(func() {
		// Dump the TUI window on failure: a bramble that refused to start
		// (unknown repo, no provider) says so there and nowhere else.
		if t.Failed() {
			if pane, err := exec.Command("tmux", "-S", h.tmuxSocket,
				"capture-pane", "-p", "-t", "@0").Output(); err == nil {
				t.Logf("bramble TUI pane:\n%s", strings.TrimRight(string(pane), "\n \t"))
			}
			if log, err := os.ReadFile(h.stubLog); err == nil && len(log) > 0 {
				t.Logf("stub agent log:\n%s", log)
			}
		}
		_ = exec.Command("tmux", "-S", h.tmuxSocket, "kill-server").Run()
	})

	h.awaitSockets(runtimeDir)
	return h
}

// awaitSockets waits for bramble to bind both of its sockets and answer a ping.
// The paths are pid-scoped with no well-known name, so they are discovered by
// globbing the private runtime dir — which can only ever hold this bramble.
func (h *harness) awaitSockets(runtimeDir string) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		socks, _ := filepath.Glob(filepath.Join(runtimeDir, "bramble-*.sock"))
		for _, s := range socks {
			if strings.Contains(filepath.Base(s), "control") {
				h.controlSock = s
				continue
			}
			h.ipcSock = s
		}
		if h.ipcSock == "" || h.controlSock == "" {
			return false
		}
		return ipc.NewClient(h.ipcSock).Ping() == nil
	}, settleTimeout, pollInterval, "bramble never came up on its sockets")
}

// --- talking to the bramble under test ---------------------------------------

// spawn creates a session and returns its ID. A parent of "" is a top-level
// session; anything else makes the new session that session's subagent.
func (h *harness) spawn(sessionType, model, parent, prompt string) session.SessionID {
	h.t.Helper()
	resp, err := ipc.NewClient(h.ipcSock).Send(&ipc.Request{
		Type: ipc.RequestNewSession,
		ID:   "it-new-session",
		Params: &ipc.NewSessionParams{
			SessionType:     sessionType,
			WorktreePath:    h.worktreePath,
			Model:           model,
			RepoName:        repoName,
			ParentSessionID: parent,
			Prompt:          prompt,
		},
	})
	require.NoError(h.t, err)
	require.True(h.t, resp.OK, "new-session failed: %s", resp.Error)

	var result ipc.NewSessionResult
	requireDecode(h.t, resp.Result, &result)
	require.NotEmpty(h.t, result.SessionID)
	return session.SessionID(result.SessionID)
}

// spawnOnNewWorktree creates a subagent with a worktree of its own rather than
// its parent's, the way `new-session --create-worktree -b <branch>` does.
//
// It deliberately passes no worktree path: that is what makes bramble create
// one, and it is also what would silently fall back to inheriting the parent's
// tree if worktree creation stopped happening.
func (h *harness) spawnOnNewWorktree(sessionType, model, parent, branch, base, prompt string) session.SessionID {
	h.t.Helper()
	resp, err := ipc.NewClient(h.ipcSock).Send(&ipc.Request{
		Type: ipc.RequestNewSession,
		ID:   "it-new-session-wt",
		Params: &ipc.NewSessionParams{
			SessionType:     sessionType,
			Branch:          branch,
			BaseBranch:      base,
			CreateWorktree:  true,
			Model:           model,
			RepoName:        repoName,
			ParentSessionID: parent,
			Prompt:          prompt,
		},
	})
	require.NoError(h.t, err)
	require.Truef(h.t, resp.OK, "new-session --create-worktree failed: %s", resp.Error)

	var result ipc.NewSessionResult
	requireDecode(h.t, resp.Result, &result)
	require.NotEmpty(h.t, result.SessionID)
	require.NotEmptyf(h.t, result.WorktreePath, "no worktree path came back for branch %s", branch)
	h.lastWorktreePath = result.WorktreePath
	return session.SessionID(result.SessionID)
}

// gitIn runs a read-only git command inside a worktree.
func (h *harness) gitIn(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(h.t, err, "git %s in %s: %s", strings.Join(args, " "), dir, out)
	return strings.TrimSpace(string(out))
}

// sessions returns every session bramble currently knows about.
func (h *harness) sessions() []ipc.SessionSummary {
	h.t.Helper()
	resp, err := ipc.NewClient(h.ipcSock).Send(&ipc.Request{
		Type: ipc.RequestListSessions, ID: "it-list",
	})
	require.NoError(h.t, err)
	require.True(h.t, resp.OK, "list-sessions failed: %s", resp.Error)

	var result ipc.ListSessionsResult
	requireDecode(h.t, resp.Result, &result)
	return result.Sessions
}

// status returns one session's status, or "" if bramble has never heard of it.
func (h *harness) status(id session.SessionID) string {
	h.t.Helper()
	for _, s := range h.sessions() {
		if s.ID == string(id) {
			return s.Status
		}
	}
	return ""
}

// awaitStatus waits for a session to reach one of the given statuses.
func (h *harness) awaitStatus(id session.SessionID, want ...string) {
	h.t.Helper()
	require.Eventuallyf(h.t, func() bool {
		got := h.status(id)
		for _, w := range want {
			if got == w {
				return true
			}
		}
		return false
	}, settleTimeout, pollInterval, "session %s never reached %v (last: %s)", id, want, h.status(id))
}

// send delivers text to a session over the control plane. queue holds it until
// the recipient is idle instead of typing into a live turn.
func (h *harness) send(from, to session.SessionID, text string, queue bool) (control.SendInputResult, error) {
	h.t.Helper()
	req, err := control.NewRequest(control.TypeSessionSendInput, "it-send",
		control.SendInputReq{
			SessionID: string(to), From: string(from),
			Text: text, Submit: true, Queue: queue,
		})
	require.NoError(h.t, err)

	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	resp, err := control.Request(ctx, h.controlSock, req)
	require.NoError(h.t, err)

	var result control.SendInputResult
	if err := resp.DecodeResponse(&result); err != nil {
		return control.SendInputResult{}, err
	}
	return result, nil
}

// pane returns a session's captured pane text as one string.
func (h *harness) pane(id session.SessionID) string {
	h.t.Helper()
	resp, err := ipc.NewClient(h.ipcSock).Send(&ipc.Request{
		Type:   ipc.RequestCapturePane,
		ID:     "it-capture",
		Params: &ipc.CapturePaneParams{SessionID: string(id), Lines: 200},
	})
	if err != nil || !resp.OK {
		return ""
	}
	var result ipc.CapturePaneResult
	requireDecode(h.t, resp.Result, &result)
	return strings.Join(result.Lines, "\n")
}

// awaitPane waits for a session's pane to contain want.
func (h *harness) awaitPane(id session.SessionID, want, because string) {
	h.t.Helper()
	require.Eventuallyf(h.t, func() bool {
		return strings.Contains(h.pane(id), want)
	}, settleTimeout, pollInterval, "%s: %q never appeared in %s's pane\n--- pane ---\n%s",
		because, want, id, h.pane(id))
}

// countInPane reports how many times want appears in a session's pane.
func (h *harness) countInPane(id session.SessionID, want string) int {
	h.t.Helper()
	return strings.Count(h.pane(id), want)
}

// tmuxTargetOf resolves a session's tmux window, for tests that need to poke
// the pane directly rather than going through bramble.
func (h *harness) tmuxTargetOf(id session.SessionID) string {
	h.t.Helper()
	req, err := control.NewRequest(control.TypeSessionList, "it-sessions", nil)
	require.NoError(h.t, err)
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	resp, err := control.Request(ctx, h.controlSock, req)
	require.NoError(h.t, err)

	var result control.SessionListResult
	require.NoError(h.t, resp.DecodeResponse(&result))
	for _, s := range result.Sessions {
		if s.ID == string(id) {
			return s.TmuxTarget
		}
	}
	h.t.Fatalf("session %s has no tmux target", id)
	return ""
}

// tmux runs a tmux command against the harness's private server.
func (h *harness) tmux(args ...string) (string, error) {
	h.t.Helper()
	full := append([]string{"-S", h.tmuxSocket}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// deliveryQueueLen counts the queue files bramble is holding. The queue lives
// under the harness's private HOME, so this only ever sees this test's mail.
func (h *harness) deliveryQueueLen() int {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.home, ".bramble", "deliveries", "*.json"))
	return len(files)
}

// startupDialog is a prompt an agent CLI puts in front of its own prompt, which
// a human would otherwise have to click through.
//
// These are the reason a "live" test is not simply a matter of spawning a
// session and waiting: a fresh worktree makes Claude ask whether the folder is
// trusted, and codex interrupts with model-deprecation and rate-limit choices.
// Left unanswered the session never reaches its prompt and the test times out
// looking like a bramble bug.
type startupDialog struct {
	name string
	// match must all appear in the pane for this dialog to be recognized.
	// Specific on purpose — answering the wrong modal picks a menu item.
	match []string
	// keys are sent, in order, to answer it.
	keys []string
}

// startupDialogs is the set answered automatically. Each entry names the choice
// it takes, because pressing Enter on an unknown menu is how a test silently
// changes a setting (codex's rate-limit prompt switches model on Enter).
var startupDialogs = []startupDialog{
	{
		// Claude, on a directory it has not seen before. Option 1, the
		// default, is "Yes, I trust this folder".
		name:  "claude folder trust",
		match: []string{"Is this a project you created or one you trust", "I trust this folder"},
		keys:  []string{"Enter"},
	},
	{
		// Codex, on a directory it has not seen before. Option 1, the default,
		// is "Yes, continue".
		name:  "codex directory trust",
		match: []string{"Do you trust the contents of this directory", "Yes, continue"},
		keys:  []string{"Enter"},
	},
	{
		// Codex, when the requested model is being retired. Option 2 keeps the
		// model the test asked for; taking the new one silently would make the
		// run untraceable to the model under test.
		name:  "codex model deprecation",
		match: []string{"will be deprecated soon", "Use existing model"},
		keys:  []string{"Down", "Enter"},
	},
	{
		// Codex, near a usage limit. Escape dismisses without switching model.
		name:  "codex rate-limit switch",
		match: []string{"Approaching rate limits", "Switch to"},
		keys:  []string{"Escape"},
	},
}

// dialogTailLines is how much of the pane's bottom a dialog is looked for in.
//
// Not the whole capture: a dialog's text stays in the scrollback after it is
// dismissed, so matching the full pane re-recognizes one that is long gone —
// and each "answer" is a keystroke into whatever has since taken its place.
const dialogTailLines = 30

// paneTail returns the last few non-empty lines of a session's pane.
func (h *harness) paneTail(id session.SessionID) string {
	h.t.Helper()
	lines := strings.Split(h.pane(id), "\n")
	kept := make([]string, 0, dialogTailLines)
	for i := len(lines) - 1; i >= 0 && len(kept) < dialogTailLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append(kept, lines[i])
	}
	return strings.Join(kept, "\n")
}

// answerStartupDialogs looks at what is currently on a session's screen and
// answers any dialog it recognizes. It reports whether it acted.
//
// A given dialog is answered once and then not again until it has disappeared
// from the screen entirely. Keying on the screen's contents instead does not
// work: a live TUI repaints a spinner and a token count every poll, so no two
// captures are equal and every poll looks like a new dialog. Without this a run
// sent Enter twenty-two times into a live session — a dismissed dialog stays in
// the scrollback just above the prompt that replaced it.
func (h *harness) answerStartupDialogs(id session.SessionID) bool {
	h.t.Helper()
	tail := h.paneTail(id)
	if tail == "" {
		return false
	}
	for _, d := range startupDialogs {
		matched := true
		for _, m := range d.match {
			if !strings.Contains(tail, m) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		key := string(id) + "\x00" + d.name
		if h.answeredDialogs[key] != "" {
			return false // already answered; waiting for it to go away
		}

		target := h.tmuxTargetOf(id)
		h.t.Logf("answering %q dialog in %s with %v", d.name, id, d.keys)
		for _, k := range d.keys {
			if _, err := h.tmux("send-keys", "-t", target, k); err != nil {
				h.t.Logf("failed to answer %q: %v", d.name, err)
				return false
			}
			time.Sleep(300 * time.Millisecond)
		}
		h.answeredDialogs[key] = "answered"
		return true
	}

	// Nothing matched: any dialog previously answered is gone, so a fresh
	// appearance of it may be answered again.
	for _, d := range startupDialogs {
		delete(h.answeredDialogs, string(id)+"\x00"+d.name)
	}
	return false
}

// awaitReady waits for a session to reach its prompt, answering first-run
// dialogs along the way. This is what a live test uses instead of awaitStatus:
// a real CLI on a fresh worktree may need a click before it ever runs a turn.
func (h *harness) awaitReady(id session.SessionID) {
	h.t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if h.status(id) == "idle" {
			return
		}
		h.answerStartupDialogs(id)
		time.Sleep(pollInterval)
	}
	h.t.Fatalf("session %s never reached its prompt\n--- pane ---\n%s", id, h.pane(id))
}

// awaitPaneClearingDialogs waits for text to appear, answering any dialog that
// blocks the agent on the way.
func (h *harness) awaitPaneClearingDialogs(id session.SessionID, want, because string) {
	h.t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(h.pane(id), want) {
			return
		}
		h.answerStartupDialogs(id)
		time.Sleep(pollInterval)
	}
	h.t.Fatalf("%s: %q never appeared in %s's pane\n--- pane ---\n%s",
		because, want, id, h.pane(id))
}

// longTurnSeconds is how long a "keep busy" prompt occupies an agent. Long
// enough that a queued message is unambiguously held across a live turn, short
// enough not to dominate the suite.
const longTurnSeconds = 20

// longTurnPrompt asks an agent to occupy itself for a bounded, predictable
// time and then say a known word.
//
// It shells out rather than asking for a long answer because generated text is
// not a reliable clock — a model told to "count slowly" may emit the whole list
// at once, leaving no live turn to queue behind. A sleep is the same length on
// every backend. Requires a session type that may run commands (builder).
func longTurnPrompt(done string) string {
	return fmt.Sprintf(
		"Run this exact shell command and wait for it to finish: sleep %d. "+
			"Then reply with exactly one line and nothing else: %s. "+
			"Do not read or edit any files.", longTurnSeconds, done)
}

// awaitWorking waits until a session is demonstrably mid-turn: bramble reports
// it running and its pane shows it has taken the prompt.
//
// Both halves matter. Status alone is true from the moment the session is
// created, so a test that queued on status alone could be queueing before the
// CLI had even started — which is a different case, and not the one under test.
func (h *harness) awaitWorking(id session.SessionID, promptEcho string) {
	h.t.Helper()
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		if h.status(id) == "running" && strings.Contains(h.pane(id), promptEcho) {
			return
		}
		h.answerStartupDialogs(id)
		time.Sleep(pollInterval)
	}
	h.t.Fatalf("session %s never started working\n--- pane ---\n%s", id, h.pane(id))
}

// reportedResultPath pulls the path out of the most recent report in a pane.
//
// The report is a pointer, not a payload, so the path is the part a parent
// actually has to be able to use; asserting only that "result:" appears would
// pass on a report naming a file that was never written.
func reportedResultPath(pane string) (string, bool) {
	matches := resultPathRE.FindAllStringSubmatch(pane, -1)
	if len(matches) == 0 {
		return "", false
	}
	return matches[len(matches)-1][1], true
}

var resultPathRE = regexp.MustCompile(`result:\s+(\S+)`)

// queuedTextFor returns the raw on-disk queue for a recipient, which is what a
// restarted bramble would read back.
func (h *harness) queuedTextFor(to session.SessionID) string {
	h.t.Helper()
	files, _ := filepath.Glob(filepath.Join(h.home, ".bramble", "deliveries", "*.json"))
	var b strings.Builder
	for _, f := range files {
		if !strings.Contains(filepath.Base(f), string(to)) {
			continue
		}
		if data, err := os.ReadFile(f); err == nil {
			b.Write(data)
		}
	}
	return b.String()
}
