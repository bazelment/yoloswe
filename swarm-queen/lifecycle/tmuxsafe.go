package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Kill a tmux window only when it is provably not you.
//
// 2026-08-27T09:09:15Z: an orchestrator reaped "dead" windows with
//
//	tmux list-windows -F '#{window_index} #{window_name}' | grep ' !'
//
// believing ' !' matched tmux's dead/bell FLAG. That format string emits no
// flags field at all, so it matched the NAME -- and bramble prefixes '!' to the
// name of a window that WANTS ATTENTION. Every match was a healthy running
// session; every genuinely dead window was missed. killed=18, sessions running
// 2-9.5h, itself among them, 336 minutes lost, a human needed to restart it.
//
// Two tmux behaviours make a naive guard fail OPEN. Both measured:
//
//	display-message -p -t '@99999'  ->  ""     rc=0   (a bad target is not an error)
//	display-message -p -t ''        ->  "@86"  rc=0   (an EMPTY -t means the ACTIVE window)
//
// So an unset $TMUX_PANE does not fail -- it answers with somebody else's
// window, and a guard comparing against that protects the wrong one while
// killing the real self. Hence: validate TMUX_PANE BEFORE interpolating it, test
// existence with list-windows (not display-message), and fail closed on any
// doubt.

// windowID matches a tmux window id: @ followed by digits. Never a name, never
// an index, never a flag.
var windowID = regexp.MustCompile(`^@\d+$`)

// paneID matches a tmux pane id.
var paneID = regexp.MustCompile(`^%\d+$`)

// Kill refusal reasons. Every one of these is a fail-closed path.
var (
	// ErrMalformedTarget means the target was not a @N window id.
	ErrMalformedTarget = errors.New("target is not a @N window id")
	// ErrSelfUnresolvable means we cannot prove the target is not ourselves.
	ErrSelfUnresolvable = errors.New("cannot identify self, so cannot prove the target is not me")
	// ErrTargetIsSelf means the target is this very session.
	ErrTargetIsSelf = errors.New("target IS this session")
	// ErrNoSuchWindow means the window is already gone.
	ErrNoSuchWindow = errors.New("no such window")
)

// Tmux runs tmux commands. Injectable so the guard is testable against a private
// tmux server rather than the live one.
type Tmux interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// ExecTmux runs the real tmux binary, optionally against a specific socket.
type ExecTmux struct {
	// Socket is passed as -S. Empty uses the default server.
	Socket string
}

// Run executes tmux and returns trimmed stdout.
func (e ExecTmux) Run(ctx context.Context, args ...string) (string, error) {
	full := args
	if e.Socket != "" {
		full = append([]string{"-S", e.Socket}, args...)
	}
	cmd := exec.CommandContext(ctx, "tmux", full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s",
			strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveSelf returns this process's own tmux window id.
//
// TMUX_PANE is validated BEFORE it is interpolated into any tmux command,
// because an empty -t silently means "the active window" -- so a blank
// TMUX_PANE would resolve to somebody else's window and the self-check would
// protect the wrong session.
func ResolveSelf(ctx context.Context, tm Tmux) (string, error) {
	if os.Getenv("TMUX") == "" {
		return "", fmt.Errorf("not inside tmux")
	}
	pane := os.Getenv("TMUX_PANE")
	if !paneID.MatchString(pane) {
		return "", fmt.Errorf("TMUX_PANE absent or malformed (%q)", pane)
	}
	out, err := tm.Run(ctx, "display-message", "-p", "-t", pane, "#{window_id}")
	if err != nil {
		return "", fmt.Errorf("resolve self from %s: %w", pane, err)
	}
	if !windowID.MatchString(out) {
		return "", fmt.Errorf("could not resolve self window from %s (got %q)", pane, out)
	}
	return out, nil
}

// WindowExists tests existence with list-windows, not display-message.
//
// display-message returns rc=0 with empty output for a bad target, so using it
// as an existence check fails open.
func WindowExists(ctx context.Context, tm Tmux, target string) bool {
	out, err := tm.Run(ctx, "list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == target {
			return true
		}
	}
	return false
}

// SafeKillWindow kills a tmux window only when it is provably not this session.
//
// selfWindow is this process's own window id; pass the result of ResolveSelf.
// When it is empty the kill is REFUSED rather than attempted -- being unable to
// identify yourself means being unable to prove you are not the target.
func SafeKillWindow(ctx context.Context, tm Tmux, target, selfWindow, label string) error {
	if !windowID.MatchString(target) {
		return fmt.Errorf("%w: %q (never a name, index or flag)", ErrMalformedTarget, target)
	}
	if selfWindow == "" {
		return fmt.Errorf("%w (target %s)", ErrSelfUnresolvable, target)
	}
	if target == selfWindow {
		return fmt.Errorf("%w: %s (%s)", ErrTargetIsSelf, target, label)
	}
	if !WindowExists(ctx, tm, target) {
		return fmt.Errorf("%w: %s", ErrNoSuchWindow, target)
	}
	if _, err := tm.Run(ctx, "kill-window", "-t", target); err != nil {
		return fmt.Errorf("kill-window %s: %w", target, err)
	}
	return nil
}
