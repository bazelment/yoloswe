package cursor

// SessionConfig holds session configuration for the Cursor Agent CLI.
type SessionConfig struct {
	StderrHandler   func([]byte)
	Env             map[string]string
	Model           string
	WorkDir         string
	CLIPath         string // Path to the agent binary (default: "agent")
	ExtraArgs       []string
	EventBufferSize int
	Force           bool // --force flag
	Trust           bool // --trust flag
	Sandbox         bool // --sandbox flag
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

// WithCLIPath sets a custom CLI binary path (default: "agent").
func WithCLIPath(path string) SessionOption {
	return func(c *SessionConfig) {
		c.CLIPath = path
	}
}

// WithForce enables the --force flag.
func WithForce() SessionOption {
	return func(c *SessionConfig) {
		c.Force = true
	}
}

// WithTrust enables the --trust flag.
func WithTrust() SessionOption {
	return func(c *SessionConfig) {
		c.Trust = true
	}
}

// WithSandbox enables the --sandbox flag.
func WithSandbox() SessionOption {
	return func(c *SessionConfig) {
		c.Sandbox = true
	}
}

// WithEnv sets additional environment variables for the CLI process.
func WithEnv(env map[string]string) SessionOption {
	return func(c *SessionConfig) {
		c.Env = env
	}
}

// WithExtraArgs sets additional CLI arguments (escape hatch).
func WithExtraArgs(args ...string) SessionOption {
	return func(c *SessionConfig) {
		c.ExtraArgs = args
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

// defaultConfig returns the default configuration.
func defaultConfig() SessionConfig {
	return SessionConfig{
		CLIPath:         "agent",
		EventBufferSize: 100,
	}
}
