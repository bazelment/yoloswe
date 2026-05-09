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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
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

// IsZero reports whether the endpoint carries no routing or auth signal —
// BaseURL, APIKey, and APIKeyEnv all empty. ProviderName/Wire/Headers alone
// don't disable IsZero because they're decorations on a routing+auth pair;
// a config that sets only those is partial and Validate will reject it.
func (e Endpoint) IsZero() bool {
	return e.BaseURL == "" && e.APIKey == "" && e.APIKeyEnv == ""
}

// hasOnlyDecorations reports whether the endpoint has ProviderName, Wire, or
// Headers set without a BaseURL/auth pair. Used to surface partial configs
// at Validate-time instead of silently treating them as disabled.
func (e Endpoint) hasOnlyDecorations() bool {
	return e.IsZero() && (e.ProviderName != "" || e.Wire != "" || len(e.Headers) > 0)
}

// providerNameRE accepts identifiers safe to interpolate into codex's
// `model_providers.<name>.*` config-path segments without quoting: ASCII
// letters/digits plus '_' and '-'. Anything else (spaces, dots, slashes,
// quotes, shell metachars) is rejected at config-load time so we never
// emit invalid `--config` args.
var providerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// headerNameRE matches header names that are safe to interpolate into codex's
// `model_providers.<name>.http_headers.<key>=...` config-path segments without
// quoting. RFC 7230 allows a much wider token set, but codex's TOML parser
// would reject any of the punctuation (`!#$%&'*+.^|~`) that's legal under
// RFC 7230 in a bare path segment, so accepting them here would just turn
// into a runtime arg error. Hyphens and underscores are the standard HTTP
// header chars after letters/digits and survive bare TOML keys.
var headerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Validate reports configuration errors. A zero Endpoint validates as nil.
// Partial endpoints — e.g. provider_name set without base_url — fail loudly
// rather than being treated as disabled, since they signal user intent that
// the wrapper would otherwise silently drop.
func (e Endpoint) Validate() error {
	if e.hasOnlyDecorations() {
		return errors.New("llmendpoint: endpoint is partially configured (provider_name/wire/headers set without base_url + api_key/api_key_env)")
	}
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
	if u.Host == "" {
		return fmt.Errorf("llmendpoint: BaseURL %q is missing a host", e.BaseURL)
	}
	if e.APIKey == "" && e.APIKeyEnv == "" {
		return errors.New("llmendpoint: either APIKey or APIKeyEnv must be set")
	}
	if e.Wire != "" && e.Wire != WireAPIChat && e.Wire != WireAPIResponses {
		return fmt.Errorf("llmendpoint: unknown wire api %q", e.Wire)
	}
	if e.ProviderName != "" && !providerNameRE.MatchString(e.ProviderName) {
		return fmt.Errorf("llmendpoint: invalid ProviderName %q (allowed: A-Za-z0-9_-)", e.ProviderName)
	}
	for k := range e.Headers {
		if !headerNameRE.MatchString(k) {
			return fmt.Errorf("llmendpoint: invalid header name %q", k)
		}
	}
	return nil
}

// Redacted returns a copy with secret-bearing fields cleared: APIKey is
// dropped and Headers are dropped entirely (keys remain non-secret in
// String() via the fingerprint, but in Redacted-then-logged contexts we
// can't make that distinction safely — operators may have stuffed auth
// tokens into header values). APIKeyEnv is preserved so logs still
// indicate where the key came from.
func (e Endpoint) Redacted() Endpoint {
	out := e.Clone()
	out.APIKey = ""
	out.Headers = nil
	return out
}

// Clone returns a deep copy. The Headers map is duplicated so callers that
// mutate the original after passing the endpoint to a provider can't fool
// equality checks against a previously-bound snapshot.
func (e Endpoint) Clone() Endpoint {
	out := e
	if e.Headers != nil {
		out.Headers = make(map[string]string, len(e.Headers))
		for k, v := range e.Headers {
			out.Headers[k] = v
		}
	}
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

// String renders the endpoint without leaking the literal API key. Header
// keys are listed (sorted, no values) and a short hash of the full key+value
// bag is appended so divergence diagnostics that include String() can also
// distinguish endpoints that differ only on header *values*. Raw values would
// risk leaking secrets operators may have shoved into headers, so the hash is
// the privacy-safe equivalent — equal headers produce equal fingerprints,
// any change flips them.
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
	hdrs := ""
	if len(e.Headers) > 0 {
		keys := make([]string, 0, len(e.Headers))
		for k := range e.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		h := sha256.New()
		for _, k := range keys {
			fmt.Fprintf(h, "%s=%s\n", k, e.Headers[k])
		}
		fp := hex.EncodeToString(h.Sum(nil))[:8]
		hdrs = fmt.Sprintf(" headers=[%s]/%s", strings.Join(keys, ","), fp)
	}
	return fmt.Sprintf("llmendpoint{base=%s provider=%s wire=%s key=%s%s}",
		e.BaseURL, e.Provider(), e.WireAPI(), keySrc, hdrs)
}
