package claude

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

// SessionConfig holds session configuration.
type SessionConfig struct {
	// PermissionHandler handles permission requests in default mode.
	PermissionHandler PermissionHandler

	// InteractiveToolHandler handles interactive tools (AskUserQuestion, ExitPlanMode).
	// These tools require user input, not permission approval.
	InteractiveToolHandler InteractiveToolHandler

	// MCPConfig configures MCP servers for custom tools.
	MCPConfig *MCPConfig

	// StderrHandler is an optional handler for CLI stderr output.
	StderrHandler func([]byte)

	// Model to use: "haiku", "sonnet", "opus"
	Model string

	// RecordingDir is the directory for recordings (default: .claude-sessions).
	RecordingDir string

	// PermissionMode controls tool execution approval.
	PermissionMode PermissionMode

	// WorkDir is the working directory for file operations.
	WorkDir string

	// CLIPath is the path to the Claude CLI binary (uses "claude" in PATH if empty).
	CLIPath string

	// SystemPrompt overrides the default system prompt.
	SystemPrompt string

	// Resume is the session ID to resume. If set, the CLI will continue
	// a previous session instead of starting a new one.
	Resume string

	// EventBufferSize is the event channel buffer size (default: 100).
	EventBufferSize int

	// DisablePlugins disables CLI plugins for faster startup.
	DisablePlugins bool

	// RecordMessages enables session recording.
	RecordMessages bool

	// DangerouslySkipPermissions skips all permission prompts (use with caution).
	// This is typically used with PermissionModePlan to enable plan mode without prompts.
	DangerouslySkipPermissions bool

	// PermissionPromptToolStdio enables stdio-based permission prompts.
	// When true, all permission prompts flow through the control protocol
	// as can_use_tool control requests instead of CLI's interactive UI.
	// This enables fully programmatic permission control.
	PermissionPromptToolStdio bool
}

// SessionOption is a functional option for configuring a Session.
type SessionOption func(*SessionConfig)

// WithModel sets the model to use.
func WithModel(model string) SessionOption {
	return func(c *SessionConfig) {
		c.Model = model
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

// WithDisablePlugins disables CLI plugins.
func WithDisablePlugins() SessionOption {
	return func(c *SessionConfig) {
		c.DisablePlugins = true
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

// defaultConfig returns the default configuration.
func defaultConfig() SessionConfig {
	return SessionConfig{
		Model:           "haiku",
		PermissionMode:  PermissionModeDefault,
		EventBufferSize: 100,
		RecordingDir:    ".claude-sessions",
	}
}
