package session

import (
	"fmt"
	"os/exec"
	"strings"
)

// TmuxNotifyPrefix is prepended to a tmux window name to indicate the
// session is waiting for user input. Used by NotifyTmuxWindow/ClearTmuxWindowNotification
// and the monitor loop's liveness check.
const TmuxNotifyPrefix = "!"

// NotifyTmuxWindow sends notification signals to a tmux window to indicate
// the session is waiting for user input:
//  1. Disable automatic-rename so the "!" prefix sticks
//  2. Send BEL character to trigger tmux bell monitoring
//  3. Prefix window name with "!" via rename-window (must be last — see below)
//  4. Display a message overlay on the current client
//
// All tmux commands are best-effort; errors are silently ignored because
// failures (e.g. tmux not available, window already gone) are non-fatal.
func NotifyTmuxWindow(windowTarget, windowName string) {
	// Disable automatic-rename so the "!" prefix will stick after rename.
	// All commands that reference windowTarget must run BEFORE rename-window,
	// because when windowTarget is a name (not a stable @ID), the rename
	// invalidates the old name as a target.
	_ = exec.Command("tmux", "set-option", "-t", windowTarget, "automatic-rename", "off").Run()

	// Send BEL character to the pane
	_ = exec.Command("tmux", "run-shell", "-t", windowTarget, `printf "\a"`).Run()

	// Prefix window name with "!" — must be last since it changes the name target
	notifyName := TmuxNotifyPrefix + windowName
	_ = exec.Command("tmux", "rename-window", "-t", windowTarget, notifyName).Run()

	// Display a message for 5 seconds on the current client (no -t),
	// so the overlay appears wherever the user is currently looking.
	msg := fmt.Sprintf("Session %q waiting for input", windowName)
	_ = exec.Command("tmux", "display-message", "-d", "5000", msg).Run()
}

// ClearTmuxWindowNotification removes the "!" prefix from a tmux window name,
// restoring the original name.
func ClearTmuxWindowNotification(windowTarget, windowName string) {
	// rename-window implicitly disables automatic-rename, so we must
	// rename first and then re-enable automatic-rename afterward.
	_ = exec.Command("tmux", "rename-window", "-t", windowTarget, windowName).Run()
	// After rename, the target name has changed (if it was name-based).
	// Use the restored windowName as the target for re-enabling automatic-rename.
	restoreTarget := windowTarget
	if windowTarget != windowName && !strings.HasPrefix(windowTarget, "@") {
		restoreTarget = windowName
	}
	_ = exec.Command("tmux", "set-option", "-t", restoreTarget, "automatic-rename", "on").Run()
}

// GetActiveTmuxWindowID returns the window_id of the currently focused tmux window.
// Returns empty string if tmux is unavailable or the query fails.
func GetActiveTmuxWindowID() string {
	out, err := exec.Command("tmux", "display-message", "-p", "#{window_id}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
