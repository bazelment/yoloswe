//go:build integration
// +build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
)

func TestCompiler_LiveMessagesAPI(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" &&
		os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" &&
		os.Getenv("ANTHROPIC_BASE_URL") == "" {
		t.Skip("skipping live recipe compiler test: none of ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or ANTHROPIC_BASE_URL is set")
	}

	modelName := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if modelName == "" {
		modelName = recipe.DefaultCompilerModel
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	var client model.ModelClient = model.NewHTTPClient()
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		recordPath := filepath.Join(t.TempDir(), "live_recipe_compile.json")
		if ws := strings.TrimSpace(os.Getenv("BUILD_WORKSPACE_DIRECTORY")); ws != "" {
			recordPath = filepath.Join(ws, "orca/recipe/testdata/live_recipe_compile.json")
		}
		t.Logf("recording live recipe exchanges to %s", recordPath)
		client = model.NewRecordingFakeClient(recordPath, client)
	}

	compiler := recipe.NewCompiler(client, recipe.WithModel(modelName))

	t.Run("DefaultRecipeRoundTrip", func(t *testing.T) {
		res, err := compiler.CompileText(ctx, recipe.DefaultName, recipe.DefaultSource)
		if err != nil {
			t.Fatalf("CompileText(DefaultSource) live call failed: %v", err)
		}
		if err := res.Recipe.Validate(); err != nil {
			t.Fatalf("Validate() failed: %v", err)
		}
		wantPhases := []string{recipe.PhaseSWE, recipe.PhaseClean, recipe.PhaseReview, recipe.PhaseIntegrate}
		if got := res.Recipe.PhaseNames(); !slices.Equal(got, wantPhases) {
			t.Fatalf("PhaseNames() = %v, want %v", got, wantPhases)
		}
		if !strings.Contains(res.EchoTable, "### Recipe `default`") {
			t.Fatalf("unexpected EchoTable:\n%s", res.EchoTable)
		}
		if res.Usage.InputTokens <= 0 || res.Usage.OutputTokens <= 0 {
			t.Fatalf("expected positive token usage, got %+v", res.Usage)
		}
		t.Logf("live default recipe compiled: tokens=%d cost=$%.6f\n%s",
			res.Usage.TotalTokens(), res.Usage.CostUSD, res.EchoTable)
	})

	t.Run("PerLaneOverrideNoReviewWithJudgmentAndUnmapped", func(t *testing.T) {
		base := recipe.Default()
		delta := "this lane: no code review; if the diff touches database migrations, escalate to the dispatcher; and send a carrier pigeon when merged"
		res, err := compiler.CompileOverride(ctx, base, "no-review-custom", delta)
		if err != nil {
			t.Fatalf("CompileOverride live call failed: %v", err)
		}
		if err := res.Recipe.Validate(); err != nil {
			t.Fatalf("Validate() failed: %v", err)
		}
		if _, hasReview := res.Recipe.Phase(recipe.PhaseReview); hasReview {
			t.Fatalf("expected review phase to be removed in override, got phases %v", res.Recipe.PhaseNames())
		}
		if res.Recipe.BaseRecipe != recipe.DefaultName {
			t.Fatalf("BaseRecipe = %q, want %q", res.Recipe.BaseRecipe, recipe.DefaultName)
		}
		if len(res.Unmapped) == 0 {
			t.Fatalf("expected unmapped phrase for carrier pigeon, got empty Unmapped")
		}
		t.Logf("live override compiled: unmapped=%v\n%s", res.Unmapped, res.EchoTable)
	})
}

var (
	_ recipe.CompilerOption = recipe.WithModel(recipe.DefaultCompilerModel)
	_ recipe.CompilerOption = recipe.WithMaxTokens(recipe.DefaultCompilerMaxTokens)
)
