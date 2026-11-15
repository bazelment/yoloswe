package recipe_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/recipe"
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
	wantAgents := map[string][]recipe.AgentSpec{
		recipe.PhaseSWE: {
			{Harness: "claude", Model: "opus", Effort: "high"},
			{Harness: "codex", Model: "gpt-6.1-sol", Effort: "high"},
		},
		recipe.PhaseClean: {
			{Harness: "claude", Model: "sonnet", Effort: "medium"},
			{Harness: "codex", Model: "gpt-6.1-sol", Effort: "medium"},
		},
		recipe.PhaseReview: {
			{Harness: "claude", Model: "sonnet", Effort: "high"},
			{Harness: "codex", Model: "gpt-6.1-sol", Effort: "high"},
		},
		recipe.PhaseIntegrate: {
			{Harness: "claude", Model: "sonnet", Effort: "medium"},
		},
	}
	for _, phase := range def.Phases {
		if want := wantAgents[phase.Name]; !slices.Equal(phase.Agents, want) {
			t.Errorf("%s agents = %+v, want %+v", phase.Name, phase.Agents, want)
		}
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

func TestWithoutIntegrate(t *testing.T) {
	t.Parallel()

	def := recipe.Default()
	noInt := recipe.WithoutIntegrate(def)
	if err := noInt.Validate(); err != nil {
		t.Fatalf("WithoutIntegrate(Default()).Validate() error: %v", err)
	}
	wantPhases := []string{recipe.PhaseSWE, recipe.PhaseClean, recipe.PhaseReview}
	if got := noInt.PhaseNames(); !slices.Equal(got, wantPhases) {
		t.Fatalf("PhaseNames() = %v, want %v", got, wantPhases)
	}
	tr, _ := noInt.MatchTransitions(recipe.PhaseReview, recipe.OutcomeDone, false, 1)
	if tr == nil || tr.To != recipe.TargetComplete {
		t.Fatalf("review done (ciGreen=false) transition = %+v, want -> %q", tr, recipe.TargetComplete)
	}

	// Custom recipe where integrate transitions to a post_review phase rather than complete:
	// WithoutIntegrate must still terminate at review -> complete and prune unreachable post_review.
	custom := recipe.Recipe{
		Name:    "custom-post-integrate",
		Version: 1,
		Phases: []recipe.Phase{
			{Name: recipe.PhaseSWE, Agents: []recipe.AgentSpec{{Harness: "claude", Model: "opus"}}},
			{Name: recipe.PhaseReview, Agents: []recipe.AgentSpec{{Harness: "claude", Model: "sonnet"}}},
			{Name: recipe.PhaseIntegrate, Agents: []recipe.AgentSpec{{Harness: "claude", Model: "sonnet"}}},
			{Name: "notify", Agents: []recipe.AgentSpec{{Harness: "claude", Model: "haiku"}}},
		},
		Transitions: []recipe.Transition{
			{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: "notify"},
			{From: "notify", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.TargetComplete},
		},
	}
	customNoInt := recipe.WithoutIntegrate(custom)
	if err := customNoInt.Validate(); err != nil {
		t.Fatalf("WithoutIntegrate(custom).Validate() error: %v", err)
	}
	if got := customNoInt.PhaseNames(); !slices.Equal(got, []string{recipe.PhaseSWE, recipe.PhaseReview}) {
		t.Fatalf("customNoInt.PhaseNames() = %v, want [swe review]", got)
	}
	trCustom, _ := customNoInt.MatchTransitions(recipe.PhaseReview, recipe.OutcomeDone, false, 1)
	if trCustom == nil || trCustom.To != recipe.TargetComplete {
		t.Fatalf("customNoInt review done transition = %+v, want -> complete", trCustom)
	}

	// Single-phase integrate recipe is rewritten to review so integrate never runs.
	single := recipe.Recipe{
		Name:    "single",
		Version: 1,
		Phases:  []recipe.Phase{{Name: recipe.PhaseIntegrate, Agents: []recipe.AgentSpec{{Harness: "claude", Model: "sonnet"}}}},
		Transitions: []recipe.Transition{
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
		},
	}
	singleOut := recipe.WithoutIntegrate(single)
	if err := singleOut.Validate(); err != nil {
		t.Fatalf("WithoutIntegrate(single).Validate() error: %v", err)
	}
	if got := singleOut.PhaseNames(); !slices.Equal(got, []string{recipe.PhaseReview}) {
		t.Fatalf("WithoutIntegrate(single) = %v, want [review]", got)
	}
}
