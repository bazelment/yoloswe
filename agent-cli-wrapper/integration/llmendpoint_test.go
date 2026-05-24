//go:build integration
// +build integration

// Manual integration tests that exercise WithLLMEndpoint against a real
// third-party LLM API endpoint. Default fixture: Baseten + moonshotai/Kimi-K2.6,
// chosen because Baseten exposes all three Anthropic/OpenAI surfaces:
//
//   - /v1/chat/completions  (OpenAI Chat Completions)
//   - /v1/responses         (OpenAI Responses API; codex 0.130+ requires this)
//   - /v1/messages          (Anthropic Messages API; claude CLI uses this)
//
// Each backend gets its own subtest and is skipped when the relevant CLI
// binary is missing or BASETEN_API_KEY is unset. None of these run under
// `bazel test //...` (the target carries `manual` and `integration` tags).
//
// Run:
//
//	BASETEN_API_KEY=... bazel test \
//	    //agent-cli-wrapper/integration:integration_test \
//	    --test_filter=TestLLMEndpoint_Baseten \
//	    --test_tag_filters=integration \
//	    --test_env=BASETEN_API_KEY \
//	    --test_output=streamed
//
// Or directly:
//
//	BASETEN_API_KEY=... go test -tags=integration -v \
//	    -run TestLLMEndpoint_Baseten \
//	    ./agent-cli-wrapper/integration/...
//
// What's verified end-to-end:
//   - The wrapper's per-backend env-var + arg translation reaches Baseten.
//   - The model id flows through unchanged.
//   - Baseten returns a usable response (the model echoes a unique sentinel).
//   - Bug fixes from PR #240 stay fixed: claude's /v1 doubling, claude's
//     hardcoded haiku side-call model, codex's incompatible default features.
package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

const (
	basetenBaseURL   = "https://inference.baseten.co/v1"
	basetenAPIKeyEnv = "BASETEN_API_KEY"
	basetenModel     = "moonshotai/Kimi-K2.6"

	// llmSentinel is unique enough that only a real model round-trip can
	// produce it. Keep it short so tiny max_tokens budgets reproduce it.
	llmSentinel       = "PURPLE-RHINO-42"
	llmEndpointPrompt = "Reply with exactly this single token, nothing else: " + llmSentinel
)

// TestLLMEndpoint_Baseten runs WithLLMEndpoint smoke against Baseten's
// Kimi-K2.6 deployment for the wrappers whose CLIs currently honor a
// custom endpoint at runtime. Today that's claude and codex; cursor has
// compile-time guards (below) but no runtime subtest:
//
//   - cursor-agent ignores OPENAI_BASE_URL when its model id is not a
//     recognized third-party model.
//
// Each subtest skips (not fails) when its prerequisites aren't met.
func TestLLMEndpoint_Baseten(t *testing.T) {
	apiKey := os.Getenv(basetenAPIKeyEnv)
	if apiKey == "" {
		t.Skipf("%s not set; export the key (see ~/.keys.sh) and re-run", basetenAPIKeyEnv)
	}

	endpoint := llmendpoint.Endpoint{
		BaseURL:      basetenBaseURL,
		APIKeyEnv:    basetenAPIKeyEnv,
		ProviderName: "baseten",
	}

	t.Run("claude/messages", func(t *testing.T) {
		if _, err := exec.LookPath("claude"); err != nil {
			t.Skip("claude CLI not on PATH")
		}
		// Claude CLI uses /v1/messages (Anthropic shape); wire is irrelevant.
		ep := endpoint
		runClaudeBaseten(t, ep)
	})

	t.Run("codex/responses", func(t *testing.T) {
		if _, err := exec.LookPath("codex"); err != nil {
			t.Skip("codex CLI not on PATH")
		}
		// Codex 0.130+ requires wire_api="responses".
		ep := endpoint
		ep.Wire = llmendpoint.WireAPIResponses
		runCodexBaseten(t, ep)
	})
}

// runClaudeBaseten verifies WithLLMEndpoint correctly drives the claude CLI
// against Baseten's /v1/messages. Regression-pins:
//   - trailing /v1 is stripped (otherwise /v1/v1/messages → 404)
//   - default-model envs are pinned so claude's preflight + post-turn calls
//     don't 404 against Baseten's single-model endpoint
func runClaudeBaseten(t *testing.T, ep llmendpoint.Endpoint) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	session := claude.NewSession(
		claude.WithModel(basetenModel),
		claude.WithWorkDir(t.TempDir()),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithLLMEndpoint(ep),
	)
	if err := session.Start(ctx); err != nil {
		t.Fatalf("claude session start: %v", err)
	}
	defer session.Stop()

	if _, err := session.SendMessage(ctx, llmEndpointPrompt); err != nil {
		t.Fatalf("claude SendMessage: %v", err)
	}

	response, err := drainClaudeForLLMEndpoint(ctx, session)
	if err != nil {
		t.Fatalf("claude turn drain: %v", err)
	}
	t.Logf("claude→baseten response: %s", truncate(response, 200))

	if !containsSecret(response, llmSentinel) {
		t.Fatalf("claude→baseten did not echo sentinel %q; got %s",
			llmSentinel, truncate(response, 500))
	}
}

func drainClaudeForLLMEndpoint(ctx context.Context, session *claude.Session) (string, error) {
	var response string
	for {
		select {
		case <-ctx.Done():
			return response, ctx.Err()
		case ev, ok := <-session.Events():
			if !ok {
				return response, errors.New("claude event channel closed before turn complete")
			}
			switch e := ev.(type) {
			case claude.TextEvent:
				if e.FullText != "" {
					response = e.FullText
				}
			case claude.TurnCompleteEvent:
				if !e.Success {
					return response, errors.New("claude turn failed: success=false")
				}
				return response, nil
			}
		}
	}
}

// runCodexBaseten verifies WithLLMEndpoint drives codex against Baseten's
// /v1/responses surface. Regression-pins:
//   - --config model_providers.<name>.* lands at app-server boot
//   - the third-party-incompatible feature denylist (multi_agent, apps,
//     browser_use, ...) is auto-applied so Baseten's strict tool-schema
//     parser doesn't 400 with `unknown variant "namespace"`
func runCodexBaseten(t *testing.T, ep llmendpoint.Endpoint) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := codex.NewClient(
		codex.WithClientName("agent-cli-wrapper-llmendpoint-test"),
		codex.WithClientVersion("1.0.0"),
		codex.WithLLMEndpoint(ep),
	)
	if err := client.Start(ctx); err != nil {
		t.Fatalf("codex client start: %v", err)
	}
	defer client.Stop()

	thread, err := client.CreateThread(ctx,
		codex.WithModel(basetenModel),
		codex.WithWorkDir(t.TempDir()),
		codex.WithApprovalPolicy(codex.ApprovalPolicyFullAuto),
		codex.WithSandbox("read-only"),
	)
	if err != nil {
		t.Fatalf("codex CreateThread: %v", err)
	}
	if err := thread.WaitReady(ctx); err != nil {
		t.Fatalf("codex thread WaitReady: %v", err)
	}

	result, err := thread.Ask(ctx, llmEndpointPrompt)
	if err != nil {
		t.Fatalf("codex Ask: %v", err)
	}
	if !result.Success {
		t.Fatalf("codex turn failed: %v\nfull text: %s",
			result.Error, truncate(result.FullText, 500))
	}
	t.Logf("codex→baseten response: %s", truncate(result.FullText, 200))

	if !containsSecret(result.FullText, llmSentinel) {
		t.Fatalf("codex→baseten did not echo sentinel %q; got %s",
			llmSentinel, truncate(result.FullText, 500))
	}
}

// Compile-time guards that the wrappers we're testing still expose the
// option signatures this test depends on. If WithLLMEndpoint disappears or
// changes shape upstream, the test fails to compile rather than silently
// degrading to a no-op.
var (
	_ claude.SessionOption = claude.WithLLMEndpoint(llmendpoint.Endpoint{})
	_ codex.ClientOption   = codex.WithLLMEndpoint(llmendpoint.Endpoint{})
	_ acp.ClientOption     = acp.WithLLMEndpoint(llmendpoint.Endpoint{})
)
