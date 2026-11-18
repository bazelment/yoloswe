package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/brief"
	"github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	planreader "github.com/bazelment/yoloswe/orca/planner"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	ActorPlanner               = "planner"
	ToolRecipeCompile          = "recipe.compile"
	ToolRecipeCompileOverride  = "recipe.compile_override"
	ToolPlannerReplan          = "planner.replan"
	ToolPlannerImport          = "planner.import"
	ToolPlannerStartup         = "planner.startup"
	ToolPlannerChat            = planreader.ToolChat
	ToolPlannerProposeWork     = planreader.ToolProposeWork
	SubmitPlanReconcileTool    = "submit_plan_reconciliation"
	SubmitOpenPlanTool         = "submit_open_plan"
	DefaultConflictAskDeadline = 30 * time.Minute
)

// PlannerSystemPrompt is the system prompt for Planner replanning and plan import turns.
const PlannerSystemPrompt = `You are the Nebula Swarm Planner.
Reconcile the owner's plan or replanning request against the current Run objective, backlog, and lanes by calling submit_plan_reconciliation.
Apply non-conflicting objective updates and backlog additions/updates/drops directly. Any unmappable or conflicting points must be listed in conflicts with options and a sensible default.
Judge whether the approach is an open problem: uncertainty about what will work, competing approaches, or an untested assumption. For an open problem call submit_open_plan with your proposed approach in summary and proposed backlog. A critic will challenge it before any tasks spawn; you will revise once. For a closed, well-understood request call submit_plan_reconciliation.
Validate at the smallest scope that can disprove the approach before funding broader work. Reuse existing work only when its assumptions fit this problem.`

const CriticSystemPrompt = `Challenge the planner's approach before tasks spawn. Is there a cheaper, smallest-scope test that could disprove it first? Is an existing pattern wrong for this problem? Identify unsupported assumptions and propose a concrete revision. Read the owner's goal, current state and memories as context. Return a concise free-text critique; do not execute tasks or ask for approval.`

// PlanBacklogSpec describes a backlog item to add or update during plan reconciliation.
type PlanBacklogSpec struct {
	ID          string   `json:"id" jsonschema:"required,description=Backlog item ID"`
	Title       string   `json:"title" jsonschema:"required,description=Item title"`
	Description string   `json:"description,omitempty" jsonschema:"description=Item description"`
	Acceptance  string   `json:"acceptance,omitempty" jsonschema:"description=Acceptance criteria"`
	Priority    string   `json:"priority,omitempty" jsonschema:"description=Priority (p0, p1, p2, p3)"`
	RecipeName  string   `json:"recipe_name,omitempty" jsonschema:"description=Optional recipe name"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"description=Prerequisite item IDs"`
}

// PlanConflictAsk describes an unmappable or conflicting plan point that requires an owner.ask.
type PlanConflictAsk struct {
	ID       string   `json:"id,omitempty" jsonschema:"description=Ask identifier"`
	Question string   `json:"question" jsonschema:"required,description=Conflict question for the owner"`
	Default  string   `json:"default" jsonschema:"required,description=Default option applied at deadline"`
	Options  []string `json:"options" jsonschema:"required,description=Candidate options"`
}

// PlanReconciliation is the structured output of a Planner replan or plan import turn.
type PlanReconciliation struct {
	Objective       string            `json:"objective,omitempty" jsonschema:"description=Updated run objective"`
	PlanRef         string            `json:"plan_ref,omitempty" jsonschema:"description=Plan reference"`
	Summary         string            `json:"summary,omitempty" jsonschema:"description=Summary of plan changes"`
	AddItems        []PlanBacklogSpec `json:"add_items,omitempty" jsonschema:"description=New backlog items to add"`
	UpdateItems     []PlanBacklogSpec `json:"update_items,omitempty" jsonschema:"description=Existing backlog items to update"`
	DropItemIDs     []string          `json:"drop_item_ids,omitempty" jsonschema:"description=Backlog item IDs to drop"`
	Conflicts       []PlanConflictAsk `json:"conflicts,omitempty" jsonschema:"description=Conflicting or unmappable items requiring owner.ask"`
	UnmappedPhrases []string          `json:"unmapped,omitempty" jsonschema:"description=Unmappable plan phrases"`
}

var (
	submitPlanReconcileSchema = model.SchemaFor[PlanReconciliation]()
	plannerDirectiveKinds     = []store.DirectiveKind{store.DirectiveRecipeEdit, store.DirectivePlanImport}
)

// Planner handles recipe compilation, per-lane overrides, recipe_edit directives,
// plan_import directives, and Dispatcher replanning requests in fresh Planner threads.
type Planner struct {
	store          store.Store
	clock          clock.Clock
	client         model.ModelClient
	critic         model.ModelClient
	planReader     planreader.PlanReader
	compiler       *recipe.Compiler
	logger         *slog.Logger
	sessions       model.AgentSessionMap
	criticModel    string
	providerName   string
	criticProvider string
	effort         string
	agentBackend   model.AgentBackend
	llmGatewayURL  string
	claudeBin      string
	codexBin       string
	modelName      string
	investigate    model.InvestigateConfig
}

var _ dispatcher.RecipeCompiler = (*Planner)(nil)

// NewPlanner constructs a Planner for recipe compilation and plan reconciliation.
func NewPlanner(
	st store.Store,
	clk clock.Clock,
	client model.ModelClient,
	modelName string,
	logger *slog.Logger,
) *Planner {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	if logger == nil {
		logger = slog.Default()
	}
	modelName = cmp.Or(strings.TrimSpace(modelName), DefaultPlannerModel)
	var comp *recipe.Compiler
	if client != nil {
		comp = recipe.NewCompiler(client, recipe.WithModel(modelName))
	}
	return &Planner{
		store:      st,
		clock:      clk,
		client:     client,
		compiler:   comp,
		logger:     logger,
		modelName:  modelName,
		planReader: planreader.NewPlanReader(nil),
	}
}

// Session returns the active CLI AgentSession for runID, if any.
func (p *Planner) Session(runID string) *model.AgentSession {
	return p.sessions.Session(runID)
}

// CloseRun stops and removes the CLI AgentSession for runID, if any.
func (p *Planner) CloseRun(runID string) error {
	return p.sessions.CloseRun(runID)
}

// Close stops all active CLI AgentSessions managed by this Planner.
func (p *Planner) Close() error {
	return p.sessions.Close()
}

func (p *Planner) nextID(prefix string) string {
	return nextUniqueID(prefix, p.clock)
}

// CompileOverride implements dispatcher.RecipeCompiler for per-lane recipe overrides.
func (p *Planner) CompileOverride(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error) {
	start := p.clock.Now()
	runID := base.RunID
	name := fmt.Sprintf("override-%s", cmp.Or(base.Name, recipe.DefaultName))

	if p.compiler == nil {
		return recipe.Recipe{}, errors.New("orca: recipe compilation requires a model client")
	}
	res, err := p.compiler.CompileOverride(ctx, base, name, override)
	if err != nil {
		return recipe.Recipe{}, fmt.Errorf("compile recipe override: %w", err)
	}
	_ = p.recordDecision(ctx, runID, "", "", ToolRecipeCompileOverride, override, res.EchoTable, res.Usage, p.clock.Now().Sub(start))
	return res.Recipe, nil
}

// CompileAndSaveRecipe compiles a natural-language recipe (or override on base)
// in a fresh Planner thread and saves it to Store.Recipes().
func (p *Planner) CompileAndSaveRecipe(
	ctx context.Context,
	runID, name, source string,
	base *recipe.Recipe,
	concurrencyCap int,
) (recipe.Recipe, error) {
	start := p.clock.Now()
	threadID, turnID, err := p.startPlannerTurn(ctx, runID, "Compile recipe "+name, source)
	if err != nil {
		return recipe.Recipe{}, err
	}

	if p.compiler == nil {
		err := errors.New("orca: recipe compilation requires a model client")
		_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, "", err)
		return recipe.Recipe{}, err
	}
	res, err := p.compiler.Compile(ctx, recipe.CompileRequest{RunID: runID, Name: name, Source: source, Base: base})
	if err != nil {
		_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, "", err)
		return recipe.Recipe{}, fmt.Errorf("compile recipe %q: %w", name, err)
	}
	compiled, usage := res.Recipe, res.Usage
	compiled.RunID = runID
	compiled.Version = 0
	if concurrencyCap > 0 {
		compiled.Limits.MaxConcurrentLanes = concurrencyCap
	}
	if err := compiled.Validate(); err != nil {
		_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusFailed, usage, "", err)
		return recipe.Recipe{}, err
	}

	saved, err := p.store.Recipes().Save(ctx, compiled)
	if err != nil {
		_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusFailed, usage, "", err)
		return recipe.Recipe{}, err
	}

	echo := saved.FormatTable()
	_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusCompleted, usage, echo, nil)
	_ = p.recordDecision(ctx, runID, threadID, turnID, ToolRecipeCompile, source,
		fmt.Sprintf("compiled recipe %s@v%d", saved.Name, saved.Version), usage, p.clock.Now().Sub(start))

	p.logger.Info("planner compiled recipe",
		"run_id", runID,
		"recipe", saved.Name,
		"version", saved.Version,
		"unmapped", saved.Unmapped,
	)
	return saved, nil
}

// ReconcileDirectives processes any active recipe_edit or plan_import directives in Store.
func (p *Planner) ReconcileDirectives(ctx context.Context, runID string, concurrencyCap int) error {
	dirs, err := p.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      runID,
		Kinds:      plannerDirectiveKinds,
		ActiveOnly: true,
	})
	if err != nil {
		return err
	}
	for i := range dirs {
		d := dirs[i]
		var recErr error
		switch d.Kind {
		case store.DirectiveRecipeEdit:
			recErr = p.handleRecipeEditDirective(ctx, runID, d, concurrencyCap)
		case store.DirectivePlanImport:
			recErr = p.handlePlanImportDirective(ctx, runID, d)
		}
		if recErr != nil {
			if ctx.Err() != nil {
				return recErr
			}
			p.logger.Warn("planner directive reconciliation failed; revoking directive",
				"run_id", runID,
				"directive_id", d.ID,
				"kind", d.Kind,
				"error", recErr,
			)
			_ = p.store.Directives().UpdateStatus(ctx, runID, d.ID, store.DirectiveRevoked)
		}
	}
	return nil
}

func (p *Planner) handleRecipeEditDirective(ctx context.Context, runID string, d store.Directive, concurrencyCap int) error {
	targetName := cmp.Or(strings.TrimSpace(d.Ref), recipe.DefaultName)
	base, err := p.store.Recipes().Get(ctx, runID, targetName, 0)
	if err != nil {
		def := recipe.Default()
		def.RunID = runID
		base = &def
	}
	if _, err := p.CompileAndSaveRecipe(ctx, runID, targetName, d.Text, base, concurrencyCap); err != nil {
		return err
	}
	return p.store.Directives().UpdateStatus(ctx, runID, d.ID, store.DirectiveHandled)
}

func (p *Planner) handlePlanImportDirective(ctx context.Context, runID string, d store.Directive) error {
	planText := strings.TrimSpace(d.Text)
	if planText == "" {
		return fmt.Errorf("%w: plan_import directive %q has empty text", store.ErrInvalidArgument, d.ID)
	}
	if err := p.runReconciliation(ctx, runID, ToolPlannerImport, "Import plan "+cmp.Or(d.Ref, d.ID), planText, d.Ref); err != nil {
		return err
	}
	return p.store.Directives().UpdateStatus(ctx, runID, d.ID, store.DirectiveHandled)
}

// HandlePlannerRequest implements dispatcher.Config.OnPlannerRequest by executing
// a fresh Planner reconciliation turn.
func (p *Planner) HandlePlannerRequest(ctx context.Context, runID, reason, planRef string) error {
	if trimmedRef := strings.TrimSpace(planRef); trimmedRef != "" {
		// Allow Nebula post references or a confined plan file recorded on an adopted run's Objective.
		kind, target := planreader.ParsePlanRef(trimmedRef)
		allowed := kind == planreader.PlanSourceNebulaPost
		if !allowed && p.store != nil && filepath.IsAbs(target) && filepath.Clean(target) == target {
			if run, err := p.store.Runs().Get(ctx, runID); err == nil && run != nil && planreader.IsAdoptionMarker(run.PlanRef) {
				if planreader.MatchesObjectivePlanRef(run.Objective, trimmedRef) {
					allowed = true
				}
			}
		}
		if !allowed {
			return errors.New("orca: planner request requires a post reference")
		}
		doc, err := p.planReader.ReadPlan(ctx, trimmedRef)
		if err != nil {
			return fmt.Errorf("load plan: %w", err)
		}
		if strings.TrimSpace(doc.Content) == "" {
			return fmt.Errorf("plan %q is empty", trimmedRef)
		}
		reason += "\n\nReferenced plan:\n" + doc.Content
	}
	return p.runReconciliation(ctx, runID, ToolPlannerReplan, "Replan: "+reason, reason, planRef)
}

func (p *Planner) runReconciliation(
	ctx context.Context,
	runID, toolName, goal, inputText, planRef string,
) (retErr error) {
	start := p.clock.Now()
	threadID, turnID, err := p.startPlannerTurn(ctx, runID, goal, inputText)
	if err != nil {
		return err
	}
	usage := model.Usage{}
	plannerUsage := model.Usage{}
	defer func() {
		if retErr != nil {
			_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusFailed, usage, "", retErr)
		}
	}()
	var reconciled PlanReconciliation

	if p.agentBackend != model.AgentBackendCLI && p.client == nil {
		return errors.New("orca: planning requires a model client")
	}
	memories, err := p.store.Memories().List(ctx, store.MemoryFilter{RunID: runID})
	if err != nil {
		return err
	}
	memoryContext, err := brief.Render("", memories, 0)
	if err != nil {
		return err
	}
	snapshot, err := dispatcher.BuildSnapshot(ctx, p.store, p.clock, runID, nil, dispatcher.SnapshotConfig{})
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("%s\nPlan Ref: %s\n\nPlan / Request:\n%s", snapshot.Text, planRef, inputText)
	req := model.Request{
		Model: p.modelName, System: PlannerSystemPrompt + memoryContext,
		Messages:  []model.Message{model.NewUserMessage(prompt)},
		Tools:     []model.ToolDefinition{planTool(SubmitPlanReconcileTool), planTool(SubmitOpenPlanTool)},
		MaxTokens: 16384,
	}
	draft, open, draftUsage, err := p.proposePlan(ctx, runID, threadID, turnID, req, true)
	usage = usage.Add(draftUsage)
	plannerUsage = plannerUsage.Add(draftUsage)
	if err != nil {
		return err
	}
	reconciled = draft
	// Startup goals must receive the same independent critic pass as open plans,
	// even when the planner classified the request as a closed reconciliation.
	// This keeps the pre-spawn safety check visible in the decision log.
	if open || toolName == ToolPlannerStartup {
		raw, _ := json.Marshal(draft)
		criticStart := p.clock.Now()
		var critique *model.Response
		var err error
		if p.critic == nil {
			err = errors.New("critic client unavailable")
		} else {
			critique, err = p.critic.CreateMessage(ctx, model.Request{
				Model: p.criticModel, System: CriticSystemPrompt + memoryContext,
				Messages: []model.Message{model.NewUserMessage(prompt + "\n\nPlanner draft:\n" + string(raw))}, MaxTokens: model.DefaultMaxTokens,
			})
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			message := "critic unavailable: " + err.Error()
			if _, recordErr := p.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
				RunID: runID, Category: store.DecisionJudgment, Actor: "critic", ThreadID: threadID, TurnID: turnID,
				Model: p.criticModel, Provider: p.criticProvider, Tool: "planner.critique", Reason: message, Result: message,
				Vetted: false, LatencyMs: max(p.clock.Now().Sub(criticStart).Milliseconds(), 0),
			}); recordErr != nil {
				return recordErr
			}
		} else {
			if critique == nil || strings.TrimSpace(critique.Text()) == "" {
				err = errors.New("critic returned no critique")
				message := "critic unavailable: " + err.Error()
				if _, recordErr := p.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
					RunID: runID, Category: store.DecisionJudgment, Actor: "critic", ThreadID: threadID, TurnID: turnID,
					Model: p.criticModel, Provider: p.criticProvider, Tool: "planner.critique", Reason: message, Result: message,
					Vetted: false, LatencyMs: max(p.clock.Now().Sub(criticStart).Milliseconds(), 0),
				}); recordErr != nil {
					return recordErr
				}
			} else {
				usage = usage.Add(critique.Usage)
				if _, err := p.store.Decisions().Append(ctx, store.Decision{
					RunID: runID, Category: store.DecisionJudgment, Actor: "critic", ThreadID: threadID, TurnID: turnID,
					Model: p.criticModel, Provider: p.criticProvider,
					Tool: "planner.critique", Reason: "challenge open approach before spawning", Result: critique.Text(), Vetted: true,
					InputTokens: critique.Usage.InputTokens, OutputTokens: critique.Usage.OutputTokens, CostUSD: critique.Usage.CostUSD,
					LatencyMs: max(p.clock.Now().Sub(criticStart).Milliseconds(), 0),
				}); err != nil {
					return err
				}
				req.Messages = []model.Message{model.NewUserMessage(prompt + "\n\nYour first draft:\n" + string(raw) + "\n\nCritic feedback:\n" + critique.Text() + "\n\nRevise once and submit the final plan using submit_plan_reconciliation.")}
				req.Tools = []model.ToolDefinition{planTool(SubmitPlanReconcileTool)}
				revised, _, revisedUsage, err := p.proposePlan(ctx, runID, threadID, turnID, req, false)
				usage = usage.Add(revisedUsage)
				plannerUsage = plannerUsage.Add(revisedUsage)
				if err != nil {
					return fmt.Errorf("planner revision: %w", err)
				}
				reconciled = revised
			}
		}
	}
	appliedSummary, err := p.applyReconciliation(ctx, p.store, runID, reconciled, planRef)
	if err != nil {
		return err
	}
	_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusCompleted, usage, appliedSummary, nil)
	return p.recordDecision(ctx, runID, threadID, turnID, toolName, goal, appliedSummary, plannerUsage, p.clock.Now().Sub(start))
}

func planTool(name string) model.ToolDefinition {
	return model.ToolDefinition{Name: name, Description: "Submit the approach in summary, objective, backlog diff and conflicts", InputSchema: slices.Clone(submitPlanReconcileSchema)}
}

func (p *Planner) proposePlan(ctx context.Context, runID, threadID, turnID string, req model.Request, allowOpen bool) (PlanReconciliation, bool, model.Usage, error) {
	if p.agentBackend == model.AgentBackendCLI {
		return p.proposePlanCLI(ctx, runID, threadID, turnID, req, allowOpen)
	}
	resp, err := p.client.CreateMessage(ctx, req)
	if err != nil {
		return PlanReconciliation{}, false, model.Usage{}, err
	}
	if resp == nil {
		return PlanReconciliation{}, false, model.Usage{}, errors.New("orca: planner returned no response")
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || (calls[0].Name != SubmitPlanReconcileTool && !(allowOpen && calls[0].Name == SubmitOpenPlanTool)) {
		return PlanReconciliation{}, false, resp.Usage, errors.New("orca: planner must submit exactly one plan")
	}
	var rec PlanReconciliation
	if err := json.Unmarshal(calls[0].Input, &rec); err != nil {
		return rec, false, resp.Usage, fmt.Errorf("decode planner submission: %w", err)
	}
	return rec, calls[0].Name == SubmitOpenPlanTool, resp.Usage, nil
}

func (p *Planner) proposePlanCLI(ctx context.Context, runID, threadID, turnID string, req model.Request, allowOpen bool) (PlanReconciliation, bool, model.Usage, error) {
	reg := model.NewToolRegistry()
	var (
		submitted     bool
		submittedOpen bool
		submittedRec  PlanReconciliation
	)
	registerSubmit := func(name string, isOpen bool) {
		reg.Register(planTool(name), func(_ context.Context, raw json.RawMessage) (string, error) {
			if submitted {
				return "plan submission already recorded", nil
			}
			var rec PlanReconciliation
			if err := json.Unmarshal(raw, &rec); err != nil {
				return "", fmt.Errorf("decode %s: %w", name, err)
			}
			submitted = true
			submittedOpen = isOpen
			submittedRec = rec
			return "plan submission recorded", nil
		})
	}
	registerSubmit(SubmitPlanReconcileTool, false)
	if allowOpen {
		registerSubmit(SubmitOpenPlanTool, true)
	}
	model.RegisterInvestigationTools(reg, p.investigate)

	userPrompt := ""
	if len(req.Messages) > 0 {
		userPrompt = req.Messages[len(req.Messages)-1].Text()
	}

	var threads store.ThreadStore
	if p.store != nil {
		threads = p.store.Threads()
	}
	turnRes, err := store.RunRoleAgentTurn(
		ctx,
		threads,
		&p.sessions,
		runID,
		threadID,
		turnID,
		ActorPlanner,
		model.AgentSessionConfig{
			Provider:      p.providerName,
			Model:         p.modelName,
			Effort:        p.effort,
			SystemPrompt:  req.System,
			LLMGatewayURL: p.llmGatewayURL,
			ClaudeBin:     p.claudeBin,
			CodexBin:      p.codexBin,
		},
		model.AgentTurnRequest{
			Prompt:       userPrompt,
			Model:        p.modelName,
			SystemPrompt: req.System,
			Registry:     reg,
			MaxToolCalls: 0,
		},
		p.logger,
	)
	if err != nil {
		return PlanReconciliation{}, false, model.Usage{}, err
	}
	var usage model.Usage
	if turnRes != nil {
		usage = turnRes.Usage
	}
	if !submitted {
		return PlanReconciliation{}, false, usage, errors.New("orca: planner must submit exactly one plan")
	}
	return submittedRec, submittedOpen, usage, nil
}

func (p *Planner) applyReconciliation(
	ctx context.Context,
	st store.Store,
	runID string,
	rec PlanReconciliation,
	fallbackPlanRef string,
) (string, error) {
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}
	newObj, newRef := planreader.PreserveAdoptedObjective(
		*run,
		rec.Objective,
		cmp.Or(strings.TrimSpace(rec.PlanRef), strings.TrimSpace(fallbackPlanRef)),
	)
	if newObj != run.Objective || newRef != run.PlanRef {
		if err := st.Runs().UpdateObjective(ctx, runID, newObj, newRef); err != nil {
			return "", err
		}
	}

	added := 0
	for i, spec := range rec.AddItems {
		id := cmp.Or(strings.TrimSpace(spec.ID), p.nextID("plan-item"))
		prio := store.Priority(strings.ToLower(strings.TrimSpace(spec.Priority)))
		if !prio.Valid() {
			prio = store.PriorityP2
		}
		item := store.BacklogItem{
			ID:          id,
			RunID:       runID,
			Title:       cmp.Or(strings.TrimSpace(spec.Title), id),
			Description: strings.TrimSpace(spec.Description),
			Acceptance:  strings.TrimSpace(spec.Acceptance),
			Priority:    prio,
			Status:      store.BacklogOpen,
			RecipeName:  strings.TrimSpace(spec.RecipeName),
			DependsOn:   slices.Clone(spec.DependsOn),
			Order:       i + 1,
		}
		if err := st.Backlog().Add(ctx, item); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return "", err
		}
		added++
	}

	updated := 0
	for _, spec := range rec.UpdateItems {
		existing, getErr := st.Backlog().Get(ctx, runID, strings.TrimSpace(spec.ID))
		if getErr != nil {
			continue
		}
		cp := existing.Clone()
		if strings.TrimSpace(spec.Title) != "" {
			cp.Title = strings.TrimSpace(spec.Title)
		}
		if strings.TrimSpace(spec.Description) != "" {
			cp.Description = strings.TrimSpace(spec.Description)
		}
		if strings.TrimSpace(spec.Acceptance) != "" {
			cp.Acceptance = strings.TrimSpace(spec.Acceptance)
		}
		if prio := store.Priority(strings.ToLower(strings.TrimSpace(spec.Priority))); prio.Valid() {
			cp.Priority = prio
		}
		if spec.DependsOn != nil {
			cp.DependsOn = slices.Clone(spec.DependsOn)
		}
		if err := st.Backlog().Update(ctx, cp, ActorPlanner, "plan import update"); err != nil {
			return "", err
		}
		updated++
	}

	dropped := 0
	for _, itemID := range rec.DropItemIDs {
		itemID = strings.TrimSpace(itemID)
		if itemID == "" {
			continue
		}
		if err := st.Backlog().Close(ctx, runID, itemID, store.BacklogDropped, ActorPlanner, "dropped by plan import"); err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		dropped++
	}

	asksCreated := 0
	now := p.clock.Now().UTC()
	for _, c := range rec.Conflicts {
		askID := cmp.Or(strings.TrimSpace(c.ID), p.nextID("plan-conflict"))
		opts := slices.Clone(c.Options)
		defOpt := strings.TrimSpace(c.Default)
		if len(opts) == 0 {
			defOpt = cmp.Or(defOpt, "keep_existing")
			opts = []string{defOpt, "apply_override"}
		} else if defOpt == "" || !slices.Contains(opts, defOpt) {
			defOpt = opts[0]
		}
		ask := controller.OwnerAsk{
			ID:       askID,
			RunID:    runID,
			Question: cmp.Or(strings.TrimSpace(c.Question), "Resolve plan conflict"),
			Options:  opts,
			Default:  defOpt,
			Deadline: now.Add(DefaultConflictAskDeadline),
		}
		raw, _ := json.Marshal(ask)
		dl := ask.Deadline
		dir := store.Directive{
			ID:        "ask-" + ask.ID,
			RunID:     runID,
			Kind:      store.DirectiveInstruction,
			Source:    store.DirectiveSourcePlanner,
			Ref:       ownerAskRefPrefix + ask.ID,
			Text:      string(raw),
			ExpiresAt: &dl,
			Status:    store.DirectiveActive,
		}
		if err := st.Directives().Add(ctx, dir); err == nil {
			asksCreated++
		}
	}

	return fmt.Sprintf("reconciled plan (added=%d updated=%d dropped=%d conflicts=%d)",
		added, updated, dropped, asksCreated), nil
}

func (p *Planner) startPlannerTurn(ctx context.Context, runID, goal, input string) (string, string, error) {
	if p.agentBackend == model.AgentBackendCLI {
		threadID := "planner-" + runID
		th, err := p.store.Threads().GetThread(ctx, threadID)
		if err != nil || th == nil {
			started, sErr := p.store.Threads().StartThread(ctx, store.Thread{
				ID:     threadID,
				RunID:  runID,
				Role:   store.ThreadRolePlanner,
				Goal:   goal,
				Status: store.ThreadStatusIdle,
			})
			if sErr != nil {
				return "", "", sErr
			}
			th = &started
		} else if th.Status == store.ThreadStatusArchived {
			_ = p.store.Threads().UpdateThreadStatus(ctx, threadID, store.ThreadStatusIdle)
		}
		turnID := p.nextID(th.ID + "-turn")
		if _, err := p.store.Threads().StartTurn(ctx, store.Turn{
			ID:       turnID,
			ThreadID: th.ID,
			RunID:    runID,
			Input:    input,
		}); err != nil {
			return "", "", err
		}
		_, _ = p.store.Threads().AppendItem(ctx, store.Item{
			ThreadID: th.ID,
			TurnID:   turnID,
			Kind:     store.ItemKindUserMessage,
			Content:  input,
		})
		return th.ID, turnID, nil
	}
	threadID := p.nextID("planner-" + runID)
	th, err := p.store.Threads().StartThread(ctx, store.Thread{
		ID:     threadID,
		RunID:  runID,
		Role:   store.ThreadRolePlanner,
		Goal:   goal,
		Status: store.ThreadStatusIdle,
	})
	if err != nil {
		return "", "", err
	}
	turnID := p.nextID(th.ID + "-turn")
	if _, err := p.store.Threads().StartTurn(ctx, store.Turn{
		ID:       turnID,
		ThreadID: th.ID,
		RunID:    runID,
		Input:    input,
	}); err != nil {
		return "", "", err
	}
	_, _ = p.store.Threads().AppendItem(ctx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnID,
		Kind:     store.ItemKindUserMessage,
		Content:  input,
	})
	return th.ID, turnID, nil
}

func (p *Planner) completePlannerTurn(
	ctx context.Context,
	threadID, turnID string,
	status store.TurnStatus,
	usage model.Usage,
	output string,
	turnErr error,
) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if output != "" {
		_, _ = p.store.Threads().AppendItem(cleanupCtx, store.Item{
			ThreadID: threadID,
			TurnID:   turnID,
			Kind:     store.ItemKindAssistantMessage,
			Content:  output,
		})
	}
	errMsg := ""
	if turnErr != nil {
		errMsg = turnErr.Error()
	}
	_ = p.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, status, usage, errMsg)
	if p.agentBackend == model.AgentBackendCLI {
		return nil
	}
	return p.store.Threads().UpdateThreadStatus(cleanupCtx, threadID, store.ThreadStatusArchived)
}

func (p *Planner) recordDecision(
	ctx context.Context,
	runID, threadID, turnID, tool, reason, result string,
	usage model.Usage,
	elapsed time.Duration,
) error {
	if strings.TrimSpace(runID) == "" || p.store == nil {
		return nil
	}
	_, err := p.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
		RunID:    runID,
		Category: store.DecisionJudgment,
		Actor:    ActorPlanner,
		Model:    p.modelName, Provider: p.providerName,
		ThreadID:     threadID,
		TurnID:       turnID,
		Tool:         tool,
		Reason:       reason,
		Result:       result,
		Vetted:       true,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      usage.CostUSD,
		LatencyMs:    max(elapsed.Milliseconds(), 0),
	})
	return err
}

// AskStream answers a complex design, research, trade-off, or plan/constraint
// question in chat using the strongest model, grounded in the run's contract,
// plan, and lane reports, and streams its conversational answer via emit.
func (p *Planner) AskStream(
	ctx context.Context,
	st store.Store,
	runID, question, findings string,
	hostRun func(string) (adopt.Run, error),
	emit func(string),
) (string, error) {
	return p.askStream(ctx, st, runID, question, findings, p.investigate, hostRun, emit)
}

func (p *Planner) askStream(
	ctx context.Context,
	st store.Store,
	runID, question, findings string,
	icfg model.InvestigateConfig,
	hostRun func(string) (adopt.Run, error),
	emit func(string),
) (string, error) {
	if st == nil {
		st = p.store
	}
	if st == nil {
		return "", errors.New("orca: planner requires a store")
	}
	if p.agentBackend != model.AgentBackendCLI && p.client == nil {
		return "", chat.ErrPlannerFallbackToQueue
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return "", errors.New("orca: planner chat requires a run ID")
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return "", nil
	}
	start := p.clock.Now()

	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}

	promptText, effectiveRef, err := p.buildChatPromptForRun(ctx, st, *run, question, findings, hostRun)
	if err != nil {
		return "", err
	}

	threadID := p.nextID("planner-" + runID)
	th, err := st.Threads().StartThread(ctx, store.Thread{
		ID:     threadID,
		RunID:  runID,
		Role:   store.ThreadRolePlanner,
		Goal:   "Planner chat: " + question,
		Status: store.ThreadStatusIdle,
	})
	if err != nil {
		return "", err
	}
	turnID := p.nextID(th.ID + "-turn")
	if _, err := st.Threads().StartTurn(ctx, store.Turn{
		ID:       turnID,
		ThreadID: th.ID,
		RunID:    runID,
		Input:    question,
	}); err != nil {
		return "", err
	}
	cleanupCtx := context.WithoutCancel(ctx)
	_, _ = st.Threads().AppendItem(cleanupCtx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnID,
		Kind:     store.ItemKindUserMessage,
		Content:  question,
	})
	_, _ = st.Threads().AppendItem(cleanupCtx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnID,
		Kind:     store.ItemKindSnapshot,
		Content:  promptText,
	})

	recordProposal := func(callCtx context.Context, pw planreader.ProposeWorkParams) (controller.OwnerAsk, error) {
		opts := slices.Clone(pw.Options)
		defOpt := strings.TrimSpace(pw.Default)
		if len(opts) == 0 {
			opts = []string{"approve_proposal", "reject_proposal"}
			if defOpt == "" {
				defOpt = "approve_proposal"
			} else if !slices.Contains(opts, defOpt) {
				opts = append([]string{defOpt}, opts...)
			}
		} else if defOpt == "" || !slices.Contains(opts, defOpt) {
			defOpt = opts[0]
		}
		now := p.clock.Now().UTC()
		askID := cmp.Or(strings.TrimSpace(pw.ID), p.nextID("proposal"))
		proposalDetails := strings.TrimSpace(pw.Description)
		question := cmp.Or(strings.TrimSpace(pw.Question), proposalDetails, "Approve proposed work")
		if proposalDetails != "" && proposalDetails != question {
			question += "\n\nProposal details (held for owner approval):\n" + proposalDetails
		}
		ask := controller.OwnerAsk{
			ID:       askID,
			RunID:    runID,
			LaneID:   strings.TrimSpace(pw.LaneID),
			Question: question,
			Options:  opts,
			Default:  defOpt,
			Deadline: now.Add(DefaultConflictAskDeadline),
		}
		if err := ask.Validate(); err != nil {
			return controller.OwnerAsk{}, err
		}
		raw, _ := json.Marshal(ask)
		dl := ask.Deadline
		dir := store.Directive{
			ID:        "ask-" + ask.ID,
			RunID:     runID,
			Kind:      store.DirectiveInstruction,
			Source:    store.DirectiveSourcePlanner,
			Scope:     ask.LaneID,
			Ref:       ownerAskRefPrefix + ask.ID,
			Text:      string(raw),
			ExpiresAt: &dl,
			Status:    store.DirectiveActive,
			CreatedAt: now,
		}
		if err := st.Directives().Add(callCtx, dir); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return controller.OwnerAsk{}, err
		}
		return ask, nil
	}

	reg := p.buildChatToolRegistryForRun(st, *run, effectiveRef, icfg, hostRun, func(callCtx context.Context, rec PlanReconciliation) (string, error) {
		if len(rec.AddItems) == 0 && len(rec.UpdateItems) == 0 && len(rec.DropItemIDs) == 0 && strings.TrimSpace(rec.Objective) == "" && strings.TrimSpace(rec.PlanRef) == "" && len(rec.Conflicts) == 0 && len(rec.UnmappedPhrases) == 0 {
			return cmp.Or(strings.TrimSpace(rec.Summary), "No plan changes proposed"), nil
		}
		propQ := strings.TrimSpace(rec.Summary)
		if propQ == "" && len(rec.AddItems) > 0 {
			var titles []string
			for i := range rec.AddItems {
				titles = append(titles, rec.AddItems[i].Title)
			}
			propQ = "Approve proposed backlog work: " + strings.Join(titles, "; ")
		}
		if propQ == "" {
			propQ = "Approve proposed Planner changes"
		}
		rawRec, err := json.Marshal(rec)
		if err != nil {
			return "", err
		}
		ask, err := recordProposal(callCtx, planreader.ProposeWorkParams{Question: propQ, Description: propQ + "\n\nStructured reconciliation (held for owner approval):\n" + string(rawRec)})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Recorded proposal %s for owner approval; no plan changes were applied", ask.ID), nil
	}, recordProposal)

	ctx, stream := withChatStream(ctx, emit)
	emitFn := stream.complete

	var (
		usage   model.Usage
		answer  string
		loopErr error
		emitted bool
	)

	if p.agentBackend == model.AgentBackendCLI {
		if hostRun != nil {
			if hr, hrErr := hostRun(runID); hrErr == nil && strings.TrimSpace(hr.Dir) != "" {
				icfg.RunDir = hr.Dir
				icfg.RepoDir = model.ResolveRunRepoDir(icfg.RepoDir, hr.Dir)
			}
		}
		turnRes, tErr := store.RunRoleAgentTurn(
			ctx,
			st.Threads(),
			&p.sessions,
			runID,
			th.ID,
			turnID,
			ActorPlanner,
			model.AgentSessionConfig{
				Provider:      p.providerName,
				Model:         p.modelName,
				Effort:        p.effort,
				SystemPrompt:  planreader.ChatSystemPrompt,
				LLMGatewayURL: p.llmGatewayURL,
				ClaudeBin:     p.claudeBin,
				CodexBin:      p.codexBin,
			},
			model.AgentTurnRequest{
				Prompt:       promptText,
				Model:        p.modelName,
				SystemPrompt: planreader.ChatSystemPrompt,
				Registry:     reg,
				MaxToolCalls: 0,
				Observer: func(name string, args json.RawMessage, _ string, _ bool) {
					chat.EmitToolCall(ctx, name, args, runID)
				},
			},
			p.logger,
		)
		loopErr = tErr
		if turnRes != nil {
			usage = turnRes.Usage
			answer = strings.TrimSpace(turnRes.Text)
		}
	} else {
		req := model.Request{
			Model:     p.modelName,
			System:    planreader.ChatSystemPrompt,
			Messages:  []model.Message{model.NewUserMessage(promptText)},
			Tools:     reg.Definitions(),
			MaxTokens: model.DefaultMaxTokens,
		}
		loopRes, lErr := model.RunToolLoop(
			ctx,
			p.client,
			req,
			reg,
			model.WithMaxTurns(model.DefaultMaxTurns),
			model.WithToolCallLog(p.logger, ActorPlanner),
			model.WithOnTurn(func(_ int, resp model.Response) {
				if txt := strings.TrimSpace(resp.Text()); txt != "" {
					answer = txt
					if len(resp.ToolCalls()) == 0 && emitFn != nil {
						emitFn(txt)
						emitted = true
					}
					_, _ = st.Threads().AppendItem(cleanupCtx, store.Item{
						ThreadID: th.ID,
						TurnID:   turnID,
						Kind:     store.ItemKindAssistantMessage,
						Content:  txt,
					})
				}
			}),
			model.WithOnToolCall(func(_ int, call model.ToolCall, resBlock model.ContentBlock) {
				chat.EmitToolCall(ctx, call.Name, call.Input, runID)
				_, _ = st.Threads().AppendItem(cleanupCtx, store.Item{
					ThreadID: th.ID,
					TurnID:   turnID,
					Kind:     store.ItemKindToolCall,
					Name:     call.Name,
					CallID:   call.ID,
					Payload:  call.Input,
				})
				_, _ = st.Threads().AppendItem(cleanupCtx, store.Item{
					ThreadID: th.ID,
					TurnID:   turnID,
					Kind:     store.ItemKindToolResult,
					Name:     call.Name,
					CallID:   call.ID,
					Content:  resBlock.Content,
					IsError:  resBlock.IsError,
				})
			}),
		)
		if lErr != nil && errors.Is(lErr, model.ErrFakeClientExhausted) && (loopRes == nil || loopRes.TurnCount == 0) {
			_ = st.Threads().CompleteTurn(cleanupCtx, th.ID, turnID, store.TurnStatusInterrupted, model.Usage{}, "")
			_ = st.Threads().UpdateThreadStatus(cleanupCtx, th.ID, store.ThreadStatusArchived)
			return "", chat.ErrPlannerFallbackToQueue
		}
		loopErr = lErr
		if loopRes != nil {
			usage = loopRes.Usage
			if finalTxt := strings.TrimSpace(loopRes.Response.Text()); finalTxt != "" {
				answer = finalTxt
			}
		}
	}

	if loopErr == nil && emitFn != nil && !emitted && answer != "" {
		emitFn(answer)
		_, _ = st.Threads().AppendItem(cleanupCtx, store.Item{
			ThreadID: th.ID,
			TurnID:   turnID,
			Kind:     store.ItemKindAssistantMessage,
			Content:  answer,
		})
	}
	if usage.CostUSD == 0 && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
		usage.CostUSD = store.EstimateUsageCostUSD(cmp.Or(p.modelName, ActorPlanner), usage.InputTokens, usage.OutputTokens)
	}
	if loopErr != nil {
		_ = st.Threads().CompleteTurn(cleanupCtx, th.ID, turnID, store.TurnStatusFailed, usage, loopErr.Error())
		if p.agentBackend != model.AgentBackendCLI {
			_ = st.Threads().UpdateThreadStatus(cleanupCtx, th.ID, store.ThreadStatusArchived)
		}
		return answer, loopErr
	}
	_ = st.Threads().CompleteTurn(cleanupCtx, th.ID, turnID, store.TurnStatusCompleted, usage, "")
	if p.agentBackend != model.AgentBackendCLI {
		_ = st.Threads().UpdateThreadStatus(cleanupCtx, th.ID, store.ThreadStatusArchived)
	}
	_, _ = st.Decisions().Append(cleanupCtx, store.Decision{
		RunID:        runID,
		Category:     store.DecisionJudgment,
		Actor:        ActorPlanner,
		Model:        p.modelName,
		Provider:     p.providerName,
		ThreadID:     th.ID,
		TurnID:       turnID,
		Tool:         ToolPlannerChat,
		Reason:       question,
		Result:       answer,
		Vetted:       true,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      usage.CostUSD,
		LatencyMs:    max(p.clock.Now().Sub(start).Milliseconds(), 0),
	})
	return answer, nil
}

func (p *Planner) buildChatPromptForRun(
	ctx context.Context,
	st store.Store,
	run store.Run,
	question, findings string,
	hostRun func(string) (adopt.Run, error),
) (string, string, error) {
	directives, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: run.ID, ActiveOnly: true})
	if err != nil {
		return "", "", err
	}
	backlogItems, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: run.ID})
	if err != nil {
		return "", "", err
	}
	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: run.ID})
	if err != nil {
		return "", "", err
	}
	memories, err := st.Memories().List(ctx, store.MemoryFilter{RunID: run.ID})
	if err != nil {
		return "", "", err
	}
	memoryContext, err := brief.Render("", memories, 0)
	if err != nil {
		return "", "", err
	}

	effectiveRef := strings.TrimSpace(run.PlanRef)
	if planreader.IsAdoptionMarker(effectiveRef) {
		effectiveRef = planreader.ExtractObjectivePlanRef(run.Objective)
	}
	var planDoc *planreader.PlanDocument
	if effectiveRef != "" && p.planReader != nil {
		if doc, readErr := p.planReader.ReadPlan(ctx, effectiveRef); readErr == nil {
			planDoc = &doc
		}
	}

	reports := collectHostAndEventLaneReports(ctx, st, run.ID, hostRun)

	var b strings.Builder
	fmt.Fprintf(&b, "# Planner Chat Context for Run `%s`\n", run.ID)
	b.WriteString("\n## Run Contract\n")
	var contractDirs []store.Directive
	for i := range directives {
		if directives[i].IsContract() || directives[i].Kind == store.DirectiveStanding {
			contractDirs = append(contractDirs, directives[i])
		}
	}
	if len(contractDirs) == 0 {
		b.WriteString("Contract & Standing Directives: (none)\n")
	} else {
		for i := range contractDirs {
			d := &contractDirs[i]
			fmt.Fprintf(&b, "- [%s] kind=%s scope=%s ref=%s: %s\n", d.ID, d.Kind, cmp.Or(d.Scope, "run"), cmp.Or(d.Ref, "-"), d.Text)
		}
	}
	if strings.TrimSpace(memoryContext) != "" {
		b.WriteString(memoryContext)
	}

	b.WriteString("\n## Run Plan\n")
	fmt.Fprintf(&b, "Current Objective: %s\n", cmp.Or(strings.TrimSpace(run.Objective), "(none)"))
	fmt.Fprintf(&b, "Current PlanRef: %s\n", cmp.Or(strings.TrimSpace(run.PlanRef), "(none)"))
	if planDoc != nil {
		fmt.Fprintf(&b, "\n### Referenced Plan (`%s`)\n%s\n", planDoc.Ref, planDoc.Content)
	}
	fmt.Fprintf(&b, "\n### Current Backlog (%d)\n", len(backlogItems))
	if len(backlogItems) == 0 {
		b.WriteString("(empty)\n")
	} else {
		for i := range backlogItems {
			it := &backlogItems[i]
			fmt.Fprintf(&b, "- %s [%s #%d %s] lane=%s title=%q acceptance=%q\n",
				it.ID, it.Priority, it.Order, it.Status, cmp.Or(it.LaneID, "-"), it.Title, it.Acceptance)
		}
	}

	fmt.Fprintf(&b, "\n## Lane Reports (%d lanes)\n", len(lanes))
	if len(lanes) == 0 && len(reports) == 0 {
		b.WriteString("(none)\n")
	} else {
		seen := map[string]bool{}
		for i := range lanes {
			ln := &lanes[i]
			seen[ln.ID] = true
			fmt.Fprintf(&b, "- %s: item=%s status=%s phase=%s round=%d pr=#%d checks=%s\n",
				ln.ID, cmp.Or(ln.ItemID, "-"), ln.Status, cmp.Or(ln.Phase, "-"), ln.Round, ln.PR, cmp.Or(ln.Checks, "-"))
			if rep := strings.TrimSpace(reports[ln.ID]); rep != "" {
				for _, line := range strings.Split(rep, "\n") {
					fmt.Fprintf(&b, "  %s\n", line)
				}
			}
		}
		for laneID, rep := range reports {
			if seen[laneID] || strings.TrimSpace(rep) == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s:\n  %s\n", laneID, strings.TrimSpace(rep))
		}
	}

	if strings.TrimSpace(findings) != "" {
		b.WriteString("\n## Concierge Partial Findings\n")
		b.WriteString(strings.TrimSpace(findings))
		b.WriteString("\n")
	}

	b.WriteString("\n## Owner Question\n")
	b.WriteString(strings.TrimSpace(question))
	b.WriteString("\n")

	return b.String(), effectiveRef, nil
}

func collectHostAndEventLaneReports(ctx context.Context, st store.Store, runID string, hostRun func(string) (adopt.Run, error)) map[string]string {
	out := map[string]string{}
	if hostRun != nil {
		if hr, err := hostRun(runID); err == nil {
			for i := range hr.Lanes {
				hl := &hr.Lanes[i]
				if strings.TrimSpace(hl.ReportPath) != "" {
					if raw, rErr := os.ReadFile(hl.ReportPath); rErr == nil && strings.TrimSpace(string(raw)) != "" {
						out[hl.ID] = "Report (" + hl.ReportPath + "):\n" + strings.TrimSpace(string(raw))
						continue
					}
				}
				if strings.TrimSpace(hl.Report) != "" {
					out[hl.ID] = "Report: " + strings.TrimSpace(hl.Report)
				}
			}
		}
	}
	if st != nil && st.Events() != nil {
		if evs, err := st.Events().List(ctx, store.EventFilter{
			RunID: runID,
			Kinds: []store.EventKind{store.EventLaneReport},
		}); err == nil {
			for i := range evs {
				ev := &evs[i]
				laneID := cmp.Or(strings.TrimSpace(ev.LaneID), "run")
				var rep lane.PhaseReport
				if len(ev.Payload) > 0 && json.Unmarshal(ev.Payload, &rep) == nil && (rep.Outcome != "" || rep.Summary != "" || rep.Raw != "") {
					if rep.LaneID != "" {
						laneID = rep.LaneID
					}
					out[laneID] = fmt.Sprintf("Report: phase=%s round=%d outcome=%s summary=%q observed=%q riskiest=%q",
						cmp.Or(rep.Phase, "-"), rep.Round, cmp.Or(string(rep.Outcome), "-"), rep.Summary, rep.Observed, rep.RiskiestStep)
					continue
				}
				if text := cmp.Or(strings.TrimSpace(ev.Summary), strings.TrimSpace(string(ev.Payload))); text != "" {
					out[laneID] = "Report: " + text
				}
			}
		}
	}
	return out
}

func (p *Planner) buildChatToolRegistryForRun(
	st store.Store,
	run store.Run,
	effectiveRef string,
	icfg model.InvestigateConfig,
	hostRun func(string) (adopt.Run, error),
	applyReconcile func(context.Context, PlanReconciliation) (string, error),
	recordProposal func(context.Context, planreader.ProposeWorkParams) (controller.OwnerAsk, error),
) *model.ToolRegistry {
	reg := model.NewToolRegistry()
	runID := run.ID

	model.AddTool[planreader.ChatStatusParams](reg, planreader.ToolStatus,
		"Read current run status, objective, settings, contract/active directives, backlog, and lanes",
		func(callCtx context.Context, _ planreader.ChatStatusParams) (string, error) {
			snap, err := dispatcher.BuildSnapshot(callCtx, st, p.clock, runID, nil, dispatcher.SnapshotConfig{})
			if err != nil {
				return "", err
			}
			return snap.Text, nil
		},
	)

	model.AddTool[planreader.ChatLaneShowParams](reg, planreader.ToolLaneShow,
		"Inspect detailed lane state, latest phase report, and recent decisions for a lane",
		func(callCtx context.Context, lp planreader.ChatLaneShowParams) (string, error) {
			laneID := strings.TrimSpace(lp.LaneID)
			if laneID == "" {
				return "", fmt.Errorf("%w: lane_id is required", store.ErrInvalidArgument)
			}
			ln, err := st.Lanes().Get(callCtx, runID, laneID)
			if err != nil {
				return "", err
			}
			reports := collectHostAndEventLaneReports(callCtx, st, runID, hostRun)
			var b strings.Builder
			fmt.Fprintf(&b, "Lane %s: item=%s status=%s phase=%s round=%d pr=#%d head=%s checks=%s\n",
				ln.ID, cmp.Or(ln.ItemID, "-"), ln.Status, cmp.Or(ln.Phase, "-"), ln.Round, ln.PR, cmp.Or(ln.PRHead, "-"), cmp.Or(ln.Checks, "-"))
			if rep := strings.TrimSpace(reports[ln.ID]); rep != "" {
				b.WriteString(rep + "\n")
			}
			return b.String(), nil
		},
	)

	model.AddTool[planreader.ChatBacklogParams](reg, planreader.ToolBacklog,
		"List backlog items for the run, optionally filtered by status",
		func(callCtx context.Context, bp planreader.ChatBacklogParams) (string, error) {
			filter := store.BacklogFilter{RunID: runID}
			if status := store.BacklogStatus(strings.TrimSpace(bp.Status)); status != "" {
				filter.Statuses = []store.BacklogStatus{status}
			}
			items, err := st.Backlog().List(callCtx, filter)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Backlog (%d):\n", len(items))
			for i := range items {
				it := &items[i]
				fmt.Fprintf(&b, "- %s [%s #%d %s] lane=%s title=%q acceptance=%q\n",
					it.ID, it.Priority, it.Order, it.Status, cmp.Or(it.LaneID, "-"), it.Title, it.Acceptance)
			}
			return b.String(), nil
		},
	)

	model.AddTool[planreader.ChatDecisionsSinceParams](reg, planreader.ToolDecisionsSince,
		"List recent decisions for the run since a sequence number",
		func(callCtx context.Context, dp planreader.ChatDecisionsSinceParams) (string, error) {
			limit := dp.Limit
			if limit <= 0 {
				limit = 20
			}
			decs, err := st.Decisions().List(callCtx, store.DecisionFilter{RunID: runID, SinceSeq: dp.SinceSeq, Limit: limit})
			if err != nil {
				return "", err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Decisions (%d):\n", len(decs))
			for i := range decs {
				d := &decs[i]
				fmt.Fprintf(&b, "- seq=%d actor=%s tool=%s lane=%s reason=%q result=%q\n",
					d.Seq, d.Actor, d.Tool, cmp.Or(d.LaneID, "-"), d.Reason, d.Result)
			}
			return b.String(), nil
		},
	)

	model.AddTool[planreader.ReadPlanParams](reg, planreader.ToolReadPlan,
		"Read a referenced plan document from a file path or Nebula post ID",
		func(callCtx context.Context, rp planreader.ReadPlanParams) (string, error) {
			ref := cmp.Or(strings.TrimSpace(rp.Ref), effectiveRef, strings.TrimSpace(run.PlanRef))
			if ref == "" || p.planReader == nil {
				return "", fmt.Errorf("%w: plan ref is required", store.ErrInvalidArgument)
			}
			doc, err := p.planReader.ReadPlan(callCtx, ref)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Plan Ref: %s (%s)\nTitle: %s\n\n%s", doc.Ref, doc.SourceKind, doc.Title, doc.Content), nil
		},
	)

	for _, toolName := range []string{SubmitPlanReconcileTool, SubmitOpenPlanTool, planreader.ToolSubmitReconciliation} {
		name := toolName
		reg.Register(planTool(name), func(callCtx context.Context, raw json.RawMessage) (string, error) {
			var rec PlanReconciliation
			if err := json.Unmarshal(raw, &rec); err != nil {
				return "", fmt.Errorf("decode %s: %w", name, err)
			}
			return applyReconcile(callCtx, rec)
		})
	}

	model.AddTool[planreader.ProposeWorkParams](reg, ToolPlannerProposeWork,
		"Record proposed work or a design decision proposal in the open-question store for owner approval",
		func(callCtx context.Context, pw planreader.ProposeWorkParams) (string, error) {
			ask, err := recordProposal(callCtx, pw)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Recorded proposal %s in open questions: %s (options: %s; default: %s)",
				ask.ID, ask.Question, strings.Join(ask.Options, " | "), ask.Default), nil
		},
	)

	if hostRun != nil {
		if hr, err := hostRun(runID); err == nil && strings.TrimSpace(hr.Dir) != "" {
			if strings.TrimSpace(icfg.RunDir) == "" {
				icfg.RunDir = hr.Dir
			} else if hr.Dir != icfg.RunDir && !slices.Contains(icfg.ExtraRoots, hr.Dir) {
				icfg.ExtraRoots = append(slices.Clone(icfg.ExtraRoots), hr.Dir)
			}
			icfg.RepoDir = model.ResolveRunRepoDir(icfg.RepoDir, hr.Dir)
		}
	}
	model.RegisterInvestigationTools(reg, icfg)
	return reg
}
