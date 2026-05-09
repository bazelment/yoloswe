package acp

import (
	"slices"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

func TestWithLLMEndpoint_ChatWireAPI(t *testing.T) {
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
	if got := cfg.Env["OPENAI_API_KEY"]; got != "sk-test" {
		t.Errorf("OPENAI_API_KEY = %q", got)
	}
	if got := cfg.Env["OPENAI_BASE_URL"]; got != "https://inference.baseten.co/v1" {
		t.Errorf("OPENAI_BASE_URL = %q", got)
	}
	if !slices.Contains(cfg.BinaryArgs, "--openai-base-url") {
		t.Errorf("BinaryArgs missing --openai-base-url: %v", cfg.BinaryArgs)
	}
	if !slices.Contains(cfg.BinaryArgs, "https://inference.baseten.co/v1") {
		t.Errorf("BinaryArgs missing url: %v", cfg.BinaryArgs)
	}
	// API key must NOT appear in BinaryArgs.
	for _, a := range cfg.BinaryArgs {
		if a == "sk-test" {
			t.Fatalf("API key leaked to BinaryArgs: %v", cfg.BinaryArgs)
		}
	}
}

func TestWithLLMEndpoint_ResponsesWireAPI(t *testing.T) {
	t.Parallel()
	cfg := defaultACPClientConfig()
	WithLLMEndpoint(llmendpoint.Endpoint{
		BaseURL: "https://example.com",
		APIKey:  "sk-test",
		Wire:    llmendpoint.WireAPIResponses,
	})(&cfg)

	if _, ok := cfg.Env["OPENAI_BASE_URL"]; ok {
		t.Errorf("Responses wire should not set OPENAI_BASE_URL: %v", cfg.Env)
	}
	if slices.Contains(cfg.BinaryArgs, "--openai-base-url") {
		t.Errorf("Responses wire should not append openai args: %v", cfg.BinaryArgs)
	}
	if got := cfg.Env["GEMINI_API_KEY"]; got != "sk-test" {
		t.Errorf("GEMINI_API_KEY = %q", got)
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
