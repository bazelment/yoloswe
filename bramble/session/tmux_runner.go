package session

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// tmuxRunner implements sessionRunner by creating a tmux window that runs the claude CLI.
type tmuxRunner struct {
	windowName     string // tmux window name (e.g., "happy-tiger")
	workDir        string // working directory for the window
	prompt         string // initial prompt
	permissionMode string // permission mode: "" (default) or "plan"
	yoloMode       bool   // skip all permission prompts
}

// Start creates a new tmux window in the current session and launches the claude CLI in it.
func (r *tmuxRunner) Start(ctx context.Context) error {
	if !IsTmuxAvailable() {
		return fmt.Errorf("tmux is not available")
	}

	if !IsInsideTmux() {
		return fmt.Errorf("not running inside tmux")
	}

	// Check if window already exists (shouldn't happen with name generation, but just in case)
	if TmuxWindowExists(r.windowName) {
		return fmt.Errorf("tmux window %q already exists", r.windowName)
	}

	// Build the claude command
	claudeCmd := "claude"
	args := []string{}

	// Add permission mode flag for planner sessions
	if r.permissionMode == "plan" {
		args = append(args, "--permission-mode", "plan")
	}

	// Add yolo mode flags to skip all permissions
	if r.yoloMode {
		args = append(args, "--allow-dangerously-skip-permissions")
		args = append(args, "--dangerously-skip-permissions")
	}

	// Add the prompt
	args = append(args, r.prompt)

	// Build the full command string for tmux
	// We need to escape the prompt properly for shell execution
	cmdStr := claudeCmd
	for _, arg := range args {
		// Simple escaping - wrap in single quotes and escape any single quotes
		escaped := "'" + arg + "'"
		cmdStr += " " + escaped
	}

	// Create tmux window with the claude command
	// -n: window name
	// -c: working directory
	// Note: We don't use -d flag, so we switch to the window immediately
	createCmd := exec.Command("tmux", "new-window", "-n", r.windowName, "-c", r.workDir, cmdStr)

	if err := createCmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux window %q: %w", r.windowName, err)
	}

	// Display a message showing how to switch back
	// Use prefix + p (previous window) or prefix + w (window list)
	displayCmd := exec.Command("tmux", "display-message", "-d", "3000",
		"Session started in new window. Press prefix+p to return to bramble, or prefix+w for window list")
	_ = displayCmd.Run() // Ignore error - not critical if message fails

	return nil
}

// RunTurn is not supported for tmux windows - all interaction happens in the tmux window directly.
// This returns nil to satisfy the interface, but should not be called in practice.
func (r *tmuxRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	// Tmux windows don't support programmatic follow-ups.
	// All interaction happens directly in the tmux window via the claude CLI.
	return nil, nil
}

// Stop kills the tmux window.
func (r *tmuxRunner) Stop() error {
	if !IsTmuxAvailable() || !IsInsideTmux() {
		return fmt.Errorf("tmux is not available or not inside tmux")
	}

	if !TmuxWindowExists(r.windowName) {
		// Window already stopped or doesn't exist - not an error
		return nil
	}

	cmd := exec.Command("tmux", "kill-window", "-t", r.windowName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to kill tmux window %q: %w", r.windowName, err)
	}

	return nil
}
