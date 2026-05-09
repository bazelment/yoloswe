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
		BaseURL: "https://inference.baseten.co/v1",
		APIKey:  "sk-test",
	})(&cfg)

	if got := cfg.Env["ANTHROPIC_BASE_URL"]; got != "https://inference.baseten.co/v1" {
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
