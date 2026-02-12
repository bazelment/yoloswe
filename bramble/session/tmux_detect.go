package session

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

// TmuxWindowExistsByID checks if a tmux window with the given ID (e.g., "@1") exists.
// Window IDs are stable and unique, unlike window names which can be renamed.
func TmuxWindowExistsByID(windowID string) bool {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return false
	}

	// Use tmux list-windows to check if the window exists
	cmd := exec.Command("tmux", "list-windows", "-F", "#{window_id}")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == windowID {
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

// KillTmuxWindowByID kills a tmux window by its window ID (e.g., "@1").
// Window IDs are stable and unique, unlike window names which can be renamed.
func KillTmuxWindowByID(windowID string) error {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return fmt.Errorf("tmux is not available or not inside tmux")
	}

	cmd := exec.Command("tmux", "kill-window", "-t", windowID)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to kill tmux window %q: %w", windowID, err)
	}

	return nil
}

// TmuxWindowPaneExitStatus returns the exit status of the first dead pane in
// the given tmux window. It returns (exitCode, true) if a dead pane was found,
// or (0, false) if no pane is dead or the window doesn't exist.
func TmuxWindowPaneExitStatus(name string) (int, bool) {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return 0, false
	}

	cmd := exec.Command("tmux", "list-panes", "-t", name, "-F", "#{pane_dead} #{pane_dead_status}")
	output, err := cmd.Output()
	if err != nil {
		return 0, false
	}

	return parsePaneExitStatus(string(output))
}

// parsePaneExitStatus parses the output of `tmux list-panes -F "#{pane_dead} #{pane_dead_status}"`.
// Returns (exitCode, true) for the first dead pane found, or (0, false) if none.
func parsePaneExitStatus(output string) (int, bool) {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		if parts[0] != "1" {
			continue // pane is alive, skip
		}
		// Pane is dead; parse exit status
		exitCode, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 1, true // unparseable status — treat as failure
		}
		return exitCode, true
	}
	return 0, false
}
