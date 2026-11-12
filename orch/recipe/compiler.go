package recipe

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orch/model"
)

// Default configuration for the natural-language recipe compiler.
const (
	DefaultCompilerModel     = "claude-opus-4-6"
	DefaultCompilerMaxTokens = 4096
	SubmitRecipeToolName     = "submit_recipe"
)

// ErrEmptyRecipeSource is returned when Compile is called with an empty natural-language source.
var ErrEmptyRecipeSource = errors.New("orch/recipe: recipe source is required")

// CompilerSystemPrompt is the planner system prompt used when compiling
// plain-language workflow specifications and per-lane deltas into typed Recipes.
const CompilerSystemPrompt = `You are the Nebula Swarm Recipe Compiler (Planner role).
Turn the owner's plain-language lane/phase workflow description (or a per-lane delta against a base recipe) into a typed Recipe by calling the submit_recipe tool.

Rules:
1. Phases:
   - Each phase has a lowercase slug name without trailing digits (e.g. "swe", "clean", "review", "security-review", "integrate", "analyze", "propose", "critique", "synthesize", "prove", "verify", "visual-review", "benchmark"). Never use "complete" or "escalate" as phase names.
   - Populate "agents" as an ordered fallback chain of AgentSpec{harness, model, effort} (harnesses: "claude", "codex"; models: "opus", "sonnet", "haiku", "gpt-5.4", "gpt-5.4-mini"; effort: "low", "medium", "high").
   - Set "mutating": true for phases that write code, commits, branches, or merges; false for read-only analysis, critique, or review phases.
   - Set "exit_contract" with "allowed_outcomes" (subset of ["done", "needs-swe", "blocked", "needs-input"]), "requires_pr", and "requires_report": true.
   - Include appropriate verification "gates" (e.g. "lint" with command "scripts/lint.sh", "scoped_tests", "pr_open", "ci_green", "review_head_pinned", "review_approved", "security_check", "flake_repeat", "screenshots", "bench_compare").

2. Transitions & Conditions:
   - Connect every phase with directed transitions to the next phase or terminal targets "complete" or "escalate". Every phase must be reachable from the first phase and have at least one outgoing transition, and at least one transition must reach "complete".
   - Put purely mechanical predicates in "on.verdict", "on.ci_green", "on.min_round", and "on.max_round".
   - Any condition that requires inspecting the diff, file paths, domain, risk, or qualitative judgment (e.g. "for docs", "under 200 lines", "for auth or migration changes", "if the change looks risky") MUST remain as plain natural-language text in "on.judgment_rule" so the controller routes it to the Dispatcher.
   - Set "advance_round": true when returning to an earlier phase for rework (e.g. review -> swe on verdict="needs-swe").

3. Limits & Merge Policy:
   - Populate "limits" with max_rounds (default 3), max_phase_attempts (default 2), phase_timeout (Go duration string such as "45m"), max_brief_bytes (default 16384), and max_concurrent_lanes (default 4).
   - Populate "merge_policy" with mode:
     * "controller" (script "pr_arm.sh", target_branch "main") when the controller/integrate step merges the PR.
     * "self" (script "own_merge.sh", target_branch "main") when lanes merge their own PR once gates/CI pass.
     * "manual" when the PR is left open for a human to merge.
     * "none" when the lane is non-mutating / analysis-only / debate and produces no PR merge.

4. Per-Lane Overrides:
   - When a base recipe is provided, treat the owner's text as a delta on top of the base recipe. Preserve base phases, agent chains, gates, transitions, limits, and merge policy except where the delta adds, removes, reorders, or modifies them.

5. Unmapped Phrases (Never Silently Drop):
   - Any phrase, clause, or instruction in the owner's input that cannot be represented in phases, transitions, limits, or merge_policy MUST be included verbatim in "unmapped". Never silently drop unmappable requests.`

type compiledAgentInput struct {
	Harness string `json:"harness" jsonschema:"required,description=CLI harness name (claude or codex)"`
	Model   string `json:"model" jsonschema:"required,description=Model identifier (e.g. opus, sonnet, haiku, gpt-5.4, gpt-5.4-mini)"`
	Effort  string `json:"effort,omitempty" jsonschema:"description=Reasoning effort level (low, medium, high)"`
}

type compiledGateInput struct {
	Name     string `json:"name" jsonschema:"required,description=Gate identifier"`
	Command  string `json:"command,omitempty" jsonschema:"description=Optional shell command for the gate"`
	Required bool   `json:"required" jsonschema:"required,description=Whether passing this gate is mandatory"`
}

type compiledExitContractInput struct {
	AllowedOutcomes []string `json:"allowed_outcomes" jsonschema:"required,description=Permitted phase report outcomes (done, needs-swe, blocked, needs-input)"`
	RequiresPR      bool     `json:"requires_pr,omitempty" jsonschema:"description=Whether the phase requires an open PR"`
	RequiresReport  bool     `json:"requires_report" jsonschema:"required,description=Whether the phase requires filing a phase report"`
}

type compiledPhaseInput struct {
	Name          string                    `json:"name" jsonschema:"required,description=Phase name without trailing digits"`
	BriefTemplate string                    `json:"brief_template,omitempty" jsonschema:"description=Instructions given to the worker agent in this phase"`
	Agents        []compiledAgentInput      `json:"agents" jsonschema:"required,description=Ordered agent/model fallback chain"`
	Gates         []compiledGateInput       `json:"gates,omitempty" jsonschema:"description=Verification gates required for this phase"`
	ExitContract  compiledExitContractInput `json:"exit_contract" jsonschema:"required,description=Phase exit contract"`
	Mutating      bool                      `json:"mutating" jsonschema:"required,description=Whether the phase modifies repository code or PR state"`
}

type compiledConditionInput struct {
	Verdict      string `json:"verdict,omitempty" jsonschema:"description=Required phase report verdict (done, needs-swe, blocked, needs-input)"`
	JudgmentRule string `json:"judgment_rule,omitempty" jsonschema:"description=Natural-language condition evaluated by the Dispatcher when judgment is needed"`
	MinRound     int    `json:"min_round,omitempty" jsonschema:"description=Minimum round number (inclusive)"`
	MaxRound     int    `json:"max_round,omitempty" jsonschema:"description=Maximum round number (inclusive)"`
	CIGreen      bool   `json:"ci_green,omitempty" jsonschema:"description=Whether green CI checks on HEAD are required"`
}

type compiledTransitionInput struct {
	From         string                 `json:"from" jsonschema:"required,description=Source phase name"`
	To           string                 `json:"to" jsonschema:"required,description=Target phase name or terminal target (complete or escalate)"`
	On           compiledConditionInput `json:"on" jsonschema:"required,description=Transition condition"`
	AdvanceRound bool                   `json:"advance_round,omitempty" jsonschema:"description=True when returning to an earlier phase on the next rework round"`
}

type compiledMergePolicyInput struct {
	Mode              MergeMode `json:"mode" jsonschema:"required,enum=controller,enum=self,enum=manual,enum=none,description=Merge mode (controller, self, manual, none)"`
	Script            string    `json:"script,omitempty" jsonschema:"description=Merge script (e.g. pr_arm.sh or own_merge.sh)"`
	TargetBranch      string    `json:"target_branch,omitempty" jsonschema:"description=Target git branch (e.g. main)"`
	RequireReviewPass bool      `json:"require_review_pass" jsonschema:"required,description=Whether a passing review on the pinned HEAD is required"`
	RequireCIGreen    bool      `json:"require_ci_green" jsonschema:"required,description=Whether green CI on HEAD is required"`
}

type compiledLimitsInput struct {
	PhaseTimeout       string        `json:"phase_timeout,omitempty" jsonschema:"description=Max duration per phase attempt (Go duration string like 45m)"`
	MaxRounds          int           `json:"max_rounds" jsonschema:"required,description=Max rework rounds before escalating"`
	MaxPhaseAttempts   int           `json:"max_phase_attempts" jsonschema:"required,description=Max attempts per phase across fallback agents"`
	MaxBriefBytes      int           `json:"max_brief_bytes,omitempty" jsonschema:"description=Max brief size in bytes"`
	MaxConcurrentLanes int           `json:"max_concurrent_lanes,omitempty" jsonschema:"description=Max concurrent active lanes"`
	parsedTimeout      time.Duration `json:"-"`
}

func (l *compiledLimitsInput) UnmarshalJSON(data []byte) error {
	var raw struct {
		PhaseTimeout       json.RawMessage `json:"phase_timeout"`
		MaxRounds          int             `json:"max_rounds"`
		MaxPhaseAttempts   int             `json:"max_phase_attempts"`
		MaxBriefBytes      int             `json:"max_brief_bytes"`
		MaxConcurrentLanes int             `json:"max_concurrent_lanes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	l.MaxRounds = raw.MaxRounds
	l.MaxPhaseAttempts = raw.MaxPhaseAttempts
	l.MaxBriefBytes = raw.MaxBriefBytes
	l.MaxConcurrentLanes = raw.MaxConcurrentLanes

	trimmed := bytes.TrimSpace(raw.PhaseTimeout)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		l.PhaseTimeout = ""
		l.parsedTimeout = 0
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		l.PhaseTimeout = s
		if s == "" {
			l.parsedTimeout = 0
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid phase_timeout duration %q: %w", s, err)
		}
		l.parsedTimeout = d
		return nil
	}
	var ns int64
	if err := json.Unmarshal(trimmed, &ns); err != nil {
		return fmt.Errorf("invalid phase_timeout %s: %w", string(trimmed), err)
	}
	l.parsedTimeout = time.Duration(ns)
	l.PhaseTimeout = formatDurationCompact(l.parsedTimeout)
	return nil
}

type compiledRecipeInput struct {
	Name        string                    `json:"name" jsonschema:"required,description=Compiled recipe name"`
	Phases      []compiledPhaseInput      `json:"phases" jsonschema:"required,description=Ordered phases in the recipe"`
	Transitions []compiledTransitionInput `json:"transitions" jsonschema:"required,description=Directed transitions between phases and terminal targets"`
	Unmapped    []string                  `json:"unmapped,omitempty" jsonschema:"description=Phrases from the owner description that could not be mapped"`
	MergePolicy compiledMergePolicyInput  `json:"merge_policy" jsonschema:"required,description=PR merge policy"`
	Limits      compiledLimitsInput       `json:"limits" jsonschema:"required,description=Execution and retry limits"`
}

func formatDurationCompact(d time.Duration) string {
	if d == 0 {
		return ""
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func recipeToCompiledInput(r Recipe) compiledRecipeInput {
	phases := make([]compiledPhaseInput, len(r.Phases))
	for i, p := range r.Phases {
		agents := make([]compiledAgentInput, len(p.Agents))
		for j, a := range p.Agents {
			agents[j] = compiledAgentInput{
				Harness: a.Harness,
				Model:   a.Model,
				Effort:  a.Effort,
			}
		}
		var gates []compiledGateInput
		if len(p.Gates) > 0 {
			gates = make([]compiledGateInput, len(p.Gates))
			for j, g := range p.Gates {
				gates[j] = compiledGateInput{
					Name:     g.Name,
					Command:  g.Command,
					Required: g.Required,
				}
			}
		}
		phases[i] = compiledPhaseInput{
			Name:          p.Name,
			BriefTemplate: p.BriefTemplate,
			Agents:        agents,
			Gates:         gates,
			ExitContract: compiledExitContractInput{
				AllowedOutcomes: slices.Clone(p.ExitContract.AllowedOutcomes),
				RequiresPR:      p.ExitContract.RequiresPR,
				RequiresReport:  p.ExitContract.RequiresReport,
			},
			Mutating: p.Mutating,
		}
	}

	transitions := make([]compiledTransitionInput, len(r.Transitions))
	for i, tr := range r.Transitions {
		transitions[i] = compiledTransitionInput{
			From: tr.From,
			To:   tr.To,
			On: compiledConditionInput{
				Verdict:      tr.On.Verdict,
				JudgmentRule: tr.On.JudgmentRule,
				MinRound:     tr.On.MinRound,
				MaxRound:     tr.On.MaxRound,
				CIGreen:      tr.On.CIGreen,
			},
			AdvanceRound: tr.AdvanceRound,
		}
	}

	return compiledRecipeInput{
		Name:        r.Name,
		Phases:      phases,
		Transitions: transitions,
		Unmapped:    slices.Clone(r.Unmapped),
		MergePolicy: compiledMergePolicyInput{
			Mode:              r.MergePolicy.Mode,
			Script:            r.MergePolicy.Script,
			TargetBranch:      r.MergePolicy.TargetBranch,
			RequireReviewPass: r.MergePolicy.RequireReviewPass,
			RequireCIGreen:    r.MergePolicy.RequireCIGreen,
		},
		Limits: compiledLimitsInput{
			PhaseTimeout:       formatDurationCompact(r.Limits.PhaseTimeout),
			MaxRounds:          r.Limits.MaxRounds,
			MaxPhaseAttempts:   r.Limits.MaxPhaseAttempts,
			MaxBriefBytes:      r.Limits.MaxBriefBytes,
			MaxConcurrentLanes: r.Limits.MaxConcurrentLanes,
			parsedTimeout:      r.Limits.PhaseTimeout,
		},
	}
}

func (in compiledRecipeInput) toRecipe(req CompileRequest) Recipe {
	phases := make([]Phase, len(in.Phases))
	for i, p := range in.Phases {
		agents := make([]AgentSpec, len(p.Agents))
		for j, a := range p.Agents {
			agents[j] = AgentSpec{
				Harness: strings.TrimSpace(a.Harness),
				Model:   strings.TrimSpace(a.Model),
				Effort:  strings.TrimSpace(a.Effort),
			}
		}
		var gates []Gate
		if len(p.Gates) > 0 {
			gates = make([]Gate, len(p.Gates))
			for j, g := range p.Gates {
				gates[j] = Gate{
					Name:     strings.TrimSpace(g.Name),
					Command:  strings.TrimSpace(g.Command),
					Required: g.Required,
				}
			}
		}
		outcomes := make([]string, len(p.ExitContract.AllowedOutcomes))
		for j, o := range p.ExitContract.AllowedOutcomes {
			outcomes[j] = strings.TrimSpace(o)
		}
		phases[i] = Phase{
			Name:          strings.TrimSpace(p.Name),
			BriefTemplate: strings.TrimSpace(p.BriefTemplate),
			Agents:        agents,
			Gates:         gates,
			ExitContract: ExitContract{
				AllowedOutcomes: outcomes,
				RequiresPR:      p.ExitContract.RequiresPR,
				RequiresReport:  p.ExitContract.RequiresReport,
			},
			Mutating: p.Mutating,
		}
	}

	transitions := make([]Transition, len(in.Transitions))
	for i, tr := range in.Transitions {
		transitions[i] = Transition{
			From: strings.TrimSpace(tr.From),
			To:   strings.TrimSpace(tr.To),
			On: Condition{
				Verdict:      strings.TrimSpace(tr.On.Verdict),
				JudgmentRule: strings.TrimSpace(tr.On.JudgmentRule),
				MinRound:     tr.On.MinRound,
				MaxRound:     tr.On.MaxRound,
				CIGreen:      tr.On.CIGreen,
			},
			AdvanceRound: tr.AdvanceRound,
		}
	}

	var unmapped []string
	for _, u := range in.Unmapped {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			unmapped = append(unmapped, trimmed)
		}
	}

	name := strings.TrimSpace(in.Name)
	if reqName := strings.TrimSpace(req.Name); reqName != "" {
		name = reqName
	}

	version := req.Version
	if version == 0 {
		if req.Base != nil && name == req.Base.Name && req.Base.Version > 0 {
			version = req.Base.Version + 1
		} else {
			version = 1
		}
	}

	runID := strings.TrimSpace(req.RunID)
	var baseRecipe string
	var baseVersion int
	if req.Base != nil {
		if runID == "" {
			runID = req.Base.RunID
		}
		baseRecipe = req.Base.Name
		baseVersion = max(req.Base.Version, 1)
	}

	return Recipe{
		RunID:       runID,
		Name:        name,
		Version:     version,
		Source:      strings.TrimSpace(req.Source),
		BaseRecipe:  baseRecipe,
		BaseVersion: baseVersion,
		Phases:      phases,
		Transitions: transitions,
		Unmapped:    unmapped,
		MergePolicy: MergePolicy{
			Mode:              MergeMode(strings.TrimSpace(string(in.MergePolicy.Mode))),
			Script:            strings.TrimSpace(in.MergePolicy.Script),
			TargetBranch:      strings.TrimSpace(in.MergePolicy.TargetBranch),
			RequireReviewPass: in.MergePolicy.RequireReviewPass,
			RequireCIGreen:    in.MergePolicy.RequireCIGreen,
		},
		Limits: Limits{
			MaxRounds:          in.Limits.MaxRounds,
			MaxPhaseAttempts:   in.Limits.MaxPhaseAttempts,
			PhaseTimeout:       in.Limits.parsedTimeout,
			MaxBriefBytes:      in.Limits.MaxBriefBytes,
			MaxConcurrentLanes: in.Limits.MaxConcurrentLanes,
		},
	}
}

// SubmitRecipeToolDefinition returns the Messages API ToolDefinition for submit_recipe.
func SubmitRecipeToolDefinition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        SubmitRecipeToolName,
		Description: "Submit the compiled typed Recipe for the owner's natural-language specification or per-lane override.",
		InputSchema: model.SchemaFor[compiledRecipeInput](),
	}
}

// RecipeToToolInput marshals r into the JSON input shape expected by the submit_recipe tool.
func RecipeToToolInput(r Recipe) (json.RawMessage, error) {
	b, err := json.Marshal(recipeToCompiledInput(r))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// CompileRequest describes a natural-language recipe compilation or per-lane override request.
type CompileRequest struct {
	// Base is an optional base recipe for per-lane overrides or incremental edits.
	Base *Recipe `json:"base,omitempty"`
	// Name is the target recipe name (defaults to Base.Name if Base is set, else DefaultName).
	Name string `json:"name,omitempty"`
	// RunID optionally tags the compiled recipe with a run identifier.
	RunID string `json:"run_id,omitempty"`
	// Source is the plain-language recipe description or override delta.
	Source string `json:"source"`
	// Version is the target version number (defaults to 1, or Base.Version+1 when overriding the same name).
	Version int `json:"version,omitempty"`
}

// CompileResult holds the compiled Recipe, its readable echo table, any unmapped
// phrases from the owner's input, and the model token/cost usage.
type CompileResult struct {
	EchoTable string      `json:"echo_table"`
	Unmapped  []string    `json:"unmapped,omitempty"`
	Recipe    Recipe      `json:"recipe"`
	Usage     model.Usage `json:"usage"`
}

// CompilerOption configures a Compiler.
type CompilerOption func(*Compiler)

// WithModel sets the model identifier used for planner compilation requests.
func WithModel(modelName string) CompilerOption {
	return func(c *Compiler) {
		if trimmed := strings.TrimSpace(modelName); trimmed != "" {
			c.modelName = trimmed
		}
	}
}

// WithMaxTokens sets the max_tokens parameter on planner compilation requests.
func WithMaxTokens(maxTokens int) CompilerOption {
	return func(c *Compiler) {
		if maxTokens > 0 {
			c.maxTokens = maxTokens
		}
	}
}

// Compiler compiles natural-language lane/phase descriptions and per-lane
// overrides into typed, validated Recipes via a ModelClient.
type Compiler struct {
	client    model.ModelClient
	modelName string
	maxTokens int
}

// NewCompiler creates a new Recipe Compiler backed by client.
func NewCompiler(client model.ModelClient, opts ...CompilerOption) *Compiler {
	c := &Compiler{
		client:    client,
		modelName: DefaultCompilerModel,
		maxTokens: DefaultCompilerMaxTokens,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// BuildCompileUserPrompt formats the user prompt sent to the model for req.
func BuildCompileUserPrompt(req CompileRequest) string {
	targetName := strings.TrimSpace(req.Name)
	if targetName == "" {
		if req.Base != nil && strings.TrimSpace(req.Base.Name) != "" {
			targetName = strings.TrimSpace(req.Base.Name)
		} else {
			targetName = DefaultName
		}
	}
	source := strings.TrimSpace(req.Source)

	var b strings.Builder
	if req.Base == nil {
		b.WriteString("Compile the following natural-language recipe into a typed Recipe by calling submit_recipe.\n\n")
		fmt.Fprintf(&b, "Recipe name: %s\n", targetName)
		b.WriteString("Natural-language specification:\n")
		b.WriteString(source)
		return b.String()
	}

	base := req.Base.Clone()
	if base.Version <= 0 {
		base.Version = 1
	}
	baseJSON, _ := json.MarshalIndent(recipeToCompiledInput(base), "", "  ")

	b.WriteString("Compile a per-lane recipe override by applying the natural-language delta to the base recipe and calling submit_recipe.\n\n")
	fmt.Fprintf(&b, "Target recipe name: %s\n", targetName)
	fmt.Fprintf(&b, "Base recipe: %s (v%d)\n\n", base.Name, base.Version)
	b.WriteString("Base recipe summary:\n")
	b.WriteString(base.FormatTable())
	b.WriteString("\nBase recipe JSON:\n")
	b.Write(baseJSON)
	b.WriteString("\n\nNatural-language override delta:\n")
	b.WriteString(source)
	return b.String()
}

// BuildCompileModelRequest constructs the Messages API Request for req.
func (c *Compiler) BuildCompileModelRequest(req CompileRequest) model.Request {
	return model.Request{
		Model:     cmp.Or(c.modelName, DefaultCompilerModel),
		System:    CompilerSystemPrompt,
		Messages:  []model.Message{model.NewUserMessage(BuildCompileUserPrompt(req))},
		Tools:     []model.ToolDefinition{SubmitRecipeToolDefinition()},
		MaxTokens: max(c.maxTokens, DefaultCompilerMaxTokens),
	}
}

// Compile executes a planner-style model call to compile req into a validated Recipe.
func (c *Compiler) Compile(ctx context.Context, req CompileRequest) (*CompileResult, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("orch/recipe: compiler requires a non-nil ModelClient")
	}
	if strings.TrimSpace(req.Source) == "" {
		return nil, ErrEmptyRecipeSource
	}

	modelReq := c.BuildCompileModelRequest(req)
	resp, err := c.client.CreateMessage(ctx, modelReq)
	if err != nil {
		return nil, fmt.Errorf("orch/recipe: model call failed: %w", err)
	}
	if resp == nil {
		return nil, errors.New("orch/recipe: model returned nil response")
	}

	parsed, err := extractCompiledRecipeInput(*resp)
	if err != nil {
		return nil, err
	}

	compiled := parsed.toRecipe(req)
	if err := compiled.Validate(); err != nil {
		return nil, fmt.Errorf("orch/recipe: invalid compiled recipe: %w", err)
	}

	return &CompileResult{
		Recipe:    compiled,
		EchoTable: compiled.FormatTable(),
		Unmapped:  slices.Clone(compiled.Unmapped),
		Usage:     resp.Usage,
	}, nil
}

// CompileText compiles a standalone plain-language recipe description.
func (c *Compiler) CompileText(ctx context.Context, name, source string) (*CompileResult, error) {
	return c.Compile(ctx, CompileRequest{
		Name:   name,
		Source: source,
	})
}

// CompileOverride compiles a per-lane or incremental recipe override from base and a plain-language delta.
func (c *Compiler) CompileOverride(ctx context.Context, base Recipe, name, delta string) (*CompileResult, error) {
	baseCopy := base.Clone()
	return c.Compile(ctx, CompileRequest{
		Base:   &baseCopy,
		Name:   name,
		Source: delta,
	})
}

// Compile is a convenience function that creates a Compiler with opts and compiles req.
func Compile(ctx context.Context, client model.ModelClient, req CompileRequest, opts ...CompilerOption) (*CompileResult, error) {
	return NewCompiler(client, opts...).Compile(ctx, req)
}

// CompileOverride is a convenience function that compiles a natural-language delta against base.
func CompileOverride(ctx context.Context, client model.ModelClient, base Recipe, name, delta string, opts ...CompilerOption) (*CompileResult, error) {
	return NewCompiler(client, opts...).CompileOverride(ctx, base, name, delta)
}

func extractCompiledRecipeInput(resp model.Response) (compiledRecipeInput, error) {
	for _, call := range resp.ToolCalls() {
		if call.Name == SubmitRecipeToolName {
			var in compiledRecipeInput
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return compiledRecipeInput{}, fmt.Errorf("orch/recipe: decode %s tool input: %w", SubmitRecipeToolName, err)
			}
			return in, nil
		}
	}

	rawText := strings.TrimSpace(resp.Text())
	if rawText == "" {
		return compiledRecipeInput{}, fmt.Errorf("orch/recipe: model response contained neither %s tool_use nor JSON text", SubmitRecipeToolName)
	}
	cleaned := stripMarkdownFence(rawText)
	var in compiledRecipeInput
	if err := json.Unmarshal([]byte(cleaned), &in); err != nil {
		return compiledRecipeInput{}, fmt.Errorf("orch/recipe: decode recipe JSON from response text: %w", err)
	}
	return in, nil
}

func stripMarkdownFence(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "```"); idx >= 0 {
		rest := s[idx+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return s
}
