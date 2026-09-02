package agy

import "time"

// SessionConfig holds configuration for one agy print-mode invocation.
type SessionConfig struct {
	StderrHandler              func([]byte)
	Env                        map[string]string
	WorkDir                    string
	CLIPath                    string
	ConversationID             string
	LogFile                    string
	Model                      string
	Effort                     string
	ExtraArgs                  []string
	AddDirs                    []string
	PrintTimeout               time.Duration
	EventBufferSize            int
	DangerouslySkipPermissions bool
	Sandbox                    bool
}

// SessionOption configures a Session.
type SessionOption func(*SessionConfig)

// WithWorkDir sets the subprocess working directory.
func WithWorkDir(dir string) SessionOption {
	return func(c *SessionConfig) {
		c.WorkDir = dir
	}
}

// WithCLIPath sets a custom agy binary path.
func WithCLIPath(path string) SessionOption {
	return func(c *SessionConfig) {
		c.CLIPath = path
	}
}

// WithConversation resumes a previous agy conversation by ID.
func WithConversation(id string) SessionOption {
	return func(c *SessionConfig) {
		c.ConversationID = id
	}
}

// WithLogFile writes agy logs to a specific file.
func WithLogFile(path string) SessionOption {
	return func(c *SessionConfig) {
		c.LogFile = path
	}
}

// WithModel selects the model agy uses for the session, e.g. "gemini-3.8-flash-low"
// or "claude-sonnet-4-6". See `agy models` for the live catalog. An empty id emits
// no --model flag, leaving agy's own default in effect.
func WithModel(id string) SessionOption {
	return func(c *SessionConfig) {
		c.Model = id
	}
}

// WithEffort sets the reasoning effort agy requests for the session.
//
// agy's own --effort flag documents "low|medium|high". This wrapper is a thin
// transport: it does not validate, normalize, or clamp the value in any way —
// it passes the string through verbatim as the argument to --effort, and an
// empty string emits no --effort flag at all. Callers that need to map a
// richer effort concept (e.g. an EffortLevel enum) onto agy's vocabulary, or
// reject/clamp values outside low|medium|high, own that logic themselves
// before calling WithEffort.
func WithEffort(level string) SessionOption {
	return func(c *SessionConfig) {
		c.Effort = level
	}
}

// WithAddDir adds an additional workspace directory. It can be repeated.
func WithAddDir(dir string) SessionOption {
	return func(c *SessionConfig) {
		c.AddDirs = append(c.AddDirs, dir)
	}
}

// WithPrintTimeout sets agy's print-mode wait timeout.
func WithPrintTimeout(timeout time.Duration) SessionOption {
	return func(c *SessionConfig) {
		c.PrintTimeout = timeout
	}
}

// WithDangerouslySkipPermissions auto-approves agy tool permission requests.
func WithDangerouslySkipPermissions() SessionOption {
	return func(c *SessionConfig) {
		c.DangerouslySkipPermissions = true
	}
}

// WithSandbox asks agy to run with terminal sandbox restrictions.
func WithSandbox() SessionOption {
	return func(c *SessionConfig) {
		c.Sandbox = true
	}
}

// WithEnv adds environment variables to the subprocess.
func WithEnv(env map[string]string) SessionOption {
	return func(c *SessionConfig) {
		c.Env = env
	}
}

// WithExtraArgs appends raw CLI arguments.
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

func defaultConfig() SessionConfig {
	return SessionConfig{
		CLIPath:         "agy",
		EventBufferSize: 100,
	}
}
