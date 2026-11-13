//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/replay"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/router/eval"
)

func TestOwnerChatRouterEval(t *testing.T) {
	if os.Getenv("ORCA_GEMINI_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("NEBULA_LLM_KEY") == "" {
		t.Skip("live routing evaluation needs a Gemini or session OpenAI gateway key")
	}
	cases, err := eval.OwnerChatCases()
	if err != nil {
		t.Fatal(err)
	}
	if split := os.Getenv("ORCA_ROUTER_SPLIT"); split != "" {
		cases, err = eval.OwnerChatCasesSplit(replay.Split(split))
	}
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
	backend := router.Select(model.NewOpenAIClient(), name, os.Getenv("ORCA_GEMINI_API_KEY"), "", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	score := eval.Evaluate(ctx, backend, cases)
	for _, r := range score.Results {
		fmt.Printf("CASE %s source=%s correct=%t expected=%v got=%s backend=%s fallback=%t latency=%s error=%q\n", r.ID, r.SourceMailID, r.Correct, r.Expected, r.Got, r.Decision.Backend, r.Decision.Fallback, r.Latency, r.Error)
	}
	fmt.Printf("SCORECARD split=%s model=%s gemini=%t: %s\n", os.Getenv("ORCA_ROUTER_SPLIT"), name, os.Getenv("ORCA_GEMINI_API_KEY") != "", score)
	if os.Getenv("ORCA_GEMINI_API_KEY") != "" && score.P95Latency >= 1500*time.Millisecond {
		t.Errorf("Gemini p95 exceeded 1.5s: %s", score.P95Latency)
	}
	if os.Getenv("ORCA_ROUTER_INNER_LOOP") == "1" {
		t.Logf("inner loop scorecard (targets intentionally not asserted): %s", score)
		return
	}
	if !score.MeetsTargets() {
		t.Errorf("router evaluation targets missed: %s", score)
	}
}
