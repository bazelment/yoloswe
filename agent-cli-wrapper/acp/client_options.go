package acp

import (
	"io"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

// ClientConfig holds ACP client configuration.
type ClientConfig struct {
	FsHandler         FsHandler
	TerminalHandler   TerminalHandler
	PermissionHandler PermissionHandler
	StderrHandler     func([]byte)
	ProtocolLogger    io.Writer
	Env               map[string]string
	BinaryPath        string
	ClientName        string
	ClientVersion     string
	BinaryArgs        []string
	EventBufferSize   int
}

func defaultACPClientConfig() ClientConfig {
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

// WithProtocolLogger sets a writer that receives all JSON-RPC messages
// exchanged with the agent subprocess. Sent messages are prefixed with ">> "
// and received messages with "<< ". The writer must be safe for concurrent
// use since reads and writes happen on different goroutines.
func WithProtocolLogger(w io.Writer) ClientOption {
	return func(c *ClientConfig) { c.ProtocolLogger = w }
}

// WithEnv sets additional environment variables for the agent subprocess.
func WithEnv(env map[string]string) ClientOption {
	return func(c *ClientConfig) { c.Env = env }
}

// WithLLMEndpoint points the gemini CLI at a third-party LLM endpoint by
// setting GEMINI_API_KEY and GOOGLE_GEMINI_BASE_URL in the subprocess env.
//
// Important: the upstream @google/gemini-cli (verified through 0.41.2 and
// current `main`) only speaks Google's GenerateContent protocol. It has no
// OpenAI/Anthropic passthrough — issue google-gemini/gemini-cli#1605 was
// closed "won't fix" — and `GOOGLE_GEMINI_BASE_URL` is purely a host-swap
// knob that still serializes Google-shaped requests to
// `${baseUrl}/v1beta/models/${model}:generateContent`.
//
// What this means in practice:
//   - To target Vertex AI or a Google-shaped reverse proxy, set BaseURL to
//     the proxy host and this option works as expected.
//   - To target an OpenAI-compatible (Baseten, vLLM, OpenAI) or
//     Anthropic-compatible endpoint, you must run a translating proxy in
//     front that accepts `:generateContent` and converts it to the target
//     wire format. Pointing this option directly at such an endpoint will
//     fail (the gemini binary will POST GenerateContent JSON the endpoint
//     can't parse).
//
// The Endpoint.WireAPI field is therefore informational only on this
// wrapper — gemini-cli has no equivalent of codex's `wire_api` switch.
//
// Existing Env/BinaryArgs entries are preserved.
func WithLLMEndpoint(ep llmendpoint.Endpoint) ClientOption {
	return func(c *ClientConfig) {
		if ep.IsZero() {
			return
		}
		if c.Env == nil {
			c.Env = make(map[string]string, 2)
		}
		if ep.BaseURL != "" {
			c.Env["GOOGLE_GEMINI_BASE_URL"] = ep.BaseURL
		}
		if key := ep.ResolvedKey(); key != "" {
			c.Env["GEMINI_API_KEY"] = key
		}
	}
}

// SessionConfig holds session-specific configuration.
type SessionConfig struct {
	CWD        string
	McpServers []McpServerConfig
}

func defaultACPSessionConfig() SessionConfig {
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
