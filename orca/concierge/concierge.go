package concierge

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
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	ActorConcierge      = "concierge"
	DefaultModel        = "claude-haiku-4-5-20251001"
	DefaultMaxTurns     = model.DefaultMaxTurns
	DefaultMaxTokens    = model.DefaultMaxTokens
	DefaultMaxReadBytes = 8192
	DefaultBacklogLimit = 25
	DefaultDecisionLim  = 25
)

// Canonical tool names exposed to the Concierge model.
const (
	ToolStatus          = "status"
	ToolLaneShow        = "lane.show"
	ToolDecisionsSince  = "decisions.since"
	ToolBacklog         = "backlog"
	ToolDirectiveSubmit = "directive.submit"
)

// Directive submission kinds accepted by directive.submit.
const (
	KindRule       = "rule"
	KindPlanImport = "plan_import"
	KindRecipeEdit = "recipe_edit"
	KindOrder      = "order"
)

// ErrUncitedAnswer is returned when the model's final answer fails to cite any
// Store fact or fabricates a non-existent Store ID.
var ErrUncitedAnswer = errors.New("orca/concierge: answer does not cite store facts")

// DirectiveHandler is an optional callback invoked synchronously when directive.submit writes a directive.
type DirectiveHandler func(ctx context.Context, runID string, dir store.Directive) (string, error)

// Config configures a Concierge instance.
type Config struct {
	OnDirective      DirectiveHandler
	Model            string
	SystemPrompt     string
	MaxTurns         int
	MaxTokens        int
	MaxReadBytes     int
	DisableCitations bool
}

func (c Config) withDefaults() Config {
	c.Model = cmp.Or(strings.TrimSpace(c.Model), DefaultModel)
	c.SystemPrompt = cmp.Or(c.SystemPrompt, SystemPrompt)
	if c.MaxTurns <= 0 {
		c.MaxTurns = DefaultMaxTurns
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = DefaultMaxTokens
	}
	if c.MaxReadBytes <= 0 {
		c.MaxReadBytes = DefaultMaxReadBytes
	}
	return c
}

// StatusParams holds optional parameters for the status tool.
type StatusParams struct {
	BacklogLimit int `json:"backlog_limit,omitempty" jsonschema:"description=Max backlog items to list in the status overview (default 10)"`
}

// LaneShowParams holds parameters for the lane.show tool.
type LaneShowParams struct {
	LaneID   string `json:"lane_id" jsonschema:"required,description=Lane ID to inspect"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"description=Max bytes to return"`
}

// DecisionsSinceParams holds parameters for the decisions.since tool.
type DecisionsSinceParams struct {
	SinceTime string `json:"since_time,omitempty" jsonschema:"description=Optional RFC3339 timestamp or lookback duration (e.g. 1h, 30m)"`
	LaneID    string `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID filter"`
	Actor     string `json:"actor,omitempty" jsonschema:"description=Optional actor filter (controller, dispatcher, concierge, planner)"`
	Category  string `json:"category,omitempty" jsonschema:"enum=auto,enum=judgment,description=Optional decision category filter (auto or judgment)"`
	SinceSeq  int64  `json:"since_seq,omitempty" jsonschema:"description=Return decisions with seq > since_seq"`
	Limit     int    `json:"limit,omitempty" jsonschema:"description=Max decisions to return (default 25)"`
}

// BacklogParams holds parameters for the backlog tool.
type BacklogParams struct {
	ItemID   string   `json:"item_id,omitempty" jsonschema:"description=Optional backlog item ID to inspect in detail"`
	Status   string   `json:"status,omitempty" jsonschema:"enum=open,enum=in_progress,enum=blocked,enum=done,enum=dropped,description=Optional status filter"`
	LaneID   string   `json:"lane_id,omitempty" jsonschema:"description=Optional lane ID filter"`
	Statuses []string `json:"statuses,omitempty" jsonschema:"description=Optional list of status filters"`
	Limit    int      `json:"limit,omitempty" jsonschema:"description=Max backlog items to return (default 25)"`
}

// DirectiveSubmitParams holds parameters for the directive.submit write tool.
type DirectiveSubmitParams struct {
	ID     string `json:"id,omitempty" jsonschema:"description=Optional directive ID"`
	Kind   string `json:"kind" jsonschema:"required,enum=rule,enum=plan_import,enum=recipe_edit,enum=order,description=Directive kind (rule, plan_import, recipe_edit, order)"`
	Text   string `json:"text,omitempty" jsonschema:"description=Directive rule, order, or recipe edit text"`
	Ref    string `json:"ref,omitempty" jsonschema:"description=Plan reference (file path or Nebula post ID) or target recipe name"`
	Scope  string `json:"scope,omitempty" jsonschema:"description=Optional scope (lane ID, recipe name, or run)"`
	Expiry string `json:"expiry,omitempty" jsonschema:"description=Optional expiry as RFC3339 timestamp or duration (e.g. 24h)"`
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional reason for submitting the directive"`
}

var conciergeToolDefs = []model.ToolDefinition{
	{Name: ToolStatus, Description: "Read run status overview including objective, lease, backlog counts, lanes, directives, and latest sequence numbers", InputSchema: model.SchemaFor[StatusParams]()},
	{Name: ToolLaneShow, Description: "Read detailed state, PR/CI checks, latest phase report, and recent events/decisions for a lane", InputSchema: model.SchemaFor[LaneShowParams]()},
	{Name: ToolDecisionsSince, Description: "Read audit decisions from DecisionStore filtered by sequence, time, lane, actor, or category", InputSchema: model.SchemaFor[DecisionsSinceParams]()},
	{Name: ToolBacklog, Description: "Read prioritized backlog items and mutation history from BacklogStore", InputSchema: model.SchemaFor[BacklogParams]()},
	{Name: ToolDirectiveSubmit, Description: "Submit a typed owner directive (rule, plan_import, recipe_edit, order) to DirectiveStore", InputSchema: model.SchemaFor[DirectiveSubmitParams]()},
}

// NormalizeToolName maps underscore aliases (e.g. "lane_show") to canonical dotted tool names.
func NormalizeToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	if isKnownTool(trimmed) {
		return trimmed
	}
	if trimmed == "backlog.list" || trimmed == "backlog_list" {
		return ToolBacklog
	}
	if candidate := strings.Replace(trimmed, "_", ".", 1); isKnownTool(candidate) {
		return candidate
	}
	return trimmed
}

func isKnownTool(name string) bool {
	switch name {
	case ToolStatus, ToolLaneShow, ToolDecisionsSince, ToolBacklog, ToolDirectiveSubmit:
		return true
	default:
		return false
	}
}

// ParseDirectiveKind maps a directive.submit kind string to store.DirectiveKind.
func ParseDirectiveKind(raw string) (store.DirectiveKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case KindRule, string(store.DirectiveStanding):
		return store.DirectiveStanding, nil
	case KindPlanImport:
		return store.DirectivePlanImport, nil
	case KindRecipeEdit:
		return store.DirectiveRecipeEdit, nil
	case KindOrder, string(store.DirectiveInstruction):
		return store.DirectiveInstruction, nil
	default:
		return "", fmt.Errorf("%w: unsupported directive kind %q (want rule, plan_import, recipe_edit, or order)", store.ErrInvalidArgument, raw)
	}
}

// TurnResult captures the outcome of one Concierge turn.
type TurnResult struct {
	ThreadID            string
	TurnID              string
	Model               string
	Answer              string
	ToolCalls           []string
	SubmittedDirectives []store.Directive
	Decisions           []store.Decision
	CitedFacts          []string
	Usage               model.Usage
	TurnCount           int
}

// Concierge hosts owner chat threads backed by read-only Store tools and directive.submit.
type Concierge struct {
	store      store.Store
	driver     lane.LaneDriver
	clock      clock.Clock
	client     model.ModelClient
	lastResult *TurnResult
	cfg        Config
	seq        int64
	mu         sync.Mutex
}

// New constructs a new Concierge.
func New(
	cfg Config,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	client model.ModelClient,
) *Concierge {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	return &Concierge{
		cfg:    cfg.withDefaults(),
		store:  st,
		driver: drv,
		clock:  clk,
		client: client,
	}
}

// ToolDefinitions returns a copy of the tool definitions exposed to ModelClient.
func (c *Concierge) ToolDefinitions() []model.ToolDefinition {
	out := make([]model.ToolDefinition, len(conciergeToolDefs))
	for i := range conciergeToolDefs {
		out[i] = conciergeToolDefs[i].Clone()
	}
	return out
}

// LastResult returns a copy of the most recent TurnResult.
func (c *Concierge) LastResult() *TurnResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastResult == nil {
		return nil
	}
	cp := *c.lastResult
	cp.ToolCalls = slices.Clone(c.lastResult.ToolCalls)
	cp.SubmittedDirectives = slices.Clone(c.lastResult.SubmittedDirectives)
	cp.Decisions = slices.Clone(c.lastResult.Decisions)
	cp.CitedFacts = slices.Clone(c.lastResult.CitedFacts)
	return &cp
}

func (c *Concierge) nextID(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("%s-%d", prefix, c.seq)
}

func (c *Concierge) syncSeq(ctx context.Context, runID string) {
	var maxObserved int64
	if threads, err := c.store.Threads().ListThreads(ctx, store.ThreadFilter{
		RunID: runID,
		Role:  store.ThreadRoleConcierge,
	}); err == nil {
		maxObserved = max(maxObserved, int64(len(threads)))
		for i := range threads {
			maxObserved = max(maxObserved, trailingSeqNumber(threads[i].ID))
		}
	}
	if dirs, err := c.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID}); err == nil {
		maxObserved = max(maxObserved, int64(len(dirs)))
		for i := range dirs {
			maxObserved = max(maxObserved, trailingSeqNumber(dirs[i].ID))
		}
	}
	c.mu.Lock()
	if maxObserved > c.seq {
		c.seq = maxObserved
	}
	c.mu.Unlock()
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

// StartThread creates a fresh Concierge thread for a new owner conversation.
func (c *Concierge) StartThread(ctx context.Context, runID, goal string) (store.Thread, error) {
	if c.store == nil {
		return store.Thread{}, errors.New("orca/concierge: store is required")
	}
	if strings.TrimSpace(runID) == "" {
		return store.Thread{}, fmt.Errorf("%w: run_id is required", store.ErrInvalidArgument)
	}
	c.syncSeq(ctx, runID)
	threadID := c.nextID("concierge-" + runID)
	if strings.TrimSpace(goal) == "" {
		goal = "Owner concierge conversation for run " + runID
	}
	return c.store.Threads().StartThread(ctx, store.Thread{
		ID:     threadID,
		RunID:  runID,
		Role:   store.ThreadRoleConcierge,
		Goal:   strings.TrimSpace(goal),
		Status: store.ThreadStatusIdle,
	})
}

// ArchiveThread marks an owner conversation thread as archived.
func (c *Concierge) ArchiveThread(ctx context.Context, threadID string) error {
	if c.store == nil {
		return errors.New("orca/concierge: store is required")
	}
	return c.store.Threads().UpdateThreadStatus(ctx, threadID, store.ThreadStatusArchived)
}

// Ask starts a fresh Concierge conversation thread for runID and answers question.
func (c *Concierge) Ask(ctx context.Context, runID, question string) (*TurnResult, error) {
	th, err := c.StartThread(ctx, runID, question)
	if err != nil {
		return nil, err
	}
	return c.AskInThread(ctx, runID, th.ID, question)
}

type conciergeTurnState struct {
	threadID            string
	turnID              string
	toolCalls           []string
	submittedDirectives []store.Directive
	decisions           []store.Decision
	turnUsage           model.Usage
	turnUsageRecorded   bool
}

// AskInThread executes a single conversational turn on an existing Concierge thread.
func (c *Concierge) AskInThread(ctx context.Context, runID, threadID, question string) (*TurnResult, error) {
	if c.store == nil {
		return nil, errors.New("orca/concierge: store is required")
	}
	if c.client == nil {
		return nil, errors.New("orca/concierge: model client is required")
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("%w: question is required", store.ErrInvalidArgument)
	}

	c.syncSeq(ctx, runID)
	priorMsgs, err := c.loadThreadMessages(ctx, threadID)
	if err != nil {
		return nil, err
	}

	turnID := c.nextID(threadID + "-turn")
	turnRec, err := c.store.Threads().StartTurn(ctx, store.Turn{
		ID:       turnID,
		ThreadID: threadID,
		RunID:    runID,
		Input:    question,
	})
	if err != nil {
		return nil, err
	}

	cleanupCtx := context.WithoutCancel(ctx)
	userContent := question
	if len(turnRec.Steers) > 0 {
		userContent = question + "\n\n[Steer: " + strings.Join(turnRec.Steers, "; ") + "]"
	}

	if _, err := c.store.Threads().AppendItem(ctx, store.Item{
		ThreadID: threadID,
		TurnID:   turnID,
		Kind:     store.ItemKindUserMessage,
		Content:  userContent,
	}); err != nil {
		_ = c.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, err.Error())
		return nil, err
	}

	state := &conciergeTurnState{
		threadID: threadID,
		turnID:   turnID,
	}

	reg := c.buildToolRegistry(runID, state)
	msgs := slices.Clone(priorMsgs)
	msgs = append(msgs, model.NewUserMessage(userContent))

	req := model.Request{
		Model:     c.cfg.Model,
		System:    c.cfg.SystemPrompt,
		Messages:  msgs,
		Tools:     reg.Definitions(),
		MaxTokens: c.cfg.MaxTokens,
	}

	loopRes, loopErr := model.RunToolLoop(
		ctx,
		c.client,
		req,
		reg,
		model.WithMaxTurns(c.cfg.MaxTurns),
		model.WithOnTurn(func(_ int, resp model.Response) {
			state.turnUsage = resp.Usage
			state.turnUsageRecorded = false
			if txt := strings.TrimSpace(resp.Text()); txt != "" {
				_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
					ThreadID: threadID,
					TurnID:   turnID,
					Kind:     store.ItemKindAssistantMessage,
					Content:  txt,
				})
			}
			for _, tc := range resp.ToolCalls() {
				if !reg.Has(tc.Name) {
					toolName := tc.Name
					reg.Register(model.ToolDefinition{Name: toolName}, func(callCtx context.Context, raw json.RawMessage) (string, error) {
						return c.handleRawToolCall(callCtx, runID, state, toolName, raw)
					})
				}
			}
		}),
		model.WithOnToolCall(func(_ int, call model.ToolCall, resBlock model.ContentBlock) {
			_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
				ThreadID: threadID,
				TurnID:   turnID,
				Kind:     store.ItemKindToolCall,
				Name:     call.Name,
				CallID:   call.ID,
				Payload:  call.Input,
			})
			_, _ = c.store.Threads().AppendItem(cleanupCtx, store.Item{
				ThreadID: threadID,
				TurnID:   turnID,
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
		answer    string
	)
	if loopRes != nil {
		usage = loopRes.Usage
		turnCount = loopRes.TurnCount
		answer = strings.TrimSpace(loopRes.Response.Text())
	}

	var citedFacts []string
	if loopErr == nil {
		var citeErr error
		citedFacts, citeErr = VerifyAnswerCitations(ctx, c.store, runID, answer, state.submittedDirectives)
		if citeErr != nil && !c.cfg.DisableCitations {
			loopErr = citeErr
		}
	}

	turnStatus := store.TurnStatusCompleted
	errMsg := ""
	if loopErr != nil {
		turnStatus = store.TurnStatusFailed
		errMsg = loopErr.Error()
	}
	if compErr := c.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, turnStatus, usage, errMsg); compErr != nil && loopErr == nil {
		loopErr = compErr
	}

	res := &TurnResult{
		ThreadID:            threadID,
		TurnID:              turnID,
		Model:               c.cfg.Model,
		Answer:              answer,
		ToolCalls:           state.toolCalls,
		SubmittedDirectives: state.submittedDirectives,
		Decisions:           state.decisions,
		CitedFacts:          citedFacts,
		Usage:               usage,
		TurnCount:           turnCount,
	}

	c.mu.Lock()
	c.lastResult = res
	c.mu.Unlock()

	if loopErr != nil {
		return res, loopErr
	}
	return res, nil
}

func (c *Concierge) loadThreadMessages(ctx context.Context, threadID string) ([]model.Message, error) {
	items, err := c.store.Threads().ListItems(ctx, threadID, "", 0, 0)
	if err != nil {
		return nil, err
	}
	var msgs []model.Message
	for i := range items {
		it := items[i]
		switch it.Kind {
		case store.ItemKindUserMessage:
			if strings.TrimSpace(it.Content) != "" {
				msgs = append(msgs, model.NewUserMessage(it.Content))
			}
		case store.ItemKindAssistantMessage:
			if strings.TrimSpace(it.Content) != "" {
				msgs = append(msgs, model.NewAssistantMessage(model.NewTextBlock(it.Content)))
			}
		}
	}
	return msgs, nil
}

func (c *Concierge) buildToolRegistry(runID string, state *conciergeTurnState) *model.ToolRegistry {
	reg := model.NewToolRegistry()
	for i := range conciergeToolDefs {
		def := conciergeToolDefs[i]
		reg.Register(def, func(ctx context.Context, raw json.RawMessage) (string, error) {
			return c.handleRawToolCall(ctx, runID, state, def.Name, raw)
		})
	}
	return reg
}

func decodeToolParams[T any](tool string, raw []byte) (T, error) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("decode %s: %w", tool, err)
	}
	return p, nil
}

func (c *Concierge) handleRawToolCall(
	ctx context.Context,
	runID string,
	state *conciergeTurnState,
	toolName string,
	raw json.RawMessage,
) (string, error) {
	canonical := NormalizeToolName(toolName)
	if state != nil {
		state.toolCalls = append(state.toolCalls, canonical)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte("{}")
	}

	switch canonical {
	case ToolStatus:
		p, err := decodeToolParams[StatusParams](canonical, trimmed)
		if err != nil {
			return "", err
		}
		return c.execStatus(ctx, runID, p)

	case ToolLaneShow:
		p, err := decodeToolParams[LaneShowParams](canonical, trimmed)
		if err != nil {
			return "", err
		}
		return c.execLaneShow(ctx, runID, p)

	case ToolDecisionsSince:
		p, err := decodeToolParams[DecisionsSinceParams](canonical, trimmed)
		if err != nil {
			return "", err
		}
		return c.execDecisionsSince(ctx, runID, p)

	case ToolBacklog:
		p, err := decodeToolParams[BacklogParams](canonical, trimmed)
		if err != nil {
			return "", err
		}
		return c.execBacklog(ctx, runID, p)

	case ToolDirectiveSubmit:
		p, err := decodeToolParams[DirectiveSubmitParams](canonical, trimmed)
		if err != nil {
			return "", err
		}
		return c.execDirectiveSubmit(ctx, runID, state, trimmed, p)

	default:
		return "", fmt.Errorf("unsupported concierge tool %q", toolName)
	}
}

func (c *Concierge) execStatus(ctx context.Context, runID string, p StatusParams) (string, error) {
	now := c.clock.Now().UTC()
	run, err := c.store.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}

	backlogItems, err := c.store.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return "", err
	}

	lanes, err := c.store.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return "", err
	}

	directives, err := c.store.Directives().List(ctx, store.DirectiveFilter{
		RunID:      runID,
		ActiveOnly: true,
	})
	if err != nil {
		return "", err
	}

	decisions, err := c.store.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
	if err != nil {
		return "", err
	}

	events, err := c.store.Events().List(ctx, store.EventFilter{RunID: runID})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Run: %s\n", run.ID)
	fmt.Fprintf(&b, "Now: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "Objective: %s\n", cmp.Or(strings.TrimSpace(run.Objective), "(none)"))
	fmt.Fprintf(&b, "PlanRef: %s\n", cmp.Or(strings.TrimSpace(run.PlanRef), "(none)"))
	fmt.Fprintf(&b, "DefaultRecipe: %s\n", cmp.Or(strings.TrimSpace(run.DefaultRecipe), recipe.DefaultName))
	if run.Lease != nil {
		fmt.Fprintf(&b, "Lease: holder=%s expires=%s expired=%t\n",
			run.Lease.HolderID, run.Lease.ExpiresAt.UTC().Format(time.RFC3339), run.Lease.Expired(now))
	} else {
		b.WriteString("Lease: (unheld)\n")
	}

	statusCounts := make(map[store.BacklogStatus]int)
	for i := range backlogItems {
		statusCounts[backlogItems[i].Status]++
	}
	fmt.Fprintf(&b, "\nBacklog (%d total: open=%d in_progress=%d blocked=%d done=%d dropped=%d):\n",
		len(backlogItems),
		statusCounts[store.BacklogOpen],
		statusCounts[store.BacklogInProgress],
		statusCounts[store.BacklogBlocked],
		statusCounts[store.BacklogDone],
		statusCounts[store.BacklogDropped],
	)
	limit := p.BacklogLimit
	if limit <= 0 {
		limit = 10
	}
	for i := 0; i < min(len(backlogItems), limit); i++ {
		it := backlogItems[i]
		fmt.Fprintf(&b, "- %s [%s %s] title=%q lane=%s\n",
			it.ID, it.Priority, it.Status, it.Title, cmp.Or(it.LaneID, "-"))
	}

	laneCounts := make(map[lane.Status]int)
	for i := range lanes {
		laneCounts[lanes[i].Status]++
	}
	fmt.Fprintf(&b, "\nLanes (%d total: planned=%d running=%d blocked=%d failed=%d done=%d):\n",
		len(lanes),
		laneCounts[lane.StatusPlanned],
		laneCounts[lane.StatusRunning],
		laneCounts[lane.StatusBlocked],
		laneCounts[lane.StatusFailed],
		laneCounts[lane.StatusDone],
	)
	for i := range lanes {
		ln := lanes[i]
		fmt.Fprintf(&b, "- %s: item=%s status=%s phase=%s round=%d attempt=%d recipe=%s pr=#%d head=%s checks=%s approval_stale=%t age=%s\n",
			ln.ID,
			cmp.Or(ln.ItemID, "-"),
			ln.Status,
			cmp.Or(ln.Phase, "-"),
			ln.Round,
			ln.Attempt,
			formatRecipeRef(ln.RecipeName, ln.RecipeVersion),
			ln.PR,
			cmp.Or(ln.PRHead, "-"),
			cmp.Or(ln.Checks, "-"),
			ln.ApprovalStale(),
			formatAge(now, ln.LastEventAt, ln.UpdatedAt, ln.CreatedAt),
		)
	}

	fmt.Fprintf(&b, "\nActive Directives (%d):\n", len(directives))
	for i := range directives {
		d := directives[i]
		fmt.Fprintf(&b, "- %s [kind=%s source=%s scope=%s ref=%s]: %s\n",
			d.ID, d.Kind, d.Source, cmp.Or(d.Scope, "run"), cmp.Or(d.Ref, "-"), d.Text)
	}

	var latestDecSeq, latestEvSeq int64
	if len(decisions) > 0 {
		latestDecSeq = decisions[len(decisions)-1].Seq
	}
	if len(events) > 0 {
		latestEvSeq = events[len(events)-1].Seq
	}
	fmt.Fprintf(&b, "\nDecisions: count=%d latest_seq=%d | Events: count=%d latest_seq=%d\n",
		len(decisions), latestDecSeq, len(events), latestEvSeq)

	return truncateUTF8(b.String(), c.cfg.MaxReadBytes), nil
}

func (c *Concierge) execLaneShow(ctx context.Context, runID string, p LaneShowParams) (string, error) {
	laneID := strings.TrimSpace(p.LaneID)
	if laneID == "" {
		return "", fmt.Errorf("%w: lane_id is required", store.ErrInvalidArgument)
	}
	now := c.clock.Now().UTC()
	ln, err := c.store.Lanes().Get(ctx, runID, laneID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Lane: %s\n", ln.ID)
	fmt.Fprintf(&b, "Run: %s\n", ln.RunID)
	fmt.Fprintf(&b, "ItemID: %s\n", cmp.Or(ln.ItemID, "-"))
	fmt.Fprintf(&b, "Title: %s\n", cmp.Or(ln.Title, "-"))
	fmt.Fprintf(&b, "Status: %s\n", ln.Status)
	fmt.Fprintf(&b, "Phase: %s (round=%d attempt=%d)\n", cmp.Or(ln.Phase, "-"), ln.Round, ln.Attempt)
	fmt.Fprintf(&b, "Recipe: %s\n", formatRecipeRef(ln.RecipeName, ln.RecipeVersion))
	fmt.Fprintf(&b, "Branch: %s (base=%s)\n", cmp.Or(ln.Branch, "-"), cmp.Or(ln.BaseBranch, "-"))
	fmt.Fprintf(&b, "SessionID: %s | TerminalID: %s\n", cmp.Or(ln.SessionID, "-"), cmp.Or(ln.TerminalID, "-"))
	fmt.Fprintf(&b, "PR: #%d | Head: %s | ApprovalSHA: %s | ApprovalStale: %t | MergeSHA: %s | Checks: %s\n",
		ln.PR,
		cmp.Or(ln.PRHead, "-"),
		cmp.Or(ln.ApprovalSHA, "-"),
		ln.ApprovalStale(),
		cmp.Or(ln.MergeSHA, "-"),
		cmp.Or(ln.Checks, "-"),
	)
	fmt.Fprintf(&b, "LastEventID: %s | Age: %s\n",
		cmp.Or(ln.LastEventID, "-"),
		formatAge(now, ln.LastEventAt, ln.UpdatedAt, ln.CreatedAt),
	)

	if ln.ItemID != "" {
		if item, itemErr := c.store.Backlog().Get(ctx, runID, ln.ItemID); itemErr == nil && item != nil {
			fmt.Fprintf(&b, "\nBacklogItem %s: priority=%s status=%s title=%q acceptance=%q\n",
				item.ID, item.Priority, item.Status, item.Title, item.Acceptance)
		}
	}

	if c.driver != nil && ln.TerminalID != "" {
		if rep, repErr := c.driver.ReadReport(ctx, runID, ln.ID, ln.TerminalID); repErr == nil && rep != nil {
			fmt.Fprintf(&b, "\nLatest PhaseReport (terminal=%s outcome=%s head=%s):\n",
				ln.TerminalID, rep.Outcome, cmp.Or(rep.HeadSHA, "-"))
			if rep.Reason != "" {
				fmt.Fprintf(&b, "  Reason: %s\n", rep.Reason)
			}
			if rep.Breaks != "" {
				fmt.Fprintf(&b, "  Breaks: %s\n", rep.Breaks)
			}
			if rep.Ask != "" {
				fmt.Fprintf(&b, "  Ask: %s (for=%s)\n", rep.Ask, cmp.Or(rep.For, "-"))
			}
			if rep.Summary != "" {
				fmt.Fprintf(&b, "  Summary: %s\n", rep.Summary)
			}
		}
	}

	evs, err := c.store.Events().List(ctx, store.EventFilter{
		RunID:  runID,
		LaneID: ln.ID,
	})
	if err == nil && len(evs) > 0 {
		fmt.Fprintf(&b, "\nRecent Events (%d):\n", len(evs))
		startIdx := max(len(evs)-5, 0)
		for i := startIdx; i < len(evs); i++ {
			ev := evs[i]
			fmt.Fprintf(&b, "- [%s seq=%d kind=%s] %s %s\n",
				ev.ID, ev.Seq, ev.Kind, ev.Summary, string(ev.Payload))
		}
	}

	decs, err := c.store.Decisions().List(ctx, store.DecisionFilter{
		RunID:  runID,
		LaneID: ln.ID,
	})
	if err == nil && len(decs) > 0 {
		fmt.Fprintf(&b, "\nRecent Decisions (%d):\n", len(decs))
		startIdx := max(len(decs)-5, 0)
		for i := startIdx; i < len(decs); i++ {
			d := decs[i]
			fmt.Fprintf(&b, "- [%s seq=%d actor=%s tool=%s] reason=%q result=%q\n",
				d.ID, d.Seq, d.Actor, d.Tool, d.Reason, d.Result)
		}
	}

	limit := c.cfg.MaxReadBytes
	if p.MaxBytes > 0 && p.MaxBytes < limit {
		limit = p.MaxBytes
	}
	return truncateUTF8(b.String(), limit), nil
}

func (c *Concierge) execDecisionsSince(ctx context.Context, runID string, p DecisionsSinceParams) (string, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultDecisionLim
	}
	hasCursor := p.SinceSeq > 0 || strings.TrimSpace(p.SinceTime) != ""
	filter := store.DecisionFilter{
		RunID:    runID,
		LaneID:   strings.TrimSpace(p.LaneID),
		Actor:    strings.TrimSpace(p.Actor),
		Category: store.DecisionCategory(strings.TrimSpace(p.Category)),
		SinceSeq: p.SinceSeq,
	}
	if strings.TrimSpace(p.SinceTime) != "" {
		t, err := parseSinceTime(c.clock.Now().UTC(), p.SinceTime)
		if err != nil {
			return "", err
		}
		filter.SinceTime = &t
	}

	decs, err := c.store.Decisions().List(ctx, filter)
	if err != nil {
		return "", err
	}

	total := len(decs)
	truncated := false
	if total > limit {
		truncated = true
		if hasCursor {
			decs = decs[:limit]
		} else {
			decs = decs[total-limit:]
		}
	}

	var b strings.Builder
	if truncated {
		fmt.Fprintf(&b, "Decisions (%d shown of %d, next since_seq=%d):\n", len(decs), total, decs[len(decs)-1].Seq)
	} else {
		fmt.Fprintf(&b, "Decisions (%d):\n", len(decs))
	}
	if len(decs) == 0 {
		b.WriteString("(none)\n")
		return b.String(), nil
	}
	for i := range decs {
		d := decs[i]
		fmt.Fprintf(&b, "- [%s seq=%d at=%s category=%s actor=%s tool=%s lane=%s vetted=%t] reason=%q result=%q\n",
			d.ID,
			d.Seq,
			d.CreatedAt.UTC().Format(time.RFC3339),
			d.Category,
			d.Actor,
			d.Tool,
			cmp.Or(d.LaneID, "-"),
			d.Vetted,
			d.Reason,
			d.Result,
		)
	}
	return truncateUTF8(b.String(), c.cfg.MaxReadBytes), nil
}

func (c *Concierge) execBacklog(ctx context.Context, runID string, p BacklogParams) (string, error) {
	if itemID := strings.TrimSpace(p.ItemID); itemID != "" {
		it, err := c.store.Backlog().Get(ctx, runID, itemID)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "BacklogItem: %s\n", it.ID)
		fmt.Fprintf(&b, "Title: %s\n", it.Title)
		fmt.Fprintf(&b, "Priority: %s (order=%d)\n", it.Priority, it.Order)
		fmt.Fprintf(&b, "Status: %s\n", it.Status)
		fmt.Fprintf(&b, "LaneID: %s\n", cmp.Or(it.LaneID, "-"))
		fmt.Fprintf(&b, "RecipeName: %s\n", cmp.Or(it.RecipeName, "-"))
		if len(it.DependsOn) > 0 {
			fmt.Fprintf(&b, "DependsOn: %s\n", strings.Join(it.DependsOn, ", "))
		}
		if it.Acceptance != "" {
			fmt.Fprintf(&b, "Acceptance: %s\n", it.Acceptance)
		}
		if it.Description != "" {
			fmt.Fprintf(&b, "Description: %s\n", it.Description)
		}
		if len(it.History) > 0 {
			fmt.Fprintf(&b, "History (%d):\n", len(it.History))
			for _, h := range it.History {
				fmt.Fprintf(&b, "- [%s actor=%s] %s\n", h.At.UTC().Format(time.RFC3339), h.Actor, h.Change)
			}
		}
		return truncateUTF8(b.String(), c.cfg.MaxReadBytes), nil
	}

	limit := p.Limit
	if limit <= 0 {
		limit = DefaultBacklogLimit
	}
	var statuses []store.BacklogStatus
	if s := strings.TrimSpace(p.Status); s != "" {
		statuses = append(statuses, store.BacklogStatus(s))
	}
	for _, s := range p.Statuses {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			statuses = append(statuses, store.BacklogStatus(trimmed))
		}
	}

	items, err := c.store.Backlog().List(ctx, store.BacklogFilter{
		RunID:    runID,
		LaneID:   strings.TrimSpace(p.LaneID),
		Statuses: statuses,
		Limit:    limit,
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Backlog (%d items):\n", len(items))
	if len(items) == 0 {
		b.WriteString("(empty)\n")
		return b.String(), nil
	}
	for i := range items {
		it := items[i]
		fmt.Fprintf(&b, "- %s [%s #%d %s] title=%q lane=%s recipe=%s",
			it.ID, it.Priority, it.Order, it.Status, it.Title,
			cmp.Or(it.LaneID, "-"), cmp.Or(it.RecipeName, "-"))
		if len(it.DependsOn) > 0 {
			fmt.Fprintf(&b, " deps=[%s]", strings.Join(it.DependsOn, ","))
		}
		if it.Acceptance != "" {
			fmt.Fprintf(&b, " acceptance=%q", it.Acceptance)
		}
		b.WriteString("\n")
	}
	return truncateUTF8(b.String(), c.cfg.MaxReadBytes), nil
}

func (c *Concierge) execDirectiveSubmit(
	ctx context.Context,
	runID string,
	state *conciergeTurnState,
	rawArgs json.RawMessage,
	p DirectiveSubmitParams,
) (string, error) {
	start := c.clock.Now()
	kind, err := ParseDirectiveKind(p.Kind)
	if err != nil {
		return "", err
	}

	text := strings.TrimSpace(p.Text)
	ref := strings.TrimSpace(p.Ref)
	if text == "" && ref == "" {
		return "", fmt.Errorf("%w: directive.submit requires text or ref", store.ErrInvalidArgument)
	}
	if kind == store.DirectivePlanImport && ref == "" {
		ref = text
	}

	now := start.UTC()
	var expiresAt *time.Time
	if strings.TrimSpace(p.Expiry) != "" {
		exp, expErr := parseFutureTime(now, p.Expiry)
		if expErr != nil {
			return "", expErr
		}
		expiresAt = &exp
	}

	dirID := strings.TrimSpace(p.ID)
	if dirID == "" {
		dirID = c.nextID("dir")
	}

	scope := strings.TrimSpace(p.Scope)
	laneID := c.resolveLaneScope(ctx, runID, scope)

	dir := store.Directive{
		ID:        dirID,
		RunID:     runID,
		Kind:      kind,
		Source:    store.DirectiveSourceConcierge,
		Scope:     scope,
		Text:      text,
		Ref:       ref,
		ExpiresAt: expiresAt,
		Status:    store.DirectiveActive,
		CreatedAt: now,
	}
	if err := c.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}

	evPayload, _ := json.Marshal(dir)
	_, _ = c.store.Events().Append(ctx, store.Event{
		RunID:   runID,
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		LaneID:  laneID,
		Source:  ActorConcierge,
		Summary: fmt.Sprintf("directive %s (%s): %s", dir.ID, dir.Kind, cmp.Or(dir.Text, dir.Ref)),
		Payload: evPayload,
		Urgent:  kind == store.DirectivePlanImport || kind == store.DirectiveRecipeEdit || kind == store.DirectiveInstruction,
	})

	resultMsg := fmt.Sprintf("submitted directive %s (kind=%s status=%s text=%q ref=%q)",
		dir.ID, dir.Kind, dir.Status, dir.Text, dir.Ref)

	var handlerErr error
	if c.cfg.OnDirective != nil {
		handlerSummary, hErr := c.cfg.OnDirective(ctx, runID, dir)
		if hErr != nil {
			handlerErr = hErr
			_ = c.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
			dir.Status = store.DirectiveRevoked
			resultMsg = fmt.Sprintf("directive %s (%s) handler failed: %v", dir.ID, dir.Kind, hErr)
		} else {
			if updatedDir, getErr := c.store.Directives().Get(ctx, runID, dir.ID); getErr == nil && updatedDir != nil {
				dir = *updatedDir
			}
			if strings.TrimSpace(handlerSummary) != "" {
				resultMsg += "\n" + strings.TrimSpace(handlerSummary)
			}
		}
	}

	var (
		threadID string
		turnID   string
		usage    model.Usage
	)
	if state != nil {
		threadID = state.threadID
		turnID = state.turnID
		if !state.turnUsageRecorded {
			usage = state.turnUsage
			state.turnUsageRecorded = true
		}
		if handlerErr == nil {
			state.submittedDirectives = append(state.submittedDirectives, dir)
		}
	}

	dec, err := c.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
		RunID:        runID,
		Category:     store.DecisionJudgment,
		Actor:        ActorConcierge,
		ThreadID:     threadID,
		TurnID:       turnID,
		LaneID:       laneID,
		Tool:         ToolDirectiveSubmit,
		Reason:       cmp.Or(strings.TrimSpace(p.Reason), fmt.Sprintf("owner submitted %s directive %s", p.Kind, dir.ID)),
		Result:       resultMsg,
		Args:         slices.Clone(rawArgs),
		Vetted:       handlerErr == nil,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CostUSD:      usage.CostUSD,
		LatencyMs:    max(c.clock.Now().Sub(start).Milliseconds(), 0),
	})
	if err != nil {
		return "", err
	}
	if state != nil {
		state.decisions = append(state.decisions, dec)
	}
	if handlerErr != nil {
		return "", handlerErr
	}

	return resultMsg, nil
}

func (c *Concierge) resolveLaneScope(ctx context.Context, runID, scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == "run" {
		return ""
	}
	if ln, err := c.store.Lanes().Get(ctx, runID, scope); err == nil && ln != nil {
		return ln.ID
	}
	return ""
}

// VerifyAnswerCitations checks that answer cites at least one concrete fact from
// the Store for runID and does not fabricate unknown lane, backlog item, or directive IDs.
func VerifyAnswerCitations(
	ctx context.Context,
	st store.Store,
	runID string,
	answer string,
	submitted []store.Directive,
) ([]string, error) {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty answer", ErrUncitedAnswer)
	}

	var candidates []string
	seenCandidates := make(map[string]bool)
	knownLanes := make(map[string]bool)
	knownItems := make(map[string]bool)
	knownDirectives := make(map[string]bool)
	knownDecisions := make(map[string]bool)

	addCandidate := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seenCandidates[s] {
			seenCandidates[s] = true
			candidates = append(candidates, s)
		}
	}

	addCandidate(runID)
	run, err := st.Runs().Get(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if run != nil {
		addCandidate(run.ID)
		addCandidate(run.PlanRef)
		if run.Lease != nil {
			addCandidate(run.Lease.HolderID)
		}
	}

	items, err := st.Backlog().List(ctx, store.BacklogFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	for i := range items {
		it := items[i]
		knownItems[it.ID] = true
		addCandidate(it.ID)
		addCandidate(it.Title)
	}

	lanes, err := st.Lanes().List(ctx, store.LaneFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	for i := range lanes {
		ln := lanes[i]
		knownLanes[ln.ID] = true
		addCandidate(ln.ID)
		addCandidate(ln.SessionID)
		addCandidate(ln.TerminalID)
		if ln.PR > 0 {
			addCandidate(fmt.Sprintf("#%d", ln.PR))
			addCandidate(fmt.Sprintf("PR %d", ln.PR))
		}
		if ln.PRHead != "" {
			addCandidate(ln.PRHead)
			if len(ln.PRHead) >= 8 {
				addCandidate(ln.PRHead[:8])
			}
			if len(ln.PRHead) >= 7 {
				addCandidate(ln.PRHead[:7])
			}
		}
	}

	dirs, err := st.Directives().List(ctx, store.DirectiveFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	for i := range dirs {
		d := dirs[i]
		knownDirectives[d.ID] = true
		addCandidate(d.ID)
		addCandidate(d.Ref)
		addCandidate(d.Text)
	}
	for i := range submitted {
		d := submitted[i]
		knownDirectives[d.ID] = true
		addCandidate(d.ID)
		addCandidate(d.Ref)
		addCandidate(d.Text)
	}

	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
	if err != nil {
		return nil, err
	}
	for i := range decs {
		d := decs[i]
		knownDecisions[d.ID] = true
		addCandidate(d.ID)
		addCandidate(fmt.Sprintf("seq %d", d.Seq))
		addCandidate(fmt.Sprintf("seq=%d", d.Seq))
	}

	recs, err := st.Recipes().ListLatest(ctx, runID)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		r := recs[i]
		addCandidate(fmt.Sprintf("%s@v%d", r.Name, r.Version))
	}

	lowerAnswer := strings.ToLower(trimmed)
	var cited []string
	for _, c := range candidates {
		if strings.Contains(lowerAnswer, strings.ToLower(c)) {
			cited = append(cited, c)
		}
	}

	for _, tok := range extractPrefixedTokens(trimmed, "lane-") {
		if !knownLanes[tok] {
			return cited, fmt.Errorf("%w: answer cites unknown lane %q", ErrUncitedAnswer, tok)
		}
	}
	for _, tok := range extractPrefixedTokens(trimmed, "item-") {
		if !knownItems[tok] {
			return cited, fmt.Errorf("%w: answer cites unknown backlog item %q", ErrUncitedAnswer, tok)
		}
	}
	for _, tok := range extractPrefixedTokens(trimmed, "dir-") {
		if !knownDirectives[tok] {
			return cited, fmt.Errorf("%w: answer cites unknown directive %q", ErrUncitedAnswer, tok)
		}
	}
	for _, tok := range extractPrefixedTokens(trimmed, "dec-") {
		if !knownDecisions[tok] {
			return cited, fmt.Errorf("%w: answer cites unknown decision %q", ErrUncitedAnswer, tok)
		}
	}

	if len(cited) == 0 {
		return nil, fmt.Errorf("%w: answer contains no Store identifiers or facts", ErrUncitedAnswer)
	}
	return cited, nil
}

func extractPrefixedTokens(text, prefix string) []string {
	var out []string
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
	for _, f := range fields {
		if strings.HasPrefix(f, prefix) && len(f) > len(prefix) {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

func formatRecipeRef(name string, version int) string {
	rec := cmp.Or(name, recipe.DefaultName)
	if version > 0 {
		return fmt.Sprintf("%s@v%d", rec, version)
	}
	return rec
}

func formatAge(now time.Time, candidates ...time.Time) string {
	var ref time.Time
	for _, t := range candidates {
		if !t.IsZero() {
			ref = t
			break
		}
	}
	if ref.IsZero() || now.Before(ref) {
		return "0s"
	}
	return now.Sub(ref).Round(time.Second).String()
}

func parseFutureTime(now time.Time, raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("%w: timestamp or duration is required", store.ErrInvalidArgument)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("%w: timestamp %q must be in the future", store.ErrInvalidArgument, s)
		}
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid timestamp or duration %q: %v", store.ErrInvalidArgument, s, err)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("%w: duration %q must be positive", store.ErrInvalidArgument, s)
	}
	return now.Add(d).UTC(), nil
}

func parseSinceTime(now time.Time, raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid since_time %q: %v", store.ErrInvalidArgument, s, err)
	}
	if d > 0 {
		d = -d
	}
	return now.Add(d).UTC(), nil
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	const suffix = "...[truncated]"
	if maxBytes <= len(suffix) {
		return validUTF8Prefix(s, maxBytes)
	}
	return validUTF8Prefix(s, maxBytes-len(suffix)) + suffix
}

func validUTF8Prefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for s != "" && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
