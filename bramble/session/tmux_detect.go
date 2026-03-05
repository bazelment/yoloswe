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

// TmuxWindowIDByName looks up the window ID for a window with the given name
// by running `tmux list-windows -F "#{window_name} #{window_id}"`. It returns
// the ID and true on success, or empty string and false if the window is not
// found or tmux is unavailable.
func TmuxWindowIDByName(name string) (string, bool) {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return "", false
	}

	cmd := exec.Command("tmux", "list-windows", "-F", "#{window_name} #{window_id}")
	output, err := cmd.Output()
	if err != nil {
		return "", false
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "<name> <id>" — split on the LAST space so window names with
		// spaces are handled correctly.
		idx := strings.LastIndex(line, " ")
		if idx < 0 {
			continue
		}
		windowName := line[:idx]
		windowID := line[idx+1:]
		if windowName == name {
			return windowID, true
		}
	}
	return "", false
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

// SelectTmuxWindow switches to the given tmux window.
//
// In normal tmux mode, select-window is sufficient. In control mode
// (tmux -CC with iTerm2), select-window updates the tmux server state but
// does NOT cause iTerm2 to bring the corresponding native window/tab to
// front. We detect this case (client_control_mode=1 + LC_TERMINAL=iTerm2)
// and use AppleScript to activate the matching iTerm2 window.
//
// This is iTerm2-specific, but tmux CC mode is itself an iTerm2-only
// feature — no other terminal implements it. The AppleScript path is
// best-effort: if it fails, select-window already succeeded so tmux
// state is correct.
func SelectTmuxWindow(windowTarget string) error {
	if err := exec.Command("tmux", "select-window", "-t", windowTarget).Run(); err != nil {
		return fmt.Errorf("tmux select-window -t %s: %w", windowTarget, err)
	}

	// In CC mode with iTerm2, also activate the native window.
	if isITermControlMode() {
		activateITermWindowForTmux(windowTarget)
	}
	return nil
}

// isITermControlMode returns true when running inside tmux CC mode under iTerm2.
func isITermControlMode() bool {
	if os.Getenv("LC_TERMINAL") != "iTerm2" {
		return false
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#{client_control_mode}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
}

// activateITermWindowForTmux finds the iTerm2 window that corresponds to
// the given tmux window target and brings it to front using AppleScript.
//
// Matching strategy: extract the working directory from the tmux window
// (pane_current_path) and look for an iTerm2 CC window whose title
// contains that path. This is robust against processes changing the pane
// title (e.g. spinner + "Claude Code") because the path portion of the
// iTerm2 CC window title always reflects the pane's working directory.
//
// If path matching finds no hit, falls back to matching the tmux window
// name against the iTerm2 window title.
func activateITermWindowForTmux(windowTarget string) {
	// Get the working directory for the target tmux window.
	out, err := exec.Command("tmux", "display-message", "-t", windowTarget,
		"-p", "#{pane_current_path}").Output()
	if err != nil {
		return
	}
	paneDir := strings.TrimSpace(string(out))

	// Also get the tmux window name for fallback matching.
	nameOut, _ := exec.Command("tmux", "display-message", "-t", windowTarget,
		"-p", "#{window_name}").Output()
	windowName := strings.TrimSpace(string(nameOut))

	// Shorten home prefix to ~ for matching iTerm2's title format.
	// Require a path-separator boundary so that e.g. HOME=/Users/alice does
	// not incorrectly match /Users/alicebob/project.
	home := os.Getenv("HOME")
	displayDir := paneDir
	if home != "" && strings.HasPrefix(paneDir, home) &&
		(len(paneDir) == len(home) || paneDir[len(home)] == '/') {
		displayDir = "~" + paneDir[len(home):]
	}

	// Guard against empty match strings: AppleScript's `n contains ""` is always
	// true, which would activate the wrong iTerm2 window.
	if displayDir == "" && windowName == "" {
		return
	}

	// AppleScript matching strategy:
	//   1. Primary: look for a CC window whose title is exactly "↣ <displayDir>"
	//      or starts with "↣ <displayDir> " (path followed by a space separator).
	//      This prevents "~/repo" from matching "~/repo-old".
	//   2. Fallback: match on window name using the same exact/prefix rule.
	//      Using exact match prevents "wt:1" from matching "wt:10".
	//
	// iTerm2 CC window titles have the form "↣ <path>" or "↣ <path> — <proc>".
	// Note: when two windows share the same pane path, both will match the
	// primary criterion and the first one encountered will be selected. This is
	// a best-effort approximation; select-window already set the correct tmux
	// server state regardless.
	var script string
	if displayDir != "" && windowName != "" {
		script = fmt.Sprintf(`tell application "iTerm2"
    set arrow to "↣ "
    set targetPath to %q
    set targetName to %q
    repeat with w in windows
        set n to name of w
        if n starts with arrow then
            set rest to text ((length of arrow) + 1) thru -1 of n
            if rest is equal to targetPath or rest starts with (targetPath & " ") then
                select w
                return
            end if
        end if
    end repeat
    repeat with w in windows
        set n to name of w
        if n starts with arrow then
            set rest to text ((length of arrow) + 1) thru -1 of n
            if rest is equal to targetName or rest starts with (targetName & " ") then
                select w
                return
            end if
        end if
    end repeat
end tell`, displayDir, windowName)
	} else if displayDir != "" {
		script = fmt.Sprintf(`tell application "iTerm2"
    set arrow to "↣ "
    set targetPath to %q
    repeat with w in windows
        set n to name of w
        if n starts with arrow then
            set rest to text ((length of arrow) + 1) thru -1 of n
            if rest is equal to targetPath or rest starts with (targetPath & " ") then
                select w
                return
            end if
        end if
    end repeat
end tell`, displayDir)
	} else {
		script = fmt.Sprintf(`tell application "iTerm2"
    set arrow to "↣ "
    set targetName to %q
    repeat with w in windows
        set n to name of w
        if n starts with arrow then
            set rest to text ((length of arrow) + 1) thru -1 of n
            if rest is equal to targetName or rest starts with (targetName & " ") then
                select w
                return
            end if
        end if
    end repeat
end tell`, windowName)
	}

	_ = exec.Command("osascript", "-e", script).Run()
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
