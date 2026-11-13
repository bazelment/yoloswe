//go:build integration
// +build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
)

const liveSentinel = "ORCH-MODEL-LIVE-42"

type lookupSecretParams struct {
	Key string `json:"key" jsonschema:"required,description=Key name to look up"`
}

func TestHTTPClient_LiveMessagesAPI(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" &&
		os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" &&
		os.Getenv("ANTHROPIC_BASE_URL") == "" {
		t.Skip("skipping live model test: none of ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or ANTHROPIC_BASE_URL is set")
	}

	modelName := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if modelName == "" {
		modelName = "claude-haiku-4-5-20251001"
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	client := model.NewHTTPClient()
	reg := model.NewToolRegistry()
	var toolInvoked bool
	model.AddTool(reg, "lookup_secret", "Look up the secret token for a given key", func(_ context.Context, p lookupSecretParams) (string, error) {
		toolInvoked = true
		return liveSentinel + ":" + p.Key, nil
	})

	res, err := client.RunToolLoop(
		ctx,
		model.Request{
			Model:  modelName,
			System: "Always call the lookup_secret tool when asked for the secret token, then reply with the exact token returned.",
			Messages: []model.Message{
				model.NewUserMessage("Call lookup_secret with key=\"test\" and reply with the returned token."),
			},
			MaxTokens: 256,
		},
		reg,
		model.WithMaxTurns(4),
	)
	if err != nil {
		t.Fatalf("RunToolLoop live call failed: %v", err)
	}
	if !toolInvoked {
		t.Fatalf("expected lookup_secret tool to be invoked during live tool loop; response=%q", res.Response.Text())
	}
	if !strings.Contains(res.Response.Text(), liveSentinel) {
		t.Fatalf("live response %q did not contain sentinel %q", res.Response.Text(), liveSentinel)
	}
	if res.Usage.InputTokens <= 0 || res.Usage.OutputTokens <= 0 {
		t.Fatalf("expected positive token counts, got %+v", res.Usage)
	}
	if res.Usage.CostUSD <= 0 {
		t.Fatalf("expected positive CostUSD, got %+v", res.Usage)
	}
	t.Logf("live tool loop completed in %d turns: tokens=%d cost=$%.6f text=%q",
		res.TurnCount, res.Usage.TotalTokens(), res.Usage.CostUSD, strings.TrimSpace(res.Response.Text()))
}

var (
	_ model.ModelClient = (*model.HTTPClient)(nil)
	_ model.ModelClient = (*model.FakeClient)(nil)
)
