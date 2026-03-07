package session

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// ansiRe matches ANSI escape sequences (CSI sequences, OSC sequences, and simple escapes).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b\x07]*[\x07\x1b\\]|\x1b[^[\]()]`)

// StripANSI removes ANSI escape sequences from a string.
func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// CaptureTmuxPane captures the last n lines of text from a tmux pane.
// Uses `tmux capture-pane -t <target> -p -J -S -<n>` to get joined wrapped lines.
// Returns non-empty, ANSI-stripped lines.
func CaptureTmuxPane(windowTarget string, n int) ([]string, error) {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return nil, fmt.Errorf("tmux is not available or not inside tmux")
	}

	startLine := fmt.Sprintf("-%d", n)
	cmd := exec.Command("tmux", "capture-pane", "-t", windowTarget, "-p", "-J", "-S", startLine)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmux capture-pane -t %s: %w", windowTarget, err)
	}

	raw := strings.Split(string(output), "\n")
	var lines []string
	for _, line := range raw {
		cleaned := strings.TrimRight(StripANSI(line), " ")
		if cleaned != "" {
			lines = append(lines, cleaned)
		}
	}
	return lines, nil
}

// PaneCursorY returns the cursor Y position (0-indexed row) for the active
// pane in the given tmux window. In Claude Code's TUI, the cursor always sits
// on the empty line immediately after the permissions line when idle:
//
//	cursor_y - 3: status bar separator (────────)
//	cursor_y - 2: info line (path branch model ctx:XX% tokens:NNk)
//	cursor_y - 1: permissions line (⏵⏵ ...)
//	cursor_y:     empty (cursor)
//
// For unfilled terminals (e.g. freshly started sessions), cursor_y < pane_height,
// which tells us exactly where content ends without needing to scan for separators.
func PaneCursorY(windowTarget string) (int, error) {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return 0, fmt.Errorf("tmux is not available or not inside tmux")
	}

	// Use display-message with the window target — tmux resolves it to the
	// active pane of that window, so multi-pane windows get the right cursor.
	cursorCmd := exec.Command("tmux", "display-message", "-t", windowTarget, "-p", "#{cursor_y}")
	cursorOut, err := cursorCmd.Output()
	if err != nil {
		return 0, fmt.Errorf("tmux display-message cursor_y: %w", err)
	}
	y, err := strconv.Atoi(strings.TrimSpace(string(cursorOut)))
	if err != nil {
		return 0, fmt.Errorf("parse cursor_y %q: %w", string(cursorOut), err)
	}
	return y, nil
}

// CaptureTmuxPaneFull captures the entire visible pane from line 0 to cursor_y,
// returning ANSI-stripped lines with their original positions preserved (empty
// lines kept as empty strings). Also returns cursor_y.
//
// Unlike CaptureTmuxPane which captures from the bottom and strips empty lines,
// this captures from the top with positional fidelity, which allows correlating
// line indices with cursor_y for precise status bar location.
func CaptureTmuxPaneFull(windowTarget string) (lines []string, cursorY int, err error) {
	cursorY, err = PaneCursorY(windowTarget)
	if err != nil {
		return nil, 0, err
	}

	cmd := exec.Command("tmux", "capture-pane", "-t", windowTarget, "-p", "-J", "-S", "0")
	output, err := cmd.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("tmux capture-pane -t %s: %w", windowTarget, err)
	}

	raw := strings.Split(string(output), "\n")
	// Keep lines up to and including cursor_y (trim trailing newline artifact).
	limit := cursorY + 1
	if limit > len(raw) {
		limit = len(raw)
	}
	lines = make([]string, limit)
	for i := 0; i < limit; i++ {
		lines[i] = strings.TrimRight(StripANSI(raw[i]), " ")
	}
	return lines, cursorY, nil
}

// ParseClaudeStatusBarWithCursor extracts structured data using the cursor
// position to precisely locate the status bar. This is more reliable than
// separator scanning, especially for unfilled terminals.
//
// The Claude Code TUI layout relative to cursor_y:
//
//	cursor_y - 4: ❯ (input prompt) — may have user text
//	cursor_y - 3: status bar separator (────────)
//	cursor_y - 2: info line (path branch model ctx:XX% tokens:NNk)
//	cursor_y - 1: permissions line (⏵⏵ ...)
//	cursor_y:     empty (cursor)
//
// Content ends at cursor_y - 5 (input area separator with ▪▪▪).
func ParseClaudeStatusBarWithCursor(lines []string, cursorY int) *PaneStatus {
	if cursorY < 3 || cursorY >= len(lines) {
		return nil
	}

	// The status bar separator should be at cursor_y - 3.
	sepIdx := cursorY - 3
	if sepIdx < 0 || sepIdx >= len(lines) {
		return nil
	}
	if !separatorRe.MatchString(strings.TrimSpace(lines[sepIdx])) {
		// Cursor might be offset by completion/spinner lines between ❯ and separator.
		// Fall back to scanning from cursor_y upward.
		sepIdx = -1
		for i := cursorY - 2; i >= cursorY-6 && i >= 0; i-- {
			if separatorRe.MatchString(strings.TrimSpace(lines[i])) {
				sepIdx = i
				break
			}
		}
		if sepIdx < 0 {
			return nil
		}
	}

	ps := &PaneStatus{SepIdx: sepIdx}

	// Parse info line (right after status separator)
	if sepIdx+1 < len(lines) {
		if m := statusBarRe.FindStringSubmatch(lines[sepIdx+1]); m != nil {
			ps.WorkDir = m[1]
			ps.Branch = m[2]
			ps.Model = m[3]
			ps.ContextPct = m[4]
			ps.TokenCount = m[5]
		}
	}

	// Parse permissions line
	if sepIdx+2 < len(lines) {
		permLine := strings.TrimSpace(lines[sepIdx+2])
		if strings.Contains(permLine, "permissions") {
			if idx := strings.Index(permLine, "⏵⏵ "); idx >= 0 {
				rest := permLine[idx+len("⏵⏵ "):]
				if end := strings.Index(rest, " ("); end > 0 {
					ps.Permissions = rest[:end]
				} else {
					ps.Permissions = rest
				}
			}
			if m := prRe.FindStringSubmatch(permLine); m != nil {
				ps.PRNumber = m[1]
			}
		}
	}

	// Find the input area separator (above the ❯ prompt, contains ▪▪▪).
	// This marks the boundary between agent content and the input chrome.
	// We use this as the content boundary rather than SepIdx.
	inputSepIdx := -1
	for i := sepIdx - 1; i >= sepIdx-4 && i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if separatorRe.MatchString(trimmed) && strings.Contains(trimmed, "▪") {
			inputSepIdx = i
			break
		}
	}
	if inputSepIdx >= 0 {
		ps.SepIdx = inputSepIdx // Use input separator as content boundary
	}

	// Look for idle/working state between input separator and status separator.
	scanEnd := sepIdx
	scanStart := scanEnd - 5
	if inputSepIdx >= 0 {
		scanStart = inputSepIdx + 1
	}
	if scanStart < 0 {
		scanStart = 0
	}
	for i := scanEnd - 1; i >= scanStart; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "❯") {
			ps.IsIdle = true
			break
		}
		if completionRe.MatchString(line) {
			ps.IsIdle = true
			ps.StatusLine = line
			break
		}
		if spinnerRe.MatchString(line) {
			ps.IsWorking = true
			ps.StatusLine = line
			break
		}
		if strings.HasPrefix(line, "●") {
			ps.IsWorking = true
			ps.StatusLine = line
			break
		}
		break
	}

	return ps
}

// PaneStatus holds structured data parsed from a Claude Code status bar.
type PaneStatus struct {
	Model       string // e.g. "Opus 4.6"
	ContextPct  string // e.g. "43%"
	TokenCount  string // e.g. "20k"
	PRNumber    string // e.g. "930" (empty if no PR)
	Branch      string // e.g. "feature/better-ci"
	WorkDir     string // e.g. "~/worktrees/kernel/feature/better-ci"
	StatusLine  string // e.g. "✻ Worked for 36m 36s" or "* Frosting…"
	Permissions string // e.g. "bypass permissions on"
	IsIdle      bool   // true if "❯" prompt visible (awaiting user input)
	IsWorking   bool   // true if spinner/activity indicator visible
	SepIdx      int    // index of separator line in the source slice (-1 if not found)
}

// separatorRe matches the "───" separator line in Claude Code TUI.
var separatorRe = regexp.MustCompile(`^─{10,}`)

// statusBarRe parses the Claude Code status bar info line:
//
//	"  ~/path  branch  Model  ctx:XX%  tokens:NNk"
//
// The line may have trailing text after the token count (e.g. context
// compaction warnings), so we don't anchor to end-of-line.
var statusBarRe = regexp.MustCompile(
	`^\s+(\S+)\s+(\S+)\s+(.+?)\s+ctx:(\d+%)\s+tokens:(\d+[kKmMbB]?)`,
)

// prRe extracts PR number from the permissions line.
var prRe = regexp.MustCompile(`PR #(\d+)`)

// completionRe matches "✻ Worked for ...", "✢ Baked for ...", "✽ Cooked for ...", etc.
// These indicate the agent just finished a turn. Claude Code uses various star/sparkle
// characters (✻ ✢ ✽ ✹) with food-themed verbs.
var completionRe = regexp.MustCompile(`^[✻✢✽✹]\s+\w+`)

// spinnerRe matches Claude Code spinner characters at the start of a line.
// These indicate the agent is actively working (e.g. "* Frosting…", "· Creating…").
var spinnerRe = regexp.MustCompile(`^[*·⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s`)

// isChromeLine returns true if the line is Claude TUI chrome (separator,
// idle prompt, completion indicator) rather than meaningful content.
func isChromeLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}
	if separatorRe.MatchString(trimmed) {
		return true
	}
	if strings.HasPrefix(trimmed, "❯") {
		return true
	}
	if completionRe.MatchString(trimmed) {
		return true
	}
	if spinnerRe.MatchString(trimmed) {
		return true
	}
	return false
}

// splashRe matches Claude Code splash banner lines (logo art).
var splashRe = regexp.MustCompile(`^\s*[▐▛█▜▌▝▘]+\s|^\s*▘▘\s+▝▝`)

// ContentLines extracts meaningful agent output from captured pane lines,
// stripping TUI chrome (separator, status bar, permissions, idle prompt,
// completion indicators, and spinner lines).
func ContentLines(lines []string, ps *PaneStatus) []string {
	end := len(lines)
	if ps != nil && ps.SepIdx >= 0 {
		end = ps.SepIdx
	}
	var result []string
	for i := 0; i < end; i++ {
		if !isChromeLine(lines[i]) && !isSplashLine(lines[i]) {
			result = append(result, lines[i])
		}
	}
	return result
}

// isSplashLine returns true if the line is part of the Claude Code splash
// banner that appears at the top of a freshly started session.
func isSplashLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	return splashRe.MatchString(line)
}

// ParseClaudeStatusBar extracts structured data from the last lines of a
// Claude Code tmux pane capture. The expected layout (bottom of pane):
//
//	❯                                           ← idle prompt (or spinner line)
//	─────────────────────────────────────────── ← separator
//	  ~/path  branch  Model  ctx:XX%  tokens:NNk
//	  ⏵⏵ bypass permissions on ... · PR #NNN
func ParseClaudeStatusBar(lines []string) *PaneStatus {
	if len(lines) < 2 {
		return nil
	}

	// Find the separator line scanning from the bottom, then extract the
	// two lines below it (status bar + permissions).
	sepIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if separatorRe.MatchString(lines[i]) {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 || sepIdx+1 >= len(lines) {
		return nil
	}

	ps := &PaneStatus{SepIdx: sepIdx}

	// Parse info line (right after separator)
	infoLine := strings.TrimSpace(lines[sepIdx+1])
	if m := statusBarRe.FindStringSubmatch(lines[sepIdx+1]); m != nil {
		ps.WorkDir = m[1]
		ps.Branch = m[2]
		ps.Model = m[3]
		ps.ContextPct = m[4]
		ps.TokenCount = m[5]
	} else if infoLine != "" {
		// Fallback: couldn't parse but line exists
		ps.StatusLine = infoLine
	}

	// Parse permissions line (two after separator)
	if sepIdx+2 < len(lines) {
		permLine := strings.TrimSpace(lines[sepIdx+2])
		if strings.Contains(permLine, "permissions") {
			// Extract permission mode
			if idx := strings.Index(permLine, "⏵⏵ "); idx >= 0 {
				rest := permLine[idx+len("⏵⏵ "):]
				if end := strings.Index(rest, " ("); end > 0 {
					ps.Permissions = rest[:end]
				} else {
					ps.Permissions = rest
				}
			}
			if m := prRe.FindStringSubmatch(permLine); m != nil {
				ps.PRNumber = m[1]
			}
		}
	}

	// Look above the separator for idle/working state.
	// Scan upward, skipping blank lines, to find the first meaningful indicator.
	for i := sepIdx - 1; i >= 0 && i >= sepIdx-5; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// Check for idle prompt — user is being asked for input
		if strings.HasPrefix(line, "❯") {
			ps.IsIdle = true
			break
		}
		// Check for completion indicator (✻ Worked for ...) — turn just finished,
		// effectively idle but the prompt may be on the next repaint.
		if completionRe.MatchString(line) {
			ps.IsIdle = true
			ps.StatusLine = line
			break
		}
		// Check for spinner — actively processing
		if spinnerRe.MatchString(line) {
			ps.IsWorking = true
			ps.StatusLine = line
			break
		}
		// Tool execution lines start with ●
		if strings.HasPrefix(line, "●") {
			ps.IsWorking = true
			ps.StatusLine = line
			break
		}
		// Any other non-empty line — likely agent output, state is ambiguous.
		// Don't set idle or working; break to avoid false positives.
		break
	}

	return ps
}
