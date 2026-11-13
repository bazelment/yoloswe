package planner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	ActorPlanner       = "planner"
	DefaultModel       = recipe.DefaultCompilerModel
	DefaultMaxTurns    = model.DefaultMaxTurns
	DefaultMaxTokens   = model.DefaultMaxTokens
	DefaultAskDeadline = 30 * time.Minute
	ownerAskRefPrefix  = "owner_ask:"
	plannerRefPrefix   = "planner_request:"
)

// Tool names exposed to or recorded by the Planner.
const (
	ToolSubmitReconciliation = "submit_reconciliation"
	ToolReadPlan             = "read.plan"
	ToolObjectiveUpdate      = "objective.update"
	ToolBacklogAdd           = "backlog.add"
	ToolBacklogUpdate        = "backlog.update"
	ToolBacklogClose         = "backlog.close"
	ToolBacklogReorder       = "backlog.reorder"
	ToolLaneStop             = "lane.stop"
	ToolLaneRebrief          = "lane.rebrief"
	ToolOwnerAsk             = "owner.ask"
	ToolRecipeCompile        = "recipe.compile"
)

// Mode identifies the execution mode of a Planner turn.
type Mode string

const (
	ModePlanImport Mode = "plan_import"
	ModeRecipeEdit Mode = "recipe_edit"
	ModeReplan     Mode = "replan"
)

// SignalKind classifies a Controller or Dispatcher trigger signal to the Planner.
type SignalKind string

const (
	SignalPlanImport        SignalKind = "plan_import"
	SignalRecipeEdit        SignalKind = "recipe_edit"
	SignalEmptyBacklog      SignalKind = "empty_backlog"
	SignalBlockedBacklog    SignalKind = "blocked_backlog"
	SignalMilestoneDone     SignalKind = "milestone_done"
	SignalRepeatedFailure   SignalKind = "repeated_failure"
	SignalDispatcherRequest SignalKind = "dispatcher_request"
)

// Signal is a trigger sent by the Controller or Dispatcher to start a Planner thread.
type Signal struct {
	RunID       string     `json:"run_id"`
	Kind        SignalKind `json:"kind"`
	Reason      string     `json:"reason,omitempty"`
	PlanRef     string     `json:"plan_ref,omitempty"`
	RecipeName  string     `json:"recipe_name,omitempty"`
	EditText    string     `json:"edit_text,omitempty"`
	DirectiveID string     `json:"directive_id,omitempty"`
}

// BacklogAddItem describes a new backlog item proposed in a Planner reconciliation.
type BacklogAddItem struct {
	ID          string   `json:"id" jsonschema:"required,description=Unique backlog item ID"`
	Title       string   `json:"title" jsonschema:"required,description=Short title of the work item"`
	Description string   `json:"description,omitempty" jsonschema:"description=Detailed description"`
	Acceptance  string   `json:"acceptance,omitempty" jsonschema:"description=Acceptance criteria"`
	Priority    string   `json:"priority,omitempty" jsonschema:"enum=p0,enum=p1,enum=p2,enum=p3,description=Item priority (p0..p3)"`
	RecipeName  string   `json:"recipe_name,omitempty" jsonschema:"description=Optional recipe name for this item"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"description=IDs of backlog items this item depends on"`
	Tags        []string `json:"tags,omitempty" jsonschema:"description=Optional tags"`
	Order       int      `json:"order,omitempty" jsonschema:"description=Explicit sort order within priority"`
}

// BacklogUpdateItem describes an update or reprioritization of an existing backlog item.
type BacklogUpdateItem struct {
	ID          string   `json:"id" jsonschema:"required,description=Backlog item ID to update"`
	Title       string   `json:"title,omitempty" jsonschema:"description=Updated title"`
	Description string   `json:"description,omitempty" jsonschema:"description=Updated description"`
	Acceptance  string   `json:"acceptance,omitempty" jsonschema:"description=Updated acceptance criteria"`
	Priority    string   `json:"priority,omitempty" jsonschema:"enum=p0,enum=p1,enum=p2,enum=p3,description=Updated priority"`
	RecipeName  string   `json:"recipe_name,omitempty" jsonschema:"description=Updated recipe name"`
	Reason      string   `json:"reason,omitempty" jsonschema:"description=Reason for updating this item"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"description=Replacement dependency IDs"`
	Tags        []string `json:"tags,omitempty" jsonschema:"description=Replacement tags"`
	Order       int      `json:"order,omitempty" jsonschema:"description=Updated sort order"`
}

// BacklogDropItem describes an existing backlog item to close as dropped.
type BacklogDropItem struct {
	ID     string `json:"id" jsonschema:"required,description=Backlog item ID to drop"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason why the plan drops this item"`
}

// UnmarshalJSON decodes either a {"id": ..., "reason": ...} object or a bare item ID string.
func (d *BacklogDropItem) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var id string
		if err := json.Unmarshal(trimmed, &id); err != nil {
			return err
		}
		d.ID = strings.TrimSpace(id)
		return nil
	}
	type plain BacklogDropItem
	var p plain
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return err
	}
	*d = BacklogDropItem(p)
	return nil
}

// BacklogDiff holds the backlog additions, updates/reprioritizations, drops, and ordering changes.
type BacklogDiff struct {
	Add     []BacklogAddItem    `json:"add,omitempty" jsonschema:"description=New backlog items to add"`
	Update  []BacklogUpdateItem `json:"update,omitempty" jsonschema:"description=Existing backlog items to update or reprioritize"`
	Drop    []BacklogDropItem   `json:"drop,omitempty" jsonschema:"description=Existing backlog items to close as dropped"`
	Reorder []string            `json:"reorder,omitempty" jsonschema:"description=Optional ordered backlog item IDs"`
}

// LaneStopSpec identifies an active lane to stop during plan reconciliation.
type LaneStopSpec struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to stop"`
	Reason string `json:"reason" jsonschema:"required,description=Reason for stopping the lane"`
}

// LaneRebriefSpec identifies an active or blocked lane to steer with updated instructions.
type LaneRebriefSpec struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to re-brief"`
	Brief  string `json:"brief" jsonschema:"required,description=Updated brief or steering instructions for the lane"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason for re-briefing the lane"`
}

// ConflictPoint describes a point in the plan that conflicts with an active owner directive.
type ConflictPoint struct {
	ID          string   `json:"id,omitempty" jsonschema:"description=Optional owner ask ID"`
	Description string   `json:"description" jsonschema:"required,description=Description of the conflict"`
	DirectiveID string   `json:"directive_id,omitempty" jsonschema:"description=Conflicting directive ID if applicable"`
	LaneID      string   `json:"lane_id,omitempty" jsonschema:"description=Optional affected lane ID"`
	Question    string   `json:"question,omitempty" jsonschema:"description=Decision question for owner.ask"`
	Default     string   `json:"default,omitempty" jsonschema:"description=Default option auto-applied at deadline"`
	Deadline    string   `json:"deadline,omitempty" jsonschema:"description=Deadline duration or RFC3339 timestamp (default 30m)"`
	Options     []string `json:"options,omitempty" jsonschema:"description=Concrete options for owner.ask"`
}

// UnmarshalJSON decodes either a structured ConflictPoint object or a bare string description.
func (c *ConflictPoint) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var desc string
		if err := json.Unmarshal(trimmed, &desc); err != nil {
			return err
		}
		c.Description = strings.TrimSpace(desc)
		return nil
	}
	type plain ConflictPoint
	var p plain
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return err
	}
	*c = ConflictPoint(p)
	return nil
}

// Reconciliation is the structured output of a Planner plan-import or replanning turn.
type Reconciliation struct {
	Objective      string            `json:"objective,omitempty" jsonschema:"description=Updated run objective (if changed by the plan)"`
	Summary        string            `json:"summary,omitempty" jsonschema:"description=One-message summary of what changed"`
	BacklogDiff    BacklogDiff       `json:"backlog_diff" jsonschema:"required,description=Backlog additions, updates, drops, and reordering"`
	LanesToStop    []LaneStopSpec    `json:"lanes_to_stop,omitempty" jsonschema:"description=Active lanes to stop"`
	LanesToRebrief []LaneRebriefSpec `json:"lanes_to_rebrief,omitempty" jsonschema:"description=Active or blocked lanes to re-brief"`
	Unmappable     []string          `json:"unmappable,omitempty" jsonschema:"description=Plan points that could not be mapped to objective, backlog, or lanes"`
	Conflicts      []ConflictPoint   `json:"conflicts,omitempty" jsonschema:"description=Plan points that conflict with active directives"`
}

func (r Reconciliation) Clone() Reconciliation {
	out := r
	out.BacklogDiff.Add = slices.Clone(r.BacklogDiff.Add)
	for i := range out.BacklogDiff.Add {
		out.BacklogDiff.Add[i].DependsOn = slices.Clone(r.BacklogDiff.Add[i].DependsOn)
		out.BacklogDiff.Add[i].Tags = slices.Clone(r.BacklogDiff.Add[i].Tags)
	}
	out.BacklogDiff.Update = slices.Clone(r.BacklogDiff.Update)
	for i := range out.BacklogDiff.Update {
		out.BacklogDiff.Update[i].DependsOn = slices.Clone(r.BacklogDiff.Update[i].DependsOn)
		out.BacklogDiff.Update[i].Tags = slices.Clone(r.BacklogDiff.Update[i].Tags)
	}
	out.BacklogDiff.Drop = slices.Clone(r.BacklogDiff.Drop)
	out.BacklogDiff.Reorder = slices.Clone(r.BacklogDiff.Reorder)
	out.LanesToStop = slices.Clone(r.LanesToStop)
	out.LanesToRebrief = slices.Clone(r.LanesToRebrief)
	out.Unmappable = slices.Clone(r.Unmappable)
	out.Conflicts = slices.Clone(r.Conflicts)
	for i := range out.Conflicts {
		out.Conflicts[i].Options = slices.Clone(r.Conflicts[i].Options)
	}
	return out
}

var submitReconciliationSchema = model.SchemaFor[Reconciliation]()

// SubmitReconciliationToolDefinition returns the Messages API ToolDefinition for submit_reconciliation.
func SubmitReconciliationToolDefinition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        ToolSubmitReconciliation,
		Description: "Submit the structured plan reconciliation (objective update, backlog diff, lanes to stop or re-brief, unmappable points, and directive conflicts).",
		InputSchema: slices.Clone(submitReconciliationSchema),
	}
}

// ReadPlanParams holds parameters for the optional read.plan tool.
type ReadPlanParams struct {
	Ref string `json:"ref" jsonschema:"required,description=Plan file path or Nebula post ID to read"`
}

var readPlanToolDef = model.ToolDefinition{
	Name:        ToolReadPlan,
	Description: "Read a referenced plan document from a file path or Nebula post ID.",
	InputSchema: model.SchemaFor[ReadPlanParams](),
}

// AppliedChanges summarizes the mutations applied to the Store by a Reconciliation.
type AppliedChanges struct {
	NewObjective     string                `json:"new_objective,omitempty"`
	NewPlanRef       string                `json:"new_plan_ref,omitempty"`
	AddedItems       []string              `json:"added_items,omitempty"`
	UpdatedItems     []string              `json:"updated_items,omitempty"`
	DroppedItems     []string              `json:"dropped_items,omitempty"`
	ReorderedItems   []string              `json:"reordered_items,omitempty"`
	StoppedLanes     []string              `json:"stopped_lanes,omitempty"`
	RebriefedLanes   []string              `json:"rebriefed_lanes,omitempty"`
	OwnerAsks        []controller.OwnerAsk `json:"owner_asks,omitempty"`
	ObjectiveUpdated bool                  `json:"objective_updated"`
}

func (a AppliedChanges) Clone() AppliedChanges {
	out := a
	out.AddedItems = slices.Clone(a.AddedItems)
	out.UpdatedItems = slices.Clone(a.UpdatedItems)
	out.DroppedItems = slices.Clone(a.DroppedItems)
	out.ReorderedItems = slices.Clone(a.ReorderedItems)
	out.StoppedLanes = slices.Clone(a.StoppedLanes)
	out.RebriefedLanes = slices.Clone(a.RebriefedLanes)
	out.OwnerAsks = slices.Clone(a.OwnerAsks)
	for i := range out.OwnerAsks {
		out.OwnerAsks[i].Options = slices.Clone(out.OwnerAsks[i].Options)
	}
	return out
}

// Result captures the outcome of a single Planner thread execution.
type Result struct {
	Plan           *PlanDocument         `json:"plan,omitempty"`
	Reconciliation *Reconciliation       `json:"reconciliation,omitempty"`
	RecipeResult   *recipe.CompileResult `json:"recipe_result,omitempty"`
	ThreadID       string                `json:"thread_id"`
	TurnID         string                `json:"turn_id"`
	Mode           Mode                  `json:"mode"`
	Model          string                `json:"model"`
	Summary        string                `json:"summary"`
	Decisions      []store.Decision      `json:"decisions,omitempty"`
	Applied        AppliedChanges        `json:"applied"`
	Usage          model.Usage           `json:"usage"`
	TurnCount      int                   `json:"turn_count"`
}

// Config configures a Planner instance.
type Config struct {
	PlanReader   PlanReader
	Model        string
	SystemPrompt string
	AskDeadline  time.Duration
	MaxTurns     int
	MaxTokens    int
}

func (c Config) withDefaults() Config {
	c.Model = cmp.Or(strings.TrimSpace(c.Model), DefaultModel)
	c.SystemPrompt = cmp.Or(c.SystemPrompt, PlanImportSystemPrompt)
	if c.PlanReader == nil {
		c.PlanReader = &DefaultPlanReader{}
	}
	if c.AskDeadline <= 0 {
		c.AskDeadline = DefaultAskDeadline
	}
	if c.MaxTurns <= 0 {
		c.MaxTurns = DefaultMaxTurns
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = DefaultMaxTokens
	}
	return c
}

// Planner executes fresh-thread plan imports, replanning reconciliations, and
// natural-language recipe edits.
type Planner struct {
	store      store.Store
	driver     lane.LaneDriver
	clock      clock.Clock
	client     model.ModelClient
	lastResult *Result
	cfg        Config
	seq        int64
	mu         sync.Mutex
}

// New constructs a new Planner.
func New(
	cfg Config,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	client model.ModelClient,
) *Planner {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	return &Planner{
		cfg:    cfg.withDefaults(),
		store:  st,
		driver: drv,
		clock:  clk,
		client: client,
	}
}

// LastResult returns a copy of the most recent Planner Result.
func (p *Planner) LastResult() *Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastResult == nil {
		return nil
	}
	cp := *p.lastResult
	if p.lastResult.Plan != nil {
		planCopy := *p.lastResult.Plan
		cp.Plan = &planCopy
	}
	if p.lastResult.Reconciliation != nil {
		recCopy := p.lastResult.Reconciliation.Clone()
		cp.Reconciliation = &recCopy
	}
	cp.Applied = p.lastResult.Applied.Clone()
	cp.Decisions = slices.Clone(p.lastResult.Decisions)
	return &cp
}

func (p *Planner) nextID(prefix string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return fmt.Sprintf("%s-%d", prefix, p.seq)
}

func (p *Planner) syncSeq(ctx context.Context, runID string) {
	var maxObserved int64
	if threads, err := p.store.Threads().ListThreads(ctx, store.ThreadFilter{
		RunID: runID,
		Role:  store.ThreadRolePlanner,
	}); err == nil {
		maxObserved = max(maxObserved, int64(len(threads)))
		for i := range threads {
			maxObserved = max(maxObserved, trailingSeqNumber(threads[i].ID))
		}
	}
	if dirs, err := p.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID}); err == nil {
		maxObserved = max(maxObserved, int64(len(dirs)))
		for i := range dirs {
			maxObserved = max(maxObserved, trailingSeqNumber(dirs[i].ID), trailingSeqNumber(dirs[i].Ref))
		}
	}
	p.mu.Lock()
	if maxObserved > p.seq {
		p.seq = maxObserved
	}
	p.mu.Unlock()
}

func trailingSeqNumber(id string) int64 {
	idx := strings.LastIndexByte(id, '-')
	if idx < 0 || idx+1 >= len(id) {
		return 0
	}
	n, err := strconv.ParseInt(id[idx+1:], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (p *Planner) startFreshThread(ctx context.Context, runID, goal string) (store.Thread, store.Turn, error) {
	p.syncSeq(ctx, runID)
	threadID := p.nextID("planner-" + runID)
	th, err := p.store.Threads().StartThread(ctx, store.Thread{
		ID:     threadID,
		RunID:  runID,
		Role:   store.ThreadRolePlanner,
		Goal:   strings.TrimSpace(goal),
		Status: store.ThreadStatusIdle,
	})
	if err != nil {
		return store.Thread{}, store.Turn{}, err
	}
	turnID := p.nextID(threadID + "-turn")
	turnRec, err := p.store.Threads().StartTurn(ctx, store.Turn{
		ID:       turnID,
		ThreadID: threadID,
		RunID:    runID,
		Input:    strings.TrimSpace(goal),
	})
	if err != nil {
		return store.Thread{}, store.Turn{}, err
	}
	return th, turnRec, nil
}

// DirectiveHandler returns a concierge.DirectiveHandler that triggers the Planner on plan_import and recipe_edit directives.
func (p *Planner) DirectiveHandler() func(ctx context.Context, runID string, dir store.Directive) (string, error) {
	return func(ctx context.Context, runID string, dir store.Directive) (string, error) {
		switch dir.Kind {
		case store.DirectivePlanImport, store.DirectiveRecipeEdit:
			res, err := p.HandleDirective(ctx, runID, dir)
			if err != nil {
				return "", err
			}
			return res.Summary, nil
		default:
			return "", nil
		}
	}
}

// OnPlannerRequest adapts Planner to dispatcher.Config.OnPlannerRequest.
func (p *Planner) OnPlannerRequest(ctx context.Context, runID, reason, planRef string) error {
	var dirID string
	if p.store != nil {
		if dirs, err := p.store.Directives().List(ctx, store.DirectiveFilter{
			RunID:      runID,
			Kinds:      []store.DirectiveKind{store.DirectiveInstruction},
			ActiveOnly: true,
		}); err == nil {
			for i := len(dirs) - 1; i >= 0; i-- {
				d := dirs[i]
				if strings.HasPrefix(strings.TrimSpace(d.Ref), plannerRefPrefix) {
					dirID = d.ID
					break
				}
			}
		}
	}
	_, err := p.HandleSignal(ctx, Signal{
		RunID:       runID,
		Kind:        SignalDispatcherRequest,
		Reason:      reason,
		PlanRef:     planRef,
		DirectiveID: dirID,
	})
	return err
}

// CompileOverride implements dispatcher.RecipeCompiler by compiling a per-lane
// recipe delta against base via the recipe package.
func (p *Planner) CompileOverride(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error) {
	compiler := recipe.NewCompiler(p.client, recipe.WithModel(p.cfg.Model), recipe.WithMaxTokens(p.cfg.MaxTokens))
	res, err := compiler.CompileOverride(ctx, base, base.Name, override)
	if err != nil {
		return recipe.Recipe{}, err
	}
	return res.Recipe, nil
}

// HandleDirective runs a fresh Planner thread for dir and marks it handled on success.
func (p *Planner) HandleDirective(ctx context.Context, runID string, dir store.Directive) (*Result, error) {
	if p.store == nil {
		return nil, errors.New("orca/planner: store is required")
	}
	runID = cmp.Or(strings.TrimSpace(runID), strings.TrimSpace(dir.RunID))
	if runID == "" {
		return nil, fmt.Errorf("%w: run_id is required", store.ErrInvalidArgument)
	}

	var (
		res *Result
		err error
	)
	switch dir.Kind {
	case store.DirectivePlanImport:
		planRef := cmp.Or(strings.TrimSpace(dir.Ref), strings.TrimSpace(dir.Text))
		res, err = p.reconcile(ctx, runID, ModePlanImport, planRef, dir.Text)

	case store.DirectiveRecipeEdit:
		recipeName := p.resolveRecipeEditTarget(ctx, runID, dir)
		editText := cmp.Or(strings.TrimSpace(dir.Text), strings.TrimSpace(dir.Ref))
		res, err = p.EditRecipe(ctx, runID, recipeName, editText)

	case store.DirectiveInstruction:
		rawRef := strings.TrimSpace(dir.Ref)
		if !strings.HasPrefix(rawRef, plannerRefPrefix) {
			return nil, fmt.Errorf("%w: unsupported planner instruction directive %q (ref=%q)", store.ErrInvalidArgument, dir.ID, dir.Ref)
		}
		planRef := strings.TrimSpace(strings.TrimPrefix(rawRef, plannerRefPrefix))
		if planRef == dir.ID {
			planRef = ""
		}
		mode := ModeReplan
		if planRef != "" {
			mode = ModePlanImport
		}
		res, err = p.reconcile(ctx, runID, mode, planRef, dir.Text)

	default:
		return nil, fmt.Errorf("%w: unsupported planner directive kind %q", store.ErrInvalidArgument, dir.Kind)
	}
	if err != nil {
		return res, err
	}

	if dir.ID != "" {
		if err := p.store.Directives().UpdateStatus(ctx, runID, dir.ID, store.DirectiveHandled); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (p *Planner) resolveRecipeEditTarget(ctx context.Context, runID string, dir store.Directive) string {
	ref := strings.TrimSpace(dir.Ref)
	text := strings.TrimSpace(dir.Text)
	if ref != "" && text != "" {
		return ref
	}
	scope := strings.TrimSpace(dir.Scope)
	if scope == "" || scope == "run" {
		return ""
	}
	if ln, err := p.store.Lanes().Get(ctx, runID, scope); err == nil && ln != nil {
		return strings.TrimSpace(ln.RecipeName)
	}
	return scope
}

// ProcessPendingDirectives scans active plan_import, recipe_edit, and planner_request
// directives for runID and executes a Planner thread for each.
func (p *Planner) ProcessPendingDirectives(ctx context.Context, runID string) ([]*Result, error) {
	if p.store == nil {
		return nil, errors.New("orca/planner: store is required")
	}
	dirs, err := p.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      runID,
		Kinds:      []store.DirectiveKind{store.DirectivePlanImport, store.DirectiveRecipeEdit, store.DirectiveInstruction},
		ActiveOnly: true,
	})
	if err != nil {
		return nil, err
	}
	var (
		results []*Result
		errs    []error
	)
	for i := range dirs {
		d := dirs[i]
		if d.Kind == store.DirectiveInstruction && !strings.HasPrefix(strings.TrimSpace(d.Ref), plannerRefPrefix) {
			continue
		}
		res, hErr := p.HandleDirective(ctx, runID, d)
		if hErr != nil {
			errs = append(errs, hErr)
			continue
		}
		results = append(results, res)
	}
	return results, errors.Join(errs...)
}

// HandleSignal triggers a fresh Planner thread in response to a Controller or Dispatcher signal.
func (p *Planner) HandleSignal(ctx context.Context, sig Signal) (*Result, error) {
	if p.store == nil {
		return nil, errors.New("orca/planner: store is required")
	}
	runID := strings.TrimSpace(sig.RunID)
	if runID == "" {
		return nil, fmt.Errorf("%w: signal run_id is required", store.ErrInvalidArgument)
	}

	if sig.DirectiveID != "" {
		dir, err := p.store.Directives().Get(ctx, runID, sig.DirectiveID)
		if err != nil {
			return nil, err
		}
		return p.HandleDirective(ctx, runID, *dir)
	}

	switch sig.Kind {
	case SignalRecipeEdit:
		return p.EditRecipe(ctx, runID, sig.RecipeName, cmp.Or(sig.EditText, sig.Reason))
	case SignalPlanImport:
		return p.ImportPlan(ctx, runID, cmp.Or(sig.PlanRef, sig.Reason))
	default:
		mode := ModeReplan
		if strings.TrimSpace(sig.PlanRef) != "" {
			mode = ModePlanImport
		}
		reason := cmp.Or(strings.TrimSpace(sig.Reason), string(sig.Kind))
		return p.reconcile(ctx, runID, mode, sig.PlanRef, reason)
	}
}

// ImportPlan reconciles planRef (file path or Nebula post ID) against the Store without approval.
func (p *Planner) ImportPlan(ctx context.Context, runID, planRef string) (*Result, error) {
	if strings.TrimSpace(planRef) == "" {
		return nil, fmt.Errorf("%w: plan_ref is required", store.ErrInvalidArgument)
	}
	return p.reconcile(ctx, runID, ModePlanImport, planRef, "")
}

// Replan reconciles the run backlog and lanes for the given trigger reason.
func (p *Planner) Replan(ctx context.Context, runID, reason string) (*Result, error) {
	return p.reconcile(ctx, runID, ModeReplan, "", reason)
}

func (p *Planner) reconcile(ctx context.Context, runID string, mode Mode, planRef, reason string) (*Result, error) {
	if p.store == nil {
		return nil, errors.New("orca/planner: store is required")
	}
	if p.client == nil {
		return nil, errors.New("orca/planner: model client is required")
	}
	start := p.clock.Now()

	run, err := p.store.Runs().Get(ctx, runID)
	if err != nil {
		return nil, err
	}

	var planDoc *PlanDocument
	effectiveRef := strings.TrimSpace(planRef)
	if effectiveRef != "" {
		doc, readErr := p.cfg.PlanReader.ReadPlan(ctx, effectiveRef)
		if readErr != nil {
			return nil, readErr
		}
		planDoc = &doc
	}

	goal := fmt.Sprintf("Planner %s for run %s", mode, runID)
	if effectiveRef != "" {
		goal = fmt.Sprintf("Import plan %s for run %s", effectiveRef, runID)
	} else if strings.TrimSpace(reason) != "" {
		goal = fmt.Sprintf("Replan run %s: %s", runID, strings.TrimSpace(reason))
	}

	th, turnRec, err := p.startFreshThread(ctx, runID, goal)
	if err != nil {
		return nil, err
	}
	cleanupCtx := context.WithoutCancel(ctx)

	promptText, err := p.buildReconcilePrompt(ctx, *run, planDoc, reason, turnRec.Steers)
	if err != nil {
		_ = p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, store.TurnStatusFailed, model.Usage{}, err.Error())
		return nil, err
	}

	if _, err := p.store.Threads().AppendItem(ctx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnRec.ID,
		Kind:     store.ItemKindSnapshot,
		Content:  promptText,
	}); err != nil {
		_ = p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, store.TurnStatusFailed, model.Usage{}, err.Error())
		return nil, err
	}

	var (
		reconciled   *Reconciliation
		applied      AppliedChanges
		decisions    []store.Decision
		turnUsage    model.Usage
		lastApplyErr error
	)

	applyOnce := func(callCtx context.Context, rec Reconciliation, usage model.Usage) (string, error) {
		if reconciled != nil {
			return FormatReconciliationSummary(runID, effectiveRef, *reconciled, applied), nil
		}
		cp := rec.Clone()
		curApplied, curDecisions, applyErr := p.applyReconciliation(callCtx, *run, th.ID, turnRec.ID, effectiveRef, cp, usage, p.clock.Now().Sub(start))
		if applyErr != nil {
			lastApplyErr = applyErr
			return "", applyErr
		}
		reconciled = &cp
		applied = curApplied
		decisions = curDecisions
		lastApplyErr = nil
		return FormatReconciliationSummary(runID, effectiveRef, cp, applied), nil
	}

	reg := model.NewToolRegistry()
	reg.Register(SubmitReconciliationToolDefinition(), func(callCtx context.Context, raw json.RawMessage) (string, error) {
		var rec Reconciliation
		if err := json.Unmarshal(raw, &rec); err != nil {
			return "", fmt.Errorf("decode %s: %w", ToolSubmitReconciliation, err)
		}
		return applyOnce(callCtx, rec, turnUsage)
	})
	reg.Register(readPlanToolDef, func(callCtx context.Context, raw json.RawMessage) (string, error) {
		var rp ReadPlanParams
		if err := json.Unmarshal(raw, &rp); err != nil {
			return "", fmt.Errorf("decode %s: %w", ToolReadPlan, err)
		}
		reqRef := strings.TrimSpace(rp.Ref)
		pinnedRef := cmp.Or(effectiveRef, strings.TrimSpace(run.PlanRef))
		if pinnedRef != "" && reqRef != "" && reqRef != effectiveRef && reqRef != strings.TrimSpace(run.PlanRef) {
			return "", fmt.Errorf("%w: read.plan ref %q does not match pinned plan %q", store.ErrInvalidArgument, reqRef, pinnedRef)
		}
		ref := cmp.Or(reqRef, pinnedRef)
		doc, readErr := p.cfg.PlanReader.ReadPlan(callCtx, ref)
		if readErr != nil {
			return "", readErr
		}
		planDoc = &doc
		return fmt.Sprintf("Plan Ref: %s (%s)\nTitle: %s\n\n%s", doc.Ref, doc.SourceKind, doc.Title, doc.Content), nil
	})

	req := model.Request{
		Model:     p.cfg.Model,
		System:    p.cfg.SystemPrompt,
		Messages:  []model.Message{model.NewUserMessage(promptText)},
		Tools:     reg.Definitions(),
		MaxTokens: p.cfg.MaxTokens,
	}

	loopRes, loopErr := model.RunToolLoop(
		ctx,
		p.client,
		req,
		reg,
		model.WithMaxTurns(p.cfg.MaxTurns),
		model.WithOnTurn(func(_ int, resp model.Response) {
			turnUsage = resp.Usage
			if txt := strings.TrimSpace(resp.Text()); txt != "" {
				_, _ = p.store.Threads().AppendItem(cleanupCtx, store.Item{
					ThreadID: th.ID,
					TurnID:   turnRec.ID,
					Kind:     store.ItemKindAssistantMessage,
					Content:  txt,
				})
			}
		}),
		model.WithOnToolCall(func(_ int, call model.ToolCall, resBlock model.ContentBlock) {
			_, _ = p.store.Threads().AppendItem(cleanupCtx, store.Item{
				ThreadID: th.ID,
				TurnID:   turnRec.ID,
				Kind:     store.ItemKindToolCall,
				Name:     call.Name,
				CallID:   call.ID,
				Payload:  call.Input,
			})
			_, _ = p.store.Threads().AppendItem(cleanupCtx, store.Item{
				ThreadID: th.ID,
				TurnID:   turnRec.ID,
				Kind:     store.ItemKindToolResult,
				Name:     call.Name,
				CallID:   call.ID,
				Content:  resBlock.Content,
				IsError:  resBlock.IsError,
			})
		}),
	)

	var (
		usage     model.Usage
		turnCount int
		modelText string
	)
	if loopRes != nil {
		usage = loopRes.Usage
		turnCount = loopRes.TurnCount
		modelText = strings.TrimSpace(loopRes.Response.Text())
	}

	// Support single-turn responses where submit_reconciliation was emitted on an end_turn response or JSON text.
	if loopErr == nil && reconciled == nil && lastApplyErr == nil && loopRes != nil {
		extracted, extErr := extractReconciliationFromResponse(loopRes.Response)
		if extErr != nil {
			loopErr = extErr
		} else {
			if _, applyErr := applyOnce(ctx, extracted, usage); applyErr != nil {
				loopErr = applyErr
			}
		}
	}

	if loopErr == nil && reconciled == nil {
		if lastApplyErr != nil {
			loopErr = lastApplyErr
		} else {
			loopErr = fmt.Errorf("orca/planner: model did not call %s", ToolSubmitReconciliation)
		}
	}

	var summary string
	if reconciled != nil {
		summary = FormatReconciliationSummary(runID, effectiveRef, *reconciled, applied)
		if modelText != "" && !strings.Contains(summary, modelText) {
			summary = singleLineSummary(modelText + " — " + summary)
		}
	}

	turnStatus := store.TurnStatusCompleted
	errMsg := ""
	if loopErr != nil {
		turnStatus = store.TurnStatusFailed
		errMsg = loopErr.Error()
	}
	if compErr := p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, turnStatus, usage, errMsg); compErr != nil && loopErr == nil {
		loopErr = compErr
	}

	res := &Result{
		ThreadID:       th.ID,
		TurnID:         turnRec.ID,
		Mode:           mode,
		Model:          p.cfg.Model,
		Summary:        summary,
		Plan:           planDoc,
		Reconciliation: reconciled,
		Applied:        applied,
		Decisions:      decisions,
		Usage:          usage,
		TurnCount:      turnCount,
	}

	p.mu.Lock()
	p.lastResult = res
	p.mu.Unlock()

	if loopErr != nil {
		return res, loopErr
	}
	return res, nil
}

func extractReconciliationFromResponse(resp model.Response) (Reconciliation, error) {
	for _, call := range resp.ToolCalls() {
		if call.Name == ToolSubmitReconciliation {
			var rec Reconciliation
			if err := json.Unmarshal(call.Input, &rec); err != nil {
				return Reconciliation{}, fmt.Errorf("orca/planner: decode %s tool input: %w", ToolSubmitReconciliation, err)
			}
			return rec, nil
		}
	}
	rawText := strings.TrimSpace(resp.Text())
	if rawText == "" {
		return Reconciliation{}, fmt.Errorf("orca/planner: model response contained neither %s tool_use nor JSON text", ToolSubmitReconciliation)
	}
	cleaned := stripMarkdownFence(rawText)
	var rec Reconciliation
	if err := json.Unmarshal([]byte(cleaned), &rec); err != nil {
		return Reconciliation{}, fmt.Errorf("orca/planner: decode reconciliation JSON from response text: %w", err)
	}
	return rec, nil
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

func (p *Planner) buildReconcilePrompt(
	ctx context.Context,
	run store.Run,
	planDoc *PlanDocument,
	reason string,
	steers []string,
) (string, error) {
	directives, err := p.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      run.ID,
		ActiveOnly: true,
	})
	if err != nil {
		return "", err
	}
	backlogItems, err := p.store.Backlog().List(ctx, store.BacklogFilter{RunID: run.ID})
	if err != nil {
		return "", err
	}
	lanes, err := p.store.Lanes().List(ctx, store.LaneFilter{RunID: run.ID})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Planner Context for Run `%s`\n", run.ID)
	fmt.Fprintf(&b, "Current Objective: %s\n", cmp.Or(strings.TrimSpace(run.Objective), "(none)"))
	fmt.Fprintf(&b, "Current PlanRef: %s\n", cmp.Or(strings.TrimSpace(run.PlanRef), "(none)"))
	fmt.Fprintf(&b, "Default Recipe: %s\n", cmp.Or(strings.TrimSpace(run.DefaultRecipe), recipe.DefaultName))
	if strings.TrimSpace(reason) != "" {
		fmt.Fprintf(&b, "Trigger Reason: %s\n", strings.TrimSpace(reason))
	}
	if len(steers) > 0 {
		fmt.Fprintf(&b, "Steers: %s\n", strings.Join(steers, "; "))
	}

	fmt.Fprintf(&b, "\n## Active Directives (%d)\n", len(directives))
	if len(directives) == 0 {
		b.WriteString("(none)\n")
	} else {
		for i := range directives {
			d := &directives[i]
			fmt.Fprintf(&b, "- [%s] kind=%s source=%s scope=%s ref=%s: %s\n",
				d.ID, d.Kind, d.Source, cmp.Or(d.Scope, "run"), cmp.Or(d.Ref, "-"), d.Text)
		}
	}

	fmt.Fprintf(&b, "\n## Current Backlog (%d)\n", len(backlogItems))
	if len(backlogItems) == 0 {
		b.WriteString("(empty)\n")
	} else {
		for i := range backlogItems {
			it := &backlogItems[i]
			fmt.Fprintf(&b, "- %s [%s #%d %s] lane=%s title=%q acceptance=%q\n",
				it.ID, it.Priority, it.Order, it.Status, cmp.Or(it.LaneID, "-"), it.Title, it.Acceptance)
		}
	}

	fmt.Fprintf(&b, "\n## Current Lanes (%d)\n", len(lanes))
	if len(lanes) == 0 {
		b.WriteString("(none)\n")
	} else {
		for i := range lanes {
			ln := &lanes[i]
			fmt.Fprintf(&b, "- %s: item=%s status=%s phase=%s round=%d pr=#%d checks=%s\n",
				ln.ID, cmp.Or(ln.ItemID, "-"), ln.Status, cmp.Or(ln.Phase, "-"), ln.Round, ln.PR, cmp.Or(ln.Checks, "-"))
		}
	}

	if planDoc != nil {
		fmt.Fprintf(&b, "\n## Referenced Plan (`%s`, source=%s)\n", planDoc.Ref, planDoc.SourceKind)
		if planDoc.Title != "" {
			fmt.Fprintf(&b, "Title: %s\n", planDoc.Title)
		}
		b.WriteString("\n")
		b.WriteString(planDoc.Content)
		b.WriteString("\n")
	}

	return b.String(), nil
}

func (p *Planner) applyReconciliation(
	ctx context.Context,
	run store.Run,
	threadID, turnID, planRef string,
	rec Reconciliation,
	usage model.Usage,
	elapsed time.Duration,
) (AppliedChanges, []store.Decision, error) {
	var (
		applied   AppliedChanges
		decisions []store.Decision
		usageUsed bool
	)
	now := p.clock.Now().UTC()

	appendDec := func(tool, laneID, reason, result string, args any) error {
		rawArgs, _ := json.Marshal(args)
		var u model.Usage
		var latMs int64
		if !usageUsed {
			u = usage
			latMs = max(elapsed.Milliseconds(), 0)
			usageUsed = true
		}
		dec, err := p.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
			RunID:        run.ID,
			Category:     store.DecisionJudgment,
			Actor:        ActorPlanner,
			ThreadID:     threadID,
			TurnID:       turnID,
			LaneID:       laneID,
			Tool:         tool,
			Reason:       reason,
			Result:       result,
			Args:         rawArgs,
			Vetted:       true,
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			CostUSD:      u.CostUSD,
			LatencyMs:    latMs,
		})
		if err != nil {
			return err
		}
		decisions = append(decisions, dec)
		return nil
	}

	for i := range rec.BacklogDiff.Update {
		itemID := strings.TrimSpace(rec.BacklogDiff.Update[i].ID)
		if itemID == "" {
			continue
		}
		if _, err := p.store.Backlog().Get(ctx, run.ID, itemID); err != nil {
			return applied, decisions, err
		}
	}
	for _, drop := range rec.BacklogDiff.Drop {
		itemID := strings.TrimSpace(drop.ID)
		if itemID == "" {
			continue
		}
		if _, err := p.store.Backlog().Get(ctx, run.ID, itemID); err != nil {
			return applied, decisions, err
		}
	}
	for _, stopSpec := range rec.LanesToStop {
		laneID := strings.TrimSpace(stopSpec.LaneID)
		if laneID == "" {
			continue
		}
		if _, err := p.store.Lanes().Get(ctx, run.ID, laneID); err != nil {
			return applied, decisions, err
		}
	}
	for _, rebriefSpec := range rec.LanesToRebrief {
		laneID := strings.TrimSpace(rebriefSpec.LaneID)
		if laneID == "" {
			continue
		}
		if _, err := p.store.Lanes().Get(ctx, run.ID, laneID); err != nil {
			return applied, decisions, err
		}
	}

	newObj := strings.TrimSpace(rec.Objective)
	newPlanRef := cmp.Or(strings.TrimSpace(planRef), run.PlanRef)
	if (newObj != "" && newObj != run.Objective) || (newPlanRef != "" && newPlanRef != run.PlanRef) {
		targetObj := cmp.Or(newObj, run.Objective)
		if err := p.store.Runs().UpdateObjective(ctx, run.ID, targetObj, newPlanRef); err != nil {
			return applied, decisions, err
		}
		applied.ObjectiveUpdated = newObj != "" && newObj != run.Objective
		applied.NewObjective = targetObj
		applied.NewPlanRef = newPlanRef
		if err := appendDec(ToolObjectiveUpdate, "", "plan reconciliation updated run objective/plan_ref",
			fmt.Sprintf("updated objective=%q plan_ref=%q", targetObj, newPlanRef),
			map[string]string{"objective": targetObj, "plan_ref": newPlanRef}); err != nil {
			return applied, decisions, err
		}
	}

	for i := range rec.BacklogDiff.Add {
		add := &rec.BacklogDiff.Add[i]
		itemID := strings.TrimSpace(add.ID)
		if itemID == "" {
			continue
		}
		prio := store.Priority(strings.TrimSpace(add.Priority))
		if prio == "" {
			prio = store.PriorityP2
		}
		item := store.BacklogItem{
			ID:          itemID,
			RunID:       run.ID,
			Title:       strings.TrimSpace(add.Title),
			Description: strings.TrimSpace(add.Description),
			Acceptance:  strings.TrimSpace(add.Acceptance),
			Priority:    prio,
			Status:      store.BacklogOpen,
			RecipeName:  strings.TrimSpace(add.RecipeName),
			DependsOn:   slices.Clone(add.DependsOn),
			Tags:        slices.Clone(add.Tags),
			Order:       add.Order,
		}
		if err := p.store.Backlog().Add(ctx, item); err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				if existing, getErr := p.store.Backlog().Get(ctx, run.ID, itemID); getErr == nil && existing != nil {
					item.Status = existing.Status
					item.LaneID = existing.LaneID
					if uErr := p.store.Backlog().Update(ctx, item, ActorPlanner, "reconciled existing item from plan"); uErr != nil {
						return applied, decisions, uErr
					}
					applied.UpdatedItems = append(applied.UpdatedItems, itemID)
					continue
				}
			}
			return applied, decisions, err
		}
		applied.AddedItems = append(applied.AddedItems, itemID)
		if err := appendDec(ToolBacklogAdd, "", "added backlog item from plan",
			fmt.Sprintf("added %s (%s)", itemID, prio), *add); err != nil {
			return applied, decisions, err
		}
	}

	for i := range rec.BacklogDiff.Update {
		upd := &rec.BacklogDiff.Update[i]
		itemID := strings.TrimSpace(upd.ID)
		if itemID == "" {
			continue
		}
		existing, err := p.store.Backlog().Get(ctx, run.ID, itemID)
		if err != nil {
			return applied, decisions, err
		}
		updated := existing.Clone()
		if strings.TrimSpace(upd.Title) != "" {
			updated.Title = strings.TrimSpace(upd.Title)
		}
		if upd.Description != "" {
			updated.Description = strings.TrimSpace(upd.Description)
		}
		if upd.Acceptance != "" {
			updated.Acceptance = strings.TrimSpace(upd.Acceptance)
		}
		if strings.TrimSpace(upd.Priority) != "" {
			updated.Priority = store.Priority(strings.TrimSpace(upd.Priority))
		}
		if strings.TrimSpace(upd.RecipeName) != "" {
			updated.RecipeName = strings.TrimSpace(upd.RecipeName)
		}
		if upd.DependsOn != nil {
			updated.DependsOn = slices.Clone(upd.DependsOn)
		}
		if upd.Tags != nil {
			updated.Tags = slices.Clone(upd.Tags)
		}
		if upd.Order > 0 {
			updated.Order = upd.Order
		}
		reason := cmp.Or(strings.TrimSpace(upd.Reason), "updated by plan reconciliation")
		if err := p.store.Backlog().Update(ctx, updated, ActorPlanner, reason); err != nil {
			return applied, decisions, err
		}
		applied.UpdatedItems = append(applied.UpdatedItems, itemID)
		if err := appendDec(ToolBacklogUpdate, "", reason,
			fmt.Sprintf("updated %s (%s)", itemID, updated.Priority), *upd); err != nil {
			return applied, decisions, err
		}
	}

	droppedSet := make(map[string]bool, len(rec.BacklogDiff.Drop))
	for _, drop := range rec.BacklogDiff.Drop {
		itemID := strings.TrimSpace(drop.ID)
		if itemID == "" {
			continue
		}
		reason := cmp.Or(strings.TrimSpace(drop.Reason), "dropped by plan reconciliation")
		if err := p.store.Backlog().Close(ctx, run.ID, itemID, store.BacklogDropped, ActorPlanner, reason); err != nil {
			return applied, decisions, err
		}
		droppedSet[itemID] = true
		applied.DroppedItems = append(applied.DroppedItems, itemID)
		if err := appendDec(ToolBacklogClose, "", reason,
			fmt.Sprintf("dropped backlog item %s", itemID), drop); err != nil {
			return applied, decisions, err
		}
	}

	if len(rec.BacklogDiff.Reorder) > 0 {
		if err := p.store.Backlog().Reorder(ctx, run.ID, rec.BacklogDiff.Reorder, ActorPlanner); err != nil {
			return applied, decisions, err
		}
		applied.ReorderedItems = slices.Clone(rec.BacklogDiff.Reorder)
		if err := appendDec(ToolBacklogReorder, "", "reordered backlog items from plan",
			fmt.Sprintf("reordered %d items", len(rec.BacklogDiff.Reorder)), rec.BacklogDiff.Reorder); err != nil {
			return applied, decisions, err
		}
	}

	for _, stopSpec := range rec.LanesToStop {
		laneID := strings.TrimSpace(stopSpec.LaneID)
		if laneID == "" {
			continue
		}
		ln, err := p.store.Lanes().Get(ctx, run.ID, laneID)
		if err != nil {
			return applied, decisions, err
		}
		if ln.Status.Terminal() {
			continue
		}
		reason := cmp.Or(strings.TrimSpace(stopSpec.Reason), "stopped by plan reconciliation")
		if p.driver != nil && (ln.SessionID != "" || ln.TerminalID != "") {
			if err := p.driver.Stop(ctx, lane.StopRequest{
				RunID:      run.ID,
				LaneID:     ln.ID,
				SessionID:  ln.SessionID,
				TerminalID: ln.TerminalID,
				Reason:     reason,
			}); err != nil {
				return applied, decisions, err
			}
		}
		ln.Status = lane.StatusFailed
		ln.LastEventAt = now
		if err := p.store.Lanes().Upsert(ctx, *ln); err != nil {
			return applied, decisions, err
		}
		if ln.ItemID != "" && !droppedSet[ln.ItemID] {
			if item, getErr := p.store.Backlog().Get(ctx, run.ID, ln.ItemID); getErr == nil && item != nil && item.Status == store.BacklogInProgress {
				item.Status = store.BacklogBlocked
				if err := p.store.Backlog().Update(ctx, *item, ActorPlanner, "lane stopped by plan reconciliation: "+reason); err != nil {
					return applied, decisions, err
				}
			}
		}
		applied.StoppedLanes = append(applied.StoppedLanes, ln.ID)
		if err := appendDec(ToolLaneStop, ln.ID, reason,
			fmt.Sprintf("stopped lane %s (%s)", ln.ID, reason), stopSpec); err != nil {
			return applied, decisions, err
		}
	}

	for _, rebriefSpec := range rec.LanesToRebrief {
		laneID := strings.TrimSpace(rebriefSpec.LaneID)
		if laneID == "" {
			continue
		}
		ln, err := p.store.Lanes().Get(ctx, run.ID, laneID)
		if err != nil {
			return applied, decisions, err
		}
		brief := strings.TrimSpace(rebriefSpec.Brief)
		reason := cmp.Or(strings.TrimSpace(rebriefSpec.Reason), "re-briefed by plan reconciliation")
		if p.driver != nil && (ln.SessionID != "" || ln.TerminalID != "") {
			if err := p.driver.Steer(ctx, lane.SteerRequest{
				RunID:      run.ID,
				LaneID:     ln.ID,
				SessionID:  ln.SessionID,
				TerminalID: ln.TerminalID,
				Message:    brief,
			}); err != nil {
				return applied, decisions, err
			}
		}
		if ln.Status == lane.StatusBlocked {
			ln.Status = lane.StatusRunning
		}
		ln.LastEventAt = now
		if err := p.store.Lanes().Upsert(ctx, *ln); err != nil {
			return applied, decisions, err
		}
		applied.RebriefedLanes = append(applied.RebriefedLanes, ln.ID)
		if err := appendDec(ToolLaneRebrief, ln.ID, reason,
			fmt.Sprintf("re-briefed lane %s", ln.ID), rebriefSpec); err != nil {
			return applied, decisions, err
		}
	}

	escalateAsk := func(cf ConflictPoint, reason string) error {
		ask, err := p.registerReconcileAsk(ctx, run.ID, now, cf)
		if err != nil {
			return err
		}
		applied.OwnerAsks = append(applied.OwnerAsks, ask)
		return appendDec(ToolOwnerAsk, ask.LaneID, reason,
			fmt.Sprintf("registered owner ask %s (default=%q)", ask.ID, ask.Default), ask)
	}

	for i := range rec.Conflicts {
		cf := &rec.Conflicts[i]
		desc := strings.TrimSpace(cf.Description)
		if desc == "" {
			continue
		}
		if err := escalateAsk(*cf, "plan conflict with active directive: "+desc); err != nil {
			return applied, decisions, err
		}
	}

	for _, unmapped := range rec.Unmappable {
		u := strings.TrimSpace(unmapped)
		if u == "" {
			continue
		}
		if err := escalateAsk(ConflictPoint{
			Description: u,
			Question:    fmt.Sprintf("How should unmappable plan item %q be handled?", u),
			Options:     []string{"skip_unmappable", "add_manual_backlog_item"},
			Default:     "skip_unmappable",
		}, "unmappable plan point: "+u); err != nil {
			return applied, decisions, err
		}
	}

	return applied, decisions, nil
}

func (p *Planner) registerReconcileAsk(
	ctx context.Context,
	runID string,
	now time.Time,
	cf ConflictPoint,
) (controller.OwnerAsk, error) {
	askID := strings.TrimSpace(cf.ID)
	if askID == "" {
		askID = p.nextID("ask-plan")
	}
	question := strings.TrimSpace(cf.Question)
	if question == "" {
		if cf.DirectiveID != "" {
			question = fmt.Sprintf("Plan conflicts with directive %s (%s); how should we proceed?", cf.DirectiveID, cf.Description)
		} else {
			question = fmt.Sprintf("How should we resolve plan conflict: %s?", cf.Description)
		}
	}
	options := slices.Clone(cf.Options)
	defOpt := strings.TrimSpace(cf.Default)
	if len(options) == 0 {
		options = []string{"keep_directive", "override_with_plan"}
		if defOpt == "" {
			defOpt = "keep_directive"
		} else if !slices.Contains(options, defOpt) {
			options = append([]string{defOpt}, options...)
		}
	} else if defOpt == "" || !slices.Contains(options, defOpt) {
		defOpt = options[0]
	}

	deadline := now.Add(p.cfg.AskDeadline).UTC()
	if strings.TrimSpace(cf.Deadline) != "" {
		if parsed, err := parseFutureDeadline(now, cf.Deadline); err == nil {
			deadline = parsed
		}
	}

	ask := controller.OwnerAsk{
		ID:       askID,
		RunID:    runID,
		LaneID:   strings.TrimSpace(cf.LaneID),
		Question: question,
		Options:  options,
		Default:  defOpt,
		Deadline: deadline,
	}
	if err := ask.Validate(); err != nil {
		return controller.OwnerAsk{}, err
	}

	raw, err := json.Marshal(ask)
	if err != nil {
		return controller.OwnerAsk{}, err
	}
	dir := store.Directive{
		ID:        "ask-" + ask.ID,
		RunID:     runID,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourcePlanner,
		Scope:     ask.LaneID,
		Ref:       ownerAskRefPrefix + ask.ID,
		Text:      string(raw),
		ExpiresAt: &deadline,
		Status:    store.DirectiveActive,
		CreatedAt: now,
	}
	if err := p.store.Directives().Add(ctx, dir); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return controller.OwnerAsk{}, err
	}
	if ask.LaneID != "" {
		if ln, err := p.store.Lanes().Get(ctx, runID, ask.LaneID); err == nil && ln != nil && ln.Status == lane.StatusRunning {
			ln.Status = lane.StatusBlocked
			ln.LastEventAt = now
			if err := p.store.Lanes().Upsert(ctx, *ln); err != nil {
				return controller.OwnerAsk{}, err
			}
		}
	}
	return ask, nil
}

func parseFutureDeadline(now time.Time, raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if t, err := time.Parse(time.RFC3339, s); err == nil && t.After(now) {
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("invalid deadline %q", raw)
	}
	return now.Add(d).UTC(), nil
}

// FormatReconciliationSummary produces a single-message summary of the applied reconciliation.
func FormatReconciliationSummary(runID, planRef string, rec Reconciliation, applied AppliedChanges) string {
	var parts []string
	header := fmt.Sprintf("Reconciled plan for %s", runID)
	if strings.TrimSpace(planRef) != "" {
		header = fmt.Sprintf("Reconciled plan %s for %s", strings.TrimSpace(planRef), runID)
	}
	parts = append(parts, header)

	if applied.ObjectiveUpdated {
		parts = append(parts, fmt.Sprintf("objective updated to %q", applied.NewObjective))
	}
	if len(applied.AddedItems) > 0 {
		parts = append(parts, fmt.Sprintf("added %d backlog item(s) [%s]", len(applied.AddedItems), strings.Join(applied.AddedItems, ", ")))
	}
	if len(applied.UpdatedItems) > 0 {
		parts = append(parts, fmt.Sprintf("updated %d backlog item(s) [%s]", len(applied.UpdatedItems), strings.Join(applied.UpdatedItems, ", ")))
	}
	if len(applied.DroppedItems) > 0 {
		parts = append(parts, fmt.Sprintf("dropped %d backlog item(s) [%s]", len(applied.DroppedItems), strings.Join(applied.DroppedItems, ", ")))
	}
	if len(applied.StoppedLanes) > 0 {
		parts = append(parts, fmt.Sprintf("stopped %d lane(s) [%s]", len(applied.StoppedLanes), strings.Join(applied.StoppedLanes, ", ")))
	}
	if len(applied.RebriefedLanes) > 0 {
		parts = append(parts, fmt.Sprintf("re-briefed %d lane(s) [%s]", len(applied.RebriefedLanes), strings.Join(applied.RebriefedLanes, ", ")))
	}
	if len(rec.Unmappable) > 0 {
		parts = append(parts, fmt.Sprintf("unmappable (%d): %s", len(rec.Unmappable), strings.Join(rec.Unmappable, "; ")))
	}
	if len(rec.Conflicts) > 0 {
		descs := make([]string, 0, len(rec.Conflicts))
		for i := range rec.Conflicts {
			descs = append(descs, rec.Conflicts[i].Description)
		}
		parts = append(parts, fmt.Sprintf("conflicts (%d): %s", len(rec.Conflicts), strings.Join(descs, "; ")))
	}
	if len(applied.OwnerAsks) > 0 {
		askIDs := make([]string, len(applied.OwnerAsks))
		for i := range applied.OwnerAsks {
			askIDs[i] = applied.OwnerAsks[i].ID
		}
		parts = append(parts, fmt.Sprintf("owner asks [%s]", strings.Join(askIDs, ", ")))
	}

	structured := strings.Join(parts, "; ") + "."
	if s := strings.TrimSpace(rec.Summary); s != "" {
		return singleLineSummary(s + " (" + structured + ")")
	}
	return singleLineSummary(structured)
}

func singleLineSummary(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// EditRecipe runs a fresh Planner thread in recipe_edit mode, recompiling the
// target recipe via the orca/recipe package and saving the new version to RecipeStore.
func (p *Planner) EditRecipe(ctx context.Context, runID, recipeName, editText string) (*Result, error) {
	if p.store == nil {
		return nil, errors.New("orca/planner: store is required")
	}
	if p.client == nil {
		return nil, errors.New("orca/planner: model client is required")
	}
	editText = strings.TrimSpace(editText)
	if editText == "" {
		return nil, recipe.ErrEmptyRecipeSource
	}
	start := p.clock.Now()

	run, err := p.store.Runs().Get(ctx, runID)
	if err != nil {
		return nil, err
	}

	targetName := cmp.Or(strings.TrimSpace(recipeName), strings.TrimSpace(run.DefaultRecipe), recipe.DefaultName)
	baseRecipe := p.resolveBaseRecipe(ctx, runID, targetName)

	goal := fmt.Sprintf("Compile recipe edit on %s (v%d) for run %s: %s", targetName, baseRecipe.Version, runID, editText)
	th, turnRec, err := p.startFreshThread(ctx, runID, goal)
	if err != nil {
		return nil, err
	}
	cleanupCtx := context.WithoutCancel(ctx)

	compileReq := recipe.CompileRequest{
		RunID:   runID,
		Base:    &baseRecipe,
		Name:    targetName,
		Source:  editText,
		Version: baseRecipe.Version + 1,
	}
	promptText := recipe.BuildCompileUserPrompt(compileReq)

	if _, err := p.store.Threads().AppendItem(ctx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnRec.ID,
		Kind:     store.ItemKindUserMessage,
		Content:  promptText,
	}); err != nil {
		_ = p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, store.TurnStatusFailed, model.Usage{}, err.Error())
		return nil, err
	}

	compiler := recipe.NewCompiler(
		p.client,
		recipe.WithModel(p.cfg.Model),
		recipe.WithMaxTokens(p.cfg.MaxTokens),
	)
	compileRes, compileErr := compiler.Compile(ctx, compileReq)
	if compileErr != nil {
		_ = p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, store.TurnStatusFailed, model.Usage{}, compileErr.Error())
		return nil, compileErr
	}

	compileRes.Recipe.RunID = runID
	saved, saveErr := p.store.Recipes().Save(ctx, compileRes.Recipe)
	if saveErr != nil {
		_ = p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, store.TurnStatusFailed, compileRes.Usage, saveErr.Error())
		return nil, saveErr
	}
	compileRes.Recipe = saved
	compileRes.EchoTable = saved.FormatTable()

	toolInput, _ := recipe.RecipeToToolInput(saved)
	_, _ = p.store.Threads().AppendItem(cleanupCtx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnRec.ID,
		Kind:     store.ItemKindToolCall,
		Name:     recipe.SubmitRecipeToolName,
		CallID:   "call-submit-recipe",
		Payload:  toolInput,
	})
	_, _ = p.store.Threads().AppendItem(cleanupCtx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnRec.ID,
		Kind:     store.ItemKindToolResult,
		Name:     recipe.SubmitRecipeToolName,
		CallID:   "call-submit-recipe",
		Content:  fmt.Sprintf("saved recipe %s@v%d", saved.Name, saved.Version),
	})
	_, _ = p.store.Threads().AppendItem(cleanupCtx, store.Item{
		ThreadID: th.ID,
		TurnID:   turnRec.ID,
		Kind:     store.ItemKindAssistantMessage,
		Content:  compileRes.EchoTable,
	})

	summary := fmt.Sprintf("Compiled recipe %s@v%d (base %s@v%d, phases=%v, merge=%s)",
		saved.Name, saved.Version, cmp.Or(saved.BaseRecipe, baseRecipe.Name), max(saved.BaseVersion, 1), saved.PhaseNames(), saved.MergePolicy.Mode)
	if len(saved.Unmapped) > 0 {
		summary += fmt.Sprintf("; unmapped: %s", strings.Join(saved.Unmapped, "; "))
	}

	rawDecArgs, _ := json.Marshal(map[string]any{
		"recipe":  saved.Name,
		"version": saved.Version,
		"source":  editText,
	})
	dec, decErr := p.store.Decisions().Append(cleanupCtx, store.Decision{
		RunID:        runID,
		Category:     store.DecisionJudgment,
		Actor:        ActorPlanner,
		ThreadID:     th.ID,
		TurnID:       turnRec.ID,
		Tool:         ToolRecipeCompile,
		Reason:       "recipe_edit: " + editText,
		Result:       summary,
		Args:         rawDecArgs,
		Vetted:       true,
		InputTokens:  compileRes.Usage.InputTokens,
		OutputTokens: compileRes.Usage.OutputTokens,
		CostUSD:      compileRes.Usage.CostUSD,
		LatencyMs:    max(p.clock.Now().Sub(start).Milliseconds(), 0),
	})
	if decErr != nil {
		_ = p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, store.TurnStatusFailed, compileRes.Usage, decErr.Error())
		return nil, decErr
	}

	if compErr := p.store.Threads().CompleteTurn(cleanupCtx, th.ID, turnRec.ID, store.TurnStatusCompleted, compileRes.Usage, ""); compErr != nil {
		return nil, compErr
	}

	res := &Result{
		ThreadID:     th.ID,
		TurnID:       turnRec.ID,
		Mode:         ModeRecipeEdit,
		Model:        p.cfg.Model,
		Summary:      summary,
		RecipeResult: compileRes,
		Decisions:    []store.Decision{dec},
		Usage:        compileRes.Usage,
		TurnCount:    1,
	}

	p.mu.Lock()
	p.lastResult = res
	p.mu.Unlock()

	return res, nil
}

func (p *Planner) resolveBaseRecipe(ctx context.Context, runID, name string) recipe.Recipe {
	versions, err := p.store.Recipes().ListVersions(ctx, runID, name)
	if err == nil && len(versions) > 0 {
		return versions[len(versions)-1].Clone()
	}
	def := recipe.Default()
	def.RunID = runID
	def.Name = name
	def.Version = 1
	return def
}
