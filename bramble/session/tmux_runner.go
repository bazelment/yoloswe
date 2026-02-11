package session

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// tmuxRunner implements sessionRunner by creating a tmux window that runs the agent CLI.
type tmuxRunner struct {
	windowName     string // tmux window name (e.g., "happy-tiger")
	workDir        string // working directory for the window
	prompt         string // initial prompt
	model          string // model ID (e.g. "opus", "gpt-5.3-codex")
	provider       string // binary name: "claude" or "codex"
	permissionMode string // permission mode: "" (default) or "plan" (claude only)
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

	// Build the agent command based on provider
	binary, args := r.buildCommand()

	// Build the full command string for tmux
	cmdStr := buildShellCommand(binary, args)

	// Create tmux window with the claude command
	// -n: window name
	// -c: working directory
	createCmd := exec.Command("tmux", "new-window", "-n", r.windowName, "-c", r.workDir, cmdStr)

	if err := createCmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux window %q: %w", r.windowName, err)
	}

	// Set remain-on-exit so the window stays open if claude crashes,
	// allowing the user to see the error output instead of the window
	// vanishing silently.
	setOptCmd := exec.Command("tmux", "set-option", "-t", r.windowName, "remain-on-exit", "on")
	_ = setOptCmd.Run()

	// Brief pause then verify the window still exists. Without remain-on-exit
	// (e.g. if the set-option failed), a broken command could cause the window
	// to vanish before we even start monitoring.
	time.Sleep(100 * time.Millisecond)
	if !TmuxWindowExists(r.windowName) {
		return fmt.Errorf("tmux window %q disappeared immediately after creation — claude may have failed to start", r.windowName)
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

// buildCommand returns the binary name and argument list for the agent CLI.
func (r *tmuxRunner) buildCommand() (binary string, args []string) {
	binary = r.provider
	if binary == "" {
		binary = ProviderClaude
	}

	// Add model flag
	if r.model != "" {
		args = append(args, "--model", r.model)
	}

	switch binary {
	case ProviderCodex:
		// Codex-specific flags
		if r.yoloMode {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
	default:
		// Claude-specific flags
		if r.yoloMode {
			args = append(args, "--allow-dangerously-skip-permissions", "--dangerously-skip-permissions")
		}
		if r.permissionMode == "plan" {
			args = append(args, "--permission-mode", "plan")
		}
	}

	// Add the prompt last
	args = append(args, r.prompt)
	return binary, args
}

// buildShellCommand constructs a shell command string with properly escaped arguments.
// Each argument is wrapped in single quotes with embedded single quotes escaped
// using the standard '\” technique.
func buildShellCommand(cmd string, args []string) string {
	cmdStr := cmd
	for _, arg := range args {
		escaped := "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		cmdStr += " " + escaped
	}
	return cmdStr
}
