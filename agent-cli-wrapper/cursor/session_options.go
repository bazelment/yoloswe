package cursor

import "github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"

// SessionConfig holds session configuration for the Cursor Agent CLI.
type SessionConfig struct {
	StderrHandler   func([]byte)
	Env             map[string]string
	Model           string
	WorkDir         string
	CLIPath         string // Path to the agent binary (default: "agent")
	Resume          string // Chat/session ID to resume.
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

// WithResume sets a chat/session ID to resume.
func WithResume(id string) SessionOption {
	return func(c *SessionConfig) {
		c.Resume = id
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

// WithLLMEndpoint points the cursor-agent CLI at a third-party LLM endpoint
// by setting OPENAI_BASE_URL and OPENAI_API_KEY in the subprocess env.
//
// Note: cursor-agent routes inference through Cursor's own backend by default.
// These env vars only affect cursor-agent builds that respect the OpenAI
// envvar convention for arbitrary models; for Cursor-managed models the
// option is a no-op as far as the upstream CLI is concerned. The option is
// provided for symmetry with the other wrappers.
//
// Existing entries in SessionConfig.Env are preserved.
func WithLLMEndpoint(ep llmendpoint.Endpoint) SessionOption {
	return func(c *SessionConfig) {
		if ep.IsZero() {
			return
		}
		if c.Env == nil {
			c.Env = make(map[string]string, 2)
		}
		if ep.BaseURL != "" {
			c.Env["OPENAI_BASE_URL"] = ep.BaseURL
		}
		if key := ep.ResolvedKey(); key != "" {
			c.Env["OPENAI_API_KEY"] = key
		}
	}
}

// defaultConfig returns the default configuration.
func defaultConfig() SessionConfig {
	return SessionConfig{
		CLIPath:         "agent",
		EventBufferSize: 100,
	}
}
