package session

import (
	"fmt"
	"os/exec"
	"strings"
)

// NotifyTmuxWindow sends notification signals to a tmux window to indicate
// the session is waiting for user input:
//  1. Prefix window name with "!" via rename-window
//  2. Disable automatic-rename so the prefix isn't overwritten
//  3. Send BEL character to trigger tmux bell monitoring
//  4. Display a message overlay
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
	notifyName := "!" + windowName
	_ = exec.Command("tmux", "rename-window", "-t", windowTarget, notifyName).Run()

	// Display a message for 5 seconds on the current client (no -t),
	// so the overlay appears wherever the user is currently looking.
	msg := fmt.Sprintf("Session %q waiting for input", windowName)
	_ = exec.Command("tmux", "display-message", "-d", "5000", msg).Run()
}

// ClearTmuxWindowNotification removes the "!" prefix from a tmux window name,
// restoring the original name.
func ClearTmuxWindowNotification(windowTarget, windowName string) {
	// Re-enable automatic-rename before rename, since rename invalidates
	// name-based targets (when windowTarget is not a stable @ID).
	_ = exec.Command("tmux", "set-option", "-t", windowTarget, "automatic-rename", "on").Run()
	_ = exec.Command("tmux", "rename-window", "-t", windowTarget, windowName).Run()
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
