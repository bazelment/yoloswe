package codex

import (
	"fmt"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

// ClientConfig holds client configuration.
//
//nolint:govet // fieldalignment: keep handlers/env grouped before scalars.
type ClientConfig struct {
	// ApprovalHandler handles tool execution approval requests.
	ApprovalHandler ApprovalHandler

	// StderrHandler is an optional handler for app-server stderr output.
	StderrHandler func([]byte)

	// Env carries additional environment variables to set on the app-server
	// subprocess (appended to os.Environ).
	Env map[string]string

	// AppServerArgs is appended to the codex command line after "app-server".
	// Used to inject `--config 'model_providers.<name>.base_url=...'` style
	// overrides without writing to ~/.codex/config.toml.
	AppServerArgs []string

	// CodexPath is the path to the Codex CLI binary (uses "codex" in PATH if empty).
	CodexPath string

	// ClientName identifies this client to the app-server.
	ClientName string

	// ClientVersion is the client version string.
	ClientVersion string

	// SessionLogPath is the path to write session logs (JSON messages).
	// If empty, no session logging is performed.
	SessionLogPath string

	// EventBufferSize is the event channel buffer size (default: 100).
	EventBufferSize int
}

func defaultCodexClientConfig() ClientConfig {
	return ClientConfig{
		ClientName:      "codex-go-sdk",
		ClientVersion:   "1.0.0",
		EventBufferSize: 100,
	}
}

// ClientOption is a functional option for configuring a Client.
type ClientOption func(*ClientConfig)

// WithCodexPath sets a custom Codex CLI binary path.
func WithCodexPath(path string) ClientOption {
	return func(c *ClientConfig) {
		c.CodexPath = path
	}
}

// WithClientName sets the client name.
func WithClientName(name string) ClientOption {
	return func(c *ClientConfig) {
		c.ClientName = name
	}
}

// WithClientVersion sets the client version.
func WithClientVersion(version string) ClientOption {
	return func(c *ClientConfig) {
		c.ClientVersion = version
	}
}

// WithEventBufferSize sets the event channel buffer size.
func WithEventBufferSize(size int) ClientOption {
	return func(c *ClientConfig) {
		c.EventBufferSize = size
	}
}

// WithStderrHandler sets a handler for app-server stderr output.
func WithStderrHandler(h func([]byte)) ClientOption {
	return func(c *ClientConfig) {
		c.StderrHandler = h
	}
}

// WithApprovalHandler sets the handler for tool approval requests.
func WithApprovalHandler(h ApprovalHandler) ClientOption {
	return func(c *ClientConfig) {
		c.ApprovalHandler = h
	}
}

// WithSessionLogPath sets the path for session logging.
// All JSON messages sent and received will be logged to this file.
func WithSessionLogPath(path string) ClientOption {
	return func(c *ClientConfig) {
		c.SessionLogPath = path
	}
}

// WithEnv sets additional environment variables for the codex app-server
// subprocess. Existing entries are preserved.
func WithEnv(env map[string]string) ClientOption {
	return func(c *ClientConfig) {
		if c.Env == nil {
			c.Env = make(map[string]string, len(env))
		}
		for k, v := range env {
			c.Env[k] = v
		}
	}
}

// WithAppServerArgs appends additional arguments to the codex app-server
// command line (escape hatch). Multiple calls accumulate.
func WithAppServerArgs(args ...string) ClientOption {
	return func(c *ClientConfig) {
		c.AppServerArgs = append(c.AppServerArgs, args...)
	}
}

// WithLLMEndpoint configures the codex app-server to route inference through
// a third-party LLM endpoint by injecting `--config model_providers.<name>.*`
// overrides at app-server boot. The API key is exposed via env var
// (named by ep.APIKeyEnv) so it never lands in process args.
//
// The final `--config model_provider="<name>"` ensures the new provider
// becomes the default, overriding any value in ~/.codex/config.toml.
//
// Wire defaults to "chat" (OpenAI-compatible). Use "responses" for OpenAI's
// Responses API.
func WithLLMEndpoint(ep llmendpoint.Endpoint) ClientOption {
	return func(c *ClientConfig) {
		if ep.IsZero() {
			return
		}
		name := ep.Provider()
		wire := string(ep.WireAPI())
		envKey := ep.APIKeyEnv
		if envKey == "" {
			// Synthesize a stable env var name when only an inline key was
			// provided; codex resolves the key via env_key.
			envKey = "CODEX_LLMENDPOINT_API_KEY"
		}

		c.AppServerArgs = append(c.AppServerArgs,
			"--config", fmt.Sprintf("model_providers.%s.name=%q", name, name),
			"--config", fmt.Sprintf("model_providers.%s.base_url=%q", name, ep.BaseURL),
			"--config", fmt.Sprintf("model_providers.%s.wire_api=%q", name, wire),
			"--config", fmt.Sprintf("model_providers.%s.env_key=%q", name, envKey),
		)
		// Optional headers: codex supports http_headers as a TOML table.
		for hk, hv := range ep.Headers {
			c.AppServerArgs = append(c.AppServerArgs,
				"--config", fmt.Sprintf("model_providers.%s.http_headers.%s=%q", name, hk, hv),
			)
		}
		// Last so it overrides any default in ~/.codex/config.toml.
		c.AppServerArgs = append(c.AppServerArgs,
			"--config", fmt.Sprintf("model_provider=%q", name),
		)

		if c.Env == nil {
			c.Env = make(map[string]string, 1)
		}
		if key := ep.ResolvedKey(); key != "" {
			c.Env[envKey] = key
		}
	}
}

// ThreadConfig holds thread-specific configuration.
type ThreadConfig struct {
	// Sandbox configures the sandbox settings. Can be a string
	// ("read-only", "workspace-write", "danger-full-access") or a
	// *SandboxConfig struct for detailed configuration.
	Sandbox interface{}

	// Config is additional configuration options.
	Config map[string]interface{}

	// Model to use (e.g., "gpt-4o", "o4-mini").
	Model string

	// ModelProvider specifies the model provider.
	ModelProvider string

	// Profile is the Codex profile to use.
	Profile string

	// WorkDir is the working directory for the thread.
	WorkDir string

	// ApprovalPolicy controls tool execution approval.
	ApprovalPolicy ApprovalPolicy
}

func defaultCodexThreadConfig() ThreadConfig {
	return ThreadConfig{}
}

// ThreadOption is a functional option for configuring a Thread.
type ThreadOption func(*ThreadConfig)

// WithModel sets the model to use.
func WithModel(model string) ThreadOption {
	return func(c *ThreadConfig) {
		c.Model = model
	}
}

// WithModelProvider sets the model provider.
func WithModelProvider(provider string) ThreadOption {
	return func(c *ThreadConfig) {
		c.ModelProvider = provider
	}
}

// WithProfile sets the Codex profile.
func WithProfile(profile string) ThreadOption {
	return func(c *ThreadConfig) {
		c.Profile = profile
	}
}

// WithWorkDir sets the working directory.
func WithWorkDir(dir string) ThreadOption {
	return func(c *ThreadConfig) {
		c.WorkDir = dir
	}
}

// WithApprovalPolicy sets the approval policy.
func WithApprovalPolicy(policy ApprovalPolicy) ThreadOption {
	return func(c *ThreadConfig) {
		c.ApprovalPolicy = policy
	}
}

// WithSandbox sets the sandbox configuration. The value can be a string
// ("read-only", "workspace-write", "danger-full-access") or a *SandboxConfig
// struct for detailed configuration.
func WithSandbox(sandbox interface{}) ThreadOption {
	return func(c *ThreadConfig) {
		c.Sandbox = sandbox
	}
}

// WithThreadConfig sets additional configuration.
func WithThreadConfig(cfg map[string]interface{}) ThreadOption {
	return func(c *ThreadConfig) {
		c.Config = cfg
	}
}

// TurnConfig holds turn-specific configuration.
type TurnConfig struct {
	// SandboxPolicy overrides sandbox for this turn.
	SandboxPolicy interface{}

	// OutputSchema for structured output.
	OutputSchema interface{}

	// ApprovalPolicy overrides thread policy for this turn.
	ApprovalPolicy ApprovalPolicy

	// Model overrides the thread model for this turn.
	Model string

	// Effort controls reasoning effort (for o-series models).
	Effort string

	// Summary provides context for the turn.
	Summary string
}

func defaultCodexTurnConfig() TurnConfig {
	return TurnConfig{}
}

// TurnOption is a functional option for configuring a Turn.
type TurnOption func(*TurnConfig)

// WithTurnApprovalPolicy overrides the approval policy for this turn.
func WithTurnApprovalPolicy(policy ApprovalPolicy) TurnOption {
	return func(c *TurnConfig) {
		c.ApprovalPolicy = policy
	}
}

// WithTurnModel overrides the model for this turn.
func WithTurnModel(model string) TurnOption {
	return func(c *TurnConfig) {
		c.Model = model
	}
}

// WithEffort sets the reasoning effort level.
func WithEffort(effort string) TurnOption {
	return func(c *TurnConfig) {
		c.Effort = effort
	}
}

// WithSummary provides context for the turn.
func WithSummary(summary string) TurnOption {
	return func(c *TurnConfig) {
		c.Summary = summary
	}
}

// WithOutputSchema sets the expected output schema.
func WithOutputSchema(schema interface{}) TurnOption {
	return func(c *TurnConfig) {
		c.OutputSchema = schema
	}
}
