package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/brief"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
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
	criticModel    string
	providerName   string
	criticProvider string
	planReader     planreader.PlanReader
	compiler       *recipe.Compiler
	logger         *slog.Logger
	modelName      string
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
	if strings.TrimSpace(planRef) != "" {
		doc, err := p.planReader.ReadPlan(ctx, planRef)
		if err != nil {
			return fmt.Errorf("load plan: %w", err)
		}
		if strings.TrimSpace(doc.Content) == "" {
			return fmt.Errorf("plan %q is empty", planRef)
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
	defer func() {
		if retErr != nil {
			_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusFailed, usage, "", retErr)
		}
	}()
	var reconciled PlanReconciliation

	if p.client == nil {
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
	draft, open, draftUsage, err := p.proposePlan(ctx, req, true)
	usage = usage.Add(draftUsage)
	if err != nil {
		return err
	}
	reconciled = draft
	if open {
		if p.critic == nil {
			return errors.New("orca: open planning requires a critic client")
		}
		raw, _ := json.Marshal(draft)
		criticStart := p.clock.Now()
		critique, err := p.critic.CreateMessage(ctx, model.Request{
			Model: p.criticModel, System: CriticSystemPrompt + memoryContext,
			Messages: []model.Message{model.NewUserMessage(prompt + "\n\nPlanner draft:\n" + string(raw))}, MaxTokens: model.DefaultMaxTokens,
		})
		if err != nil {
			return fmt.Errorf("critic: %w", err)
		}
		if critique == nil || strings.TrimSpace(critique.Text()) == "" {
			return errors.New("orca: critic returned no critique")
		}
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
		revised, _, revisedUsage, err := p.proposePlan(ctx, req, false)
		usage = usage.Add(revisedUsage)
		if err != nil {
			return fmt.Errorf("planner revision: %w", err)
		}
		reconciled = revised
	}
	appliedSummary, err := p.applyReconciliation(ctx, runID, reconciled, planRef)
	if err != nil {
		return err
	}
	_ = p.completePlannerTurn(ctx, threadID, turnID, store.TurnStatusCompleted, usage, appliedSummary, nil)
	return p.recordDecision(ctx, runID, threadID, turnID, toolName, goal, appliedSummary, usage, p.clock.Now().Sub(start))
}

func planTool(name string) model.ToolDefinition {
	return model.ToolDefinition{Name: name, Description: "Submit the approach in summary, objective, backlog diff and conflicts", InputSchema: slices.Clone(submitPlanReconcileSchema)}
}

func (p *Planner) proposePlan(ctx context.Context, req model.Request, allowOpen bool) (PlanReconciliation, bool, model.Usage, error) {
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

func (p *Planner) applyReconciliation(
	ctx context.Context,
	runID string,
	rec PlanReconciliation,
	fallbackPlanRef string,
) (string, error) {
	run, err := p.store.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}
	newObj := cmp.Or(strings.TrimSpace(rec.Objective), run.Objective)
	newRef := cmp.Or(strings.TrimSpace(rec.PlanRef), strings.TrimSpace(fallbackPlanRef), run.PlanRef)
	if newObj != run.Objective || newRef != run.PlanRef {
		if err := p.store.Runs().UpdateObjective(ctx, runID, newObj, newRef); err != nil {
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
		if err := p.store.Backlog().Add(ctx, item); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return "", err
		}
		added++
	}

	updated := 0
	for _, spec := range rec.UpdateItems {
		existing, getErr := p.store.Backlog().Get(ctx, runID, strings.TrimSpace(spec.ID))
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
		if err := p.store.Backlog().Update(ctx, cp, ActorPlanner, "plan import update"); err != nil {
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
		if err := p.store.Backlog().Close(ctx, runID, itemID, store.BacklogDropped, ActorPlanner, "dropped by plan import"); err != nil && !errors.Is(err, store.ErrNotFound) {
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
		if err := p.store.Directives().Add(ctx, dir); err == nil {
			asksCreated++
		}
	}

	return fmt.Sprintf("reconciled plan (added=%d updated=%d dropped=%d conflicts=%d)",
		added, updated, dropped, asksCreated), nil
}

func (p *Planner) startPlannerTurn(ctx context.Context, runID, goal, input string) (string, string, error) {
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
