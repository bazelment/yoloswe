// Package llmendpoint describes a third-party LLM API endpoint that an
// agent-cli-wrapper subpackage can be pointed at.
//
// Each wrapper (claude, codex, cursor, acp/gemini) accepts an Endpoint via a
// WithLLMEndpoint option and translates it into the env vars and CLI flags its
// CLI binary expects. The translation is per-wrapper because each upstream CLI
// honors a different convention:
//
//   - claude: ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN (Anthropic-shaped only)
//   - codex:  --config model_providers.<name>.{base_url,wire_api,env_key}
//   - acp:    GEMINI_API_KEY/GOOGLE_GEMINI_BASE_URL or OPENAI_BASE_URL+--openai-base-url
//   - cursor: best-effort OPENAI_BASE_URL/OPENAI_API_KEY (cursor-agent ignores
//     these for non-Cursor models; the option exists for symmetry)
package llmendpoint

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// WireAPI identifies the request/response shape spoken by the endpoint.
type WireAPI string

const (
	// WireAPIChat is the OpenAI-compatible Chat Completions shape (Baseten,
	// LiteLLM, vLLM, OpenRouter, most third-party gateways).
	WireAPIChat WireAPI = "chat"
	// WireAPIResponses is the OpenAI Responses API shape (codex native).
	WireAPIResponses WireAPI = "responses"
)

// DefaultProviderName is used when Endpoint.ProviderName is empty.
const DefaultProviderName = "custom"

// Endpoint describes a single third-party LLM endpoint.
//
// Prefer APIKeyEnv over APIKey so the literal key never has to be carried
// through Go memory or serialized config. Wrappers will resolve the key by
// reading the named env var at subprocess-launch time.
//
//nolint:govet // fieldalignment: keep semantically-ordered fields readable.
type Endpoint struct {
	// BaseURL is the HTTPS endpoint (e.g. "https://inference.baseten.co/v1").
	BaseURL string

	// APIKey is the resolved literal key. Avoid setting this directly; use
	// APIKeyEnv instead so the key is read from the process env at launch.
	APIKey string

	// APIKeyEnv is the env var name holding the API key (e.g. "BASETEN_API_KEY").
	APIKeyEnv string

	// ProviderName labels the endpoint inside the underlying CLI's config
	// (codex's `model_providers.<name>`, etc.). Defaults to DefaultProviderName.
	ProviderName string

	// Wire selects the request/response shape; defaults to WireAPIChat.
	Wire WireAPI

	// Headers carries optional extra HTTP headers to inject on each request.
	// Not every wrapper supports this; unsupported wrappers ignore the field.
	Headers map[string]string
}

// IsZero reports whether the endpoint is unset (no BaseURL).
func (e Endpoint) IsZero() bool {
	return e.BaseURL == "" && e.APIKey == "" && e.APIKeyEnv == ""
}

// Validate reports configuration errors. A zero Endpoint validates as nil.
func (e Endpoint) Validate() error {
	if e.IsZero() {
		return nil
	}
	if e.BaseURL == "" {
		return errors.New("llmendpoint: BaseURL is required")
	}
	u, err := url.Parse(e.BaseURL)
	if err != nil {
		return fmt.Errorf("llmendpoint: invalid BaseURL %q: %w", e.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("llmendpoint: BaseURL must be http(s), got %q", u.Scheme)
	}
	if e.APIKey == "" && e.APIKeyEnv == "" {
		return errors.New("llmendpoint: either APIKey or APIKeyEnv must be set")
	}
	if e.Wire != "" && e.Wire != WireAPIChat && e.Wire != WireAPIResponses {
		return fmt.Errorf("llmendpoint: unknown wire api %q", e.Wire)
	}
	if e.ProviderName != "" && strings.ContainsAny(e.ProviderName, ` "'.\`) {
		return fmt.Errorf("llmendpoint: invalid ProviderName %q", e.ProviderName)
	}
	return nil
}

// Redacted returns a copy with APIKey cleared. APIKeyEnv is preserved so logs
// still indicate where the key came from.
func (e Endpoint) Redacted() Endpoint {
	out := e
	out.APIKey = ""
	return out
}

// ResolvedKey returns APIKey if set, else os.Getenv(APIKeyEnv), else "".
func (e Endpoint) ResolvedKey() string {
	if e.APIKey != "" {
		return e.APIKey
	}
	if e.APIKeyEnv != "" {
		return os.Getenv(e.APIKeyEnv)
	}
	return ""
}

// Provider returns ProviderName or DefaultProviderName.
func (e Endpoint) Provider() string {
	if e.ProviderName == "" {
		return DefaultProviderName
	}
	return e.ProviderName
}

// WireAPI returns Wire or WireAPIChat as the default.
func (e Endpoint) WireAPI() WireAPI {
	if e.Wire == "" {
		return WireAPIChat
	}
	return e.Wire
}

// String renders the endpoint without leaking the literal API key.
func (e Endpoint) String() string {
	if e.IsZero() {
		return "llmendpoint{}"
	}
	keySrc := "<unset>"
	switch {
	case e.APIKeyEnv != "":
		keySrc = "$" + e.APIKeyEnv
	case e.APIKey != "":
		keySrc = "<inline>"
	}
	return fmt.Sprintf("llmendpoint{base=%s provider=%s wire=%s key=%s}",
		e.BaseURL, e.Provider(), e.WireAPI(), keySrc)
}
