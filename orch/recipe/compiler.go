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

const (
	DefaultCompilerModel     = "claude-opus-4-6"
	DefaultCompilerMaxTokens = 4096
	SubmitRecipeToolName     = "submit_recipe"
)

// ErrEmptyRecipeSource is returned when Compile is called with an empty source.
var ErrEmptyRecipeSource = errors.New("orch/recipe: recipe source is required")

// CompilerSystemPrompt is the planner system prompt for recipe compilation.
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
	Name        string              `json:"name" jsonschema:"required,description=Compiled recipe name"`
	Phases      []Phase             `json:"phases" jsonschema:"required,description=Ordered phases in the recipe"`
	Transitions []Transition        `json:"transitions" jsonschema:"required,description=Directed transitions between phases and terminal targets"`
	Unmapped    []string            `json:"unmapped,omitempty" jsonschema:"description=Phrases from the owner description that could not be mapped"`
	MergePolicy MergePolicy         `json:"merge_policy" jsonschema:"required,description=PR merge policy"`
	Limits      compiledLimitsInput `json:"limits" jsonschema:"required,description=Execution and retry limits"`
}

var submitRecipeSchema = model.SchemaFor[compiledRecipeInput]()

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
	cloned := r.Clone()
	return compiledRecipeInput{
		Name:        cloned.Name,
		Phases:      cloned.Phases,
		Transitions: cloned.Transitions,
		Unmapped:    cloned.Unmapped,
		MergePolicy: cloned.MergePolicy,
		Limits: compiledLimitsInput{
			PhaseTimeout:       formatDurationCompact(cloned.Limits.PhaseTimeout),
			MaxRounds:          cloned.Limits.MaxRounds,
			MaxPhaseAttempts:   cloned.Limits.MaxPhaseAttempts,
			MaxBriefBytes:      cloned.Limits.MaxBriefBytes,
			MaxConcurrentLanes: cloned.Limits.MaxConcurrentLanes,
			parsedTimeout:      cloned.Limits.PhaseTimeout,
		},
	}
}

func (in compiledRecipeInput) toRecipe(req CompileRequest) Recipe {
	phases := make([]Phase, len(in.Phases))
	for i, p := range in.Phases {
		p = p.Clone()
		p.Name = strings.TrimSpace(p.Name)
		p.BriefTemplate = strings.TrimSpace(p.BriefTemplate)
		for j, a := range p.Agents {
			p.Agents[j] = AgentSpec{
				Harness: strings.TrimSpace(a.Harness),
				Model:   strings.TrimSpace(a.Model),
				Effort:  strings.TrimSpace(a.Effort),
			}
		}
		for j, g := range p.Gates {
			p.Gates[j] = Gate{
				Name:     strings.TrimSpace(g.Name),
				Command:  strings.TrimSpace(g.Command),
				Required: g.Required,
			}
		}
		for j, o := range p.ExitContract.AllowedOutcomes {
			p.ExitContract.AllowedOutcomes[j] = strings.TrimSpace(o)
		}
		phases[i] = p
	}

	transitions := make([]Transition, len(in.Transitions))
	for i, tr := range in.Transitions {
		tr.From = strings.TrimSpace(tr.From)
		tr.To = strings.TrimSpace(tr.To)
		tr.On.Verdict = strings.TrimSpace(tr.On.Verdict)
		tr.On.JudgmentRule = strings.TrimSpace(tr.On.JudgmentRule)
		transitions[i] = tr
	}

	var unmapped []string
	for _, u := range in.Unmapped {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			unmapped = append(unmapped, trimmed)
		}
	}

	name := cmp.Or(strings.TrimSpace(req.Name), strings.TrimSpace(in.Name))

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
		runID = cmp.Or(runID, req.Base.RunID)
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
		InputSchema: slices.Clone(submitRecipeSchema),
	}
}

// RecipeToToolInput marshals r into the JSON input shape expected by the submit_recipe tool.
func RecipeToToolInput(r Recipe) (json.RawMessage, error) {
	return json.Marshal(recipeToCompiledInput(r))
}

// CompileRequest describes a natural-language recipe compilation or per-lane override request.
type CompileRequest struct {
	Base *Recipe `json:"base,omitempty"`
	// Name defaults to Base.Name if Base is set, else DefaultName.
	Name   string `json:"name,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	Source string `json:"source"`
	// Version defaults to 1, or Base.Version+1 when overriding the same recipe name.
	Version int `json:"version,omitempty"`
}

// CompileResult holds the compiled Recipe, echo table, unmapped phrases, and model usage.
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
	var baseName string
	if req.Base != nil {
		baseName = strings.TrimSpace(req.Base.Name)
	}
	targetName := cmp.Or(strings.TrimSpace(req.Name), baseName, DefaultName)
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
