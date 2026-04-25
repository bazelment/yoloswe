package claude

import (
	"context"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/protocol"
)

// PermissionMode controls tool execution approval.
type PermissionMode string

const (
	// PermissionModeDefault prompts the user for each dangerous operation.
	PermissionModeDefault PermissionMode = "default"
	// PermissionModeAcceptEdits auto-approves file modifications.
	PermissionModeAcceptEdits PermissionMode = "acceptEdits"
	// PermissionModePlan reviews plan before execution.
	PermissionModePlan PermissionMode = "plan"
	// PermissionModeBypass auto-approves all tools (use with caution).
	PermissionModeBypass PermissionMode = "bypassPermissions"
)

// AgentDefinition defines a sub-agent for the Claude CLI.
type AgentDefinition struct {
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	Prompt          string   `json:"prompt,omitempty"`
	Model           string   `json:"model,omitempty"`
	AllowedTools    []string `json:"allowed_tools,omitempty"`
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
}

// SessionConfig holds session configuration.
type SessionConfig struct {
	InteractiveToolHandler     InteractiveToolHandler
	PermissionHandler          PermissionHandler
	ElicitationHandler         func(ctx context.Context, req protocol.ElicitationRequest) (protocol.ElicitationResponse, error)
	MCPConfig                  *MCPConfig
	StderrHandler              func([]byte)
	Env                        map[string]string
	Tools                      *string
	HookCallbackHandler        func(ctx context.Context, req protocol.HookCallbackRequest) (map[string]any, error)
	UsageHTTPClient            UsageHTTPClient
	Model                      string
	SystemPrompt               string
	Resume                     string
	RecordingDir               string
	CLIPath                    string
	WorkDir                    string
	OAuthToken                 string
	UsageBaseURL               string
	Effort                     EffortLevel
	PermissionMode             PermissionMode
	Betas                      []string
	ExtraArgs                  []string
	AllowedTools               []string
	Agents                     []AgentDefinition
	DisallowedTools            []string
	MaxTurns                   int
	MaxBudgetUSD               float64
	EventBufferSize            int
	KeepUserSettings           bool
	PermissionPromptToolStdio  bool
	DangerouslySkipPermissions bool
	RecordMessages             bool
	DisablePlugins             bool
}

// SessionOption is a functional option for configuring a Session.
type SessionOption func(*SessionConfig)

// WithModel sets the model to use.
func WithModel(model string) SessionOption {
	return func(c *SessionConfig) {
		c.Model = model
	}
}

// WithEffort sets the reasoning effort level for models that support effort.
// EffortAuto omits the --effort flag and lets the CLI/model default apply.
func WithEffort(level EffortLevel) SessionOption {
	return func(c *SessionConfig) {
		c.Effort = level
	}
}

// WithWorkDir sets the working directory.
func WithWorkDir(dir string) SessionOption {
	return func(c *SessionConfig) {
		c.WorkDir = dir
	}
}

// WithPermissionMode sets the permission mode.
func WithPermissionMode(mode PermissionMode) SessionOption {
	return func(c *SessionConfig) {
		c.PermissionMode = mode
	}
}

// WithCLIPath sets a custom CLI binary path.
func WithCLIPath(path string) SessionOption {
	return func(c *SessionConfig) {
		c.CLIPath = path
	}
}

// WithDisablePlugins disables CLI plugins by pointing --plugin-dir to /dev/null.
func WithDisablePlugins() SessionOption {
	return func(c *SessionConfig) {
		c.DisablePlugins = true
	}
}

// WithKeepUserSettings preserves the user's CLI settings and plugins.
// By default, SDK sessions disable external setting sources (--setting-sources "")
// and respect the DisablePlugins flag. When KeepUserSettings is true, the session
// behaves like an interactive CLI session, loading user/project settings and plugins.
func WithKeepUserSettings() SessionOption {
	return func(c *SessionConfig) {
		c.KeepUserSettings = true
	}
}

// WithDangerouslySkipPermissions skips all permission prompts.
// This is typically used with PermissionModePlan to enable plan mode without prompts.
func WithDangerouslySkipPermissions() SessionOption {
	return func(c *SessionConfig) {
		c.DangerouslySkipPermissions = true
	}
}

// WithRecording enables session recording.
func WithRecording(dir string) SessionOption {
	return func(c *SessionConfig) {
		c.RecordMessages = true
		if dir != "" {
			c.RecordingDir = dir
		}
	}
}

// WithPermissionHandler sets a custom permission handler.
func WithPermissionHandler(h PermissionHandler) SessionOption {
	return func(c *SessionConfig) {
		c.PermissionHandler = h
	}
}

// WithInteractiveToolHandler sets a handler for interactive tools
// (AskUserQuestion, ExitPlanMode). These tools require user input,
// not permission approval, so they bypass the permission handler.
func WithInteractiveToolHandler(handler InteractiveToolHandler) SessionOption {
	return func(c *SessionConfig) {
		c.InteractiveToolHandler = handler
	}
}

// WithEventBufferSize sets the event channel buffer size.
func WithEventBufferSize(size int) SessionOption {
	return func(c *SessionConfig) {
		c.EventBufferSize = size
	}
}

// WithStderrHandler sets a handler for CLI stderr output.
func WithStderrHandler(h func([]byte)) SessionOption {
	return func(c *SessionConfig) {
		c.StderrHandler = h
	}
}

// WithMCPConfig sets the MCP server configuration for custom tools.
func WithMCPConfig(cfg *MCPConfig) SessionOption {
	return func(c *SessionConfig) {
		c.MCPConfig = cfg
	}
}

// WithSystemPrompt sets a custom system prompt.
func WithSystemPrompt(prompt string) SessionOption {
	return func(c *SessionConfig) {
		c.SystemPrompt = prompt
	}
}

// WithPermissionPromptToolStdio enables stdio-based permission prompts.
// This causes all tool permissions to be sent as can_use_tool control requests
// instead of being handled by the CLI's interactive UI.
// Use this with WithPermissionHandler to implement programmatic permission control.
func WithPermissionPromptToolStdio() SessionOption {
	return func(c *SessionConfig) {
		c.PermissionPromptToolStdio = true
	}
}

// WithSDKTools is a convenience option that configures an SDK MCP server.
// If the session already has an MCPConfig, the SDK server is added to it;
// otherwise a new MCPConfig is created.
func WithSDKTools(serverName string, handler SDKToolHandler) SessionOption {
	return func(c *SessionConfig) {
		if c.MCPConfig == nil {
			c.MCPConfig = NewMCPConfig()
		}
		c.MCPConfig.AddSDKServer(serverName, handler)
	}
}

// WithResume sets a session ID to resume instead of starting a new session.
// When resuming, the CLI will load the previous conversation context.
func WithResume(sessionID string) SessionOption {
	return func(c *SessionConfig) {
		c.Resume = sessionID
	}
}

// WithMaxTurns sets the SDK-enforced turn limit.
func WithMaxTurns(n int) SessionOption {
	return func(c *SessionConfig) {
		c.MaxTurns = n
	}
}

// WithMaxBudgetUSD sets the SDK-enforced budget limit in USD.
func WithMaxBudgetUSD(budget float64) SessionOption {
	return func(c *SessionConfig) {
		c.MaxBudgetUSD = budget
	}
}

// WithAllowedTools sets the list of tools that Claude is allowed to use.
func WithAllowedTools(tools ...string) SessionOption {
	return func(c *SessionConfig) {
		c.AllowedTools = tools
	}
}

// WithDisallowedTools sets the list of tools that Claude is not allowed to use.
func WithDisallowedTools(tools ...string) SessionOption {
	return func(c *SessionConfig) {
		c.DisallowedTools = tools
	}
}

// WithBetas enables beta features.
func WithBetas(betas ...string) SessionOption {
	return func(c *SessionConfig) {
		c.Betas = betas
	}
}

// WithAgents sets the agent definitions for sub-agents.
func WithAgents(agents ...AgentDefinition) SessionOption {
	return func(c *SessionConfig) {
		c.Agents = agents
	}
}

// WithEnv sets additional environment variables for the CLI process.
func WithEnv(env map[string]string) SessionOption {
	return func(c *SessionConfig) {
		c.Env = env
	}
}

// WithOAuthToken sets the OAuth token used by Usage.
func WithOAuthToken(token string) SessionOption {
	return func(c *SessionConfig) {
		c.OAuthToken = token
	}
}

// WithUsageBaseURL overrides the Anthropic API base URL used by Usage.
func WithUsageBaseURL(url string) SessionOption {
	return func(c *SessionConfig) {
		c.UsageBaseURL = url
	}
}

// WithUsageHTTPClient overrides the HTTP client used by Usage.
func WithUsageHTTPClient(client UsageHTTPClient) SessionOption {
	return func(c *SessionConfig) {
		c.UsageHTTPClient = client
	}
}

// WithTools sets the base set of available built-in tools.
// Use "" to disable all built-in tools (useful when only MCP tools should be available).
// Use "default" to use all tools. Use comma-separated names for specific tools.
func WithTools(tools string) SessionOption {
	return func(c *SessionConfig) {
		c.Tools = &tools
	}
}

// WithExtraArgs sets additional CLI arguments (escape hatch).
func WithExtraArgs(args ...string) SessionOption {
	return func(c *SessionConfig) {
		c.ExtraArgs = args
	}
}

// WithHookCallbackHandler registers a handler for hook_callback control requests.
func WithHookCallbackHandler(h func(ctx context.Context, req protocol.HookCallbackRequest) (map[string]any, error)) SessionOption {
	return func(c *SessionConfig) {
		c.HookCallbackHandler = h
	}
}

// WithElicitationHandler registers a handler for elicitation control requests.
func WithElicitationHandler(h func(ctx context.Context, req protocol.ElicitationRequest) (protocol.ElicitationResponse, error)) SessionOption {
	return func(c *SessionConfig) {
		c.ElicitationHandler = h
	}
}

// defaultConfig returns the default configuration.
func defaultConfig() SessionConfig {
	return SessionConfig{
		Model:           "haiku",
		PermissionMode:  PermissionModeDefault,
		EventBufferSize: 100,
		RecordingDir:    ".claude-sessions",
	}
}
