package tmuxctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/bazelment/yoloswe/bramble/session"
)

// runFunc executes a tmux invocation (args do NOT include the leading "tmux")
// and returns its stdout. Injectable so the argv-construction logic can be
// tested without a real tmux.
type runFunc func(ctx context.Context, args []string) (string, error)

// execController is the real Controller, shelling out to the tmux binary.
type execController struct {
	run        runFunc // executor; defaults to execTmux
	socketPath string  // optional `-S <path>` for an isolated tmux server (tests)
	pasteSeq   atomic.Uint64
}

// New returns a Controller backed by the default tmux server.
func New() Controller { return &execController{} }

// NewWithSocketPath returns a Controller that talks to an isolated tmux server
// addressed by an absolute socket path (`tmux -S <path>`). Used by
// integration/e2e tests to avoid touching the developer's real tmux server;
// an absolute path under a temp dir is sandbox-safe, unlike a bare `-L` name
// which resolves under $TMUX_TMPDIR.
func NewWithSocketPath(socketPath string) Controller {
	return &execController{socketPath: socketPath}
}

func (c *execController) exec(ctx context.Context, args []string) (string, error) {
	if c.run != nil {
		return c.run(ctx, args)
	}
	return execTmux(ctx, c.socketPath, args)
}

// execTmux runs the real tmux binary. The first element of args must be an
// allowlisted subcommand.
func execTmux(ctx context.Context, socketPath string, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("tmuxctl: empty tmux args")
	}
	if err := checkOp(args[0]); err != nil {
		return "", err
	}
	full := args
	cmd := exec.CommandContext(ctx, "tmux", full...)
	if socketPath != "" {
		cmd.Args = append([]string{"tmux", "-S", socketPath}, args...)
		// Drop ambient $TMUX so the explicitly-addressed server is used as the
		// client context. Otherwise commands like `list-windows -a`, whose
		// scope depends on the current client, resolve against whatever server
		// the calling process is attached to and return wrong/empty results.
		cmd.Env = envWithout(os.Environ(), "TMUX")
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmuxctl: tmux %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// --- argv builders (pure; unit-tested) --------------------------------------

// sendSpecialArgs builds `send-keys -t <target> <KeyName>` for a named key.
// Returns an error for an unknown key so unknowns never reach tmux.
func sendSpecialArgs(target string, key SpecialKey) ([]string, error) {
	k, ok := tmuxKey[key]
	if !ok {
		return nil, fmt.Errorf("tmuxctl: unknown special key %q", key)
	}
	return []string{"send-keys", "-t", target, k}, nil
}

// setBufferArgs builds `set-buffer -b <bufName> -- <text>`. A named buffer keeps
// concurrent pastes to different panes from clobbering one shared buffer.
func setBufferArgs(bufName, text string) []string {
	return []string{"set-buffer", "-b", bufName, "--", text}
}

// pasteBufferArgs builds `paste-buffer -d -p -t <target> -b <bufName>`.
//
//	-p: bracketed paste, so the receiving app treats it as pasted text (not typed
//	    keystrokes) — important so multi-line prompts are not interpreted as a
//	    series of Enter-submits by an agent TUI.
//	-d: delete the buffer after pasting.
func pasteBufferArgs(target, bufName string) []string {
	return []string{"paste-buffer", "-d", "-p", "-t", target, "-b", bufName}
}

// pasteBufferName derives a unique buffer name from the target and a per-paste
// sequence number. The target is sanitized for readability, but the trailing seq
// is what guarantees uniqueness: the sanitizing replacer is not injective (e.g.
// "@3" and "w3" both sanitize to "w3"), so without the seq two concurrent pastes
// to colliding targets could share a buffer and clobber each other between
// set-buffer and paste-buffer.
func pasteBufferName(target string, seq uint64) string {
	repl := strings.NewReplacer("$", "", "@", "w", "%", "p", ".", "_", ":", "_")
	return fmt.Sprintf("bramble-%s-%d", repl.Replace(target), seq)
}

// --- Controller: writes ------------------------------------------------------

func (c *execController) SendSpecial(ctx context.Context, target string, key SpecialKey) error {
	args, err := sendSpecialArgs(target, key)
	if err != nil {
		return err
	}
	_, err = c.exec(ctx, args)
	return err
}

// Paste delivers text to a pane via set-buffer + bracketed paste-buffer. This is
// preferred over send-keys for prompt text: no shell escaping, and bracketed
// paste prevents embedded newlines from being interpreted as submits.
func (c *execController) Paste(ctx context.Context, target, text string) error {
	buf := pasteBufferName(target, c.pasteSeq.Add(1))
	if _, err := c.exec(ctx, setBufferArgs(buf, text)); err != nil {
		return err
	}
	_, err := c.exec(ctx, pasteBufferArgs(target, buf))
	return err
}

// --- Controller: navigation / lifecycle -------------------------------------

func (c *execController) Select(ctx context.Context, target string) error {
	_, err := c.exec(ctx, []string{"select-window", "-t", target})
	return err
}

func (c *execController) NewWindow(ctx context.Context, name, cwd, cmd string) (string, error) {
	args := []string{"new-window", "-P", "-F", "#{window_id}"}
	if name != "" {
		args = append(args, "-n", name)
	}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	if cmd != "" {
		args = append(args, cmd)
	}
	out, err := c.exec(ctx, args)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (c *execController) Kill(ctx context.Context, target string) error {
	_, err := c.exec(ctx, []string{"kill-window", "-t", target})
	return err
}

// --- Controller: reads -------------------------------------------------------

func (c *execController) Capture(ctx context.Context, target string, lines int) ([]string, error) {
	if lines <= 0 {
		lines = 10
	}
	out, err := c.exec(ctx, []string{"capture-pane", "-t", target, "-p", "-J", "-S", fmt.Sprintf("-%d", lines)})
	if err != nil {
		return nil, err
	}
	var result []string
	for _, line := range strings.Split(out, "\n") {
		cleaned := strings.TrimRight(session.StripANSI(line), " ")
		if cleaned != "" {
			result = append(result, cleaned)
		}
	}
	return result, nil
}

// captureFull captures the pane from line 0 to the cursor row, returning
// ANSI-stripped lines with positional fidelity plus cursor_y. Used by Status to
// locate the Claude status bar relative to the cursor.
func (c *execController) captureFull(ctx context.Context, target string) ([]string, int, error) {
	cursorOut, err := c.exec(ctx, []string{"display-message", "-t", target, "-p", "#{cursor_y}"})
	if err != nil {
		return nil, 0, err
	}
	cursorY, err := strconv.Atoi(strings.TrimSpace(cursorOut))
	if err != nil {
		return nil, 0, fmt.Errorf("tmuxctl: parse cursor_y %q: %w", cursorOut, err)
	}
	out, err := c.exec(ctx, []string{"capture-pane", "-t", target, "-p", "-S", "0"})
	if err != nil {
		return nil, 0, err
	}
	raw := strings.Split(out, "\n")
	limit := cursorY + 1
	if limit > len(raw) {
		limit = len(raw)
	}
	lines := make([]string, limit)
	for i := 0; i < limit; i++ {
		lines[i] = strings.TrimRight(session.StripANSI(raw[i]), " ")
	}
	return lines, cursorY, nil
}

func (c *execController) Status(ctx context.Context, target string) (*session.PaneStatus, error) {
	lines, cursorY, err := c.captureFull(ctx, target)
	if err != nil {
		return nil, err
	}
	return session.ParseClaudeStatusBarWithCursor(lines, cursorY), nil
}

// fieldSep is the field delimiter for `-F` list formats. tmux sanitizes control
// bytes (tab, unit-separator) to "_" in format output when the locale is minimal
// (e.g. a sandbox or a remote shell with no LANG), which silently corrupts
// tab-delimited parsing. A printable ASCII "|" is never sanitized. Free-text
// fields (names, paths) are placed LAST and split with a limit so an embedded
// "|" cannot shift the fixed leading fields.
const fieldSep = "|"

func (c *execController) ListSessions(ctx context.Context) ([]TmuxSession, error) {
	// Free-text field (name) last.
	const fmtStr = "#{session_id}|#{session_windows}|#{session_attached}|#{session_name}"
	out, err := c.exec(ctx, []string{"list-sessions", "-F", fmtStr})
	if err != nil {
		return nil, err
	}
	var sessions []TmuxSession
	for _, line := range nonEmptyLines(out) {
		f := strings.SplitN(line, fieldSep, 4)
		if len(f) < 4 {
			continue
		}
		sessions = append(sessions, TmuxSession{
			ID:       f[0],
			Windows:  atoiOr(f[1], 0),
			Attached: f[2] != "0" && f[2] != "",
			Name:     f[3],
		})
	}
	return sessions, nil
}

func (c *execController) ListWindows(ctx context.Context, sessionTarget string) ([]TmuxWindow, error) {
	// Free-text field (name) last.
	const fmtStr = "#{window_id}|#{window_index}|#{window_active}|#{window_panes}|#{window_name}"
	args := []string{"list-windows", "-F", fmtStr}
	if sessionTarget != "" {
		args = append(args, "-t", sessionTarget)
	} else {
		// No session specified: list windows across every session on the server.
		// Without -a, tmux requires a current client/session ($TMUX), which is
		// absent for a controller driving an isolated or remote server.
		args = append(args, "-a")
	}
	out, err := c.exec(ctx, args)
	if err != nil {
		return nil, err
	}
	var windows []TmuxWindow
	for _, line := range nonEmptyLines(out) {
		f := strings.SplitN(line, fieldSep, 5)
		if len(f) < 5 {
			continue
		}
		windows = append(windows, TmuxWindow{
			ID:     f[0],
			Index:  atoiOr(f[1], 0),
			Active: f[2] == "1",
			Panes:  atoiOr(f[3], 0),
			Name:   f[4],
		})
	}
	return windows, nil
}

func (c *execController) ListPanes(ctx context.Context, windowTarget string) ([]TmuxPane, error) {
	// Fixed numeric fields first; free-text (command, cwd) last. We split with a
	// limit of 6 so the last segment holds "command|cwd", then split that once
	// more — a "|" inside a path stays in cwd rather than shifting fields.
	const fmtStr = "#{pane_id}|#{pane_index}|#{pane_active}|#{pane_width}|#{pane_height}|#{pane_current_command}|#{pane_current_path}"
	args := []string{"list-panes", "-F", fmtStr}
	if windowTarget != "" {
		args = append(args, "-t", windowTarget)
	}
	out, err := c.exec(ctx, args)
	if err != nil {
		return nil, err
	}
	var panes []TmuxPane
	for _, line := range nonEmptyLines(out) {
		f := strings.SplitN(line, fieldSep, 7)
		if len(f) < 7 {
			continue
		}
		panes = append(panes, TmuxPane{
			ID:      f[0],
			Index:   atoiOr(f[1], 0),
			Active:  f[2] == "1",
			Width:   atoiOr(f[3], 0),
			Height:  atoiOr(f[4], 0),
			Command: f[5],
			CWD:     f[6],
		})
	}
	return panes, nil
}

// envWithout returns env with any entry for the given key removed.
func envWithout(env []string, key string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
