package acp

import (
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

// TestWithLLMEndpoint_GoogleShape verifies the Google-shaped env-var wiring
// that gemini-cli actually honors. The chat/responses Wire field is
// informational only on this wrapper — gemini-cli speaks GenerateContent
// regardless — so neither wire value should leak OpenAI-specific config.
func TestWithLLMEndpoint_GoogleShape(t *testing.T) {
	t.Parallel()
	cfg := defaultACPClientConfig()
	WithLLMEndpoint(llmendpoint.Endpoint{
		BaseURL: "https://inference.baseten.co/v1",
		APIKey:  "sk-test",
		Wire:    llmendpoint.WireAPIChat,
	})(&cfg)

	if got := cfg.Env["GEMINI_API_KEY"]; got != "sk-test" {
		t.Errorf("GEMINI_API_KEY = %q", got)
	}
	if got := cfg.Env["GOOGLE_GEMINI_BASE_URL"]; got != "https://inference.baseten.co/v1" {
		t.Errorf("GOOGLE_GEMINI_BASE_URL = %q", got)
	}
	// gemini-cli has no OpenAI passthrough; nothing OpenAI-shaped should be set.
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if _, ok := cfg.Env[k]; ok {
			t.Errorf("unexpected env var %s set: %v", k, cfg.Env)
		}
	}
	for _, a := range cfg.BinaryArgs {
		if a == "--openai-base-url" || a == "--openai-api-key" {
			t.Errorf("unexpected --openai-* arg appended: %v", cfg.BinaryArgs)
		}
		if a == "sk-test" {
			t.Fatalf("API key leaked to BinaryArgs: %v", cfg.BinaryArgs)
		}
	}
}

// TestWithLLMEndpoint_ResponsesWireAPI confirms the wire field really is
// informational — the wiring is identical to the chat case.
func TestWithLLMEndpoint_ResponsesWireAPI(t *testing.T) {
	t.Parallel()
	cfg := defaultACPClientConfig()
	WithLLMEndpoint(llmendpoint.Endpoint{
		BaseURL: "https://example.com",
		APIKey:  "sk-test",
		Wire:    llmendpoint.WireAPIResponses,
	})(&cfg)

	if got := cfg.Env["GEMINI_API_KEY"]; got != "sk-test" {
		t.Errorf("GEMINI_API_KEY = %q", got)
	}
	if got := cfg.Env["GOOGLE_GEMINI_BASE_URL"]; got != "https://example.com" {
		t.Errorf("GOOGLE_GEMINI_BASE_URL = %q", got)
	}
	if _, ok := cfg.Env["OPENAI_BASE_URL"]; ok {
		t.Errorf("OPENAI_BASE_URL should never be set on gemini: %v", cfg.Env)
	}
}

func TestWithLLMEndpoint_zeroIsNoop(t *testing.T) {
	t.Parallel()
	cfg := defaultACPClientConfig()
	WithLLMEndpoint(llmendpoint.Endpoint{})(&cfg)
	if len(cfg.Env) != 0 {
		t.Errorf("zero endpoint should be no-op: %v", cfg.Env)
	}
}

// TestWithBinaryArgs_ReplacesNotAppends pins the documented behavior that
// WithBinaryArgs replaces BinaryArgs wholesale. WithLLMEndpoint no longer
// touches BinaryArgs (gemini-cli has no relevant flags), so this is purely
// a regression-pin on WithBinaryArgs's own contract.
func TestWithBinaryArgs_ReplacesNotAppends(t *testing.T) {
	t.Parallel()
	cfg := defaultACPClientConfig()
	if len(cfg.BinaryArgs) == 0 {
		t.Fatal("precondition: defaultACPClientConfig should seed BinaryArgs")
	}
	WithBinaryArgs("--replaced")(&cfg)
	if len(cfg.BinaryArgs) != 1 || cfg.BinaryArgs[0] != "--replaced" {
		t.Fatalf("WithBinaryArgs should replace: got %v", cfg.BinaryArgs)
	}
}

// TestWithLLMEndpoint_AfterWithEnv_Survives verifies the ordering the
// gemini provider relies on: WithEnv replaces the env map wholesale, so
// callers must apply WithLLMEndpoint AFTER WithEnv (or the LLM creds get
// dropped). The provider does this; this test guards it.
func TestWithLLMEndpoint_AfterWithEnv_Survives(t *testing.T) {
	t.Parallel()
	cfg := defaultACPClientConfig()
	WithEnv(map[string]string{"FOO": "bar"})(&cfg)
	WithLLMEndpoint(llmendpoint.Endpoint{
		BaseURL: "https://example.com",
		APIKey:  "sk-test",
		Wire:    llmendpoint.WireAPIChat,
	})(&cfg)
	if got := cfg.Env["GEMINI_API_KEY"]; got != "sk-test" {
		t.Errorf("LLMEndpoint creds dropped after WithEnv: %v", cfg.Env)
	}
	if got := cfg.Env["FOO"]; got != "bar" {
		t.Errorf("WithEnv var dropped: %v", cfg.Env)
	}
}

// Compile-time guard: WithBinaryArgs and WithLLMEndpoint signatures stay
// stable. Drift here would silently break the gemini provider's option
// pipeline before any test ran.
var (
	_ ClientOption = WithBinaryArgs("--experimental-acp")
	_ ClientOption = WithLLMEndpoint(llmendpoint.Endpoint{})
)
