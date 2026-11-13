package dispatcher

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
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

// Canonical tool names exposed to the Dispatcher model.
const (
	ToolBacklogAdd     = "backlog.add"
	ToolBacklogUpdate  = "backlog.update"
	ToolBacklogReorder = "backlog.reorder"
	ToolBacklogClose   = "backlog.close"
	ToolLaneSpawn      = "lane.spawn"
	ToolLaneSteer      = "lane.steer"
	ToolLaneRetry      = "lane.retry"
	ToolLaneStop       = "lane.stop"
	ToolOwnerAsk       = "owner.ask"
	ToolDirectiveNote  = "directive.note"
	ToolPlannerRequest = "planner.request"
	ToolReadLaneReport = "read.lane_report"
	ToolReadPR         = "read.pr"
	ToolReadCI         = "read.ci"
)

// DefaultMaxBriefBytes is the fallback brief/note size cap when a recipe does
// not specify Limits.MaxBriefBytes.
const DefaultMaxBriefBytes = 16 * 1024

// BacklogAddParams holds parameters for the backlog.add tool.
type BacklogAddParams struct {
	ID          string   `json:"id" jsonschema:"required,description=Unique backlog item ID"`
	Title       string   `json:"title" jsonschema:"required,description=Short title of the work item"`
	Description string   `json:"description,omitempty" jsonschema:"description=Detailed description of the work"`
	Acceptance  string   `json:"acceptance,omitempty" jsonschema:"description=Acceptance criteria"`
	Priority    string   `json:"priority,omitempty" jsonschema:"enum=p0,enum=p1,enum=p2,enum=p3,description=Item priority (p0..p3)"`
	RecipeName  string   `json:"recipe_name,omitempty" jsonschema:"description=Optional recipe name for this item"`
	Reason      string   `json:"reason,omitempty" jsonschema:"description=Reason for adding this item"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"description=IDs of backlog items this item depends on"`
	Tags        []string `json:"tags,omitempty" jsonschema:"description=Optional classification tags"`
	Order       int      `json:"order,omitempty" jsonschema:"description=Explicit sort order within priority"`
}

// BacklogUpdateParams holds parameters for the backlog.update tool.
type BacklogUpdateParams struct {
	ID          string   `json:"id" jsonschema:"required,description=Backlog item ID to update"`
	Title       string   `json:"title,omitempty" jsonschema:"description=Updated title"`
	Description string   `json:"description,omitempty" jsonschema:"description=Updated description"`
	Acceptance  string   `json:"acceptance,omitempty" jsonschema:"description=Updated acceptance criteria"`
	Priority    string   `json:"priority,omitempty" jsonschema:"enum=p0,enum=p1,enum=p2,enum=p3,description=Updated priority"`
	Status      string   `json:"status,omitempty" jsonschema:"enum=open,enum=blocked,description=Updated status. Never in_progress: lane.spawn sets that itself. Use backlog.close for done/dropped"`
	RecipeName  string   `json:"recipe_name,omitempty" jsonschema:"description=Updated recipe name"`
	Reason      string   `json:"reason" jsonschema:"required,description=Reason for updating this item"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"description=Replacement dependency IDs"`
	Tags        []string `json:"tags,omitempty" jsonschema:"description=Replacement tags"`
}

// BacklogReorderParams holds parameters for the backlog.reorder tool.
type BacklogReorderParams struct {
	Reason  string   `json:"reason,omitempty" jsonschema:"description=Reason for reordering"`
	ItemIDs []string `json:"item_ids" jsonschema:"required,description=Ordered backlog item IDs"`
}

// BacklogCloseParams holds parameters for the backlog.close tool.
type BacklogCloseParams struct {
	ID     string `json:"id" jsonschema:"required,description=Backlog item ID to close"`
	Status string `json:"status" jsonschema:"required,enum=done,enum=dropped,description=Terminal status (done or dropped)"`
	Reason string `json:"reason" jsonschema:"required,description=Reason for closing this item"`
}

// LaneSpawnParams holds parameters for the lane.spawn tool.
type LaneSpawnParams struct {
	ItemID         string `json:"item_id" jsonschema:"required" jsonschema_description:"ID of an open backlog item marked ready in the snapshot. Spawning marks it in_progress itself, so do not call backlog.update first"`
	LaneID         string `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID (defaults to item_id)"`
	Brief          string `json:"brief" jsonschema:"required,description=Task brief for the lane"`
	RecipeName     string `json:"recipe_name,omitempty" jsonschema:"description=Optional recipe name"`
	RecipeOverride string `json:"recipe_override,omitempty" jsonschema:"description=Optional natural-language recipe override or recipe name for this lane"`
	Branch         string `json:"branch,omitempty" jsonschema:"description=Optional git branch name"`
	BaseBranch     string `json:"base_branch,omitempty" jsonschema:"description=Optional base branch name"`
	Reason         string `json:"reason,omitempty" jsonschema:"description=Reason for spawning this lane"`
}

// LaneSteerParams holds parameters for the lane.steer tool.
type LaneSteerParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to steer"`
	Note   string `json:"note" jsonschema:"required,description=Steering instruction to inject into the lane"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason for steering this lane"`
}

// LaneRetryParams holds parameters for the lane.retry tool.
type LaneRetryParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to retry"`
	Phase  string `json:"phase,omitempty" jsonschema:"description=Optional phase to retry (defaults to current lane phase or recipe first phase)"`
	Note   string `json:"note,omitempty" jsonschema:"description=Optional additional brief or guidance for the retry"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason for retrying this lane"`
	Round  int    `json:"round,omitempty" jsonschema:"description=Target round number (must be > max recorded round for the phase; defaults to next unused round)"`
}

// LaneStopParams holds parameters for the lane.stop tool.
type LaneStopParams struct {
	LaneID string `json:"lane_id" jsonschema:"required,description=Lane ID to stop"`
	Reason string `json:"reason" jsonschema:"required,description=Reason for stopping the lane"`
}

// OwnerAskParams holds parameters for the owner.ask tool.
type OwnerAskParams struct {
	ID        string   `json:"id,omitempty" jsonschema:"description=Optional unique ask ID"`
	LaneID    string   `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID blocked on this question"`
	Question  string   `json:"question" jsonschema:"required,description=One-sentence decision question for the owner"`
	Default   string   `json:"default" jsonschema:"required" jsonschema_description:"Option auto-applied at deadline. If it is not already one of options it is added as an option, so a card's own stated default can be passed as is"`
	Deadline  string   `json:"deadline" jsonschema:"required" jsonschema_description:"Deadline as RFC3339 timestamp, positive duration (30m, 2h, 7d) or UTC clock time (05:30Z = next occurrence)"`
	SteerNote string   `json:"steer_note,omitempty" jsonschema:"description=Optional steering note to send when the default is applied"`
	Reason    string   `json:"reason,omitempty" jsonschema:"description=Why owner input is required"`
	Options   []string `json:"options" jsonschema:"required" jsonschema_description:"Concrete options the owner can choose from: short labels, a few words each (put the detail in the question)"`
}

// DirectiveNoteParams holds parameters for the directive.note tool.
type DirectiveNoteParams struct {
	ID     string `json:"id,omitempty" jsonschema:"description=Optional directive ID"`
	Fact   string `json:"fact" jsonschema:"required,description=Fact or operational note to persist in the store"`
	Expiry string `json:"expiry,omitempty" jsonschema_description:"Optional expiry as RFC3339 timestamp, duration (24h, 7d) or UTC clock time (05:30Z = next occurrence)"`
	Scope  string `json:"scope,omitempty" jsonschema:"description=Optional lane ID or scope"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Reason for recording this note"`
}

// PlannerRequestParams holds parameters for the planner.request tool.
type PlannerRequestParams struct {
	Reason  string `json:"reason" jsonschema:"required,description=Why replanning or plan reconciliation is needed"`
	PlanRef string `json:"plan_ref,omitempty" jsonschema:"description=Optional Nebula plan post reference"`
	Urgent  bool   `json:"urgent,omitempty" jsonschema:"description=Whether replanning is urgent"`
}

// ReadLaneReportParams holds parameters for the read.lane_report tool.
type ReadLaneReportParams struct {
	LaneID     string `json:"lane_id" jsonschema:"required,description=Lane ID whose phase report to read"`
	TerminalID string `json:"terminal_id,omitempty" jsonschema:"description=Optional terminal ID"`
	Reason     string `json:"reason,omitempty" jsonschema:"description=Optional reason for reading the report"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to return"`
}

// ReadPRParams holds parameters for the read.pr tool.
type ReadPRParams struct {
	LaneID   string `json:"lane_id" jsonschema:"required,description=ID of a lane listed in the snapshot Lanes table (never a placeholder such as '-')"`
	Reason   string `json:"reason,omitempty" jsonschema:"description=Optional reason for reading PR info"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to return"`
}

// ReadCIParams holds parameters for the read.ci tool.
type ReadCIParams struct {
	LaneID   string `json:"lane_id" jsonschema:"required,description=ID of a lane listed in the snapshot Lanes table (never a placeholder such as '-')"`
	Reason   string `json:"reason,omitempty" jsonschema:"description=Optional reason for reading CI info"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"description=Maximum bytes to return"`
}

// ProposedAction is one typed tool call proposed by the Dispatcher model; it must
// pass Vet before being applied.
type ProposedAction struct {
	BacklogAdd     *BacklogAddParams     `json:"-"`
	BacklogUpdate  *BacklogUpdateParams  `json:"-"`
	BacklogReorder *BacklogReorderParams `json:"-"`
	BacklogClose   *BacklogCloseParams   `json:"-"`
	LaneSpawn      *LaneSpawnParams      `json:"-"`
	LaneSteer      *LaneSteerParams      `json:"-"`
	LaneRetry      *LaneRetryParams      `json:"-"`
	LaneStop       *LaneStopParams       `json:"-"`
	OwnerAsk       *OwnerAskParams       `json:"-"`
	DirectiveNote  *DirectiveNoteParams  `json:"-"`
	PlannerRequest *PlannerRequestParams `json:"-"`
	ReadLaneReport *ReadLaneReportParams `json:"-"`
	ReadPR         *ReadPRParams         `json:"-"`
	ReadCI         *ReadCIParams         `json:"-"`
	ID             string                `json:"id,omitempty"`
	Tool           string                `json:"tool"`
	LaneID         string                `json:"lane_id,omitempty"`
	ItemID         string                `json:"item_id,omitempty"`
	Reason         string                `json:"reason,omitempty"`
	Args           json.RawMessage       `json:"args,omitempty"`
}

// Target returns a human-readable identifier (lane ID, item ID, or "run") for
// formatting rejection messages.
func (a ProposedAction) Target() string {
	return cmp.Or(a.LaneID, a.ItemID, "run")
}

// Proposal is a batch of ProposedActions returned by an Advisor turn.
type Proposal struct {
	Unresolved string           `json:"unresolved,omitempty"`
	Actions    []ProposedAction `json:"actions"`
}

// Advisor proposes orchestration actions from a rendered Store snapshot.
type Advisor interface {
	Propose(ctx context.Context, snapshot string) (Proposal, error)
}

// NormalizeToolName maps underscore tool aliases (e.g. "lane_spawn") to the
// canonical dotted tool names ("lane.spawn").
func NormalizeToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	if isKnownTool(trimmed) {
		return trimmed
	}
	if candidate := strings.Replace(trimmed, "_", ".", 1); isKnownTool(candidate) {
		return candidate
	}
	return trimmed
}

func isKnownTool(tool string) bool {
	switch tool {
	case ToolBacklogAdd, ToolBacklogUpdate, ToolBacklogReorder, ToolBacklogClose,
		ToolLaneSpawn, ToolLaneSteer, ToolLaneRetry, ToolLaneStop,
		ToolOwnerAsk, ToolDirectiveNote, ToolPlannerRequest,
		ToolReadLaneReport, ToolReadPR, ToolReadCI:
		return true
	default:
		return false
	}
}

type jsonAliases struct {
	Item      string `json:"item"`
	Lane      string `json:"lane"`
	Recipe    string `json:"recipe"`
	Message   string `json:"message"`
	Brief     string `json:"brief"`
	Text      string `json:"text"`
	ExpiresAt string `json:"expires_at"`
}

func decodeAliases(raw []byte) jsonAliases {
	var a jsonAliases
	_ = json.Unmarshal(raw, &a)
	return a
}

func decodeParams[T any](tool string, raw []byte) (*T, error) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode %s: %w", tool, err)
	}
	return &p, nil
}

// ParseAction decodes raw tool call JSON arguments into a typed ProposedAction.
func ParseAction(callID, toolName string, raw json.RawMessage) (ProposedAction, error) {
	canonical := NormalizeToolName(toolName)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte("{}")
	}
	act := ProposedAction{
		ID:   callID,
		Tool: canonical,
		Args: slices.Clone(trimmed),
	}

	switch canonical {
	case ToolBacklogAdd:
		p, err := decodeParams[BacklogAddParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogAdd = p
		act.ItemID = p.ID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("add backlog item %s", p.ID))

	case ToolBacklogUpdate:
		p, err := decodeParams[BacklogUpdateParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogUpdate = p
		act.ItemID = p.ID
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolBacklogReorder:
		p, err := decodeParams[BacklogReorderParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogReorder = p
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), "reorder backlog items")

	case ToolBacklogClose:
		p, err := decodeParams[BacklogCloseParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.BacklogClose = p
		act.ItemID = p.ID
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolLaneSpawn:
		p, err := decodeParams[LaneSpawnParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.ItemID == "" || p.LaneID == "" || (p.RecipeOverride == "" && p.RecipeName == "") {
			a := decodeAliases(trimmed)
			p.ItemID = cmp.Or(p.ItemID, a.Item)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			if p.RecipeOverride == "" && p.RecipeName == "" {
				p.RecipeOverride = a.Recipe
			}
		}
		act.LaneSpawn = p
		act.ItemID = p.ItemID
		act.LaneID = cmp.Or(strings.TrimSpace(p.LaneID), strings.TrimSpace(p.ItemID))
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("spawn lane %s for item %s", act.LaneID, p.ItemID))

	case ToolLaneSteer:
		p, err := decodeParams[LaneSteerParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" || p.Note == "" {
			a := decodeAliases(trimmed)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Note = cmp.Or(p.Note, a.Message)
		}
		act.LaneSteer = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Note))

	case ToolLaneRetry:
		p, err := decodeParams[LaneRetryParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" || p.Note == "" {
			a := decodeAliases(trimmed)
			p.LaneID = cmp.Or(p.LaneID, a.Lane)
			p.Note = cmp.Or(p.Note, a.Brief)
		}
		act.LaneRetry = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Note), fmt.Sprintf("retry lane %s", p.LaneID))

	case ToolLaneStop:
		p, err := decodeParams[LaneStopParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.LaneStop = p
		act.LaneID = p.LaneID
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolOwnerAsk:
		p, err := decodeParams[OwnerAskParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		p.Options, p.Default = canonicalizeAskDefault(p.Options, p.Default)
		act.OwnerAsk = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Question))

	case ToolDirectiveNote:
		p, err := decodeParams[DirectiveNoteParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.Fact == "" || p.Expiry == "" {
			a := decodeAliases(trimmed)
			p.Fact = cmp.Or(p.Fact, a.Text)
			p.Expiry = cmp.Or(p.Expiry, a.ExpiresAt)
		}
		act.DirectiveNote = p
		act.LaneID = p.Scope
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), strings.TrimSpace(p.Fact))

	case ToolPlannerRequest:
		p, err := decodeParams[PlannerRequestParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		act.PlannerRequest = p
		act.Reason = strings.TrimSpace(p.Reason)

	case ToolReadLaneReport:
		p, err := decodeParams[ReadLaneReportParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.ReadLaneReport = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("read lane report for %s", p.LaneID))

	case ToolReadPR:
		p, err := decodeParams[ReadPRParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.ReadPR = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("read PR info for %s", p.LaneID))

	case ToolReadCI:
		p, err := decodeParams[ReadCIParams](canonical, trimmed)
		if err != nil {
			return act, err
		}
		if p.LaneID == "" {
			p.LaneID = decodeAliases(trimmed).Lane
		}
		act.ReadCI = p
		act.LaneID = p.LaneID
		act.Reason = cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("read CI status for %s", p.LaneID))
	}
	return act, nil
}

// StateView holds the verified Store state inspected by Invariant checks.
//
//nolint:govet
type StateView struct {
	Now            time.Time
	Run            *store.Run
	Lanes          map[string]lane.Lane
	Backlog        map[string]store.BacklogItem
	Recipes        map[string]recipe.Recipe
	VersionRecipes map[string]recipe.Recipe
	MaxBriefBytes  int
	Memories       []store.MemoryEntry
}

// LoadStateView loads the current Run, Lanes, Backlog, and Recipes from st into
// a StateView for invariant vetting.
func LoadStateView(
	ctx context.Context,
	st store.Store,
	clk clock.Clock,
	runID string,
	maxBriefBytes int,
) (*StateView, error) {
	return loadStateViewWithCache(ctx, st, clk, runID, maxBriefBytes, nil)
}

func loadStateViewWithCache(
	ctx context.Context,
	st store.Store,
	clk clock.Clock,
	runID string,
	maxBriefBytes int,
	pinnedCache map[string]recipe.Recipe,
) (*StateView, error) {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	backlog, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	latestRecipes, err := st.Recipes().ListLatest(ctx, runID)
	if err != nil {
		return nil, err
	}
	memories, _ := st.Memories().List(ctx, store.MemoryFilter{RunID: runID})

	view := &StateView{
		Now:            clk.Now().UTC(),
		Run:            run,
		Lanes:          make(map[string]lane.Lane, len(lanes)),
		Backlog:        make(map[string]store.BacklogItem, len(backlog)),
		Recipes:        make(map[string]recipe.Recipe, len(latestRecipes)+1),
		VersionRecipes: make(map[string]recipe.Recipe, len(latestRecipes)+1),
		MaxBriefBytes:  maxBriefBytes,
		Memories:       memories,
	}
	defRecipe := recipe.Default()
	view.Recipes[recipe.DefaultName] = defRecipe
	view.VersionRecipes[recipeVersionKey(recipe.DefaultName, defRecipe.Version)] = defRecipe

	for i := range latestRecipes {
		r := latestRecipes[i]
		view.Recipes[r.Name] = r
		view.VersionRecipes[recipeVersionKey(r.Name, r.Version)] = r
	}
	for i := range lanes {
		ln := lanes[i]
		view.Lanes[ln.ID] = ln
		if ln.RecipeName != "" && ln.RecipeVersion > 0 {
			key := recipeVersionKey(ln.RecipeName, ln.RecipeVersion)
			if _, ok := view.VersionRecipes[key]; !ok {
				if cached, cOk := pinnedCache[key]; cOk {
					view.VersionRecipes[key] = cached
				} else if pinned, gErr := st.Recipes().Get(ctx, runID, ln.RecipeName, ln.RecipeVersion); gErr == nil && pinned != nil {
					view.VersionRecipes[key] = *pinned
					if pinnedCache != nil {
						pinnedCache[key] = *pinned
					}
				}
			}
		}
	}
	for i := range backlog {
		it := backlog[i]
		view.Backlog[it.ID] = it
	}
	return view, nil
}

func recipeVersionKey(name string, version int) string {
	return fmt.Sprintf("%s@%d", name, version)
}

// Lane looks up a lane by ID.
func (s *StateView) Lane(id string) (lane.Lane, bool) {
	if s == nil {
		return lane.Lane{}, false
	}
	ln, ok := s.Lanes[id]
	return ln, ok
}

// BacklogItem looks up a backlog item by ID.
func (s *StateView) BacklogItem(id string) (store.BacklogItem, bool) {
	if s == nil {
		return store.BacklogItem{}, false
	}
	it, ok := s.Backlog[id]
	return it, ok
}

func (s *StateView) resolveRecipeName(name string) string {
	if name != "" {
		return name
	}
	if s != nil && s.Run != nil && s.Run.DefaultRecipe != "" {
		return s.Run.DefaultRecipe
	}
	return recipe.DefaultName
}

// RecipeByName resolves the latest recipe for name (falling back to recipe.Default).
func (s *StateView) RecipeByName(name string) (recipe.Recipe, bool) {
	if s == nil {
		def := recipe.Default()
		return def, name == "" || name == recipe.DefaultName
	}
	name = s.resolveRecipeName(name)
	if r, ok := s.Recipes[name]; ok {
		return r, true
	}
	if name == recipe.DefaultName {
		return recipe.Default(), true
	}
	return recipe.Recipe{}, false
}

// RecipeForLane resolves the pinned or latest recipe for ln.
func (s *StateView) RecipeForLane(ln lane.Lane) (recipe.Recipe, bool) {
	name := s.resolveRecipeName(ln.RecipeName)
	if s != nil && ln.RecipeVersion > 0 {
		if r, ok := s.VersionRecipes[recipeVersionKey(name, ln.RecipeVersion)]; ok {
			return r, true
		}
	}
	return s.RecipeByName(name)
}

// ResolveSpawnRecipe resolves the base recipe and effective recipe (including
// named-recipe or deterministic overrides) for a lane.spawn action so vet and
// execution evaluate the same recipe.
func (s *StateView) ResolveSpawnRecipe(p LaneSpawnParams) (base recipe.Recipe, effective recipe.Recipe, hasBase bool) {
	item, _ := s.BacklogItem(strings.TrimSpace(p.ItemID))
	baseName := cmp.Or(strings.TrimSpace(p.RecipeName), item.RecipeName)
	base, hasBase = s.RecipeByName(baseName)
	if !hasBase {
		base = recipe.Default()
	}
	effective = base
	override := strings.TrimSpace(p.RecipeOverride)
	if override != "" {
		if existingOverride, exists := s.RecipeByName(override); exists && override != recipe.DefaultName {
			effective = existingOverride
		} else {
			effective = CompileRecipeOverride(base, override)
		}
	}
	return base, effective, hasBase
}

func (s *StateView) currentTime() time.Time {
	if s != nil && !s.Now.IsZero() {
		return s.Now
	}
	return time.Now().UTC()
}

func (s *StateView) maxBriefBytesForLane(laneID string) int {
	ln, _ := s.Lane(laneID)
	r, _ := s.RecipeForLane(ln)
	return s.EffectiveMaxBriefBytes(r)
}

// RunningLaneCount returns the number of lanes currently in StatusRunning.
func (s *StateView) RunningLaneCount() int {
	if s == nil {
		return 0
	}
	count := 0
	for id := range s.Lanes {
		if s.Lanes[id].Status == lane.StatusRunning {
			count++
		}
	}
	return count
}

// EffectiveMaxBriefBytes returns the brief size limit for r, falling back to
// s.MaxBriefBytes or DefaultMaxBriefBytes.
func (s *StateView) EffectiveMaxBriefBytes(r recipe.Recipe) int {
	if r.Limits.MaxBriefBytes > 0 {
		if s != nil && s.MaxBriefBytes > 0 {
			return min(r.Limits.MaxBriefBytes, s.MaxBriefBytes)
		}
		return r.Limits.MaxBriefBytes
	}
	if s != nil && s.MaxBriefBytes > 0 {
		return s.MaxBriefBytes
	}
	return DefaultMaxBriefBytes
}

// Invariant is a deterministic rule a proposed Dispatcher action must not violate.
type Invariant struct {
	Check func(st *StateView, action ProposedAction) error
	Name  string
}

// DefaultInvariants returns the standard invariant suite for Dispatcher tool calls.
func DefaultInvariants() []Invariant {
	return []Invariant{
		{
			Name:  "no merge outside recipe",
			Check: checkNoMergeOutsideRecipe,
		},
		{
			Name:  "tool must be in dispatcher toolkit",
			Check: checkKnownTool,
		},
		{
			Name:  "owner.ask requires options, default, and deadline",
			Check: checkOwnerAsk,
		},
		{
			Name:  "brief size within cap",
			Check: checkBriefSizeCap,
		},
		{
			Name:  "lane must exist",
			Check: checkLaneExists,
		},
		{
			Name:  "phase must be declared",
			Check: checkPhaseDeclared,
		},
		{
			Name:  "no spawn onto an occupied lane or ineligible item",
			Check: checkSpawnInvariants,
		},
		{
			Name:  "a spawn or retry must not reuse a recorded round",
			Check: checkNoRoundReuse,
		},
		{
			Name:  "lane state must allow steer, retry, or stop",
			Check: checkLaneActionState,
		},
		{
			Name:  "backlog mutation invariants",
			Check: checkBacklogInvariants,
		},
		{
			Name:  "directive.note and planner.request invariants",
			Check: checkDirectiveAndPlannerInvariants,
		},
	}
}

// Vet filters a Proposal to the actions that satisfy every invariant in invs,
// returning the accepted actions and formatted rejection reasons.
func Vet(st *StateView, p Proposal, invs []Invariant) (accepted []ProposedAction, rejected []string) {
	for i := range p.Actions {
		act := p.Actions[i]
		var bad string
		for _, inv := range invs {
			if err := inv.Check(st, act); err != nil {
				bad = fmt.Sprintf("%s [%s]: %s — %v", act.Tool, act.Target(), inv.Name, err)
				break
			}
		}
		if bad != "" {
			rejected = append(rejected, bad)
			continue
		}
		accepted = append(accepted, act)
	}
	return accepted, rejected
}

// VetAction checks a single ProposedAction against invs and returns an error
// describing the first violated invariant, or nil if all pass.
func VetAction(st *StateView, action ProposedAction, invs []Invariant) error {
	_, rejected := Vet(st, Proposal{Actions: []ProposedAction{action}}, invs)
	if len(rejected) > 0 {
		return errors.New(rejected[0])
	}
	return nil
}

func checkKnownTool(_ *StateView, act ProposedAction) error {
	if !isKnownTool(act.Tool) {
		return fmt.Errorf("tool %q is not permitted for the dispatcher", act.Tool)
	}
	return nil
}

func checkNoMergeOutsideRecipe(st *StateView, act ProposedAction) error {
	lowerTool := strings.ToLower(act.Tool)
	if strings.Contains(lowerTool, "merge") {
		return fmt.Errorf("direct merge tool %q is forbidden; merges are governed by the recipe's integrate phase and merge policy", act.Tool)
	}

	switch act.Tool {
	case ToolLaneSpawn:
		if act.LaneSpawn == nil {
			return nil
		}
		_, r, _ := st.ResolveSpawnRecipe(*act.LaneSpawn)
		if r.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(act.LaneSpawn.Brief) {
			return fmt.Errorf("brief instructs lane to merge directly, but recipe %q has merge mode %q (not %q)",
				r.Name, r.MergePolicy.Mode, recipe.MergeModeSelf)
		}

	case ToolLaneSteer:
		if act.LaneSteer == nil {
			return nil
		}
		if ln, ok := st.Lane(act.LaneSteer.LaneID); ok {
			r, _ := st.RecipeForLane(ln)
			if r.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(act.LaneSteer.Note) {
				return fmt.Errorf("steer note instructs lane to merge directly, but recipe %q has merge mode %q",
					r.Name, r.MergePolicy.Mode)
			}
		}

	case ToolLaneRetry:
		if act.LaneRetry == nil {
			return nil
		}
		ln, ok := st.Lane(act.LaneRetry.LaneID)
		if !ok {
			return nil
		}
		r, _ := st.RecipeForLane(ln)
		targetPhase := cmp.Or(strings.TrimSpace(act.LaneRetry.Phase), ln.Phase, r.FirstPhase())
		if targetPhase == recipe.TargetComplete {
			return fmt.Errorf("only the controller may mark a lane complete after recipe gates pass")
		}
		if r.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(act.LaneRetry.Note) {
			return fmt.Errorf("retry note instructs lane to merge directly, but recipe %q has merge mode %q",
				r.Name, r.MergePolicy.Mode)
		}
		if ph, hasPhase := r.Phase(targetPhase); hasPhase {
			if (targetPhase == recipe.PhaseIntegrate || hasGate(ph, "review_approved")) && r.MergePolicy.RequireReviewPass && ln.ApprovalStale() {
				return fmt.Errorf("cannot enter phase %q with stale or missing review approval (head=%q approval=%q)",
					targetPhase, ln.PRHead, ln.ApprovalSHA)
			}
			if targetPhase == recipe.PhaseIntegrate && r.MergePolicy.RequireCIGreen && !isCIGreen(ln.Checks) {
				return fmt.Errorf("cannot enter integrate phase %q while CI checks are %q",
					targetPhase, cmp.Or(ln.Checks, "pending"))
			}
		}

	case ToolBacklogClose:
		if act.BacklogClose == nil {
			return nil
		}
		if store.BacklogStatus(act.BacklogClose.Status) == store.BacklogDone {
			if it, ok := st.BacklogItem(act.BacklogClose.ID); ok && it.LaneID != "" {
				if ln, hasLane := st.Lane(it.LaneID); hasLane && ln.Status != lane.StatusDone {
					return fmt.Errorf("cannot close backlog item %q as done while assigned lane %q is %s (recipe not completed)",
						it.ID, ln.ID, ln.Status)
				}
			}
		}
	}
	return nil
}

func containsDirectMergeInstruction(text string) bool {
	lower := strings.ToLower(text)
	for _, pat := range []string{
		"gh pr merge",
		"hub merge",
		"merge the pr yourself",
		"merge your own pr",
		"git push origin main",
		"git push origin master",
	} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// canonicalizeAskDefault makes a default that is not (exactly or up to case and
// surrounding space) one of the options an option of its own. Decision cards
// routinely state "default if unanswered" separately from their choices, and
// the owner has to be able to see what will be applied if they do not answer.
// An empty default or an empty option list is left for checkOwnerAsk to reject.
func canonicalizeAskDefault(options []string, def string) ([]string, string) {
	def = strings.TrimSpace(def)
	if def == "" || len(options) == 0 {
		return options, def
	}
	// Options are trimmed so the default, which applyOwnerAsk also trims, still
	// compares equal to its option under controller.OwnerAsk.Validate.
	trimmed := make([]string, len(options))
	for i, opt := range options {
		trimmed[i] = strings.TrimSpace(opt)
	}
	for _, opt := range trimmed {
		if strings.EqualFold(opt, def) {
			return trimmed, opt
		}
	}
	return append(trimmed, def), def
}

func checkOwnerAsk(st *StateView, act ProposedAction) error {
	if act.Tool != ToolOwnerAsk {
		return nil
	}
	p := act.OwnerAsk
	if p == nil {
		return errors.New("missing owner.ask parameters")
	}
	if strings.TrimSpace(p.Question) == "" {
		return errors.New("question is required")
	}
	if len(p.Options) == 0 {
		return errors.New("options are required (decide, don't ask)")
	}
	for i, opt := range p.Options {
		if strings.TrimSpace(opt) == "" {
			return fmt.Errorf("option %d must not be empty", i)
		}
	}
	if strings.TrimSpace(p.Default) == "" {
		return errors.New("default option is required")
	}
	if !slices.Contains(p.Options, p.Default) {
		return fmt.Errorf("default %q must be one of options %v", p.Default, p.Options)
	}
	if strings.TrimSpace(p.Deadline) == "" {
		return errors.New("deadline is required")
	}
	now := st.currentTime()
	dl, err := parseFutureTime(now, p.Deadline)
	if err != nil {
		return fmt.Errorf("invalid deadline %q: %w", p.Deadline, err)
	}
	if !dl.After(now) {
		return fmt.Errorf("deadline %s must be in the future (now=%s)", dl.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	return nil
}

func checkBriefSizeCap(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolLaneSpawn:
		if act.LaneSpawn == nil {
			return errors.New("missing lane.spawn parameters")
		}
		if strings.TrimSpace(act.LaneSpawn.Brief) == "" {
			return errors.New("brief is required")
		}
		_, r, _ := st.ResolveSpawnRecipe(*act.LaneSpawn)
		maxBytes := st.EffectiveMaxBriefBytes(r)
		if len(act.LaneSpawn.Brief) > maxBytes {
			return fmt.Errorf("brief size %d bytes exceeds recipe cap of %d bytes", len(act.LaneSpawn.Brief), maxBytes)
		}

	case ToolLaneSteer:
		if act.LaneSteer == nil {
			return errors.New("missing lane.steer parameters")
		}
		if strings.TrimSpace(act.LaneSteer.Note) == "" {
			return errors.New("steer note is required")
		}
		maxBytes := st.maxBriefBytesForLane(act.LaneSteer.LaneID)
		if len(act.LaneSteer.Note) > maxBytes {
			return fmt.Errorf("steer note size %d bytes exceeds cap of %d bytes", len(act.LaneSteer.Note), maxBytes)
		}

	case ToolLaneRetry:
		if act.LaneRetry == nil || act.LaneRetry.Note == "" {
			return nil
		}
		maxBytes := st.maxBriefBytesForLane(act.LaneRetry.LaneID)
		if len(act.LaneRetry.Note) > maxBytes {
			return fmt.Errorf("retry note size %d bytes exceeds cap of %d bytes", len(act.LaneRetry.Note), maxBytes)
		}
	}
	return nil
}

func checkLaneExists(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolLaneSteer, ToolLaneRetry, ToolLaneStop,
		ToolReadLaneReport, ToolReadPR, ToolReadCI:
		if strings.TrimSpace(act.LaneID) == "" {
			return errors.New("lane_id is required")
		}
		if _, ok := st.Lane(act.LaneID); !ok {
			return fmt.Errorf("lane %q is not in the store", act.LaneID)
		}
	case ToolOwnerAsk:
		if act.OwnerAsk != nil && strings.TrimSpace(act.OwnerAsk.LaneID) != "" {
			if _, ok := st.Lane(act.OwnerAsk.LaneID); !ok {
				return fmt.Errorf("lane %q is not in the store", act.OwnerAsk.LaneID)
			}
		}
	}
	return nil
}

func checkPhaseDeclared(st *StateView, act ProposedAction) error {
	if act.Tool != ToolLaneRetry || act.LaneRetry == nil {
		return nil
	}
	phase := strings.TrimSpace(act.LaneRetry.Phase)
	if phase == "" || phase == recipe.TargetComplete {
		return nil
	}
	ln, ok := st.Lane(act.LaneRetry.LaneID)
	if !ok {
		return nil
	}
	r, ok := st.RecipeForLane(ln)
	if !ok {
		return fmt.Errorf("recipe %q not found for lane %q", ln.RecipeName, ln.ID)
	}
	if _, declared := r.Phase(phase); !declared {
		return fmt.Errorf("phase %q is not declared by recipe %q (%v)", phase, r.Name, r.PhaseNames())
	}
	return nil
}

func checkNoRoundReuse(st *StateView, act ProposedAction) error {
	if act.Tool != ToolLaneRetry || act.LaneRetry == nil {
		return nil
	}
	ln, ok := st.Lane(act.LaneRetry.LaneID)
	if !ok {
		return nil
	}
	r, _ := st.RecipeForLane(ln)
	phase := cmp.Or(strings.TrimSpace(act.LaneRetry.Phase), ln.Phase, r.FirstPhase())
	if phase == "" {
		return nil
	}
	recorded := ln.MaxRound(phase)
	if act.LaneRetry.Round > 0 && recorded > 0 && act.LaneRetry.Round <= recorded {
		return fmt.Errorf("round %d for phase %q would overwrite an existing attempt (max recorded is %d)",
			act.LaneRetry.Round, phase, recorded)
	}
	targetRound := act.LaneRetry.Round
	if targetRound <= 0 {
		targetRound = recorded + 1
	}
	if r.Limits.MaxRounds > 0 && targetRound > r.Limits.MaxRounds {
		return fmt.Errorf("round %d exceeds recipe %q max_rounds %d", targetRound, r.Name, r.Limits.MaxRounds)
	}
	return nil
}

func checkSpawnInvariants(st *StateView, act ProposedAction) error {
	if act.Tool != ToolLaneSpawn {
		return nil
	}
	p := act.LaneSpawn
	if p == nil {
		return errors.New("missing lane.spawn parameters")
	}
	itemID := strings.TrimSpace(p.ItemID)
	if itemID == "" {
		return errors.New("item_id is required")
	}
	item, ok := st.BacklogItem(itemID)
	if !ok {
		return fmt.Errorf("backlog item %q does not exist", itemID)
	}
	if item.Status.Terminal() {
		return fmt.Errorf("backlog item %q is already %s", itemID, item.Status)
	}
	if item.Status == store.BacklogInProgress {
		return fmt.Errorf("backlog item %q is already in progress on lane %q", itemID, cmp.Or(item.LaneID, itemID))
	}

	for _, depID := range item.DependsOn {
		dep, hasDep := st.BacklogItem(depID)
		if !hasDep {
			return fmt.Errorf("backlog item %q depends on unknown item %q", itemID, depID)
		}
		if dep.Status != store.BacklogDone {
			return fmt.Errorf("backlog item %q depends on %q which is %s (must be done)", itemID, depID, dep.Status)
		}
	}

	laneID := cmp.Or(strings.TrimSpace(p.LaneID), itemID)
	if strings.ContainsRune(laneID, '.') {
		return fmt.Errorf("lane_id %q must not contain '.'", laneID)
	}
	if existing, hasLane := st.Lane(laneID); hasLane {
		if existing.Status == lane.StatusDone {
			return fmt.Errorf("lane %q is already done", laneID)
		}
		if existing.Status != lane.StatusPlanned || existing.SessionID != "" || existing.TerminalID != "" ||
			len(existing.Sessions) > 0 || len(existing.Terminals) > 0 {
			return fmt.Errorf("lane %q is already %s; spawning onto an occupied lane would collide", laneID, existing.Status)
		}
	}

	_, r, hasRecipe := st.ResolveSpawnRecipe(*p)
	if !hasRecipe && strings.TrimSpace(p.RecipeName) != "" {
		return fmt.Errorf("unknown recipe %q", p.RecipeName)
	}
	if r.Limits.MaxConcurrentLanes > 0 && st.RunningLaneCount() >= r.Limits.MaxConcurrentLanes {
		return fmt.Errorf("concurrency limit reached (%d/%d running lanes)",
			st.RunningLaneCount(), r.Limits.MaxConcurrentLanes)
	}
	return nil
}

func checkLaneActionState(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolLaneSteer:
		ln, ok := st.Lane(act.LaneID)
		if !ok {
			return nil
		}
		if ln.Status.Terminal() {
			return fmt.Errorf("cannot steer lane %q in terminal status %s", ln.ID, ln.Status)
		}
		if ln.SessionID == "" && ln.TerminalID == "" {
			return fmt.Errorf("cannot steer lane %q before a session or terminal has started", ln.ID)
		}

	case ToolLaneRetry:
		ln, ok := st.Lane(act.LaneID)
		if !ok {
			return nil
		}
		if ln.Status == lane.StatusRunning {
			return fmt.Errorf("lane %q is currently running; stop or wait for it before retrying", ln.ID)
		}
		if ln.Status == lane.StatusDone {
			return fmt.Errorf("lane %q is already done", ln.ID)
		}

	case ToolLaneStop:
		if act.LaneStop == nil || strings.TrimSpace(act.LaneStop.Reason) == "" {
			return errors.New("reason is required to stop a lane")
		}
		ln, ok := st.Lane(act.LaneID)
		if !ok {
			return nil
		}
		if ln.Status.Terminal() {
			return fmt.Errorf("lane %q is already in terminal status %s", ln.ID, ln.Status)
		}
	}
	return nil
}

func checkBacklogInvariants(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolBacklogAdd:
		p := act.BacklogAdd
		if p == nil {
			return errors.New("missing backlog.add parameters")
		}
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("backlog item id is required")
		}
		if strings.TrimSpace(p.Title) == "" {
			return errors.New("backlog item title is required")
		}
		if _, exists := st.BacklogItem(p.ID); exists {
			return fmt.Errorf("backlog item %q already exists", p.ID)
		}
		if p.Priority != "" && !store.Priority(p.Priority).Valid() {
			return fmt.Errorf("invalid priority %q", p.Priority)
		}
		for _, depID := range p.DependsOn {
			if depID == p.ID {
				return fmt.Errorf("backlog item %q cannot depend on itself", p.ID)
			}
			if _, ok := st.BacklogItem(depID); !ok {
				return fmt.Errorf("dependency %q does not exist in backlog", depID)
			}
		}

	case ToolBacklogUpdate:
		p := act.BacklogUpdate
		if p == nil {
			return errors.New("missing backlog.update parameters")
		}
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("backlog item id is required")
		}
		if strings.TrimSpace(p.Reason) == "" {
			return errors.New("reason is required for backlog.update")
		}
		if _, exists := st.BacklogItem(p.ID); !exists {
			return fmt.Errorf("backlog item %q does not exist", p.ID)
		}
		if p.Priority != "" && !store.Priority(p.Priority).Valid() {
			return fmt.Errorf("invalid priority %q", p.Priority)
		}
		if p.Status != "" {
			status := store.BacklogStatus(p.Status)
			if !status.Valid() {
				return fmt.Errorf("invalid backlog status %q", p.Status)
			}
			if status.Terminal() {
				return fmt.Errorf("use %s to set terminal status %q", ToolBacklogClose, p.Status)
			}
			if status == store.BacklogInProgress {
				return fmt.Errorf("status %q is owned by %s, which marks the item in_progress when it spawns the lane; do not set it by hand", p.Status, ToolLaneSpawn)
			}
		}

	case ToolBacklogReorder:
		p := act.BacklogReorder
		if p == nil || len(p.ItemIDs) == 0 {
			return errors.New("item_ids is required for backlog.reorder")
		}
		seen := make(map[string]bool, len(p.ItemIDs))
		for _, id := range p.ItemIDs {
			if seen[id] {
				return fmt.Errorf("duplicate item_id %q in backlog.reorder", id)
			}
			seen[id] = true
			if _, exists := st.BacklogItem(id); !exists {
				return fmt.Errorf("backlog item %q does not exist", id)
			}
		}

	case ToolBacklogClose:
		p := act.BacklogClose
		if p == nil {
			return errors.New("missing backlog.close parameters")
		}
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("backlog item id is required")
		}
		if strings.TrimSpace(p.Reason) == "" {
			return errors.New("reason is required for backlog.close")
		}
		if _, exists := st.BacklogItem(p.ID); !exists {
			return fmt.Errorf("backlog item %q does not exist", p.ID)
		}
		status := store.BacklogStatus(p.Status)
		if !status.Terminal() {
			return fmt.Errorf("backlog.close requires status 'done' or 'dropped', got %q", p.Status)
		}
	}
	return nil
}

func checkDirectiveAndPlannerInvariants(st *StateView, act ProposedAction) error {
	switch act.Tool {
	case ToolDirectiveNote:
		p := act.DirectiveNote
		if p == nil || strings.TrimSpace(p.Fact) == "" {
			return errors.New("fact is required for directive.note")
		}
		if strings.TrimSpace(p.Expiry) != "" {
			now := st.currentTime()
			exp, err := parseFutureTime(now, p.Expiry)
			if err != nil {
				return fmt.Errorf("invalid expiry %q: %w", p.Expiry, err)
			}
			if !exp.After(now) {
				return fmt.Errorf("expiry %s must be in the future (now=%s)", exp.Format(time.RFC3339), now.Format(time.RFC3339))
			}
		}

	case ToolPlannerRequest:
		p := act.PlannerRequest
		if p == nil || strings.TrimSpace(p.Reason) == "" {
			return errors.New("reason is required for planner.request")
		}
	}
	return nil
}

func parseFutureTime(now time.Time, raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, errors.New("empty time string")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(d).UTC(), nil
	}
	// Owner cards state deadlines as "7d" or as a UTC clock time ("05:30Z").
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return now.AddDate(0, 0, n).UTC(), nil
		}
	}
	if clock, err := time.Parse("15:04Z", s); err == nil {
		now = now.UTC()
		t := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, time.UTC)
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp, duration (e.g. 30m, 2h, 7d) or UTC clock time (e.g. 05:30Z, next occurrence)")
}

func isCIGreen(checks string) bool {
	switch strings.ToLower(strings.TrimSpace(checks)) {
	case "green", "pass", "passed", "success", "ok", "true":
		return true
	default:
		return false
	}
}

func hasGate(p recipe.Phase, gateName string) bool {
	return slices.ContainsFunc(p.Gates, func(g recipe.Gate) bool {
		return g.Name == gateName
	})
}
