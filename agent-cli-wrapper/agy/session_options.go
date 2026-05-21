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
