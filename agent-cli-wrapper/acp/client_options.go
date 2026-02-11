package acp

// ClientConfig holds ACP client configuration.
type ClientConfig struct {
	FsHandler         FsHandler
	TerminalHandler   TerminalHandler
	PermissionHandler PermissionHandler
	StderrHandler     func([]byte)
	Env               map[string]string
	BinaryPath        string
	ClientName        string
	ClientVersion     string
	BinaryArgs        []string
	EventBufferSize   int
}

func defaultClientConfig() ClientConfig {
	return ClientConfig{
		BinaryPath:      "gemini",
		BinaryArgs:      []string{"--experimental-acp"},
		ClientName:      "acp-go-sdk",
		ClientVersion:   "1.0.0",
		EventBufferSize: 100,
	}
}

// ClientOption is a functional option for configuring a Client.
type ClientOption func(*ClientConfig)

// WithBinaryPath sets the path to the ACP agent binary.
func WithBinaryPath(path string) ClientOption {
	return func(c *ClientConfig) { c.BinaryPath = path }
}

// WithBinaryArgs sets the command-line arguments for the agent binary.
func WithBinaryArgs(args ...string) ClientOption {
	return func(c *ClientConfig) { c.BinaryArgs = args }
}

// WithClientName sets the client name for identification.
func WithClientName(name string) ClientOption {
	return func(c *ClientConfig) { c.ClientName = name }
}

// WithClientVersion sets the client version string.
func WithClientVersion(version string) ClientOption {
	return func(c *ClientConfig) { c.ClientVersion = version }
}

// WithEventBufferSize sets the event channel buffer size.
func WithEventBufferSize(size int) ClientOption {
	return func(c *ClientConfig) { c.EventBufferSize = size }
}

// WithStderrHandler sets a handler for agent stderr output.
func WithStderrHandler(h func([]byte)) ClientOption {
	return func(c *ClientConfig) { c.StderrHandler = h }
}

// WithFsHandler sets the file system handler.
func WithFsHandler(h FsHandler) ClientOption {
	return func(c *ClientConfig) { c.FsHandler = h }
}

// WithTerminalHandler sets the terminal handler.
func WithTerminalHandler(h TerminalHandler) ClientOption {
	return func(c *ClientConfig) { c.TerminalHandler = h }
}

// WithPermissionHandler sets the permission handler.
func WithPermissionHandler(h PermissionHandler) ClientOption {
	return func(c *ClientConfig) { c.PermissionHandler = h }
}

// WithEnv sets additional environment variables for the agent subprocess.
func WithEnv(env map[string]string) ClientOption {
	return func(c *ClientConfig) { c.Env = env }
}

// SessionConfig holds session-specific configuration.
type SessionConfig struct {
	CWD        string
	McpServers []McpServerConfig
}

func defaultSessionConfig() SessionConfig {
	return SessionConfig{}
}

// SessionOption is a functional option for configuring a Session.
type SessionOption func(*SessionConfig)

// WithSessionCWD sets the working directory for the session.
func WithSessionCWD(cwd string) SessionOption {
	return func(c *SessionConfig) { c.CWD = cwd }
}

// WithSessionMcpServers sets MCP server configurations for the session.
func WithSessionMcpServers(servers ...McpServerConfig) SessionOption {
	return func(c *SessionConfig) { c.McpServers = servers }
}
