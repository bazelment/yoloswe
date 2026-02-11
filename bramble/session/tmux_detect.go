package session

import (
	"os"
	"os/exec"
	"strings"
)

// IsInsideTmux returns true if the current process is running inside tmux.
// This is determined by checking if the TMUX environment variable is set.
func IsInsideTmux() bool {
	return os.Getenv("TMUX") != ""
}

// IsTmuxAvailable returns true if the tmux command is available in PATH.
func IsTmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// ListTmuxWindows returns a list of window names in the current tmux session.
// Returns an empty slice if tmux is not available or not inside tmux.
func ListTmuxWindows() ([]string, error) {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return nil, nil
	}

	cmd := exec.Command("tmux", "list-windows", "-F", "#{window_name}")
	output, err := cmd.Output()
	if err != nil {
		// No windows or error - return empty list
		return nil, nil
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}

	return lines, nil
}

// TmuxWindowExists checks if a tmux window with the given name exists in the current session.
func TmuxWindowExists(name string) bool {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return false
	}

	windows, err := ListTmuxWindows()
	if err != nil {
		return false
	}

	for _, w := range windows {
		if w == name {
			return true
		}
	}
	return false
}

// TmuxWindowPaneDead checks if any pane in the given tmux window has exited.
// This is useful when remain-on-exit is set — the window stays but the process is dead.
// Returns true if any pane is dead, false if all are running or if the window doesn't exist.
func TmuxWindowPaneDead(name string) bool {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return false
	}

	cmd := exec.Command("tmux", "list-panes", "-t", name, "-F", "#{pane_dead}")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	return parsePaneDeadOutput(string(output))
}

// parsePaneDeadOutput returns true if any line in the tmux list-panes output
// indicates a dead pane (value "1"). Handles multi-pane windows where the
// output contains one line per pane.
func parsePaneDeadOutput(output string) bool {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "1" {
			return true
		}
	}
	return false
}
