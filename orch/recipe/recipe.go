// Package recipe defines versioned lane/phase workflows compiled from natural
// language and provides the shipped default recipe (swe -> clean -> review -> integrate).
package recipe

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefaultName is the canonical name of the shipped default recipe.
const DefaultName = "default"

// Phase names used by the shipped default recipe.
const (
	PhaseSWE       = "swe"
	PhaseClean     = "clean"
	PhaseReview    = "review"
	PhaseIntegrate = "integrate"
)

// Standard phase report outcomes used in exit contracts and transitions.
const (
	OutcomeDone       = "done"
	OutcomeNeedsSWE   = "needs-swe"
	OutcomeBlocked    = "blocked"
	OutcomeNeedsInput = "needs-input"
)

// Special transition targets that terminate or escalate a lane rather than
// entering another named phase.
const (
	TargetComplete = "complete"
	TargetEscalate = "escalate"
)

// MergeMode specifies how a lane's PR is merged once its recipe reaches integration.
type MergeMode string

const (
	// MergeModeController runs the merge gate and merge script from the controller/integrate step.
	MergeModeController MergeMode = "controller"
	// MergeModeSelf allows the lane to merge its own PR once gates pass.
	MergeModeSelf MergeMode = "self"
	// MergeModeManual leaves the PR open for a human to merge.
	MergeModeManual MergeMode = "manual"
	// MergeModeNone applies to non-code/analysis recipes that produce no PR merge.
	MergeModeNone MergeMode = "none"
)

// Valid reports whether m is a recognized MergeMode.
func (m MergeMode) Valid() bool {
	switch m {
	case MergeModeController, MergeModeSelf, MergeModeManual, MergeModeNone:
		return true
	default:
		return false
	}
}

// AgentSpec describes one entry in a phase's agent/model fallback chain.
type AgentSpec struct {
	Harness string `json:"harness"`
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
}

// Gate is a verification check required before or at phase exit.
type Gate struct {
	Name     string `json:"name"`
	Command  string `json:"command,omitempty"`
	Required bool   `json:"required"`
}

// ExitContract defines what a phase must produce when it finishes.
type ExitContract struct {
	AllowedOutcomes []string `json:"allowed_outcomes"`
	RequiresPR      bool     `json:"requires_pr,omitempty"`
	RequiresReport  bool     `json:"requires_report"`
}

// Phase is one step in a recipe's lifecycle.
type Phase struct {
	Name          string       `json:"name"`
	BriefTemplate string       `json:"brief_template,omitempty"`
	Agents        []AgentSpec  `json:"agents"`
	Gates         []Gate       `json:"gates,omitempty"`
	ExitContract  ExitContract `json:"exit_contract"`
	Mutating      bool         `json:"mutating"`
}

// Clone returns a deep copy of p.
func (p Phase) Clone() Phase {
	out := p
	out.Agents = slices.Clone(p.Agents)
	out.Gates = slices.Clone(p.Gates)
	out.ExitContract.AllowedOutcomes = slices.Clone(p.ExitContract.AllowedOutcomes)
	return out
}

// Condition specifies when a Transition fires.
//
// Mechanical conditions (Verdict, CIGreen, MinRound, MaxRound) are evaluated
// deterministically by the controller. Conditions with a non-empty JudgmentRule
// require a Dispatcher judgment turn evaluated against that rule text.
type Condition struct {
	Verdict      string `json:"verdict,omitempty"`
	JudgmentRule string `json:"judgment_rule,omitempty"`
	MinRound     int    `json:"min_round,omitempty"`
	MaxRound     int    `json:"max_round,omitempty"`
	CIGreen      bool   `json:"ci_green,omitempty"`
}

// NeedsJudgment reports whether this condition requires LLM judgment rather
// than purely mechanical evaluation.
func (c Condition) NeedsJudgment() bool {
	return strings.TrimSpace(c.JudgmentRule) != ""
}

// MatchesMechanical reports whether the mechanical fields of c are satisfied by
// the given verdict, CI status, and round number.
func (c Condition) MatchesMechanical(verdict string, ciGreen bool, round int) bool {
	if c.Verdict != "" && c.Verdict != verdict {
		return false
	}
	if c.CIGreen && !ciGreen {
		return false
	}
	if c.MinRound > 0 && round < c.MinRound {
		return false
	}
	if c.MaxRound > 0 && round > c.MaxRound {
		return false
	}
	return true
}

// Transition is a directed edge from one phase to another (or to TargetComplete / TargetEscalate).
type Transition struct {
	From         string    `json:"from"`
	To           string    `json:"to"`
	On           Condition `json:"on"`
	AdvanceRound bool      `json:"advance_round,omitempty"`
}

// Limits bounds rework rounds, retries, timeouts, and brief sizes for a recipe.
type Limits struct {
	MaxRounds          int           `json:"max_rounds"`
	MaxPhaseAttempts   int           `json:"max_phase_attempts"`
	PhaseTimeout       time.Duration `json:"phase_timeout"`
	MaxBriefBytes      int           `json:"max_brief_bytes"`
	MaxConcurrentLanes int           `json:"max_concurrent_lanes,omitempty"`
}

// MergePolicy governs how a lane's branch/PR is integrated into the target branch.
type MergePolicy struct {
	Mode              MergeMode `json:"mode"`
	Script            string    `json:"script,omitempty"`
	TargetBranch      string    `json:"target_branch,omitempty"`
	RequireReviewPass bool      `json:"require_review_pass"`
	RequireCIGreen    bool      `json:"require_ci_green"`
}

// Recipe is a versioned lane workflow consisting of its natural-language source
// and its compiled phases, transitions, limits, and merge policy.
type Recipe struct {
	CreatedAt   time.Time    `json:"created_at"`
	RunID       string       `json:"run_id,omitempty"`
	Name        string       `json:"name"`
	Source      string       `json:"source"`
	BaseRecipe  string       `json:"base_recipe,omitempty"`
	Phases      []Phase      `json:"phases"`
	Transitions []Transition `json:"transitions"`
	Unmapped    []string     `json:"unmapped,omitempty"`
	MergePolicy MergePolicy  `json:"merge_policy"`
	Limits      Limits       `json:"limits"`
	Version     int          `json:"version"`
	BaseVersion int          `json:"base_version,omitempty"`
}

// Clone returns a deep copy of r.
func (r Recipe) Clone() Recipe {
	out := r
	if r.Phases != nil {
		out.Phases = make([]Phase, len(r.Phases))
		for i, p := range r.Phases {
			out.Phases[i] = p.Clone()
		}
	}
	out.Transitions = slices.Clone(r.Transitions)
	out.Unmapped = slices.Clone(r.Unmapped)
	return out
}

// PhaseNames returns the ordered phase names in r.
func (r *Recipe) PhaseNames() []string {
	names := make([]string, len(r.Phases))
	for i, p := range r.Phases {
		names[i] = p.Name
	}
	return names
}

// FirstPhase returns the name of the first phase, or "" if r has no phases.
func (r *Recipe) FirstPhase() string {
	if len(r.Phases) == 0 {
		return ""
	}
	return r.Phases[0].Name
}

// Phase looks up a phase by name.
func (r *Recipe) Phase(name string) (Phase, bool) {
	for _, p := range r.Phases {
		if p.Name == name {
			return p.Clone(), true
		}
	}
	return Phase{}, false
}

// TransitionsFrom returns all transitions originating from fromPhase in declaration order.
func (r *Recipe) TransitionsFrom(fromPhase string) []Transition {
	var out []Transition
	for _, tr := range r.Transitions {
		if tr.From == fromPhase {
			out = append(out, tr)
		}
	}
	return out
}

// MatchTransitions evaluates transitions out of fromPhase against a phase outcome.
// It returns the first matching purely mechanical transition (if any), and any
// candidate transitions whose mechanical preconditions match but require
// Dispatcher judgment to evaluate JudgmentRule.
func (r *Recipe) MatchTransitions(fromPhase, verdict string, ciGreen bool, round int) (mechanical *Transition, judgment []Transition) {
	for _, tr := range r.Transitions {
		if tr.From != fromPhase {
			continue
		}
		if !tr.On.MatchesMechanical(verdict, ciGreen, round) {
			continue
		}
		if tr.On.NeedsJudgment() {
			judgment = append(judgment, tr)
			continue
		}
		if mechanical == nil {
			matched := tr
			mechanical = &matched
		}
	}
	return mechanical, judgment
}

func hasTrailingDigit(s string) bool {
	return s != "" && s[len(s)-1] >= '0' && s[len(s)-1] <= '9'
}

// Validate checks that r is structurally sound.
func (r *Recipe) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("recipe name is required")
	}
	if r.Version < 0 {
		return fmt.Errorf("recipe %q: version must be >= 0, got %d", r.Name, r.Version)
	}
	if len(r.Phases) == 0 {
		return fmt.Errorf("recipe %q: at least one phase is required", r.Name)
	}
	seenPhases := make(map[string]bool, len(r.Phases))
	for _, p := range r.Phases {
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("recipe %q: phase name must not be empty", r.Name)
		}
		if seenPhases[p.Name] {
			return fmt.Errorf("recipe %q: duplicate phase name %q", r.Name, p.Name)
		}
		if p.Name == TargetComplete || p.Name == TargetEscalate {
			return fmt.Errorf("recipe %q: phase name %q is reserved for transition targets", r.Name, p.Name)
		}
		if hasTrailingDigit(p.Name) {
			return fmt.Errorf("recipe %q: phase name %q must not end in a digit (ambiguous with <phase><round> attempt keys)", r.Name, p.Name)
		}
		if len(p.Agents) == 0 {
			return fmt.Errorf("recipe %q phase %q: at least one agent spec is required", r.Name, p.Name)
		}
		for _, a := range p.Agents {
			if strings.TrimSpace(a.Harness) == "" || strings.TrimSpace(a.Model) == "" {
				return fmt.Errorf("recipe %q phase %q: agent spec requires non-empty harness and model", r.Name, p.Name)
			}
		}
		seenPhases[p.Name] = true
	}

	for i, tr := range r.Transitions {
		if !seenPhases[tr.From] {
			return fmt.Errorf("recipe %q transition %d: unknown source phase %q", r.Name, i, tr.From)
		}
		if tr.To != TargetComplete && tr.To != TargetEscalate && !seenPhases[tr.To] {
			return fmt.Errorf("recipe %q transition %d: unknown target phase %q", r.Name, i, tr.To)
		}
	}

	if !r.MergePolicy.Mode.Valid() {
		return fmt.Errorf("recipe %q: invalid merge policy mode %q", r.Name, r.MergePolicy.Mode)
	}
	if r.Limits.MaxRounds < 0 || r.Limits.MaxPhaseAttempts < 0 || r.Limits.MaxBriefBytes < 0 || r.Limits.PhaseTimeout < 0 {
		return fmt.Errorf("recipe %q: limits must not be negative", r.Name)
	}
	return nil
}

// FormatTable renders a concise Markdown summary of the compiled recipe for
// owner confirmation or inspection.
func (r *Recipe) FormatTable() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Recipe `%s` (v%d)\n\n", r.Name, r.Version)
	if r.Source != "" {
		fmt.Fprintf(&b, "> %s\n\n", strings.ReplaceAll(strings.TrimSpace(r.Source), "\n", "\n> "))
	}
	b.WriteString("| Phase | Agents | Mutating | Gates |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, p := range r.Phases {
		agents := make([]string, len(p.Agents))
		for i, a := range p.Agents {
			if a.Effort != "" {
				agents[i] = fmt.Sprintf("%s:%s(%s)", a.Harness, a.Model, a.Effort)
			} else {
				agents[i] = fmt.Sprintf("%s:%s", a.Harness, a.Model)
			}
		}
		gates := make([]string, len(p.Gates))
		for i, g := range p.Gates {
			gates[i] = g.Name
		}
		gateStr := "-"
		if len(gates) > 0 {
			gateStr = strings.Join(gates, ", ")
		}
		fmt.Fprintf(&b, "| %s | %s | %t | %s |\n", p.Name, strings.Join(agents, " -> "), p.Mutating, gateStr)
	}

	b.WriteString("\n| From | On | To | Advance Round |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, tr := range r.Transitions {
		var condParts []string
		if tr.On.Verdict != "" {
			condParts = append(condParts, "verdict="+tr.On.Verdict)
		}
		if tr.On.CIGreen {
			condParts = append(condParts, "ci_green")
		}
		if tr.On.MinRound > 0 {
			condParts = append(condParts, fmt.Sprintf("round>=%d", tr.On.MinRound))
		}
		if tr.On.MaxRound > 0 {
			condParts = append(condParts, fmt.Sprintf("round<=%d", tr.On.MaxRound))
		}
		if tr.On.JudgmentRule != "" {
			condParts = append(condParts, fmt.Sprintf("judgment(%q)", tr.On.JudgmentRule))
		}
		condStr := "*"
		if len(condParts) > 0 {
			condStr = strings.Join(condParts, " & ")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %t |\n", tr.From, condStr, tr.To, tr.AdvanceRound)
	}

	fmt.Fprintf(&b, "\nLimits: max_rounds=%d, max_phase_attempts=%d, phase_timeout=%s, merge=%s\n",
		r.Limits.MaxRounds, r.Limits.MaxPhaseAttempts, r.Limits.PhaseTimeout, r.MergePolicy.Mode)
	if len(r.Unmapped) > 0 {
		fmt.Fprintf(&b, "Unmapped phrases: %s\n", strings.Join(r.Unmapped, "; "))
	}
	return b.String()
}

// DefaultSource is the natural-language specification for the shipped default recipe.
const DefaultSource = "Each task runs swe -> clean -> review -> integrate. " +
	"SWE implements the change, runs lint and scoped tests, and opens a ready PR. " +
	"Clean trims redundant comments and polishes the PR without changing behavior. " +
	"Review checks the PR against the brief and requires green CI on the reviewed HEAD; " +
	"if review reports needs-swe, return to swe on the next round (up to 3 rounds, then escalate). " +
	"Integrate merges the PR once review passes and CI is green on the pinned HEAD."

// Default returns a fresh copy of the shipped default recipe (swe -> clean -> review -> integrate).
func Default() Recipe {
	return Recipe{
		Name:    DefaultName,
		Version: 1,
		Source:  DefaultSource,
		Phases: []Phase{
			{
				Name: PhaseSWE,
				Agents: []AgentSpec{
					{Harness: "claude", Model: "opus", Effort: "high"},
					{Harness: "codex", Model: "gpt-5.4", Effort: "high"},
				},
				BriefTemplate: "Implement the backlog item, pass lint and scoped tests, push branch, and open a ready PR.",
				ExitContract: ExitContract{
					AllowedOutcomes: []string{OutcomeDone, OutcomeBlocked, OutcomeNeedsInput},
					RequiresPR:      true,
					RequiresReport:  true,
				},
				Gates: []Gate{
					{Name: "lint", Command: "scripts/lint.sh", Required: true},
					{Name: "scoped_tests", Required: true},
					{Name: "pr_open", Required: true},
				},
				Mutating: true,
			},
			{
				Name: PhaseClean,
				Agents: []AgentSpec{
					{Harness: "claude", Model: "sonnet", Effort: "medium"},
					{Harness: "codex", Model: "gpt-5.4-mini", Effort: "medium"},
				},
				BriefTemplate: "Trim redundant comments, polish the diff, verify lint and scoped tests remain green, and push.",
				ExitContract: ExitContract{
					AllowedOutcomes: []string{OutcomeDone, OutcomeBlocked, OutcomeNeedsInput},
					RequiresPR:      true,
					RequiresReport:  true,
				},
				Gates: []Gate{
					{Name: "lint", Command: "scripts/lint.sh", Required: true},
					{Name: "scoped_tests", Required: true},
				},
				Mutating: true,
			},
			{
				Name: PhaseReview,
				Agents: []AgentSpec{
					{Harness: "claude", Model: "sonnet", Effort: "high"},
					{Harness: "codex", Model: "gpt-5.4", Effort: "high"},
				},
				BriefTemplate: "Review the PR against the brief and verify required CI checks are green on the exact reviewed HEAD.",
				ExitContract: ExitContract{
					AllowedOutcomes: []string{OutcomeDone, OutcomeNeedsSWE, OutcomeBlocked, OutcomeNeedsInput},
					RequiresPR:      true,
					RequiresReport:  true,
				},
				Gates: []Gate{
					{Name: "ci_green", Required: true},
					{Name: "review_head_pinned", Required: true},
				},
				Mutating: false,
			},
			{
				Name: PhaseIntegrate,
				Agents: []AgentSpec{
					{Harness: "claude", Model: "sonnet", Effort: "medium"},
				},
				BriefTemplate: "Verify pinned review approval and green CI checks on HEAD, then integrate the PR into the target branch.",
				ExitContract: ExitContract{
					AllowedOutcomes: []string{OutcomeDone, OutcomeNeedsSWE, OutcomeBlocked, OutcomeNeedsInput},
					RequiresPR:      true,
					RequiresReport:  true,
				},
				Gates: []Gate{
					{Name: "ci_green", Required: true},
					{Name: "review_approved", Required: true},
				},
				Mutating: true,
			},
		},
		Transitions: []Transition{
			{
				From: PhaseSWE,
				On:   Condition{Verdict: OutcomeDone},
				To:   PhaseClean,
			},
			{
				From: PhaseClean,
				On:   Condition{Verdict: OutcomeDone},
				To:   PhaseReview,
			},
			{
				From: PhaseReview,
				On:   Condition{Verdict: OutcomeDone, CIGreen: true},
				To:   PhaseIntegrate,
			},
			{
				From:         PhaseReview,
				On:           Condition{Verdict: OutcomeNeedsSWE, MaxRound: 2},
				To:           PhaseSWE,
				AdvanceRound: true,
			},
			{
				From: PhaseReview,
				On:   Condition{Verdict: OutcomeNeedsSWE, MinRound: 3},
				To:   TargetEscalate,
			},
			{
				From: PhaseIntegrate,
				On:   Condition{Verdict: OutcomeDone, CIGreen: true},
				To:   TargetComplete,
			},
			{
				From:         PhaseIntegrate,
				On:           Condition{Verdict: OutcomeNeedsSWE},
				To:           PhaseSWE,
				AdvanceRound: true,
			},
		},
		Limits: Limits{
			MaxRounds:          3,
			MaxPhaseAttempts:   2,
			PhaseTimeout:       45 * time.Minute,
			MaxBriefBytes:      16 * 1024,
			MaxConcurrentLanes: 4,
		},
		MergePolicy: MergePolicy{
			Mode:              MergeModeController,
			Script:            "pr_arm.sh",
			TargetBranch:      "main",
			RequireReviewPass: true,
			RequireCIGreen:    true,
		},
	}
}
