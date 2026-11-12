package recipe_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/recipe"
)

const goldenReplayFixturePath = "testdata/golden_replay.json"

type goldenCase struct {
	Verify   func(t *testing.T, res *recipe.CompileResult)
	ID       string
	Request  recipe.CompileRequest
	Expected recipe.Recipe
}

func overrideFrom(base recipe.Recipe, name, source string) recipe.Recipe {
	r := base.Clone()
	r.Name = name
	r.Version = 1
	if name == base.Name && base.Version > 0 {
		r.Version = base.Version + 1
	}
	r.Source = source
	r.BaseRecipe = base.Name
	r.BaseVersion = max(base.Version, 1)
	return r
}

func goldenCases() []goldenCase {
	def := recipe.Default()

	case01 := goldenCase{
		ID: "01_default_roundtrip",
		Request: recipe.CompileRequest{
			Name:   recipe.DefaultName,
			Source: recipe.DefaultSource,
		},
		Expected: def.Clone(),
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if !slices.Equal(res.Recipe.PhaseNames(), []string{"swe", "clean", "review", "integrate"}) {
				t.Fatalf("PhaseNames = %v", res.Recipe.PhaseNames())
			}
			if len(res.Unmapped) != 0 {
				t.Fatalf("expected no unmapped phrases, got %v", res.Unmapped)
			}
		},
	}

	noReview := overrideFrom(def, "no-review", "this lane: no code review")
	integrateNoReview := def.Phases[3].Clone()
	integrateNoReview.BriefTemplate = "Verify green CI checks on HEAD, then integrate the PR into the target branch."
	integrateNoReview.Gates = []recipe.Gate{
		{Name: "ci_green", Required: true},
	}
	noReview.Phases = []recipe.Phase{
		def.Phases[0].Clone(),
		def.Phases[1].Clone(),
		integrateNoReview,
	}
	noReview.Transitions = []recipe.Transition{
		{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseClean},
		{From: recipe.PhaseClean, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
		{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
		{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
	}
	noReview.MergePolicy.RequireReviewPass = false
	case02 := goldenCase{
		ID: "02_override_no_code_review",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   noReview.Name,
			Source: noReview.Source,
		},
		Expected: noReview,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if _, hasReview := res.Recipe.Phase(recipe.PhaseReview); hasReview {
				t.Fatal("expected review phase to be removed")
			}
			if res.Recipe.MergePolicy.RequireReviewPass {
				t.Fatal("expected RequireReviewPass=false")
			}
			if res.Recipe.BaseRecipe != recipe.DefaultName || res.Recipe.BaseVersion != 1 {
				t.Fatalf("base metadata = (%q, %d), want (default, 1)", res.Recipe.BaseRecipe, res.Recipe.BaseVersion)
			}
		},
	}

	skipClean := overrideFrom(def, "skip-clean", "this lane: skip clean, go straight from swe to review")
	skipClean.Phases = []recipe.Phase{
		def.Phases[0].Clone(),
		def.Phases[2].Clone(),
		def.Phases[3].Clone(),
	}
	skipClean.Transitions = []recipe.Transition{
		{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 2}, To: recipe.PhaseSWE, AdvanceRound: true},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}, To: recipe.TargetEscalate},
		{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
		{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
	}
	case03 := goldenCase{
		ID: "03_override_skip_clean",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   skipClean.Name,
			Source: skipClean.Source,
		},
		Expected: skipClean,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if !slices.Equal(res.Recipe.PhaseNames(), []string{"swe", "review", "integrate"}) {
				t.Fatalf("PhaseNames = %v, want [swe review integrate]", res.Recipe.PhaseNames())
			}
		},
	}

	skipCleanDocs := overrideFrom(def, recipe.DefaultName, "skip clean for docs")
	skipCleanDocs.Transitions = slices.Insert(skipCleanDocs.Transitions, 0, recipe.Transition{
		From: recipe.PhaseSWE,
		On: recipe.Condition{
			Verdict:      recipe.OutcomeDone,
			JudgmentRule: "if the change only modifies documentation or markdown files",
		},
		To: recipe.PhaseReview,
	})
	case04 := goldenCase{
		ID: "04_override_skip_clean_for_docs",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   skipCleanDocs.Name,
			Source: skipCleanDocs.Source,
		},
		Expected: skipCleanDocs,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.Version != 2 {
				t.Fatalf("Version = %d, want 2", res.Recipe.Version)
			}
			mech, judg := res.Recipe.MatchTransitions(recipe.PhaseSWE, recipe.OutcomeDone, false, 1)
			if mech == nil || mech.To != recipe.PhaseClean {
				t.Fatalf("expected mechanical fallback to clean, got %+v", mech)
			}
			if len(judg) != 1 || judg[0].To != recipe.PhaseReview || !judg[0].On.NeedsJudgment() {
				t.Fatalf("expected judgment transition to review, got %+v", judg)
			}
		},
	}

	skipClean200 := overrideFrom(def, recipe.DefaultName, "skip clean under 200 lines")
	skipClean200.Transitions = slices.Insert(skipClean200.Transitions, 0, recipe.Transition{
		From: recipe.PhaseSWE,
		On: recipe.Condition{
			Verdict:      recipe.OutcomeDone,
			JudgmentRule: "if the diff is under 200 lines changed",
		},
		To: recipe.PhaseReview,
	})
	case05 := goldenCase{
		ID: "05_override_skip_clean_under_200_lines",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   skipClean200.Name,
			Source: skipClean200.Source,
		},
		Expected: skipClean200,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			_, judg := res.Recipe.MatchTransitions(recipe.PhaseSWE, recipe.OutcomeDone, false, 1)
			if len(judg) != 1 || !strings.Contains(judg[0].On.JudgmentRule, "200 lines") {
				t.Fatalf("expected 200-line judgment transition, got %+v", judg)
			}
		},
	}

	secAuth := overrideFrom(def, "security-auth", "security review for auth changes")
	secPhase := recipe.Phase{
		Name:          "security-review",
		Agents:        slices.Clone(def.Phases[0].Agents),
		BriefTemplate: "Perform a security audit of authentication, authorization, and permission changes on the PR.",
		ExitContract: recipe.ExitContract{
			AllowedOutcomes: []string{recipe.OutcomeDone, recipe.OutcomeNeedsSWE, recipe.OutcomeBlocked, recipe.OutcomeNeedsInput},
			RequiresPR:      true,
			RequiresReport:  true,
		},
		Gates: []recipe.Gate{
			{Name: "security_check", Required: true},
			{Name: "ci_green", Required: true},
		},
		Mutating: false,
	}
	secAuth.Phases = []recipe.Phase{
		def.Phases[0].Clone(),
		def.Phases[1].Clone(),
		secPhase,
		def.Phases[2].Clone(),
		def.Phases[3].Clone(),
	}
	secAuth.Transitions = []recipe.Transition{
		{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseClean},
		{
			From: recipe.PhaseClean,
			On: recipe.Condition{
				Verdict:      recipe.OutcomeDone,
				JudgmentRule: "if the change touches authentication, authorization, credentials, or permissions",
			},
			To: "security-review",
		},
		{From: recipe.PhaseClean, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
		{From: "security-review", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
		{From: "security-review", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 2}, To: recipe.PhaseSWE, AdvanceRound: true},
		{From: "security-review", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}, To: recipe.TargetEscalate},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 2}, To: recipe.PhaseSWE, AdvanceRound: true},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}, To: recipe.TargetEscalate},
		{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
		{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
	}
	case06 := goldenCase{
		ID: "06_override_security_review_for_auth",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   secAuth.Name,
			Source: secAuth.Source,
		},
		Expected: secAuth,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			p, ok := res.Recipe.Phase("security-review")
			if !ok || p.Mutating {
				t.Fatalf("expected non-mutating security-review phase, got %+v (ok=%v)", p, ok)
			}
			_, judg := res.Recipe.MatchTransitions(recipe.PhaseClean, recipe.OutcomeDone, false, 1)
			if len(judg) != 1 || judg[0].To != "security-review" {
				t.Fatalf("expected judgment transition to security-review, got %+v", judg)
			}
		},
	}

	selfMerge := overrideFrom(def, "self-merge", "lanes merge their own PR once CI is green")
	selfMerge.Phases[3].BriefTemplate = "Merge the lane's own PR using own_merge.sh once review passes and CI is green on HEAD."
	selfMerge.MergePolicy = recipe.MergePolicy{
		Mode:              recipe.MergeModeSelf,
		Script:            "own_merge.sh",
		TargetBranch:      "main",
		RequireReviewPass: true,
		RequireCIGreen:    true,
	}
	case07 := goldenCase{
		ID: "07_override_lanes_merge_own_pr",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   selfMerge.Name,
			Source: selfMerge.Source,
		},
		Expected: selfMerge,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.MergePolicy.Mode != recipe.MergeModeSelf || res.Recipe.MergePolicy.Script != "own_merge.sh" {
				t.Fatalf("MergePolicy = %+v, want self/own_merge.sh", res.Recipe.MergePolicy)
			}
		},
	}

	analysisOnly := recipe.Recipe{
		Name:    "analysis",
		Version: 1,
		Source:  "Analysis-only lane: Opus investigates the root cause and writes a structured findings report; no code changes, no PR, and no merge.",
		Phases: []recipe.Phase{
			{
				Name:          "analyze",
				Agents:        slices.Clone(def.Phases[0].Agents),
				BriefTemplate: "Investigate the root cause, gather evidence from the codebase, and file a structured findings report without modifying code or opening a PR.",
				ExitContract: recipe.ExitContract{
					AllowedOutcomes: []string{recipe.OutcomeDone, recipe.OutcomeBlocked, recipe.OutcomeNeedsInput},
					RequiresPR:      false,
					RequiresReport:  true,
				},
				Mutating: false,
			},
		},
		Transitions: []recipe.Transition{
			{From: "analyze", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.TargetComplete},
		},
		Limits: recipe.Limits{
			MaxRounds:          1,
			MaxPhaseAttempts:   2,
			PhaseTimeout:       30 * time.Minute,
			MaxBriefBytes:      16 * 1024,
			MaxConcurrentLanes: 4,
		},
		MergePolicy: recipe.MergePolicy{
			Mode:              recipe.MergeModeNone,
			RequireReviewPass: false,
			RequireCIGreen:    false,
		},
	}
	case08 := goldenCase{
		ID: "08_analysis_only",
		Request: recipe.CompileRequest{
			Name:   "analysis",
			Source: analysisOnly.Source,
		},
		Expected: analysisOnly,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.MergePolicy.Mode != recipe.MergeModeNone {
				t.Fatalf("MergePolicy.Mode = %q, want none", res.Recipe.MergePolicy.Mode)
			}
			if res.Recipe.Phases[0].Mutating || res.Recipe.Phases[0].ExitContract.RequiresPR {
				t.Fatalf("expected non-mutating phase without PR, got %+v", res.Recipe.Phases[0])
			}
		},
	}

	debate := recipe.Recipe{
		Name:    "debate",
		Version: 1,
		Source:  "Debate workflow: propose a design with Claude Opus, critique it with Codex GPT-5.4 looking for failure modes, and synthesize the final recommendation with Claude Opus. Read-only, no PR or merge.",
		Phases: []recipe.Phase{
			{
				Name: "propose",
				Agents: []recipe.AgentSpec{
					{Harness: "claude", Model: "opus", Effort: "high"},
				},
				BriefTemplate: "Propose a concrete architecture and design with trade-offs and file a phase report.",
				ExitContract: recipe.ExitContract{
					AllowedOutcomes: []string{recipe.OutcomeDone, recipe.OutcomeBlocked, recipe.OutcomeNeedsInput},
					RequiresPR:      false,
					RequiresReport:  true,
				},
				Mutating: false,
			},
			{
				Name: "critique",
				Agents: []recipe.AgentSpec{
					{Harness: "codex", Model: "gpt-5.4", Effort: "high"},
				},
				BriefTemplate: "Critique the proposed design, identify failure modes, edge cases, and missing invariants, and file a phase report.",
				ExitContract: recipe.ExitContract{
					AllowedOutcomes: []string{recipe.OutcomeDone, recipe.OutcomeBlocked, recipe.OutcomeNeedsInput},
					RequiresPR:      false,
					RequiresReport:  true,
				},
				Mutating: false,
			},
			{
				Name: "synthesize",
				Agents: []recipe.AgentSpec{
					{Harness: "claude", Model: "opus", Effort: "high"},
				},
				BriefTemplate: "Synthesize the initial proposal and critique into a final recommendation report.",
				ExitContract: recipe.ExitContract{
					AllowedOutcomes: []string{recipe.OutcomeDone, recipe.OutcomeBlocked, recipe.OutcomeNeedsInput},
					RequiresPR:      false,
					RequiresReport:  true,
				},
				Mutating: false,
			},
		},
		Transitions: []recipe.Transition{
			{From: "propose", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: "critique"},
			{From: "critique", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: "synthesize"},
			{From: "synthesize", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.TargetComplete},
		},
		Limits: recipe.Limits{
			MaxRounds:          1,
			MaxPhaseAttempts:   2,
			PhaseTimeout:       30 * time.Minute,
			MaxBriefBytes:      16 * 1024,
			MaxConcurrentLanes: 4,
		},
		MergePolicy: recipe.MergePolicy{
			Mode:              recipe.MergeModeNone,
			RequireReviewPass: false,
			RequireCIGreen:    false,
		},
	}
	case09 := goldenCase{
		ID: "09_debate",
		Request: recipe.CompileRequest{
			Name:   "debate",
			Source: debate.Source,
		},
		Expected: debate,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if !slices.Equal(res.Recipe.PhaseNames(), []string{"propose", "critique", "synthesize"}) {
				t.Fatalf("PhaseNames = %v", res.Recipe.PhaseNames())
			}
		},
	}

	rehearsal := recipe.Recipe{
		Name:    "rehearsal",
		Version: 1,
		Source:  "Proof or rehearsal lane: SWE builds a reproduction or proof-of-concept and runs scoped_tests and proof_check gates, then a verify phase confirms the proof holds. Leave the PR open for manual inspection, do not auto-merge.",
		Phases: []recipe.Phase{
			{
				Name:          "prove",
				Agents:        slices.Clone(def.Phases[0].Agents),
				BriefTemplate: "Build the reproduction or proof-of-concept, run scoped_tests and proof_check, and open a PR with the proof.",
				ExitContract: recipe.ExitContract{
					AllowedOutcomes: []string{recipe.OutcomeDone, recipe.OutcomeBlocked, recipe.OutcomeNeedsInput},
					RequiresPR:      true,
					RequiresReport:  true,
				},
				Gates: []recipe.Gate{
					{Name: "scoped_tests", Required: true},
					{Name: "proof_check", Required: true},
					{Name: "pr_open", Required: true},
				},
				Mutating: true,
			},
			{
				Name:          "verify",
				Agents:        slices.Clone(def.Phases[2].Agents),
				BriefTemplate: "Verify that the reproduction or proof-of-concept holds and satisfies the brief.",
				ExitContract: recipe.ExitContract{
					AllowedOutcomes: []string{recipe.OutcomeDone, recipe.OutcomeNeedsSWE, recipe.OutcomeBlocked, recipe.OutcomeNeedsInput},
					RequiresPR:      true,
					RequiresReport:  true,
				},
				Gates: []recipe.Gate{
					{Name: "proof_verified", Required: true},
				},
				Mutating: false,
			},
		},
		Transitions: []recipe.Transition{
			{From: "prove", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: "verify"},
			{From: "verify", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.TargetComplete},
			{From: "verify", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 1}, To: "prove", AdvanceRound: true},
			{From: "verify", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 2}, To: recipe.TargetEscalate},
		},
		Limits: recipe.Limits{
			MaxRounds:          2,
			MaxPhaseAttempts:   2,
			PhaseTimeout:       45 * time.Minute,
			MaxBriefBytes:      16 * 1024,
			MaxConcurrentLanes: 4,
		},
		MergePolicy: recipe.MergePolicy{
			Mode:              recipe.MergeModeManual,
			TargetBranch:      "main",
			RequireReviewPass: true,
			RequireCIGreen:    false,
		},
	}
	case10 := goldenCase{
		ID: "10_proof_rehearsal",
		Request: recipe.CompileRequest{
			Name:   "rehearsal",
			Source: rehearsal.Source,
		},
		Expected: rehearsal,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.MergePolicy.Mode != recipe.MergeModeManual {
				t.Fatalf("MergePolicy.Mode = %q, want manual", res.Recipe.MergePolicy.Mode)
			}
		},
	}

	integrateSelf := def.Phases[3].Clone()
	integrateSelf.BriefTemplate = "Merge the lane's own PR once review passes and CI is green on HEAD."
	compositeOwner := recipe.Recipe{
		Name:    "owner-custom",
		Version: 1,
		Source:  "Each task: Opus writes it, Sonnet reviews; auth or migration changes also get a security review; lanes merge their own PR once CI is green; stop after 2 review rounds and ask me.",
		Phases: []recipe.Phase{
			def.Phases[0].Clone(),
			secPhase.Clone(),
			def.Phases[2].Clone(),
			integrateSelf,
		},
		Transitions: []recipe.Transition{
			{
				From: recipe.PhaseSWE,
				On: recipe.Condition{
					Verdict:      recipe.OutcomeDone,
					JudgmentRule: "if the change touches auth or database migrations",
				},
				To: "security-review",
			},
			{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
			{From: "security-review", On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
			{From: "security-review", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 1}, To: recipe.PhaseSWE, AdvanceRound: true},
			{From: "security-review", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 2}, To: recipe.TargetEscalate},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 1}, To: recipe.PhaseSWE, AdvanceRound: true},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 2}, To: recipe.TargetEscalate},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
		},
		Limits: recipe.Limits{
			MaxRounds:          2,
			MaxPhaseAttempts:   2,
			PhaseTimeout:       45 * time.Minute,
			MaxBriefBytes:      16 * 1024,
			MaxConcurrentLanes: 4,
		},
		MergePolicy: recipe.MergePolicy{
			Mode:              recipe.MergeModeSelf,
			Script:            "own_merge.sh",
			TargetBranch:      "main",
			RequireReviewPass: true,
			RequireCIGreen:    true,
		},
	}
	case11 := goldenCase{
		ID: "11_composite_opus_sonnet_auth_self_merge_2rounds",
		Request: recipe.CompileRequest{
			Name:   "owner-custom",
			Source: compositeOwner.Source,
		},
		Expected: compositeOwner,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.Limits.MaxRounds != 2 {
				t.Fatalf("MaxRounds = %d, want 2", res.Recipe.Limits.MaxRounds)
			}
			if res.Recipe.MergePolicy.Mode != recipe.MergeModeSelf {
				t.Fatalf("MergePolicy.Mode = %q, want self", res.Recipe.MergePolicy.Mode)
			}
			trEsc, _ := res.Recipe.MatchTransitions(recipe.PhaseReview, recipe.OutcomeNeedsSWE, false, 2)
			if trEsc == nil || trEsc.To != recipe.TargetEscalate {
				t.Fatalf("expected escalate on round 2 needs-swe, got %+v", trEsc)
			}
		},
	}

	hotfix := recipe.Recipe{
		Name:    "hotfix",
		Version: 1,
		Source:  "Incident hotfix: Opus implements the minimal fix and passes lint and scoped_tests, Sonnet reviews requiring green CI, and controller merges immediately to main. Skip clean, max 2 rounds, 20m phase timeout.",
		Phases: []recipe.Phase{
			def.Phases[0].Clone(),
			def.Phases[2].Clone(),
			def.Phases[3].Clone(),
		},
		Transitions: []recipe.Transition{
			{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 1}, To: recipe.PhaseSWE, AdvanceRound: true},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 2}, To: recipe.TargetEscalate},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
		},
		Limits: recipe.Limits{
			MaxRounds:          2,
			MaxPhaseAttempts:   2,
			PhaseTimeout:       20 * time.Minute,
			MaxBriefBytes:      16 * 1024,
			MaxConcurrentLanes: 4,
		},
		MergePolicy: def.MergePolicy,
	}
	case12 := goldenCase{
		ID: "12_incident_hotfix",
		Request: recipe.CompileRequest{
			Name:   "hotfix",
			Source: hotfix.Source,
		},
		Expected: hotfix,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.Limits.PhaseTimeout != 20*time.Minute {
				t.Fatalf("PhaseTimeout = %s, want 20m", res.Recipe.Limits.PhaseTimeout)
			}
		},
	}

	testDoctorSWE := def.Phases[0].Clone()
	testDoctorSWE.BriefTemplate = "Reproduce and fix the flaky test, verify with lint, scoped_tests, and flake_repeat gates, and open a PR."
	testDoctorSWE.Gates = []recipe.Gate{
		{Name: "lint", Command: "scripts/lint.sh", Required: true},
		{Name: "scoped_tests", Required: true},
		{Name: "flake_repeat", Required: true},
		{Name: "pr_open", Required: true},
	}
	testDoctor := recipe.Recipe{
		Name:    "test-doctor",
		Version: 1,
		Source:  "Flaky test fix lane: SWE reproduces and fixes the flake with lint, scoped_tests, and flake_repeat gates; review verifies the fix and green CI; controller merges to main. Up to 2 rounds.",
		Phases: []recipe.Phase{
			testDoctorSWE,
			def.Phases[2].Clone(),
			def.Phases[3].Clone(),
		},
		Transitions: []recipe.Transition{
			{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 1}, To: recipe.PhaseSWE, AdvanceRound: true},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 2}, To: recipe.TargetEscalate},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
		},
		Limits: recipe.Limits{
			MaxRounds:          2,
			MaxPhaseAttempts:   2,
			PhaseTimeout:       45 * time.Minute,
			MaxBriefBytes:      16 * 1024,
			MaxConcurrentLanes: 4,
		},
		MergePolicy: def.MergePolicy,
	}
	case13 := goldenCase{
		ID: "13_test_doctor_flake_fix",
		Request: recipe.CompileRequest{
			Name:   "test-doctor",
			Source: testDoctor.Source,
		},
		Expected: testDoctor,
	}

	uiSWE := def.Phases[0].Clone()
	uiSWE.BriefTemplate = "Implement the UI fix, capture before/after screenshots, pass lint and scoped_tests, and open a PR."
	uiSWE.Gates = []recipe.Gate{
		{Name: "lint", Command: "scripts/lint.sh", Required: true},
		{Name: "scoped_tests", Required: true},
		{Name: "screenshots", Required: true},
		{Name: "pr_open", Required: true},
	}
	visualReview := def.Phases[2].Clone()
	visualReview.Name = "visual-review"
	visualReview.BriefTemplate = "Score before/after screenshots against the UI rubric and verify required CI checks are green on HEAD."
	uiSweep := recipe.Recipe{
		Name:    "ui-sweep",
		Version: 1,
		Source:  "UI sweep fix lane: SWE implements the UI fix and captures before/after screenshots (gates: lint, scoped_tests, screenshots); clean polishes the diff; visual-review checks screenshot rubric and green CI; integrate merges to main.",
		Phases: []recipe.Phase{
			uiSWE,
			def.Phases[1].Clone(),
			visualReview,
			def.Phases[3].Clone(),
		},
		Transitions: []recipe.Transition{
			{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseClean},
			{From: recipe.PhaseClean, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: "visual-review"},
			{From: "visual-review", On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
			{From: "visual-review", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 2}, To: recipe.PhaseSWE, AdvanceRound: true},
			{From: "visual-review", On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}, To: recipe.TargetEscalate},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
		},
		Limits:      def.Limits,
		MergePolicy: def.MergePolicy,
	}
	case14 := goldenCase{
		ID: "14_ui_sweep_visual",
		Request: recipe.CompileRequest{
			Name:   "ui-sweep",
			Source: uiSweep.Source,
		},
		Expected: uiSweep,
	}

	codexSWE := overrideFrom(def, "codex-swe", "this lane: use codex gpt-5.4 high effort as primary agent for swe with claude opus high effort as fallback, and allow up to 3 phase attempts")
	codexSWE.Phases[0].Agents = []recipe.AgentSpec{
		{Harness: "codex", Model: "gpt-5.4", Effort: "high"},
		{Harness: "claude", Model: "opus", Effort: "high"},
	}
	codexSWE.Limits.MaxPhaseAttempts = 3
	case15 := goldenCase{
		ID: "15_override_codex_primary_swe",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   codexSWE.Name,
			Source: codexSWE.Source,
		},
		Expected: codexSWE,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.Phases[0].Agents[0].Harness != "codex" {
				t.Fatalf("first SWE agent = %+v, want codex", res.Recipe.Phases[0].Agents[0])
			}
			if res.Recipe.Limits.MaxPhaseAttempts != 3 {
				t.Fatalf("MaxPhaseAttempts = %d, want 3", res.Recipe.Limits.MaxPhaseAttempts)
			}
		},
	}

	releaseManual := overrideFrom(def, "release-manual", "target the release/v2 branch and leave the PR open for manual human merge after review and CI pass; remove the integrate phase")
	releaseManual.Phases = []recipe.Phase{
		def.Phases[0].Clone(),
		def.Phases[1].Clone(),
		def.Phases[2].Clone(),
	}
	releaseManual.Transitions = []recipe.Transition{
		{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseClean},
		{From: recipe.PhaseClean, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 2}, To: recipe.PhaseSWE, AdvanceRound: true},
		{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}, To: recipe.TargetEscalate},
	}
	releaseManual.MergePolicy = recipe.MergePolicy{
		Mode:              recipe.MergeModeManual,
		TargetBranch:      "release/v2",
		RequireReviewPass: true,
		RequireCIGreen:    true,
	}
	case16 := goldenCase{
		ID: "16_override_manual_merge_release_branch",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   releaseManual.Name,
			Source: releaseManual.Source,
		},
		Expected: releaseManual,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.MergePolicy.Mode != recipe.MergeModeManual || res.Recipe.MergePolicy.TargetBranch != "release/v2" {
				t.Fatalf("MergePolicy = %+v", res.Recipe.MergePolicy)
			}
		},
	}

	riskGated := overrideFrom(def, "risk-gated", "if review passes and CI is green but the change looks risky or modifies public APIs, escalate to the dispatcher instead of auto-integrating")
	riskGated.Transitions = slices.Insert(riskGated.Transitions, 2, recipe.Transition{
		From: recipe.PhaseReview,
		On: recipe.Condition{
			Verdict:      recipe.OutcomeDone,
			CIGreen:      true,
			JudgmentRule: "if the change looks risky or modifies public APIs",
		},
		To: recipe.TargetEscalate,
	})
	case17 := goldenCase{
		ID: "17_override_risky_change_judgment_escalation",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   riskGated.Name,
			Source: riskGated.Source,
		},
		Expected: riskGated,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			mech, judg := res.Recipe.MatchTransitions(recipe.PhaseReview, recipe.OutcomeDone, true, 1)
			if mech == nil || mech.To != recipe.PhaseIntegrate {
				t.Fatalf("expected mechanical transition to integrate, got %+v", mech)
			}
			if len(judg) != 1 || judg[0].To != recipe.TargetEscalate {
				t.Fatalf("expected judgment transition to escalate, got %+v", judg)
			}
		},
	}

	benchSWE := def.Phases[0].Clone()
	benchSWE.BriefTemplate = "Implement the optimization, run before/after benchmarks, pass lint, scoped_tests, and bench_compare gates, and open a PR."
	benchSWE.Gates = []recipe.Gate{
		{Name: "lint", Command: "scripts/lint.sh", Required: true},
		{Name: "scoped_tests", Required: true},
		{Name: "bench_compare", Required: true},
		{Name: "pr_open", Required: true},
	}
	benchmark := recipe.Recipe{
		Name:    "benchmark",
		Version: 1,
		Source:  "Performance optimization lane: SWE implements the optimization and runs lint, scoped_tests, and bench_compare gates; review checks benchmark numbers and green CI; integrate merges on pass. If benchmark improvement is under 5%, escalate after review.",
		Phases: []recipe.Phase{
			benchSWE,
			def.Phases[2].Clone(),
			def.Phases[3].Clone(),
		},
		Transitions: []recipe.Transition{
			{From: recipe.PhaseSWE, On: recipe.Condition{Verdict: recipe.OutcomeDone}, To: recipe.PhaseReview},
			{
				From: recipe.PhaseReview,
				On: recipe.Condition{
					Verdict:      recipe.OutcomeDone,
					CIGreen:      true,
					JudgmentRule: "if the benchmark improvement is under 5%",
				},
				To: recipe.TargetEscalate,
			},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.PhaseIntegrate},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MaxRound: 2}, To: recipe.PhaseSWE, AdvanceRound: true},
			{From: recipe.PhaseReview, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE, MinRound: 3}, To: recipe.TargetEscalate},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeDone, CIGreen: true}, To: recipe.TargetComplete},
			{From: recipe.PhaseIntegrate, On: recipe.Condition{Verdict: recipe.OutcomeNeedsSWE}, To: recipe.PhaseSWE, AdvanceRound: true},
		},
		Limits:      def.Limits,
		MergePolicy: def.MergePolicy,
	}
	case18 := goldenCase{
		ID: "18_benchmark_optimization",
		Request: recipe.CompileRequest{
			Name:   "benchmark",
			Source: benchmark.Source,
		},
		Expected: benchmark,
	}

	unmappedSlack := def.Clone()
	unmappedSlack.Name = "default-with-extras"
	unmappedSlack.Version = 1
	unmappedSlack.Source = "Run the standard swe -> clean -> review -> integrate workflow, post a celebration GIF to #eng-general on Slack when merged, and charge token usage to billing code CC-900."
	unmappedSlack.Unmapped = []string{
		"post a celebration GIF to #eng-general on Slack when merged",
		"charge token usage to billing code CC-900",
	}
	case19 := goldenCase{
		ID: "19_unmapped_slack_and_billing",
		Request: recipe.CompileRequest{
			Name:   "default-with-extras",
			Source: unmappedSlack.Source,
		},
		Expected: unmappedSlack,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			wantUnmapped := []string{
				"post a celebration GIF to #eng-general on Slack when merged",
				"charge token usage to billing code CC-900",
			}
			if !slices.Equal(res.Unmapped, wantUnmapped) {
				t.Fatalf("Unmapped = %v, want %v", res.Unmapped, wantUnmapped)
			}
			for _, phrase := range wantUnmapped {
				if !strings.Contains(res.EchoTable, phrase) {
					t.Errorf("EchoTable missing unmapped phrase %q:\n%s", phrase, res.EchoTable)
				}
			}
		},
	}

	unmappedEspresso := overrideFrom(skipClean, "fast-espresso", "this lane: skip clean, set phase timeout to 30m, and brew a fresh pot of espresso when CI turns green")
	unmappedEspresso.BaseRecipe = recipe.DefaultName
	unmappedEspresso.Limits.PhaseTimeout = 30 * time.Minute
	unmappedEspresso.Unmapped = []string{
		"brew a fresh pot of espresso when CI turns green",
	}
	case20 := goldenCase{
		ID: "20_override_unmapped_espresso_and_timeout",
		Request: recipe.CompileRequest{
			Base:   &def,
			Name:   unmappedEspresso.Name,
			Source: unmappedEspresso.Source,
		},
		Expected: unmappedEspresso,
		Verify: func(t *testing.T, res *recipe.CompileResult) {
			if res.Recipe.Limits.PhaseTimeout != 30*time.Minute {
				t.Fatalf("PhaseTimeout = %s, want 30m", res.Recipe.Limits.PhaseTimeout)
			}
			wantUnmapped := []string{"brew a fresh pot of espresso when CI turns green"}
			if !slices.Equal(res.Unmapped, wantUnmapped) {
				t.Fatalf("Unmapped = %v, want %v", res.Unmapped, wantUnmapped)
			}
			if !strings.Contains(res.EchoTable, "brew a fresh pot of espresso when CI turns green") {
				t.Fatalf("EchoTable missing unmapped phrase:\n%s", res.EchoTable)
			}
		},
	}

	return []goldenCase{
		case01, case02, case03, case04, case05,
		case06, case07, case08, case09, case10,
		case11, case12, case13, case14, case15,
		case16, case17, case18, case19, case20,
	}
}

func buildExpectedExchanges(t *testing.T, cases []goldenCase) []model.RecordedExchange {
	t.Helper()
	compiler := recipe.NewCompiler(model.NewFakeClient())
	exchanges := make([]model.RecordedExchange, len(cases))
	for i, tc := range cases {
		req := compiler.BuildCompileModelRequest(tc.Request)
		toolInput, err := recipe.RecipeToToolInput(tc.Expected)
		if err != nil {
			t.Fatalf("case %s RecipeToToolInput: %v", tc.ID, err)
		}
		resp := model.Response{
			ID:         fmt.Sprintf("msg_golden_%02d", i+1),
			Model:      recipe.DefaultCompilerModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewTextBlock(fmt.Sprintf("Compiled recipe %q for case %s.", tc.Expected.Name, tc.ID)),
				model.NewToolUseBlock(fmt.Sprintf("toolu_golden_%02d", i+1), recipe.SubmitRecipeToolName, toolInput),
			),
			Usage: model.Usage{
				InputTokens:  int64(1100 + i*25),
				OutputTokens: int64(420 + i*10),
			},
			Turns: 1,
		}
		resp.Usage.CostUSD = model.CalculateCost(resp.Model, resp.Usage, nil)
		exchanges[i] = model.RecordedExchange{
			Request:  req,
			Response: resp,
		}
	}
	return exchanges
}

func TestGoldenRecipeCompilation(t *testing.T) {
	t.Parallel()

	cases := goldenCases()
	if len(cases) != 20 {
		t.Fatalf("len(goldenCases) = %d, want 20", len(cases))
	}

	if os.Getenv("UPDATE_FIXTURES") == "1" {
		exchanges := buildExpectedExchanges(t, cases)
		outPath := goldenReplayFixturePath
		if ws := os.Getenv("BUILD_WORKSPACE_DIRECTORY"); ws != "" {
			outPath = filepath.Join(ws, "orch/recipe", goldenReplayFixturePath)
		}
		if err := model.SaveRecording(outPath, exchanges); err != nil {
			t.Fatalf("SaveRecording(%s): %v", outPath, err)
		}
	}

	exchanges, err := model.LoadRecording(goldenReplayFixturePath)
	if err != nil {
		t.Fatalf("LoadRecording(%s): %v", goldenReplayFixturePath, err)
	}
	if len(exchanges) != len(cases) {
		t.Fatalf("len(exchanges) = %d, want %d", len(exchanges), len(cases))
	}

	for i, tc := range cases {
		ex := exchanges[i]
		t.Run(tc.ID, func(t *testing.T) {
			t.Parallel()

			fake := model.NewFakeClient()
			fake.EnqueueExchange(ex)
			fake.SetStrictMatch(true)

			compiler := recipe.NewCompiler(fake)
			res, err := compiler.Compile(t.Context(), tc.Request)
			if err != nil {
				t.Fatalf("Compile(%s) error: %v", tc.ID, err)
			}

			if err := res.Recipe.Validate(); err != nil {
				t.Fatalf("compiled recipe failed Validate(): %v", err)
			}
			if !reflect.DeepEqual(res.Recipe, tc.Expected) {
				gotJSON, _ := json.MarshalIndent(res.Recipe, "", "  ")
				wantJSON, _ := json.MarshalIndent(tc.Expected, "", "  ")
				t.Fatalf("case %s recipe mismatch:\ngot:\n%s\nwant:\n%s", tc.ID, gotJSON, wantJSON)
			}
			if res.EchoTable != tc.Expected.FormatTable() {
				t.Fatalf("case %s EchoTable mismatch:\ngot:\n%s\nwant:\n%s", tc.ID, res.EchoTable, tc.Expected.FormatTable())
			}
			if !slices.Equal(res.Unmapped, tc.Expected.Unmapped) {
				t.Fatalf("case %s Unmapped = %v, want %v", tc.ID, res.Unmapped, tc.Expected.Unmapped)
			}
			if res.Usage.InputTokens <= 0 || res.Usage.OutputTokens <= 0 || res.Usage.CostUSD <= 0 {
				t.Fatalf("case %s unexpected Usage: %+v", tc.ID, res.Usage)
			}
			if tc.Verify != nil {
				tc.Verify(t, res)
			}
		})
	}

	t.Run("SequentialReplayBatch", func(t *testing.T) {
		t.Parallel()

		replayClient, err := model.LoadFakeClient(goldenReplayFixturePath)
		if err != nil {
			t.Fatalf("LoadFakeClient: %v", err)
		}
		replayClient.SetStrictMatch(true)

		for _, tc := range cases {
			var res *recipe.CompileResult
			if tc.Request.Base != nil {
				res, err = recipe.CompileOverride(t.Context(), replayClient, *tc.Request.Base, tc.Request.Name, tc.Request.Source)
			} else {
				res, err = recipe.Compile(t.Context(), replayClient, tc.Request)
			}
			if err != nil {
				t.Fatalf("sequential replay %s error: %v", tc.ID, err)
			}
			if !reflect.DeepEqual(res.Recipe, tc.Expected) {
				t.Fatalf("sequential replay %s mismatch", tc.ID)
			}
		}
		if len(replayClient.Requests) != len(cases) {
			t.Fatalf("len(replayClient.Requests) = %d, want %d", len(replayClient.Requests), len(cases))
		}
	})
}

func TestCompilerJSONTextFallback(t *testing.T) {
	t.Parallel()

	want := recipe.Default()
	rawJSON, err := recipe.RecipeToToolInput(want)
	if err != nil {
		t.Fatalf("RecipeToToolInput: %v", err)
	}

	fenced := "Here is the compiled recipe:\n```json\n" + string(rawJSON) + "\n```"
	fake := model.NewFakeClient(model.Response{
		ID:         "msg_text_fallback",
		Model:      recipe.DefaultCompilerModel,
		StopReason: model.StopReasonEndTurn,
		Message:    model.NewAssistantMessage(model.NewTextBlock(fenced)),
		Usage:      model.Usage{InputTokens: 800, OutputTokens: 350},
	})

	res, err := recipe.Compile(t.Context(), fake, recipe.CompileRequest{
		Name:   recipe.DefaultName,
		Source: recipe.DefaultSource,
	})
	if err != nil {
		t.Fatalf("Compile from fenced JSON text error: %v", err)
	}
	if !reflect.DeepEqual(res.Recipe, want) {
		t.Fatalf("recipe mismatch from fenced JSON:\ngot:  %+v\nwant: %+v", res.Recipe, want)
	}
}

func TestCompilerRejectsInvalidCompiledRecipes(t *testing.T) {
	t.Parallel()

	t.Run("EmptySource", func(t *testing.T) {
		t.Parallel()
		compiler := recipe.NewCompiler(model.NewFakeClient())
		_, err := compiler.Compile(t.Context(), recipe.CompileRequest{Name: "test", Source: "   "})
		if !errors.Is(err, recipe.ErrEmptyRecipeSource) {
			t.Fatalf("err = %v, want ErrEmptyRecipeSource", err)
		}
	})

	t.Run("NilClient", func(t *testing.T) {
		t.Parallel()
		compiler := recipe.NewCompiler(nil)
		_, err := compiler.CompileText(t.Context(), "test", "some recipe")
		if err == nil {
			t.Fatal("expected error for nil ModelClient")
		}
	})

	t.Run("EmptyModelResponse", func(t *testing.T) {
		t.Parallel()
		fake := model.NewFakeClient(model.Response{
			ID:         "msg_empty",
			Model:      recipe.DefaultCompilerModel,
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(),
		})
		_, err := recipe.NewCompiler(fake).CompileText(t.Context(), "test", "some recipe")
		if err == nil || !strings.Contains(err.Error(), "neither submit_recipe tool_use nor JSON text") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("MalformedToolInputJSON", func(t *testing.T) {
		t.Parallel()
		fake := model.NewFakeClient(model.Response{
			ID:         "msg_bad_json",
			Model:      recipe.DefaultCompilerModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("call_bad", recipe.SubmitRecipeToolName, json.RawMessage(`{"name": 123`)),
			),
		})
		_, err := recipe.NewCompiler(fake).CompileText(t.Context(), "test", "some recipe")
		if err == nil {
			t.Fatal("expected JSON decode error")
		}
	})

	t.Run("InvalidDurationString", func(t *testing.T) {
		t.Parallel()
		fake := model.NewFakeClient(model.Response{
			ID:         "msg_bad_dur",
			Model:      recipe.DefaultCompilerModel,
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("call_dur", recipe.SubmitRecipeToolName, json.RawMessage(`{
					"name": "bad-dur",
					"phases": [{"name": "swe", "agents": [{"harness": "claude", "model": "opus"}], "exit_contract": {"allowed_outcomes": ["done"], "requires_report": true}, "mutating": true}],
					"transitions": [{"from": "swe", "to": "complete", "on": {"verdict": "done"}}],
					"merge_policy": {"mode": "none", "require_review_pass": false, "require_ci_green": false},
					"limits": {"max_rounds": 1, "max_phase_attempts": 1, "phase_timeout": "not-a-duration"}
				}`)),
			),
		})
		_, err := recipe.NewCompiler(fake).CompileText(t.Context(), "bad-dur", "some recipe")
		if err == nil || !strings.Contains(err.Error(), "invalid phase_timeout duration") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	invalidMutations := []struct {
		mutate  func(r *recipe.Recipe)
		name    string
		reqName string
		wantErr string
	}{
		{
			name:    "EmptyRecipeName",
			reqName: "",
			mutate: func(r *recipe.Recipe) {
				r.Name = ""
			},
			wantErr: "recipe name is required",
		},
		{
			name:    "NoPhases",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases = nil
			},
			wantErr: "at least one phase is required",
		},
		{
			name:    "EmptyPhaseName",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Name = "  "
			},
			wantErr: "phase name must not be empty",
		},
		{
			name:    "DuplicatePhaseName",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[1].Name = r.Phases[0].Name
			},
			wantErr: "duplicate phase name",
		},
		{
			name:    "ReservedPhaseNameComplete",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Name = recipe.TargetComplete
			},
			wantErr: "reserved for transition targets",
		},
		{
			name:    "ReservedPhaseNameEscalate",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Name = recipe.TargetEscalate
			},
			wantErr: "reserved for transition targets",
		},
		{
			name:    "PhaseNameTrailingDigit",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Name = "swe2"
			},
			wantErr: "must not end in a digit",
		},
		{
			name:    "PhaseWithNoAgents",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Agents = nil
			},
			wantErr: "at least one agent spec is required",
		},
		{
			name:    "AgentMissingHarness",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Agents[0].Harness = ""
			},
			wantErr: "requires non-empty harness and model",
		},
		{
			name:    "AgentMissingModel",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Agents[0].Model = ""
			},
			wantErr: "requires non-empty harness and model",
		},
		{
			name:    "GateEmptyName",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].Gates[0].Name = ""
			},
			wantErr: "gate name must not be empty",
		},
		{
			name:    "AllowedOutcomeEmpty",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Phases[0].ExitContract.AllowedOutcomes = []string{"done", ""}
			},
			wantErr: "allowed outcome must not be empty",
		},
		{
			name:    "NoTransitions",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Transitions = nil
			},
			wantErr: "at least one transition is required",
		},
		{
			name:    "UnknownTransitionSource",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Transitions[0].From = "nonexistent"
			},
			wantErr: "unknown source phase",
		},
		{
			name:    "UnknownTransitionTarget",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Transitions[0].To = "nonexistent"
			},
			wantErr: "unknown target phase",
		},
		{
			name:    "NegativeRoundBounds",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Transitions[0].On.MinRound = -1
			},
			wantErr: "round bounds must not be negative",
		},
		{
			name:    "MinRoundExceedsMaxRound",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Transitions[0].On.MinRound = 4
				r.Transitions[0].On.MaxRound = 2
			},
			wantErr: "min_round (4) exceeds max_round (2)",
		},
		{
			name:    "NoTransitionToComplete",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				for i := range r.Transitions {
					if r.Transitions[i].To == recipe.TargetComplete {
						r.Transitions[i].To = recipe.TargetEscalate
					}
				}
			},
			wantErr: `at least one transition to "complete" is required`,
		},
		{
			name:    "DeadEndPhaseWithNoOutgoingTransition",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Transitions = slices.DeleteFunc(r.Transitions, func(tr recipe.Transition) bool {
					return tr.From == recipe.PhaseClean
				})
			},
			wantErr: `phase "clean": no outgoing transitions`,
		},
		{
			name:    "UnreachablePhase",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Transitions[0].To = recipe.PhaseReview
			},
			wantErr: `phase "clean": unreachable from first phase "swe"`,
		},
		{
			name:    "InvalidMergePolicyMode",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.MergePolicy.Mode = "invalid-mode"
			},
			wantErr: "invalid merge policy mode",
		},
		{
			name:    "NegativeLimits",
			reqName: "bad",
			mutate: func(r *recipe.Recipe) {
				r.Limits.MaxRounds = -1
			},
			wantErr: "limits must not be negative",
		},
	}

	for _, tc := range invalidMutations {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bad := recipe.Default()
			tc.mutate(&bad)

			toolInput, err := recipe.RecipeToToolInput(bad)
			if err != nil {
				t.Fatalf("RecipeToToolInput: %v", err)
			}

			fake := model.NewFakeClient(model.Response{
				ID:         "msg_invalid_" + tc.name,
				Model:      recipe.DefaultCompilerModel,
				StopReason: model.StopReasonToolUse,
				Message: model.NewAssistantMessage(
					model.NewToolUseBlock("call_invalid", recipe.SubmitRecipeToolName, toolInput),
				),
			})

			compiler := recipe.NewCompiler(fake)
			_, err = compiler.Compile(t.Context(), recipe.CompileRequest{
				Name:   tc.reqName,
				Source: "invalid recipe test case",
			})
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestCompilerOptionsAndBaseNameFallback(t *testing.T) {
	t.Parallel()

	c := recipe.NewCompiler(model.NewFakeClient(), recipe.WithModel("claude-sonnet-4-6"), recipe.WithMaxTokens(1024))
	req := c.BuildCompileModelRequest(recipe.CompileRequest{
		Name:   recipe.DefaultName,
		Source: recipe.DefaultSource,
	})
	if req.Model != "claude-sonnet-4-6" {
		t.Fatalf("Model = %q, want claude-sonnet-4-6", req.Model)
	}
	if req.MaxTokens != 1024 {
		t.Fatalf("MaxTokens = %d, want 1024", req.MaxTokens)
	}

	base := recipe.Default()
	submitted := recipe.Default()
	submitted.Name = ""
	toolInput, err := recipe.RecipeToToolInput(submitted)
	if err != nil {
		t.Fatalf("RecipeToToolInput: %v", err)
	}
	fake := model.NewFakeClient(model.Response{
		ID:         "msg_base_fallback",
		Model:      recipe.DefaultCompilerModel,
		StopReason: model.StopReasonToolUse,
		Message: model.NewAssistantMessage(
			model.NewToolUseBlock("call_base", recipe.SubmitRecipeToolName, toolInput),
		),
	})
	res, err := recipe.NewCompiler(fake).Compile(t.Context(), recipe.CompileRequest{
		Base:   &base,
		Source: "keep default phases",
	})
	if err != nil {
		t.Fatalf("Compile with Base.Name fallback error: %v", err)
	}
	if res.Recipe.Name != recipe.DefaultName || res.Recipe.Version != 2 {
		t.Fatalf("got Name=%q Version=%d, want default v2", res.Recipe.Name, res.Recipe.Version)
	}
}
