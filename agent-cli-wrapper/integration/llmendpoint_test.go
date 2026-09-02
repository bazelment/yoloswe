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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
// custom endpoint at runtime. Today that's claude and codex; cursor has no
// runtime subtest:
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
	runClaudeLLMEndpoint(t, ep, llmEndpointSmokeConfig{
		label:      "baseten",
		model:      basetenModel,
		prompt:     llmEndpointPrompt,
		sentinel:   llmSentinel,
		timeout:    90 * time.Second,
		clientName: "agent-cli-wrapper-llmendpoint-test",
	})
}

type llmEndpointSmokeConfig struct {
	label      string
	model      string
	prompt     string
	sentinel   string
	timeout    time.Duration
	clientName string
	// claudeMaxAttempts applies only when a successful Claude turn contains
	// no text. Zero means one attempt; transport and terminal errors are not
	// retried.
	claudeMaxAttempts int
}

func runClaudeLLMEndpoint(t *testing.T, ep llmendpoint.Endpoint, smoke llmEndpointSmokeConfig) {
	t.Helper()

	maxAttempts := smoke.claudeMaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var response string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// One deadline PER ATTEMPT, not one shared across all of them. The
		// retry exists for a CLI/wrapper defect that finalizes a turn with no
		// text, so each retry is a fresh session that needs a full turn's worth
		// of budget; sharing smoke.timeout meant only the first two or three of
		// the declared attempts could ever run, and the rest died on a deadline
		// that had already expired. The sibling bramble test
		// (bramble/integration/openrouter_test.go) gives each attempt its own
		// deadline for the same reason — the two live tests this PR describes
		// identically must behave identically.
		response = ""
		err := func() error {
			ctx, cancel := context.WithTimeout(context.Background(), smoke.timeout)
			defer cancel()
			var attemptErr error
			response, attemptErr = runClaudeLLMEndpointAttempt(ctx, t, ep, smoke)
			return attemptErr
		}()
		if err == nil {
			break
		}

		var noText *claudeNoTextError
		if !errors.As(err, &noText) {
			// A deadline here is this attempt's own budget running out, not a
			// transport failure — say so, or the message sends the next reader
			// to debug OpenRouter, which is exactly what the retry's comments
			// exist to prevent.
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("claude→%s attempt %d/%d exceeded its %s budget: %v "+
					"(this is the per-attempt deadline, not an endpoint or transport fault)",
					smoke.label, attempt, maxAttempts, smoke.timeout, err)
			}
			failLLMEndpointTurn(t, "claude", smoke.label, "turn", err)
			return
		}
		if attempt == maxAttempts {
			t.Fatalf("claude→%s CLI/wrapper completed with no text after %d attempts "+
				"(thinking_observed=%t, output_tokens=%d); sentinel %q was not verified",
				smoke.label, attempt, noText.thinkingObserved, noText.outputTokens, smoke.sentinel)
		}
		t.Logf("claude→%s CLI/wrapper completed with no text on attempt %d/%d "+
			"(thinking_observed=%t, output_tokens=%d); retrying",
			smoke.label, attempt, maxAttempts, noText.thinkingObserved, noText.outputTokens)
	}
	t.Logf("claude→%s response: %s", smoke.label, truncate(response, 200))
	assertLLMEndpointSentinel(t, "claude", smoke, response)
}

func runClaudeLLMEndpointAttempt(
	ctx context.Context,
	t *testing.T,
	ep llmendpoint.Endpoint,
	smoke llmEndpointSmokeConfig,
) (string, error) {
	t.Helper()

	session := claude.NewSession(
		claude.WithModel(smoke.model),
		claude.WithWorkDir(t.TempDir()),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithLLMEndpoint(ep),
	)
	if err := session.Start(ctx); err != nil {
		return "", fmt.Errorf("session start: %w", err)
	}
	defer session.Stop()

	if _, err := session.SendMessage(ctx, smoke.prompt); err != nil {
		return "", fmt.Errorf("SendMessage: %w", err)
	}
	return drainClaudeForLLMEndpoint(ctx, session)
}

type claudeNoTextError struct {
	outputTokens     int
	thinkingObserved bool
}

func (e *claudeNoTextError) Error() string {
	return fmt.Sprintf("claude CLI/wrapper turn completed without text (thinking_observed=%t, output_tokens=%d)",
		e.thinkingObserved, e.outputTokens)
}

func drainClaudeForLLMEndpoint(ctx context.Context, session *claude.Session) (string, error) {
	var response string
	var thinkingObserved bool
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
			case claude.ThinkingEvent:
				thinkingObserved = thinkingObserved || e.Thinking != "" || e.FullThinking != ""
			case claude.TurnCompleteEvent:
				if !e.Success {
					if e.Error != nil {
						return response, fmt.Errorf("claude turn failed: %w", e.Error)
					}
					return response, errors.New("claude turn failed: success=false")
				}
				outputTokens := e.Usage.OutputTokens
				completed, err := session.WaitForTurn(ctx)
				if err != nil {
					return response, fmt.Errorf("read completed Claude turn: %w", err)
				}
				if completed != nil {
					if strings.TrimSpace(completed.Text) != "" {
						response = completed.Text
					}
					thinkingObserved = thinkingObserved || strings.TrimSpace(completed.Thinking) != ""
					outputTokens = completed.Usage.OutputTokens
				}
				// Claude's handleResult does not require a text block before it
				// finalizes a successful turn. TurnCompleteEvent follows that CLI
				// result and all parsed assistant content, and WaitForTurn above
				// reads the cached canonical result. No later text can arrive for
				// this turn, so a successful no-text result must be retried in a
				// fresh session instead of waiting until ctx expires.
				if strings.TrimSpace(response) == "" {
					return response, &claudeNoTextError{
						outputTokens:     outputTokens,
						thinkingObserved: thinkingObserved,
					}
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
	runCodexLLMEndpoint(t, ep, llmEndpointSmokeConfig{
		label:      "baseten",
		model:      basetenModel,
		prompt:     llmEndpointPrompt,
		sentinel:   llmSentinel,
		timeout:    90 * time.Second,
		clientName: "agent-cli-wrapper-llmendpoint-test",
	})
}

func runCodexLLMEndpoint(t *testing.T, ep llmendpoint.Endpoint, smoke llmEndpointSmokeConfig) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), smoke.timeout)
	defer cancel()

	client := codex.NewClient(
		codex.WithClientName(smoke.clientName),
		codex.WithClientVersion("1.0.0"),
		codex.WithLLMEndpoint(ep),
	)
	if err := client.Start(ctx); err != nil {
		t.Fatalf("codex client start: %v", err)
	}
	defer client.Stop()

	thread, err := client.CreateThread(ctx,
		codex.WithModel(smoke.model),
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

	result, err := thread.Ask(ctx, smoke.prompt)
	if err != nil {
		failLLMEndpointTurn(t, "codex", smoke.label, "Ask", err)
		return
	}
	if !result.Success {
		if isOpenRouterHighDemand(smoke.label, fmt.Sprint(result.Error)) {
			t.Fatalf("codex→%s OpenRouter throttled the request: %v",
				smoke.label, result.Error)
		}
		t.Fatalf("codex→%s turn failed: %v\nfull text: %s",
			smoke.label, result.Error, truncate(result.FullText, 500))
	}
	t.Logf("codex→%s response: %s", smoke.label, truncate(result.FullText, 200))
	assertLLMEndpointSentinel(t, "codex", smoke, result.FullText)
}

func failLLMEndpointTurn(t *testing.T, cli, label, operation string, err error) {
	t.Helper()
	if isOpenRouterHighDemand(label, err.Error()) {
		t.Fatalf("%s→%s OpenRouter throttled the request during %s: %v",
			cli, label, operation, err)
	}
	t.Fatalf("%s→%s %s: %v", cli, label, operation, err)
}

func assertLLMEndpointSentinel(t *testing.T, cli string, smoke llmEndpointSmokeConfig, response string) {
	t.Helper()
	if isOpenRouterHighDemand(smoke.label, response) {
		t.Fatalf("%s→%s OpenRouter returned a throttle response: %s",
			cli, smoke.label, truncate(response, 500))
	}
	if strings.TrimSpace(response) == "" {
		t.Fatalf("%s→%s CLI/wrapper completed with no text; sentinel %q was not verified",
			cli, smoke.label, smoke.sentinel)
	}
	if !containsSecret(response, smoke.sentinel) {
		t.Fatalf("%s→%s returned the wrong answer; expected sentinel %q, got %s",
			cli, smoke.label, smoke.sentinel, truncate(response, 500))
	}
}

func isOpenRouterHighDemand(label, message string) bool {
	return label == "openrouter" &&
		strings.Contains(strings.ToLower(message), "currently experiencing high demand")
}

// Compile-time guards that the wrappers we're testing still expose the
// option signatures this test depends on. If WithLLMEndpoint disappears or
// changes shape upstream, the test fails to compile rather than silently
// degrading to a no-op.
var (
	_ claude.SessionOption = claude.WithLLMEndpoint(llmendpoint.Endpoint{})
	_ codex.ClientOption   = codex.WithLLMEndpoint(llmendpoint.Endpoint{})
)
