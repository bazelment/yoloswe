//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/router"
)

func TestOwnerChatRouterEval(t *testing.T) {
	if os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("NEBULA_LLM_KEY") == "" {
		t.Skip("live fallback evaluation needs a session OpenAI gateway key")
	}
	cases, err := router.OwnerChatCases()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ORCA_ROUTER_INNER_LOOP") == "1" {
		cases = cases[:5]
	}
	name := os.Getenv("ORCH_MODEL")
	if name == "" {
		name = "gpt-6.1-sol"
	}
	backend := router.FallbackRouter{Client: model.NewOpenAIClient(), Model: name}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	score := router.Evaluate(ctx, backend, cases)
	for _, r := range score.Results {
		fmt.Printf("CASE %s source=%s correct=%t expected=%v got=%v clarification=%t latency=%s error=%q\n", r.ID, r.SourceMailID, r.Correct, r.Expected, r.Got, r.Decision.NeedsClarification, r.Latency.Round(time.Millisecond), r.Error)
	}
	fmt.Printf("SCORECARD fallback model=%s: %s\n", name, score)
	fmt.Println("Gemini scoring skipped: no Gemini key exists for this owner. Labels unaudited; orchestrator will fund independent label audit.")
	if os.Getenv("ORCA_ROUTER_INNER_LOOP") == "1" {
		t.Logf("inner loop scorecard (targets intentionally not asserted): %s", score)
		return
	}
	if !score.MeetsTargets() {
		t.Errorf("router evaluation targets missed: %s", score)
	}
}
