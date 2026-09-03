package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
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
type tmuxRunner struct { //nolint:govet // fieldalignment: keep launch configuration readable.
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
	llmEndpoint     llmendpoint.Endpoint
	yoloMode        bool // skip all permission prompts
	killOnStop      bool // kill tmux window on Stop()
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
	endpointEnv := r.endpointEnv()
	names := make([]string, 0, len(endpointEnv))
	for name := range endpointEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		args = append(args, "-e", name+"="+endpointEnv[name])
	}
	return args
}

// endpointEnv delegates endpoint translation to the same wrapper options used
// by in-process sessions, then returns the subprocess environment they produce.
//
// Secret exposure: the values below are spliced into `tmux new-window -e
// NAME=VALUE`, so a resolved API key is present in the tmux *client's* argv
// (readable via ps/proc by the same user) for the duration of that exec. tmux
// offers no argv-free way to seed a new window's environment, and the
// alternative — a transient on-disk key file the launched shell sources — buys
// a narrower window at the cost of a file lifecycle to get wrong. Documented
// rather than hidden; codex.WithLLMEndpoint's doc comment is corrected to
// match, since its env_key indirection only keeps the key out of *codex's*
// argv, not out of tmux's.
func (r *tmuxRunner) endpointEnv() map[string]string {
	if r.llmEndpoint.IsZero() {
		return nil
	}
	switch r.provider {
	case ProviderCodex:
		cfg := codex.ClientConfig{}
		codex.WithLLMEndpoint(r.llmEndpoint)(&cfg)
		return cfg.Env
	case ProviderClaude, "":
		cfg := claude.SessionConfig{}
		claude.WithModel(r.model)(&cfg)
		claude.WithLLMEndpoint(r.llmEndpoint)(&cfg)
		// The wrapper sets both ANTHROPIC_AUTH_TOKEN (Bearer) and
		// ANTHROPIC_API_KEY (x-api-key) because proxies differ on which they
		// accept. That is right for `claude -p`, which never prompts, but the
		// interactive CLI this runner launches reacts to ANTHROPIC_API_KEY on
		// a machine that also has an account login by blocking on a modal
		// ("Detected a custom API key in your environment"), whose default
		// answer is No — which would decline the endpoint's key and leave the
		// window pointed at ANTHROPIC_BASE_URL with the user's own
		// credentials. Nothing outside the integration harness answers startup
		// dialogs, so the window would simply park there. Bearer alone
		// authenticates against OpenRouter (verified: 401 on a bad key, 429 on
		// a good one), so drop the x-api-key half here. The cost is a
		// hypothetical Bearer-rejecting proxy, which would need its own
		// non-interactive path anyway.
		//
		// This is a deliberate divergence from the in-process path, which keeps
		// both variables because `claude -p` never prompts. It is pinned from
		// both sides — see TestTmuxRunnerNewWindowArgs_OpenRouterClaudeFullArgv
		// here and TestWithLLMEndpoint_setsEnv in the wrapper — so
		// an x-api-key-only gateway fails on a documented decision rather than
		// an accident. Such a gateway must use the in-process path.
		//
		// Shadowed with an empty value, NOT deleted. A tmux window's
		// environment is the server's global environment merged with the -e
		// overrides, so omitting a pair only declines to *add* it — it cannot
		// clear a value the user exported before the tmux server started,
		// which is the common case for anyone who has used claude with an API
		// key. Measured on tmux 3.4: with ANTHROPIC_API_KEY in the server
		// environment, a window launched without the pair sees the user's own
		// key, and one launched with `-e ANTHROPIC_API_KEY=` sees "". Deleting
		// therefore produced exactly what this code exists to prevent — the
		// modal fires anyway, and answering it sends the user's real Anthropic
		// credential to the third-party endpoint as x-api-key.
		cfg.Env["ANTHROPIC_API_KEY"] = ""
		return cfg.Env
	default:
		return nil
	}
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
	model := agent.CLIModelArg(r.model, provider)
	if provider == ProviderCodex {
		args = append(args, r.codexEndpointArgs()...)
		if model != "" {
			args = append(args, "-m", model)
		}
	} else if model != "" {
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
		// which is what a polling orchestrator reads as a lane still working,
		// and its parent is only told it finished when the window finally dies.
		//
		// Codex appends its own JSON payload as a trailing argument; `bramble
		// notify` takes no positional arguments, so the extra one is ignored.
		if r.sessionID != "" && r.brambleBin != "" {
			args = append(args, "-c", codexNotifyConfig(r.brambleBin, r.sessionID))
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
		} else {
			// agy binds its tools (shell cwd, file writes) to a registered
			// "project" resource, not to the process's actual working
			// directory: launching plain in workDir with no --new-project
			// leaves the session on agy's built-in default-cli-project, whose
			// projectResources is empty, so the shell tool falls back to
			// ~/.gemini/antigravity-cli/scratch regardless of tmux's -c. This
			// was confirmed by driving a real session and by direct CLI
			// checks: `agy --model ... --prompt-interactive 'run pwd'`
			// printed the scratch dir until --new-project was added, at which
			// point it registered workDir as a project resource
			// (~/.gemini/config/projects/<uuid>.json) and pwd/writes bound
			// correctly. --new-project only applies to a fresh session; a
			// resumed one already has its project bound via --conversation.
			args = append(args, "--new-project")
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

// codexEndpointArgs reuses codex.WithLLMEndpoint's config translation for the
// interactive CLI. Every flag that option emits applies here; only the spelling
// of one differs, because app-server's --config is -c on the interactive CLI
// (`codex --help`). In particular the third-party feature denylist applies:
// --disable is a top-level interactive option ("Equivalent to
// -c features.<name>=false"), and it is what keeps codex from sending tool
// schemas that strict Responses providers reject with HTTP 400 "unknown
// variant `namespace`" — see thirdPartyIncompatibleFeatures in
// agent-cli-wrapper/codex/client_options.go. Dropping it made the tmux path
// fail against gateways where the in-process path succeeds; OpenRouter's
// leniency is why the live test never caught it.
//
// Translating the whole stream rather than filtering for --config is the point:
// the previous filter silently dropped every flag it did not recognize, so a
// future addition to WithLLMEndpoint would go missing here again with no
// symptom until a request 400s.
//
// The walk is element-wise on purpose. Reading the stream as flag/value pairs
// would trade the silent drop for a silent desync — one single-token flag and
// every later pair reverses into `value flag`. Rewriting each token
// independently has no arity to get wrong: --config is the only spelling that
// differs, and no value WithLLMEndpoint emits (a model_providers.* assignment
// or a feature name) can collide with that literal.
func (r *tmuxRunner) codexEndpointArgs() []string {
	if r.llmEndpoint.IsZero() {
		return nil
	}
	cfg := codex.ClientConfig{}
	codex.WithLLMEndpoint(r.llmEndpoint)(&cfg)
	args := make([]string, 0, len(cfg.AppServerArgs))
	for _, a := range cfg.AppServerArgs {
		if a == "--config" {
			a = "-c"
		}
		args = append(args, a)
	}
	return args
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
