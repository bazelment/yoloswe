//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/bramble/session"
)

// requireTool skips the test when a required binary is not installed, rather
// than failing: these tests are about bramble, not about the developer's box.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

// brambleBinary locates the bramble under test.
//
// $BRAMBLE_BIN wins so a developer can point at a binary they just built;
// otherwise it comes from the bazel runfiles the BUILD file wires in as data.
// A stale bramble on PATH is deliberately NOT a fallback — that has already
// cost a debugging session or two.
func brambleBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("BRAMBLE_BIN"); bin != "" {
		require.FileExistsf(t, bin, "$BRAMBLE_BIN does not exist")
		return bin
	}
	if srcDir := os.Getenv("TEST_SRCDIR"); srcDir != "" {
		for _, candidate := range []string{
			filepath.Join(srcDir, "_main", "bramble", "bramble_", "bramble"),
			filepath.Join(srcDir, "bramble", "bramble_", "bramble"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	t.Skip("no bramble binary: run under bazel, or set $BRAMBLE_BIN")
	return ""
}

// seedRepo builds the worktree layout bramble expects — <root>/<repo>/.bare
// plus a checked-out worktree — and returns the worktree path.
func seedRepo(t *testing.T, wtRoot string) string {
	t.Helper()

	repoDir := filepath.Join(wtRoot, repoName)
	bare := filepath.Join(repoDir, ".bare")
	worktree := filepath.Join(repoDir, "main")
	seed := filepath.Join(t.TempDir(), "seed")

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// A developer's global hooks or signing config can otherwise make
		// these commands fail in ways that have nothing to do with the test.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=bramble-it", "GIT_AUTHOR_EMAIL=it@example.com",
			"GIT_COMMITTER_NAME=bramble-it", "GIT_COMMITTER_EMAIL=it@example.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}

	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	require.NoError(t, os.MkdirAll(seed, 0o755))
	git("", "init", "--bare", "--initial-branch=main", bare)

	// A bare repo has no index to commit into, so the first commit is pushed
	// in from a scratch clone.
	git(seed, "init", "--initial-branch=main", ".")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "README.md"), []byte("subagent it\n"), 0o644))
	git(seed, "add", "README.md")
	git(seed, "commit", "-m", "seed")
	git(seed, "remote", "add", "origin", bare)
	git(seed, "push", "origin", "main")

	git(bare, "worktree", "add", worktree, "main")
	return worktree
}

// stubAgentScript stands in for an agent CLI running in a tmux pane.
//
// It is deliberately faithful about the two things bramble depends on and
// nothing else: it answers whatever is typed at it, and it runs the notify
// program bramble passes via `-c notify=[...]` when a turn ends. That notify
// hook is the only reason bramble ever learns a tmux session went idle — the
// gap that left real codex sessions stuck at "running" forever.
//
// It stays on stdin afterwards, like a real REPL, so the window survives and
// can take follow-ups.
const stubAgentScript = `#!/usr/bin/env bash
# Scripted stand-in for an agent CLI. See support_test.go.
set -u

notify=()
prompt=""
while [ $# -gt 0 ]; do
  case "$1" in
    -c)
      case "${2:-}" in
        notify=*)
          raw="${2#notify=}"; raw="${raw#[}"; raw="${raw%]}"
          IFS=',' read -ra parts <<< "$raw"
          for p in "${parts[@]}"; do p="${p%\"}"; p="${p#\"}"; notify+=("$p"); done
          ;;
      esac
      shift 2 ;;
    --model|--profile|--settings) shift 2 ;;
    --*) shift ;;
    *) prompt="$1"; shift ;;
  esac
done

log() {
  [ -n "${BRAMBLE_IT_STUB_LOG:-}" ] && printf '%s\n' "$*" >> "$BRAMBLE_IT_STUB_LOG"
  return 0
}

log "argv-parsed notify=${notify[*]:-<none>} prompt=${prompt:-<none>}"
log "env BRAMBLE_SOCK=${BRAMBLE_SOCK:-<unset>} BRAMBLE_SESSION_ID=${BRAMBLE_SESSION_ID:-<unset>}"

respond() {
  printf '> %s\n' "$1"
  printf 'STUB-REPLY %s\n' "$1"
  if [ ${#notify[@]} -gt 0 ]; then
    "${notify[@]}" >/dev/null 2>&1 || true
    # bramble passes --silent, which makes the hook swallow its own errors, so
    # a failure would look identical to success. Repeat it without --silent
    # purely for the log; notify is idempotent, so the extra call is harmless.
    diag=()
    for a in "${notify[@]}"; do [ "$a" = "--silent" ] || diag+=("$a"); done
    out=$("${diag[@]}" 2>&1); rc=$?
    log "notify rc=$rc out=${out}"
  else
    log "notify SKIPPED: no notify program was passed"
  fi
}

# A real agent CLI takes seconds to boot before it can answer. This stand-in
# would otherwise reply in under a millisecond — inside the window where
# runner.Start() has not yet recorded the tmux window id, so the pane capture
# that gives a subagent report its "result:" path has nothing to read yet.
# Sleep long enough to be realistic rather than pathological. (The related
# status-clobber race that this once exposed is fixed in runSession and pinned
# by TestFastIdleIsNotClobberedByStartup.)
sleep 0.5

if [ -n "$prompt" ]; then respond "$prompt"; fi

# Act like a prompt: one turn per submitted line. STUB-EXIT ends the process
# cleanly (status 0), which is how bramble sees a session complete normally as
# opposed to the window being killed, which it reports as a failure.
while IFS= read -r line; do
  case "$line" in
    STUB-EXIT) log "exiting cleanly"; exit 0 ;;
  esac
  [ -n "$line" ] && respond "$line"
done
`

// installStubAgent writes the stand-in as `codex` into a fresh directory and
// returns that directory, for prepending to PATH.
//
// It masquerades as codex because bramble picks a backend from the model ID,
// so a `gpt-*` model routes to whatever binary named `codex` is first on PATH.
func installStubAgent(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "stubbin")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(path, []byte(stubAgentScript), 0o755))
	return dir
}

// requireDecode re-marshals a decoded-to-any IPC result into its typed form.
// ipc.Response.Result is any, so it arrives as a map.
func requireDecode(t *testing.T, from any, into any) {
	t.Helper()
	raw, err := json.Marshal(from)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, into))
}

// shellQuote wraps s for safe use inside the `sh -c` string that launches
// bramble.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// exportOf turns a KEY=VALUE pair into a shell `export`, quoting only the
// value so the name stays a valid identifier.
func exportOf(kv string) string {
	name, value, found := strings.Cut(kv, "=")
	if !found {
		return "export " + kv + "=''; "
	}
	return "export " + name + "=" + shellQuote(value) + "; "
}

// liveBackend describes one real agent CLI the subagent path must work with.
type liveBackendSpec struct {
	// provider is bramble's name for it, used only in messages.
	provider string
	// binary is what has to be on PATH.
	binary string
	// model routes bramble to this backend; the model ID is the only thing
	// that selects a backend.
	model string
	// authProbe runs non-interactively and succeeds only when the CLI is
	// logged in. A logged-out CLI does not fail — it sits on a login prompt
	// forever — so this is checked up front rather than discovered as a
	// mysterious timeout.
	authProbe []string
	// authWant, when set, must appear in the probe's output.
	authWant string
	// envOverride names an env var that replaces the model, for trying a
	// different one without editing the test.
	envOverride string
}

// liveBackends is every backend these tests drive for real. They run by
// default — the whole point is to exercise the actual CLIs — and skip only when
// one is genuinely unavailable on this machine.
var liveBackends = []liveBackendSpec{
	{
		provider: "claude", binary: "claude", model: "sonnet",
		authProbe:   []string{"claude", "--version"},
		envOverride: "BRAMBLE_IT_CLAUDE_MODEL",
	},
	{
		provider: "codex", binary: "codex", model: "gpt-5.5",
		authProbe: []string{"codex", "login", "status"}, authWant: "Logged in",
		envOverride: "BRAMBLE_IT_CODEX_MODEL",
	},
	{
		provider: "cursor", binary: "cursor-agent", model: "cursor-default",
		authProbe: []string{"cursor-agent", "status"}, authWant: "Logged in",
		envOverride: "BRAMBLE_IT_CURSOR_MODEL",
	},
}

// resolve returns the model to use, honouring the env override.
func (b liveBackendSpec) resolve() string {
	if m := os.Getenv(b.envOverride); m != "" {
		return m
	}
	return b.model
}

// require skips the test when this backend cannot be driven on this machine.
func (b liveBackendSpec) require(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath(b.binary); err != nil {
		t.Skipf("%s is not installed", b.binary)
	}
	if len(b.authProbe) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, b.authProbe[0], b.authProbe[1:]...).CombinedOutput()
		if err != nil {
			t.Skipf("%s is not usable (%v): %s", b.binary, err, out)
		}
		if b.authWant != "" && !strings.Contains(string(out), b.authWant) {
			t.Skipf("%s is not logged in: %s", b.binary, out)
		}
	}
	return b.resolve()
}

// dumpPanesOnFailure logs the sessions' panes, which is the only record of
// what an agent CLI actually did.
func dumpPanesOnFailure(t *testing.T, h *harness, ids ...session.SessionID) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, id := range ids {
			t.Logf("--- pane %s ---\n%s", id, h.pane(id))
		}
	})
}

// shortTempDir returns a directory shallow enough to hold unix sockets, whose
// paths are capped at ~107 bytes.
//
// bazel's test tmpdir plus a descriptive test name already exceeds that, and
// the symptoms point nowhere near the cause: tmux says "File name too long",
// while bramble simply never appears on the sockets the harness is watching
// for.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bit")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Leave room for the longest name that goes under here:
	// run/bramble-control-<pid>.sock
	require.Less(t, len(dir), 60, "temp root is too long to hold unix sockets")
	return dir
}
