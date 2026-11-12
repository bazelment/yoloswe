package recipe_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orch/recipe"
)

func TestDefaultRecipe(t *testing.T) {
	t.Parallel()

	def := recipe.Default()
	if err := def.Validate(); err != nil {
		t.Fatalf("Default().Validate() unexpected error: %v", err)
	}
	if def.Name != recipe.DefaultName {
		t.Fatalf("Name = %q, want %q", def.Name, recipe.DefaultName)
	}
	if def.Version != 1 {
		t.Fatalf("Version = %d, want 1", def.Version)
	}

	wantPhases := []string{
		recipe.PhaseSWE,
		recipe.PhaseClean,
		recipe.PhaseReview,
		recipe.PhaseIntegrate,
	}
	if got := def.PhaseNames(); !slices.Equal(got, wantPhases) {
		t.Fatalf("PhaseNames() = %v, want %v", got, wantPhases)
	}
	if def.FirstPhase() != recipe.PhaseSWE {
		t.Fatalf("FirstPhase() = %q, want %q", def.FirstPhase(), recipe.PhaseSWE)
	}

	tr1, judg := def.MatchTransitions(recipe.PhaseSWE, recipe.OutcomeDone, false, 1)
	if tr1 == nil || tr1.To != recipe.PhaseClean || len(judg) != 0 {
		t.Fatalf("swe done transition = (%+v, %v), want -> clean", tr1, judg)
	}
	tr2, _ := def.MatchTransitions(recipe.PhaseClean, recipe.OutcomeDone, false, 1)
	if tr2 == nil || tr2.To != recipe.PhaseReview {
		t.Fatalf("clean done transition = %+v, want -> review", tr2)
	}
	tr3, _ := def.MatchTransitions(recipe.PhaseReview, recipe.OutcomeDone, true, 1)
	if tr3 == nil || tr3.To != recipe.PhaseIntegrate {
		t.Fatalf("review done+ci_green transition = %+v, want -> integrate", tr3)
	}
	trRework, _ := def.MatchTransitions(recipe.PhaseReview, recipe.OutcomeNeedsSWE, false, 2)
	if trRework == nil || trRework.To != recipe.PhaseSWE || !trRework.AdvanceRound {
		t.Fatalf("review needs-swe r2 transition = %+v, want -> swe (AdvanceRound=true)", trRework)
	}
	trEscalate, _ := def.MatchTransitions(recipe.PhaseReview, recipe.OutcomeNeedsSWE, false, 3)
	if trEscalate == nil || trEscalate.To != recipe.TargetEscalate {
		t.Fatalf("review needs-swe r3 transition = %+v, want -> escalate", trEscalate)
	}
	trDone, _ := def.MatchTransitions(recipe.PhaseIntegrate, recipe.OutcomeDone, true, 1)
	if trDone == nil || trDone.To != recipe.TargetComplete {
		t.Fatalf("integrate done+ci_green transition = %+v, want -> complete", trDone)
	}

	table := def.FormatTable()
	for _, p := range wantPhases {
		if !strings.Contains(table, p) {
			t.Errorf("FormatTable() missing phase %q:\n%s", p, table)
		}
	}
}

func TestRecipeValidationAndJudgmentTransitions(t *testing.T) {
	t.Parallel()

	r := recipe.Default()
	r.Transitions = append(r.Transitions, recipe.Transition{
		From: recipe.PhaseClean,
		On: recipe.Condition{
			Verdict:      recipe.OutcomeDone,
			JudgmentRule: "if the change touches auth or migrations",
		},
		To: recipe.PhaseReview,
	})

	mech, judg := r.MatchTransitions(recipe.PhaseClean, recipe.OutcomeDone, false, 1)
	if mech == nil || mech.To != recipe.PhaseReview {
		t.Fatalf("expected mechanical transition to review, got %+v", mech)
	}
	if len(judg) != 1 || !judg[0].On.NeedsJudgment() {
		t.Fatalf("expected 1 judgment transition, got %+v", judg)
	}

	cloned := r.Clone()
	cloned.Phases[0].Name = "mutated"
	if r.Phases[0].Name == "mutated" {
		t.Fatal("Clone() did not deep-copy Phases slice")
	}

	bad := recipe.Default()
	bad.Phases[0].Name = "swe2"
	if err := bad.Validate(); err == nil {
		t.Fatal("expected Validate() error for phase name ending in digit")
	}
}
