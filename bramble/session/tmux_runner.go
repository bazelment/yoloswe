package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// ControlSockEnvVar is the environment variable carrying the control socket
// path. It is declared here, in the package that injects it into tmux windows,
// because control already imports session — so session importing control back
// would be a cycle. control.SockEnvVar aliases this constant to keep a single
// source of truth.
const ControlSockEnvVar = "BRAMBLE_CONTROL_SOCK"

// SessionIDEnvVar carries a session's own bramble session ID into its tmux
// window, giving the agent inside a return address it can hand to peers.
const SessionIDEnvVar = "BRAMBLE_SESSION_ID"

// IPCSockEnvVar carries the legacy IPC socket path. Like ControlSockEnvVar it
// is declared here, in the package that injects it into tmux windows, and
// ipc.SockEnvVar aliases it. Keeping the producer's literal and the consumer's
// literal linked at compile time means a rename cannot leave session exporting
// one name while ipc clients read another.
const IPCSockEnvVar = "BRAMBLE_SOCK"

// tmuxRunner implements sessionRunner by creating a tmux window that runs the agent CLI.
type tmuxRunner struct {
	windowName      string // tmux window name (e.g., "happy-tiger")
	workDir         string // working directory for the window
	prompt          string // initial prompt
	model           string // model ID (e.g. "opus", "gpt-5.5")
	provider        string // binary name: "claude" or "codex"
	permissionMode  string // permission mode: "" (default) or "plan" (claude only)
	resumeSessionID string // CLI session ID to resume (empty for new sessions)
	windowID        string // stable window ID captured atomically at creation time
	sessionID       string // bramble session ID for IPC notification hook
	brambleBin      string // absolute path to the bramble binary for hook commands
	brambleSock     string // IPC socket path to pass to hook commands
	controlSock     string // control socket path, so the session can drive peers
	yoloMode        bool   // skip all permission prompts
	killOnStop      bool   // kill tmux window on Stop()
}

// envArgs returns the "-e VAR=value" pairs that give the agent inside the
// window its bramble identity and the sockets it needs to reach the TUI and
// its peers. Empty values are omitted so a partially configured manager still
// produces a valid tmux invocation.
//
// The session ID is the agent's own return address: list-sessions reports it,
// and capture-pane/send-input take it as --session-id. The control socket is
// the only write path into another session's pane. Without both, sessions can
// observe each other but can never reply.
func (r *tmuxRunner) envArgs() []string {
	var args []string
	for _, kv := range [][2]string{
		{IPCSockEnvVar, r.brambleSock},
		{SessionIDEnvVar, r.sessionID},
		{ControlSockEnvVar, r.controlSock},
	} {
		if kv[1] != "" {
			args = append(args, "-e", kv[0]+"="+kv[1])
		}
	}
	return args
}

// newWindowArgs builds the full argv for the `tmux new-window` that launches
// this session. It is split out of Start() so the env exports can be asserted
// at the invocation boundary rather than only at envArgs(): a regression that
// stopped splicing envArgs() into the command would leave every helper test
// passing while windows silently lost their identity and sockets.
//
// -P -F "#{window_id}" prints the new window's stable ID to stdout, capturing
// it atomically at creation time to avoid the TOCTOU race of a post-hoc name
// lookup (TmuxWindowIDByName) when two sessions start concurrently with the
// same window name. -e sets an environment variable in the new window; -n names
// it and -c sets its working directory.
func (r *tmuxRunner) newWindowArgs() []string {
	binary, args := r.buildCommand()
	cmdStr := buildShellCommand(binary, args)

	tmuxArgs := []string{"new-window", "-P", "-F", "#{window_id}"}
	tmuxArgs = append(tmuxArgs, r.envArgs()...)
	tmuxArgs = append(tmuxArgs, "-n", r.windowName, "-c", r.workDir, cmdStr)
	return tmuxArgs
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

	createCmd := exec.Command("tmux", r.newWindowArgs()...)

	out, err := createCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to create tmux window %q: %w", r.windowName, err)
	}
	r.windowID = strings.TrimSpace(string(out))

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

func (r *tmuxRunner) CLISessionID() string { return "" }

// WindowID returns the stable tmux window ID captured atomically during Start.
// Returns empty string if Start has not been called or if capture failed.
func (r *tmuxRunner) WindowID() string { return r.windowID }

// RunTurn is not supported for tmux windows - all interaction happens in the tmux window directly.
// This returns nil to satisfy the interface, but should not be called in practice.
func (r *tmuxRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	// Tmux windows don't support programmatic follow-ups.
	// All interaction happens directly in the tmux window via the claude CLI.
	return nil, nil
}

// Stop kills the tmux window.
func (r *tmuxRunner) Stop() error {
	if !r.killOnStop {
		return nil
	}

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
	provider := r.provider
	if provider == "" {
		provider = ProviderClaude
	}
	// The binary is not always the provider name — cursor's agent CLI is
	// "agent". Go through the canonical mapping so a new provider whose binary
	// differs cannot silently exec the wrong thing.
	binary = agent.BinaryForProvider(provider)

	// Add the model flag. Some of bramble's IDs are placeholders for "the CLI's
	// own default", and some name another provider's model, so let the registry
	// decide whether this CLI can be given anything at all.
	if model := agent.CLIModelArg(r.model, provider); model != "" {
		args = append(args, "--model", model)
	}

	switch provider {
	case ProviderCodex:
		// Codex-specific flags
		if r.yoloMode {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		// Codex's notify program is its analogue of Claude's Stop hook: it runs
		// when a turn completes. Without it bramble never learns a codex window
		// went idle, so the session sits at "running" forever after answering —
		// which breaks both halves of subagent messaging. Nothing drains the
		// queue for it, and its parent is only told it finished when the window
		// finally dies.
		//
		// Codex appends its own JSON payload as a trailing argument; `bramble
		// notify` takes no positional arguments, so the extra one is ignored.
		if r.sessionID != "" && r.brambleBin != "" {
			args = append(args, "-c", codexNotifyConfig(r.brambleBin, r.sessionID))
		}
	case ProviderGemini:
		// Gemini-specific flags
		// Note: Do NOT use --experimental-acp in tmux mode. ACP is for programmatic
		// JSON-RPC communication (TUI mode), not interactive CLI usage (tmux mode).
		//
		// IMPORTANT: Gemini CLI requires folder trust before running commands.
		// Users must run `gemini` once in the project directory and select
		// "Trust folder" from the prompt. Trust is saved to ~/.gemini/trustedFolders.json.
		// Without this, tmux sessions will hang at the trust prompt.
		if r.yoloMode {
			args = append(args, "--yolo")
		}
		if r.permissionMode == "plan" {
			args = append(args, "--approval-mode", "plan")
		}
	case ProviderCursor:
		// Cursor-specific flags. Note: do NOT use -p/--print in tmux mode.
		// --print is for scripted one-shot calls; a tmux window is an
		// interactive session a human attaches to.
		//
		// Cursor refuses to run in an untrusted directory, and bramble always
		// starts a session in a freshly created worktree — without --trust the
		// window opens on a blocking trust prompt. The in-process ACP path
		// trusts unconditionally for the same reason.
		args = append(args, "--trust")
		if r.yoloMode {
			args = append(args, "--yolo")
		}
		if r.permissionMode == "plan" {
			args = append(args, "--mode", "plan")
		}
		if r.resumeSessionID != "" {
			args = append(args, "--resume", r.resumeSessionID)
		}
	case ProviderAgy:
		if r.yoloMode {
			args = append(args, "--dangerously-skip-permissions")
		}
		if r.permissionMode == "plan" {
			args = append(args, "--sandbox")
		}
		if r.resumeSessionID != "" {
			args = append(args, "--conversation", r.resumeSessionID)
		}
		args = append(args, "--prompt-interactive")
	default:
		// Claude-specific flags
		if r.yoloMode {
			args = append(args, "--allow-dangerously-skip-permissions", "--dangerously-skip-permissions")
		}
		if r.permissionMode == "plan" {
			args = append(args, "--permission-mode", "plan")
		}
		if r.resumeSessionID != "" {
			args = append(args, "--resume", r.resumeSessionID)
		}
		// Inject a Stop hook so Claude calls back when a turn finishes and
		// the CLI is waiting for user input (Claude provider only).
		// The hook command is run by a shell, so the session ID must be
		// single-quoted to handle spaces or special characters safely.
		// BRAMBLE_SOCK is set via "tmux new-window -e" so the hook process
		// inherits it from the tmux window environment.
		if r.sessionID != "" {
			shellQuote := func(s string) string {
				return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
			}
			quotedBin := shellQuote(r.brambleBin)
			quotedID := shellQuote(r.sessionID)
			type hookEntry struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			type hookGroup struct {
				Hooks []hookEntry `json:"hooks"`
			}
			hookSettings := struct {
				Hooks struct {
					Stop []hookGroup `json:"Stop"`
				} `json:"hooks"`
			}{
				Hooks: struct {
					Stop []hookGroup `json:"Stop"`
				}{
					Stop: []hookGroup{{
						Hooks: []hookEntry{{
							Type:    "command",
							Command: quotedBin + " notify --silent --session-id " + quotedID,
						}},
					}},
				},
			}
			if hookJSON, err := json.Marshal(hookSettings); err == nil {
				args = append(args, "--settings", string(hookJSON))
			}
		}
	}

	// Add the prompt last. When resuming, Claude CLI supports "--resume <id> <message>"
	// to resume the conversation and deliver the message in one command. Always pass
	// the prompt so the user's message is not silently discarded.
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

// codexNotifyConfig renders the `-c notify=[...]` override that makes a codex
// window report its own idleness, mirroring the Claude Stop hook built above.
//
// The value is TOML, so each element is a quoted string. Go's %q produces the
// same escaping TOML wants for the characters that can appear in a path or a
// generated session ID.
func codexNotifyConfig(brambleBin, sessionID string) string {
	parts := []string{brambleBin, "notify", "--silent", "--session-id", sessionID}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = fmt.Sprintf("%q", p)
	}
	return "notify=[" + strings.Join(quoted, ",") + "]"
}
