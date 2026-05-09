package claude

import (
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

func TestWithLLMEndpoint_setsEnv(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Env = map[string]string{"PRESERVED": "yes"}

	WithLLMEndpoint(llmendpoint.Endpoint{
		BaseURL: "https://inference.baseten.co",
		APIKey:  "sk-test",
	})(&cfg)

	if got := cfg.Env["ANTHROPIC_BASE_URL"]; got != "https://inference.baseten.co" {
		t.Errorf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := cfg.Env["ANTHROPIC_AUTH_TOKEN"]; got != "sk-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q", got)
	}
	if got := cfg.Env["ANTHROPIC_API_KEY"]; got != "sk-test" {
		t.Errorf("ANTHROPIC_API_KEY = %q", got)
	}
	if got := cfg.Env["PRESERVED"]; got != "yes" {
		t.Errorf("preserved env lost: %q", got)
	}
}

func TestWithLLMEndpoint_resolvesFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_TEST_KEY", "from-env")
	cfg := defaultConfig()

	WithLLMEndpoint(llmendpoint.Endpoint{
		BaseURL:   "https://example.com",
		APIKeyEnv: "CLAUDE_TEST_KEY",
	})(&cfg)

	if got := cfg.Env["ANTHROPIC_AUTH_TOKEN"]; got != "from-env" {
		t.Errorf("got %q", got)
	}
}

func TestWithLLMEndpoint_zeroIsNoop(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	WithLLMEndpoint(llmendpoint.Endpoint{})(&cfg)
	if len(cfg.Env) != 0 {
		t.Errorf("zero endpoint should be no-op, got env=%v", cfg.Env)
	}
}

func TestWithLLMEndpoint_pinsModelDefaultsWhenModelSet(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Model = "moonshotai/Kimi-K2.6"
	WithLLMEndpoint(llmendpoint.Endpoint{BaseURL: "https://x", APIKey: "k"})(&cfg)

	for _, name := range []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
	} {
		if got := cfg.Env[name]; got != "moonshotai/Kimi-K2.6" {
			t.Errorf("%s = %q, want moonshotai/Kimi-K2.6", name, got)
		}
	}
}

func TestWithLLMEndpoint_skipsModelPinWhenModelEmpty(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Model = ""
	WithLLMEndpoint(llmendpoint.Endpoint{BaseURL: "https://x", APIKey: "k"})(&cfg)
	if _, set := cfg.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"]; set {
		t.Errorf("model-pin should be skipped when c.Model is empty")
	}
}

func TestWithLLMEndpoint_stripsTrailingV1(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"https://inference.baseten.co/v1", "https://inference.baseten.co"},
		{"https://inference.baseten.co/v1/", "https://inference.baseten.co"},
		{"https://example.com", "https://example.com"},
		{"https://example.com/api", "https://example.com/api"},
	}
	for _, tc := range cases {
		cfg := defaultConfig()
		WithLLMEndpoint(llmendpoint.Endpoint{BaseURL: tc.in, APIKey: "k"})(&cfg)
		if got := cfg.Env["ANTHROPIC_BASE_URL"]; got != tc.want {
			t.Errorf("BaseURL %q -> ANTHROPIC_BASE_URL=%q, want %q", tc.in, got, tc.want)
		}
	}
}
