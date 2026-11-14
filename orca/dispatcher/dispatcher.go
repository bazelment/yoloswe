package dispatcher

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/brief"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	ActorDispatcher       = "dispatcher"
	DefaultModel          = "claude-sonnet-5-5"
	DefaultHardBatchModel = "claude-opus-4-6"
	DefaultMaxTurns       = model.DefaultMaxTurns
	DefaultMaxTokens      = model.DefaultMaxTokens
	DefaultMaxReadBytes   = 4096
	ownerAskRefPrefix     = "owner_ask:"
	plannerRefPrefix      = "planner_request:"
)

var (
	_ controller.Judge = (*Dispatcher)(nil)
	_ Advisor          = (*Dispatcher)(nil)
)

// RecipeCompiler compiles a natural-language recipe override against a base recipe.
type RecipeCompiler interface {
	CompileOverride(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error)
}

// RecipeCompilerFunc adapts a function to RecipeCompiler.
type RecipeCompilerFunc func(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error)

func (f RecipeCompilerFunc) CompileOverride(ctx context.Context, base recipe.Recipe, override string) (recipe.Recipe, error) {
	return f(ctx, base, override)
}

// Config configures a Dispatcher instance.
type Config struct {
	RecipeCompiler   RecipeCompiler
	PRProbe          controller.PRProbe
	OnPlannerRequest func(ctx context.Context, runID, reason, planRef string) error
	OnOwnerAnswer    func(ctx context.Context, runID, askID, answer string) error
	// Logger, when set, receives one INFO line per tool call.
	Logger *slog.Logger
	// Temperature is the sampling temperature for judgment turns. Nil omits it
	// from the request: newer models reject the parameter outright.
	Temperature    *float64
	Model          string
	Provider       string
	PromptVersion  string
	HardBatchModel string
	SystemPrompt   string
	ThreadID       string
	Invariants     []Invariant
	Snapshot       SnapshotConfig
	MaxTurns       int
	MaxTokens      int
	MaxBriefBytes  int
	MaxReadBytes   int
	// AllowMultiAction lifts the one-primary-action-per-batch contract
	// (checkOnePrimaryAction). Production leaves it false; tests that script
	// every tool into a single batch set it.
	AllowMultiAction bool
}

func (c Config) withDefaults() Config {
	c.Model = cmp.Or(strings.TrimSpace(c.Model), DefaultModel)
	c.HardBatchModel = cmp.Or(strings.TrimSpace(c.HardBatchModel), DefaultHardBatchModel)
	c.SystemPrompt = cmp.Or(c.SystemPrompt, SystemPrompt)
	if len(c.Invariants) == 0 {
		c.Invariants = DefaultInvariants()
	}
	if c.MaxTurns <= 0 {
		c.MaxTurns = DefaultMaxTurns
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = DefaultMaxTokens
	}
	if c.MaxBriefBytes <= 0 {
		c.MaxBriefBytes = DefaultMaxBriefBytes
	}
	if c.MaxReadBytes <= 0 {
		c.MaxReadBytes = DefaultMaxReadBytes
	}
	c.Snapshot = c.Snapshot.withDefaults()
	return c
}

// BatchResult captures the outcome of a single Dispatcher judgment batch turn.
type BatchResult struct {
	ThreadID  string
	TurnID    string
	Model     string
	Summary   string
	Decisions []store.Decision
	Accepted  []ProposedAction
	Rejected  []string
	Snapshot  Snapshot
	Usage     model.Usage
	TurnCount int
}

// Dispatcher handles debounced judgment batches from the Controller by building
// a compact Store snapshot, running a bounded tool loop on ModelClient, vetting
// every tool call against deterministic invariants, and logging decisions.
type Dispatcher struct {
	store      store.Store
	driver     lane.LaneDriver
	clock      clock.Clock
	client     model.ModelClient
	lastResult *BatchResult
	cfg        Config
	seq        int64
	mu         sync.Mutex
}

// New constructs a new Dispatcher.
func New(
	cfg Config,
	st store.Store,
	drv lane.LaneDriver,
	clk clock.Clock,
	client model.ModelClient,
) *Dispatcher {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	return &Dispatcher{
		cfg:    cfg.withDefaults(),
		store:  st,
		driver: drv,
		clock:  clk,
		client: client,
	}
}

// SetOwnerAnswer installs the controller-owned resolution path for owner.answer.
func (d *Dispatcher) SetOwnerAnswer(fn func(context.Context, string, string, string) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg.OnOwnerAnswer = fn
}

// Judge implements controller.Judge by executing a fresh Dispatcher turn for events.
func (d *Dispatcher) Judge(ctx context.Context, runID string, events []store.Event) error {
	_, err := d.Dispatch(ctx, runID, events)
	return err
}

// Propose implements Advisor by running one model turn over snapshot and decoding
// the proposed tool calls into a Proposal without mutating Store state.
func (d *Dispatcher) Propose(ctx context.Context, snapshot string) (Proposal, error) {
	if d.client == nil {
		return Proposal{}, errors.New("orca/dispatcher: model client is required")
	}
	resp, err := d.client.CreateMessage(ctx, model.Request{
		Model:     d.cfg.Model,
		System:    d.cfg.SystemPrompt,
		Messages:  []model.Message{model.NewUserMessage(snapshot)},
		Tools:     d.ToolDefinitions(),
		MaxTokens: d.cfg.MaxTokens,
	})
	if err != nil {
		return Proposal{}, err
	}
	calls := resp.ToolCalls()
	out := Proposal{
		Unresolved: strings.TrimSpace(resp.Text()),
		Actions:    make([]ProposedAction, 0, len(calls)),
	}
	for i, tc := range calls {
		callID := cmp.Or(tc.ID, fmt.Sprintf("act-%d", i+1))
		act, pErr := ParseAction(callID, tc.Name, tc.Input)
		if pErr != nil {
			return out, pErr
		}
		out.Actions = append(out.Actions, act)
	}
	return out, nil
}

// LastResult returns the most recent BatchResult produced by Dispatch or Judge.
func (d *Dispatcher) LastResult() *BatchResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastResult == nil {
		return nil
	}
	cp := *d.lastResult
	cp.Decisions = slices.Clone(d.lastResult.Decisions)
	cp.Accepted = slices.Clone(d.lastResult.Accepted)
	cp.Rejected = slices.Clone(d.lastResult.Rejected)
	return &cp
}

// BuildSnapshot constructs a compact Store snapshot using the Dispatcher's configuration.
func (d *Dispatcher) BuildSnapshot(ctx context.Context, runID string, events []store.Event) (Snapshot, error) {
	return BuildSnapshot(ctx, d.store, d.clock, runID, events, d.cfg.Snapshot)
}

// ToolDefinitions returns the canonical tool definitions exposed to ModelClient.
func (d *Dispatcher) ToolDefinitions() []model.ToolDefinition {
	out := make([]model.ToolDefinition, len(dispatcherToolDefs))
	for i := range dispatcherToolDefs {
		out[i] = dispatcherToolDefs[i].Clone()
	}
	return out
}

func (d *Dispatcher) nextID(prefix string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	return fmt.Sprintf("%s-%d", prefix, d.seq)
}

func (d *Dispatcher) syncSeq(ctx context.Context, runID, threadID string) {
	var maxObserved int64
	if turns, err := d.store.Threads().ListTurns(ctx, threadID); err == nil {
		maxObserved = max(maxObserved, int64(len(turns)))
		for i := range turns {
			maxObserved = max(maxObserved, int64(turns[i].Seq), trailingSeqNumber(turns[i].ID))
		}
	}
	if dirs, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID}); err == nil {
		maxObserved = max(maxObserved, int64(len(dirs)))
		for i := range dirs {
			maxObserved = max(maxObserved, trailingSeqNumber(dirs[i].ID), trailingSeqNumber(dirs[i].Ref))
		}
	}
	d.mu.Lock()
	if maxObserved > d.seq {
		d.seq = maxObserved
	}
	d.mu.Unlock()
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

//nolint:govet // state groups are ordered by execution concerns.
type turnExecState struct {
	view          *StateView
	pinnedRecipes map[string]recipe.Recipe
	turnUsage     map[int]model.Usage
	turnUsed      map[int]bool
	threadID      string
	turnID        string
	decisions     []store.Decision
	accepted      []ProposedAction
	rejected      []string
	currentTurn   int
	nextActionSeq int
	traceID       string
	input         string
	model         string
}

// Dispatch executes one judgment batch turn and returns its structured BatchResult.
func (d *Dispatcher) Dispatch(ctx context.Context, runID string, events []store.Event) (*BatchResult, error) {
	if d.store == nil {
		return nil, errors.New("orca/dispatcher: store is required")
	}
	if d.client == nil {
		return nil, errors.New("orca/dispatcher: model client is required")
	}

	threadID, err := d.ensureThread(ctx, runID)
	if err != nil {
		return nil, err
	}
	d.syncSeq(ctx, runID, threadID)

	turnID := d.nextID(threadID + "-turn")
	turnRec, err := d.store.Threads().StartTurn(ctx, store.Turn{
		ID:       turnID,
		ThreadID: threadID,
		RunID:    runID,
		Input:    fmt.Sprintf("judgment batch (%d events)", len(events)),
	})
	if err != nil {
		return nil, err
	}

	cleanupCtx := context.WithoutCancel(ctx)
	snapCfg := d.cfg.Snapshot
	if len(turnRec.Steers) > 0 {
		snapCfg.Steers = append(slices.Clone(snapCfg.Steers), turnRec.Steers...)
	}

	snap, err := BuildSnapshot(ctx, d.store, d.clock, runID, events, snapCfg)
	if err != nil {
		_ = d.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, err.Error())
		for _, steerMsg := range turnRec.Steers {
			_ = d.store.Threads().SteerTurn(cleanupCtx, threadID, "", steerMsg)
		}
		return nil, err
	}

	if _, err := d.store.Threads().AppendItem(ctx, store.Item{
		ThreadID: threadID,
		TurnID:   turnID,
		Kind:     store.ItemKindSnapshot,
		Content:  snap.Text,
	}); err != nil {
		_ = d.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, store.TurnStatusFailed, model.Usage{}, err.Error())
		for _, steerMsg := range turnRec.Steers {
			_ = d.store.Threads().SteerTurn(cleanupCtx, threadID, "", steerMsg)
		}
		return nil, err
	}

	state := &turnExecState{
		threadID:      threadID,
		turnID:        turnID,
		pinnedRecipes: make(map[string]recipe.Recipe),
		turnUsage:     make(map[int]model.Usage),
		turnUsed:      make(map[int]bool),
		traceID:       turnID,
		input:         snap.Text,
	}

	reg := d.buildToolRegistry(runID, state)
	modelName := d.selectModel(events)
	state.model = modelName

	req := model.Request{
		Model:       modelName,
		System:      d.systemPromptWithMemories(ctx, runID),
		Messages:    []model.Message{model.NewUserMessage(snap.Text)},
		Tools:       toolsForBatch(reg.Definitions(), events),
		MaxTokens:   d.cfg.MaxTokens,
		Temperature: d.cfg.Temperature,
	}

	loopRes, loopErr := model.RunToolLoop(
		ctx,
		retryOnTruncationClient{inner: d.client},
		req,
		reg,
		model.WithMaxTurns(d.cfg.MaxTurns),
		model.WithToolCallLog(d.cfg.Logger, ActorDispatcher),
		model.WithOnTurn(func(turn int, resp model.Response) {
			state.currentTurn = turn
			state.turnUsage[turn] = resp.Usage
			if txt := strings.TrimSpace(resp.Text()); txt != "" {
				_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
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
						return d.handleRawToolCall(callCtx, runID, state, toolName, raw)
					})
				}
			}
		}),
		model.WithOnToolCall(func(_ int, call model.ToolCall, resBlock model.ContentBlock) {
			_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
				ThreadID: threadID,
				TurnID:   turnID,
				Kind:     store.ItemKindToolCall,
				Name:     call.Name,
				CallID:   call.ID,
				Payload:  call.Input,
			})
			_, _ = d.store.Threads().AppendItem(cleanupCtx, store.Item{
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
		summary   string
	)
	if loopRes != nil {
		usage = loopRes.Usage
		turnCount = loopRes.TurnCount
		summary = strings.TrimSpace(loopRes.Response.Text())
	}

	turnStatus := store.TurnStatusCompleted
	errMsg := ""
	if loopErr != nil {
		turnStatus = store.TurnStatusFailed
		errMsg = loopErr.Error()
	}
	if compErr := d.store.Threads().CompleteTurn(cleanupCtx, threadID, turnID, turnStatus, usage, errMsg); compErr != nil && loopErr == nil {
		loopErr = compErr
	}

	res := &BatchResult{
		ThreadID:  threadID,
		TurnID:    turnID,
		Model:     modelName,
		Summary:   summary,
		Snapshot:  snap,
		Decisions: state.decisions,
		Accepted:  state.accepted,
		Rejected:  state.rejected,
		Usage:     usage,
		TurnCount: turnCount,
	}

	d.mu.Lock()
	d.lastResult = res
	d.mu.Unlock()

	if loopErr != nil {
		return res, loopErr
	}
	return res, nil
}

func (d *Dispatcher) systemPromptWithMemories(ctx context.Context, runID string) string {
	ms, err := d.store.Memories().List(ctx, store.MemoryFilter{RunID: runID})
	if err != nil {
		return d.cfg.SystemPrompt
	}
	return d.cfg.SystemPrompt + "\n\n## Memory Index\n" + store.MemoryIndex(ms)
}

func (d *Dispatcher) ensureThread(ctx context.Context, runID string) (string, error) {
	threadID := strings.TrimSpace(d.cfg.ThreadID)
	if threadID == "" {
		threadID = "dispatcher-" + runID
	}
	if th, err := d.store.Threads().GetThread(ctx, threadID); err == nil && th != nil {
		return th.ID, nil
	}
	goal := "Orchestrate backlog and lanes for run " + runID
	if run, err := d.store.Runs().Get(ctx, runID); err == nil && run != nil && run.Objective != "" {
		goal = run.Objective
	}
	th, err := d.store.Threads().StartThread(ctx, store.Thread{
		ID:     threadID,
		RunID:  runID,
		Role:   store.ThreadRoleDispatcher,
		Goal:   goal,
		Status: store.ThreadStatusIdle,
	})
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return threadID, nil
		}
		return "", err
	}
	return th.ID, nil
}

func (d *Dispatcher) selectModel(events []store.Event) string {
	if d.cfg.HardBatchModel != "" && isHardBatch(events) {
		return d.cfg.HardBatchModel
	}
	return d.cfg.Model
}

// toolsForBatch drops the read.* tools from a batch that names no lane: they
// only expand a lane's truncated excerpt, and a model offered them anyway
// invents a lane ID.
func toolsForBatch(defs []model.ToolDefinition, events []store.Event) []model.ToolDefinition {
	if batchNamesLane(events) {
		return defs
	}
	return slices.DeleteFunc(slices.Clone(defs), func(def model.ToolDefinition) bool { return isReadOnlyTool(def.Name) })
}

// batchNamesLane reports whether any event of the batch is about a lane.
func batchNamesLane(events []store.Event) bool {
	return slices.ContainsFunc(events, func(ev store.Event) bool { return strings.TrimSpace(ev.LaneID) != "" })
}

func isHardBatch(events []store.Event) bool {
	if len(events) >= 4 {
		return true
	}
	for i := range events {
		if events[i].Urgent {
			return true
		}
	}
	return false
}

var dispatcherToolDefs = []model.ToolDefinition{
	{Name: ToolBacklogAdd, Description: "Add a new prioritized item to the run backlog", InputSchema: model.SchemaFor[BacklogAddParams]()},
	{Name: ToolBacklogUpdate, Description: "Edit an existing open or blocked backlog item (title, acceptance, priority, dependencies, open/blocked). Never sets in_progress: lane.spawn does that", InputSchema: model.SchemaFor[BacklogUpdateParams]()},
	{Name: ToolBacklogReorder, Description: "Reorder backlog items within their priority tier", InputSchema: model.SchemaFor[BacklogReorderParams]()},
	{Name: ToolBacklogClose, Description: "Close a backlog item as done or dropped", InputSchema: model.SchemaFor[BacklogCloseParams]()},
	{Name: ToolLaneSpawn, Description: "Spawn a worker lane for an open, ready backlog item. This also marks the item in_progress, so it is the only call needed to start work on it", InputSchema: model.SchemaFor[LaneSpawnParams]()},
	{Name: ToolLaneSteer, Description: "Send a steering note to an active or blocked lane", InputSchema: model.SchemaFor[LaneSteerParams]()},
	{Name: ToolLaneRetry, Description: "Retry a stalled, blocked, or failed lane on a fresh round", InputSchema: model.SchemaFor[LaneRetryParams]()},
	{Name: ToolLaneStop, Description: "Stop a non-terminal lane and mark it failed", InputSchema: model.SchemaFor[LaneStopParams]()},
	{Name: ToolOwnerAsk, Description: "Pose a decision to the owner that only the owner can make (product scope, spend, credentials, one-way/destructive policy) and that the batch presents as still open, with options, a mandatory default, and a deadline. Never use it for engineering, ordering or retry choices, or for work the owner already ordered", InputSchema: model.SchemaFor[OwnerAskParams]()},
	{Name: ToolOwnerAnswer, Description: "If the owner's message answers one of the listed open owner asks, resolve it with that ask's id; otherwise treat the message as work", InputSchema: model.SchemaFor[OwnerAnswerParams]()},
	{Name: ToolDirectiveNote, Description: "Persist a standing rule, preference or decision that later batches need and that no backlog item, lane or other call in this batch carries. At most one per batch, holding every point in its fact. Not a log of your own actions or questions", InputSchema: model.SchemaFor[DirectiveNoteParams]()},
	{Name: ToolPlannerRequest, Description: "Request a Planner turn to replan or import a plan. The reason is delivered to the Planner and also stored as an active directive, so it carries any standing rule given with the request", InputSchema: model.SchemaFor[PlannerRequestParams]()},
	{Name: ToolReadLaneReport, Description: "Read a bounded phase report for a lane", InputSchema: model.SchemaFor[ReadLaneReportParams]()},
	{Name: ToolReadPR, Description: "Read bounded PR metadata and approval status for a lane", InputSchema: model.SchemaFor[ReadPRParams]()},
	{Name: ToolReadCI, Description: "Read bounded CI check status and recent CI events for a lane", InputSchema: model.SchemaFor[ReadCIParams]()},
	{Name: "memory.read", Description: "Read the full body of a memory by ID", InputSchema: model.SchemaFor[struct {
		ID string `json:"id"`
	}]()},
}

func (d *Dispatcher) buildToolRegistry(runID string, state *turnExecState) *model.ToolRegistry {
	reg := model.NewToolRegistry()
	for i := range dispatcherToolDefs {
		def := dispatcherToolDefs[i]
		reg.Register(def, func(ctx context.Context, raw json.RawMessage) (string, error) {
			return d.handleRawToolCall(ctx, runID, state, def.Name, raw)
		})
	}
	return reg
}

type invariantViolationError struct {
	msg string
}

func (e *invariantViolationError) Error() string {
	return e.msg
}

func (d *Dispatcher) handleRawToolCall(
	ctx context.Context,
	runID string,
	state *turnExecState,
	toolName string,
	raw json.RawMessage,
) (string, error) {
	if toolName == "memory.read" {
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		m, err := d.store.Memories().Get(ctx, "", strings.TrimSpace(p.ID))
		if err != nil {
			return "", err
		}
		if m.RunID != "" && m.RunID != runID {
			return "", store.ErrNotFound
		}
		return fmt.Sprintf("%s\n%s", m.Name, m.Body), nil
	}
	start := d.clock.Now()
	var callID string
	if state != nil {
		state.nextActionSeq++
		callID = fmt.Sprintf("act-%d", state.nextActionSeq)
	}

	act, parseErr := ParseAction(callID, toolName, raw)
	if parseErr != nil {
		rejMsg := fmt.Sprintf("%s [run]: invalid arguments — %v", NormalizeToolName(toolName), parseErr)
		if state != nil {
			state.rejected = append(state.rejected, rejMsg)
		}
		_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+rejMsg, d.clock.Now().Sub(start))
		return "", errors.New(rejMsg)
	}

	var view *StateView
	if state != nil && state.view != nil {
		view = state.view
	} else {
		var pinned map[string]recipe.Recipe
		if state != nil {
			pinned = state.pinnedRecipes
		}
		var err error
		view, err = loadStateViewWithCache(ctx, d.store, d.clock, runID, d.cfg.MaxBriefBytes, pinned)
		if err != nil {
			return "", err
		}
		if state != nil {
			state.view = view
		}
	}

	if vetErr := VetAction(view, act, d.cfg.Invariants); vetErr != nil {
		rejMsg := vetErr.Error()
		if state != nil {
			state.rejected = append(state.rejected, rejMsg)
		}
		_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+rejMsg, d.clock.Now().Sub(start))
		return "", fmt.Errorf("invariant violation: %s", rejMsg)
	}

	if state != nil && !d.cfg.AllowMultiAction {
		if oneErr := checkOnePrimaryAction(state.accepted, act); oneErr != nil {
			rejMsg := fmt.Sprintf("%s [%s]: one primary action per batch — %v. The batch is answered: do not retry or substitute another mutating call, end your turn", act.Tool, act.Target(), oneErr)
			state.rejected = append(state.rejected, rejMsg)
			_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+rejMsg, d.clock.Now().Sub(start))
			return "", fmt.Errorf("invariant violation: %s", rejMsg)
		}
	}

	result, execErr := d.applyVettedAction(ctx, runID, view, act)
	if execErr != nil {
		if state != nil && !isReadOnlyTool(act.Tool) {
			state.view = nil
		}
		var invErr *invariantViolationError
		if errors.As(execErr, &invErr) {
			if state != nil {
				state.rejected = append(state.rejected, invErr.msg)
			}
			_ = d.recordDecision(ctx, runID, state, act, false, "rejected: "+invErr.msg, d.clock.Now().Sub(start))
			return "", fmt.Errorf("invariant violation: %s", invErr.msg)
		}
		_ = d.recordDecision(ctx, runID, state, act, true, "error: "+execErr.Error(), d.clock.Now().Sub(start))
		return "", execErr
	}

	if state != nil {
		if !isReadOnlyTool(act.Tool) {
			state.view = nil
		}
		state.accepted = append(state.accepted, act)
	}
	if err := d.recordDecision(ctx, runID, state, act, true, result, d.clock.Now().Sub(start)); err != nil {
		return "", err
	}
	return result, nil
}

// isFanOutTool reports whether a tool may repeat or combine within a batch:
// the work that is naturally plural and independent per target.
func isFanOutTool(tool string) bool {
	return tool == ToolBacklogAdd || tool == ToolLaneSpawn || tool == ToolLaneRetry
}

// checkOnePrimaryAction enforces one primary action per batch: the first
// mutating call wins, and any further one is rejected (visibly, so the model
// and the decision log see it) unless both are fan-out tools. Read tools are
// never limited.
func checkOnePrimaryAction(accepted []ProposedAction, act ProposedAction) error {
	if isReadOnlyTool(act.Tool) {
		return nil
	}
	for i := range accepted {
		prev := accepted[i]
		if isReadOnlyTool(prev.Tool) || (isFanOutTool(prev.Tool) && isFanOutTool(act.Tool)) {
			continue
		}
		return fmt.Errorf("this batch already applied %s [%s]; answer a batch with a single primary action (only %s, %s and %s may combine)",
			prev.Tool, prev.Target(), ToolBacklogAdd, ToolLaneSpawn, ToolLaneRetry)
	}
	return nil
}

func isReadOnlyTool(tool string) bool {
	return tool == ToolReadLaneReport || tool == ToolReadPR || tool == ToolReadCI
}

func (d *Dispatcher) recordDecision(
	ctx context.Context,
	runID string,
	state *turnExecState,
	act ProposedAction,
	vetted bool,
	result string,
	elapsed time.Duration,
) error {
	var (
		threadID string
		turnID   string
		usage    model.Usage
	)
	if state != nil {
		threadID = state.threadID
		turnID = state.turnID
		if !state.turnUsed[state.currentTurn] {
			usage = state.turnUsage[state.currentTurn]
			state.turnUsed[state.currentTurn] = true
		}
	}

	traceID, modelName, input := "", "", ""
	if state != nil {
		traceID, modelName, input = state.traceID, state.model, state.input
	}
	dec, err := d.store.Decisions().Append(context.WithoutCancel(ctx), store.Decision{
		RunID:         runID,
		Category:      store.DecisionJudgment,
		Actor:         ActorDispatcher,
		ThreadID:      threadID,
		TurnID:        turnID,
		LaneID:        act.LaneID,
		Tool:          cmp.Or(act.Tool, "unknown"),
		Reason:        cmp.Or(act.Reason, result),
		Result:        result,
		Args:          act.Args,
		Vetted:        vetted,
		InputTokens:   usage.InputTokens,
		OutputTokens:  usage.OutputTokens,
		CostUSD:       usage.CostUSD,
		LatencyMs:     max(elapsed.Milliseconds(), 0),
		TraceID:       traceID,
		Model:         modelName,
		Input:         input,
		Output:        result,
		Provider:      d.cfg.Provider,
		PromptVersion: d.cfg.PromptVersion,
		OutcomeLabel:  outcomeLabel(result),
	})
	if err != nil {
		return err
	}
	if state != nil {
		state.decisions = append(state.decisions, dec)
	}
	return nil
}

func outcomeLabel(result string) string {
	s := strings.ToLower(result)
	switch {
	case strings.Contains(s, "owner corrected"), strings.Contains(s, "corrected"):
		return "owner corrected"
	case strings.Contains(s, "merged"):
		return "merged"
	case strings.Contains(s, "reworked"), strings.Contains(s, "retry"):
		return "reworked"
	case strings.Contains(s, "abandoned"), strings.Contains(s, "dropped"):
		return "abandoned"
	default:
		return ""
	}
}

func (d *Dispatcher) applyVettedAction(
	ctx context.Context,
	runID string,
	view *StateView,
	act ProposedAction,
) (string, error) {
	switch act.Tool {
	case ToolBacklogAdd:
		return d.applyBacklogAdd(ctx, runID, *act.BacklogAdd)
	case ToolBacklogUpdate:
		return d.applyBacklogUpdate(ctx, runID, view, *act.BacklogUpdate)
	case ToolBacklogReorder:
		return d.applyBacklogReorder(ctx, runID, *act.BacklogReorder)
	case ToolBacklogClose:
		return d.applyBacklogClose(ctx, runID, *act.BacklogClose)
	case ToolLaneSpawn:
		return d.applyLaneSpawn(ctx, runID, view, *act.LaneSpawn)
	case ToolLaneSteer:
		return d.applyLaneSteer(ctx, runID, view, *act.LaneSteer)
	case ToolLaneRetry:
		return d.applyLaneRetry(ctx, runID, view, *act.LaneRetry)
	case ToolLaneStop:
		return d.applyLaneStop(ctx, runID, view, *act.LaneStop)
	case ToolOwnerAsk:
		return d.applyOwnerAsk(ctx, runID, view, *act.OwnerAsk)
	case ToolOwnerAnswer:
		if d.cfg.OnOwnerAnswer == nil {
			return "", errors.New("owner answer handler is not configured")
		}
		return fmt.Sprintf("resolved owner ask %s", act.OwnerAnswer.AskID), d.cfg.OnOwnerAnswer(ctx, runID, act.OwnerAnswer.AskID, act.OwnerAnswer.Answer)
	case ToolDirectiveNote:
		return d.applyDirectiveNote(ctx, runID, view, *act.DirectiveNote)
	case ToolPlannerRequest:
		return d.applyPlannerRequest(ctx, runID, *act.PlannerRequest)
	case ToolReadLaneReport:
		return d.applyReadLaneReport(ctx, runID, view, *act.ReadLaneReport)
	case ToolReadPR:
		return d.applyReadPR(ctx, runID, view, *act.ReadPR)
	case ToolReadCI:
		return d.applyReadCI(ctx, runID, view, *act.ReadCI)
	default:
		return "", fmt.Errorf("unsupported tool %q", act.Tool)
	}
}

func (d *Dispatcher) applyBacklogAdd(ctx context.Context, runID string, p BacklogAddParams) (string, error) {
	prio := store.Priority(p.Priority)
	if prio == "" {
		prio = store.PriorityP2
	}
	item := store.BacklogItem{
		ID:          strings.TrimSpace(p.ID),
		RunID:       runID,
		Title:       strings.TrimSpace(p.Title),
		Description: strings.TrimSpace(p.Description),
		Acceptance:  strings.TrimSpace(p.Acceptance),
		Priority:    prio,
		Status:      store.BacklogOpen,
		RecipeName:  strings.TrimSpace(p.RecipeName),
		DependsOn:   slices.Clone(p.DependsOn),
		Tags:        slices.Clone(p.Tags),
		Order:       p.Order,
	}
	if err := d.store.Backlog().Add(ctx, item); err != nil {
		return "", err
	}
	return fmt.Sprintf("added backlog item %s (%s)", item.ID, item.Priority), nil
}

func (d *Dispatcher) applyBacklogUpdate(ctx context.Context, runID string, view *StateView, p BacklogUpdateParams) (string, error) {
	existing, ok := view.BacklogItem(p.ID)
	if !ok {
		return "", fmt.Errorf("%w: backlog item %q", store.ErrNotFound, p.ID)
	}
	updated := existing.Clone()
	if strings.TrimSpace(p.Title) != "" {
		updated.Title = strings.TrimSpace(p.Title)
	}
	if p.Description != "" {
		updated.Description = strings.TrimSpace(p.Description)
	}
	if p.Acceptance != "" {
		updated.Acceptance = strings.TrimSpace(p.Acceptance)
	}
	if p.Priority != "" {
		updated.Priority = store.Priority(p.Priority)
	}
	if p.Status != "" {
		updated.Status = store.BacklogStatus(p.Status)
	}
	if p.RecipeName != "" {
		updated.RecipeName = strings.TrimSpace(p.RecipeName)
	}
	if p.DependsOn != nil {
		updated.DependsOn = slices.Clone(p.DependsOn)
	}
	if p.Tags != nil {
		updated.Tags = slices.Clone(p.Tags)
	}
	if err := d.store.Backlog().Update(ctx, updated, ActorDispatcher, strings.TrimSpace(p.Reason)); err != nil {
		return "", err
	}
	return fmt.Sprintf("updated backlog item %s", updated.ID), nil
}

func (d *Dispatcher) applyBacklogReorder(ctx context.Context, runID string, p BacklogReorderParams) (string, error) {
	if err := d.store.Backlog().Reorder(ctx, runID, p.ItemIDs, ActorDispatcher); err != nil {
		return "", err
	}
	return fmt.Sprintf("reordered %d backlog items", len(p.ItemIDs)), nil
}

func (d *Dispatcher) applyBacklogClose(ctx context.Context, runID string, p BacklogCloseParams) (string, error) {
	status := store.BacklogStatus(p.Status)
	if err := d.store.Backlog().Close(ctx, runID, p.ID, status, ActorDispatcher, strings.TrimSpace(p.Reason)); err != nil {
		return "", err
	}
	return fmt.Sprintf("closed backlog item %s as %s", p.ID, status), nil
}

func (d *Dispatcher) applyLaneSpawn(ctx context.Context, runID string, view *StateView, p LaneSpawnParams) (string, error) {
	item, ok := view.BacklogItem(p.ItemID)
	if !ok {
		return "", fmt.Errorf("%w: backlog item %q", store.ErrNotFound, p.ItemID)
	}
	laneID := cmp.Or(strings.TrimSpace(p.LaneID), item.ID)

	base, resolved, _ := view.ResolveSpawnRecipe(p)
	r := base

	override := strings.TrimSpace(p.RecipeOverride)
	if override != "" {
		if resolved.Version > 0 {
			r = resolved
		} else {
			var compiled recipe.Recipe
			if d.cfg.RecipeCompiler != nil {
				var err error
				compiled, err = d.cfg.RecipeCompiler.CompileOverride(ctx, base, override)
				if err != nil {
					return "", err
				}
			} else {
				return "", fmt.Errorf("natural-language recipe override %q requires a recipe compiler", override)
			}
			compiled.RunID = runID
			compiled.Name = fmt.Sprintf("override-%s", laneID)
			compiled.Version = 0
			if maxBytes := view.EffectiveMaxBriefBytes(compiled); maxBytes > 0 && len(p.Brief) > maxBytes {
				return "", &invariantViolationError{
					msg: fmt.Sprintf("%s [%s]: brief size within cap — brief size %d bytes exceeds cap of %d bytes",
						ToolLaneSpawn, laneID, len(p.Brief), maxBytes),
				}
			}
			if compiled.MergePolicy.Mode != recipe.MergeModeSelf && containsDirectMergeInstruction(p.Brief) {
				return "", &invariantViolationError{
					msg: fmt.Sprintf("%s [%s]: no merge outside recipe — brief instructs lane to merge directly, which is forbidden under recipe %q (merge_mode=%s)",
						ToolLaneSpawn, laneID, compiled.Name, compiled.MergePolicy.Mode),
				}
			}
			saved, err := d.store.Recipes().Save(ctx, compiled)
			if err != nil {
				return "", err
			}
			r = saved
		}
	}

	firstPhaseName := r.FirstPhase()
	firstPhase, hasFirst := r.Phase(firstPhaseName)
	if !hasFirst {
		return "", fmt.Errorf("recipe %q has no phases", r.Name)
	}

	now := d.clock.Now().UTC()
	ln := lane.Lane{
		ID:            laneID,
		RunID:         runID,
		ItemID:        item.ID,
		Title:         item.Title,
		Branch:        strings.TrimSpace(p.Branch),
		BaseBranch:    strings.TrimSpace(p.BaseBranch),
		Status:        lane.StatusPlanned,
		Phase:         firstPhase.Name,
		Round:         1,
		Attempt:       1,
		RecipeName:    r.Name,
		RecipeVersion: max(r.Version, 1),
		CreatedAt:     now,
		UpdatedAt:     now,
		LastEventAt:   now,
	}
	if existing, exists := view.Lane(laneID); exists {
		ln = existing.Clone()
		ln.ItemID = item.ID
		ln.Title = cmp.Or(item.Title, ln.Title)
		ln.Branch = cmp.Or(strings.TrimSpace(p.Branch), ln.Branch)
		ln.BaseBranch = cmp.Or(strings.TrimSpace(p.BaseBranch), ln.BaseBranch)
		ln.RecipeName = r.Name
		ln.RecipeVersion = max(r.Version, 1)
		ln.Phase = firstPhase.Name
		ln.Round = ln.MaxRound(firstPhase.Name) + 1
		ln.Attempt = 1
		ln.LastEventAt = now
	}
	if settled, msg, err := d.reconcileLanePRState(ctx, runID, &ln); settled || err != nil {
		return msg, err
	}

	workerBrief, err := d.buildWorkerBrief(view, ln, r, firstPhase, ln.Round, p.Brief)
	if err != nil {
		return "", err
	}

	if d.driver != nil && len(firstPhase.Agents) > 0 {
		spawnRes, err := d.spawnLaneSession(ctx, runID, ln, firstPhase.Name, ln.Round, firstPhase.Agents[0], workerBrief)
		if err != nil {
			return "", err
		}
		ln.Status = lane.StatusRunning
		if strings.TrimSpace(spawnRes.BaseCommit) != "" {
			ln.BaseCommit = strings.TrimSpace(spawnRes.BaseCommit)
		}
		ln.RecordAttempt(firstPhase.Name, ln.Round, spawnRes.SessionID, spawnRes.TerminalID)
	}

	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}

	updatedItem := item.Clone()
	updatedItem.Status = store.BacklogInProgress
	updatedItem.LaneID = ln.ID
	if override != "" {
		updatedItem.RecipeName = r.Name
	}
	if err := d.store.Backlog().Update(ctx, updatedItem, ActorDispatcher, fmt.Sprintf("spawned lane %s", ln.ID)); err != nil {
		return "", err
	}

	return fmt.Sprintf("spawned lane %s for item %s on %s (round %d, recipe %s@v%d)",
		ln.ID, item.ID, ln.Phase, ln.Round, ln.RecipeName, ln.RecipeVersion), nil
}

func (d *Dispatcher) reconcileLanePRState(ctx context.Context, runID string, ln *lane.Lane) (bool, string, error) {
	if d.cfg.PRProbe == nil || ln == nil || (ln.PR <= 0 && strings.TrimSpace(ln.Branch) == "") {
		return false, "", nil
	}
	prState, err := d.cfg.PRProbe(ctx, "", ln.Branch, ln.PR)
	if err != nil {
		return false, "", nil
	}
	if prState.Number > 0 && ln.PR <= 0 {
		ln.PR = prState.Number
	}
	state := strings.ToUpper(strings.TrimSpace(prState.State))
	switch state {
	case "MERGED", "CLOSED":
	default:
		return false, "", nil
	}
	if d.driver != nil {
		if strings.TrimSpace(ln.TerminalID) != "" || strings.TrimSpace(ln.SessionID) != "" {
			_ = d.driver.Stop(ctx, lane.StopRequest{
				RunID:      runID,
				LaneID:     ln.ID,
				SessionID:  ln.SessionID,
				TerminalID: ln.TerminalID,
				Reason:     "pr " + strings.ToLower(state),
			})
		}
		if r, ok := d.driver.(lane.SessionReleaser); ok && strings.TrimSpace(ln.SessionID) != "" {
			_ = r.ReleaseSession(ctx, ln.SessionID)
		}
	}
	if state == "MERGED" {
		ln.Status = lane.StatusDone
		ln.MergeSHA = cmp.Or(strings.TrimSpace(prState.MergeSHA), ln.MergeSHA, ln.PRHead)
	} else {
		ln.Status = lane.StatusFailed
	}
	ln.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, *ln); err != nil {
		return true, "", err
	}
	itemID := cmp.Or(ln.ItemID, ln.ID)
	if itemID != "" {
		if item, err := d.store.Backlog().Get(ctx, runID, itemID); err == nil && !item.Status.Terminal() {
			status := store.BacklogDone
			if state == "CLOSED" {
				status = store.BacklogDropped
			}
			_ = d.store.Backlog().Close(ctx, runID, itemID, status, ActorDispatcher, fmt.Sprintf("PR #%d %s", ln.PR, strings.ToLower(state)))
		}
	}
	return true, fmt.Sprintf("lane %s PR #%d already %s; stopped terminals", ln.ID, ln.PR, strings.ToLower(state)), nil
}

func (d *Dispatcher) applyLaneSteer(ctx context.Context, runID string, view *StateView, p LaneSteerParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	if settled, msg, err := d.reconcileLanePRState(ctx, runID, &ln); settled || err != nil {
		return msg, err
	}
	if d.driver != nil {
		if err := d.driver.Steer(ctx, lane.SteerRequest{
			RunID:      runID,
			LaneID:     ln.ID,
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
			Message:    p.Note,
		}); err != nil {
			return "", err
		}
	}
	if ln.Status == lane.StatusBlocked {
		ln.Status = lane.StatusRunning
	}
	ln.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}
	return fmt.Sprintf("steered lane %s", ln.ID), nil
}

func (d *Dispatcher) applyLaneRetry(ctx context.Context, runID string, view *StateView, p LaneRetryParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	if settled, msg, err := d.reconcileLanePRState(ctx, runID, &ln); settled || err != nil {
		return msg, err
	}
	r, ok := view.RecipeForLane(ln)
	if !ok {
		r = recipe.Default()
	}
	targetPhaseName := cmp.Or(strings.TrimSpace(p.Phase), ln.Phase, r.FirstPhase())
	targetPhase, ok := r.Phase(targetPhaseName)
	if !ok {
		return "", fmt.Errorf("unknown phase %q in recipe %q", targetPhaseName, r.Name)
	}

	round := p.Round
	if round <= 0 {
		round = ln.MaxRound(targetPhase.Name) + 1
	}

	workerBrief, err := d.buildWorkerBrief(view, ln, r, targetPhase, round, p.Note)
	if err != nil {
		return "", err
	}
	var sessionID, terminalID string

	if d.driver != nil && len(targetPhase.Agents) > 0 {
		agent := targetPhase.Agents[0]
		if ln.SessionID == "" {
			res, err := d.spawnLaneSession(ctx, runID, ln, targetPhase.Name, round, agent, workerBrief)
			if err != nil {
				return "", err
			}
			sessionID = res.SessionID
			terminalID = res.TerminalID
			if strings.TrimSpace(res.BaseCommit) != "" {
				ln.BaseCommit = strings.TrimSpace(res.BaseCommit)
			}
		} else {
			res, err := d.driver.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
				RunID:     runID,
				LaneID:    ln.ID,
				SessionID: ln.SessionID,
				Phase:     targetPhase.Name,
				Round:     round,
				Harness:   agent.Harness,
				Model:     agent.Model,
				Effort:    agent.Effort,
				Brief:     workerBrief,
			})
			if err != nil {
				return "", err
			}
			sessionID = res.SessionID
			terminalID = res.TerminalID
		}
	} else {
		sessionID = cmp.Or(ln.SessionID, fmt.Sprintf("sess-%s-r%d", ln.ID, round))
		terminalID = fmt.Sprintf("term-%s-%s", ln.ID, lane.PhaseRoundKey(targetPhase.Name, round))
	}

	ln.Status = lane.StatusRunning
	ln.Attempt = 1
	ln.RecordAttempt(targetPhase.Name, round, sessionID, terminalID)
	ln.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}
	return fmt.Sprintf("retried lane %s in phase %s (round %d)", ln.ID, targetPhase.Name, round), nil
}

func (d *Dispatcher) spawnLaneSession(
	ctx context.Context,
	runID string,
	ln lane.Lane,
	phase string,
	round int,
	agent recipe.AgentSpec,
	brief string,
) (lane.SpawnResult, error) {
	return d.driver.SpawnSession(ctx, lane.SpawnRequest{
		RunID:      runID,
		LaneID:     ln.ID,
		Phase:      phase,
		Round:      round,
		Harness:    agent.Harness,
		Model:      agent.Model,
		Effort:     agent.Effort,
		Brief:      brief,
		Branch:     ln.Branch,
		BaseBranch: ln.BaseBranch,
		Worktree:   ln.Worktree,
	})
}

// buildWorkerBrief is the single dispatcher path for briefs sent to a driver.
// Keeping the report contract here ensures initial spawns and retries have the
// same handback instructions as controller-started phase terminals.
func (d *Dispatcher) buildWorkerBrief(view *StateView, ln lane.Lane, r recipe.Recipe, phase recipe.Phase, round int, extra string) (string, error) {
	raw := d.buildRetryBriefText(view, ln, phase, extra)
	instructions := ""
	if d.driver != nil {
		instructions = d.driver.ReportInstructions(ln.RunID, ln.ID, phase.Name, round)
	}
	text, err := brief.Render(raw, view.Memories, 0)
	if err != nil {
		return "", err
	}
	return lane.AppendReportInstructions(text, instructions, view.EffectiveMaxBriefBytes(r)), nil
}

func (d *Dispatcher) buildRetryBriefText(view *StateView, ln lane.Lane, phase recipe.Phase, note string) string {
	var parts []string
	if ln.Title != "" {
		parts = append(parts, ln.Title)
	}
	mechanical := phase.Name == recipe.PhaseClean || phase.Name == recipe.PhaseReview
	if ln.ItemID != "" && !mechanical {
		if item, ok := view.BacklogItem(ln.ItemID); ok {
			if item.Description != "" {
				parts = append(parts, item.Description)
			}
			if item.Acceptance != "" {
				parts = append(parts, "Acceptance: "+item.Acceptance)
			}
		}
	}
	var skill string
	if d.driver != nil {
		skill = d.driver.PhaseSkill(phase.Name)
	}
	if skill != "" {
		parts = []string{skill}
		if ln.Title != "" {
			parts = append(parts, "Goal: "+ln.Title)
		}
	} else if phase.BriefTemplate != "" {
		parts = append(parts, phase.BriefTemplate)
	}
	if strings.TrimSpace(note) != "" {
		parts = append(parts, strings.TrimSpace(note))
	}
	return strings.Join(parts, "\n\n")
}

func (d *Dispatcher) applyLaneStop(ctx context.Context, runID string, view *StateView, p LaneStopParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	if d.driver != nil && (ln.SessionID != "" || ln.TerminalID != "") {
		if err := d.driver.Stop(ctx, lane.StopRequest{
			RunID:      runID,
			LaneID:     ln.ID,
			SessionID:  ln.SessionID,
			TerminalID: ln.TerminalID,
			Reason:     p.Reason,
		}); err != nil {
			return "", err
		}
	}
	ln.Status = lane.StatusFailed
	ln.LastEventAt = d.clock.Now().UTC()
	if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
		return "", err
	}
	if ln.ItemID != "" {
		if item, hasItem := view.BacklogItem(ln.ItemID); hasItem && item.Status == store.BacklogInProgress {
			item.Status = store.BacklogBlocked
			_ = d.store.Backlog().Update(ctx, item, ActorDispatcher, "lane stopped: "+p.Reason)
		}
	}
	return fmt.Sprintf("stopped lane %s (%s)", ln.ID, p.Reason), nil
}

func (d *Dispatcher) applyOwnerAsk(ctx context.Context, runID string, view *StateView, p OwnerAskParams) (string, error) {
	now := d.clock.Now().UTC()
	dl, err := parseFutureTime(now, p.Deadline)
	if err != nil {
		return "", err
	}
	askID := strings.TrimSpace(p.ID)
	if askID == "" {
		askID = d.nextID("ask")
	}

	ask := controller.OwnerAsk{
		ID:        askID,
		RunID:     runID,
		LaneID:    strings.TrimSpace(p.LaneID),
		Question:  strings.TrimSpace(p.Question),
		Options:   slices.Clone(p.Options),
		Default:   strings.TrimSpace(p.Default),
		SteerNote: strings.TrimSpace(p.SteerNote),
		Deadline:  dl,
	}
	if err := ask.Validate(); err != nil {
		return "", err
	}

	raw, err := json.Marshal(ask)
	if err != nil {
		return "", err
	}
	dir := store.Directive{
		ID:        "ask-" + ask.ID,
		RunID:     runID,
		Kind:      store.DirectiveInstruction,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     ask.LaneID,
		Ref:       ownerAskRefPrefix + ask.ID,
		Text:      string(raw),
		ExpiresAt: &dl,
		Status:    store.DirectiveActive,
	}
	if err := d.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}

	if ask.LaneID != "" {
		if ln, ok := view.Lane(ask.LaneID); ok && ln.Status == lane.StatusRunning {
			ln.Status = lane.StatusBlocked
			ln.LastEventAt = now
			if err := d.store.Lanes().Upsert(ctx, ln); err != nil {
				return "", err
			}
		}
	}
	return fmt.Sprintf("registered owner ask %s (default=%q deadline=%s)", ask.ID, ask.Default, dl.Format(time.RFC3339)), nil
}

func (d *Dispatcher) applyDirectiveNote(ctx context.Context, runID string, _ *StateView, p DirectiveNoteParams) (string, error) {
	now := d.clock.Now().UTC()
	var expiresAt *time.Time
	if strings.TrimSpace(p.Expiry) != "" {
		exp, err := parseFutureTime(now, p.Expiry)
		if err != nil {
			return "", err
		}
		expiresAt = &exp
	}
	dirID := strings.TrimSpace(p.ID)
	if dirID == "" {
		dirID = d.nextID("note")
	}
	dir := store.Directive{
		ID:        dirID,
		RunID:     runID,
		Kind:      store.DirectiveNote,
		Source:    store.DirectiveSourceDispatcher,
		Scope:     strings.TrimSpace(p.Scope),
		Text:      strings.TrimSpace(p.Fact),
		ExpiresAt: expiresAt,
		Status:    store.DirectiveActive,
	}
	if err := d.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}
	if expiresAt != nil {
		return fmt.Sprintf("recorded time-limited directive note %s", dir.ID), nil
	}
	if existing, getErr := d.store.Memories().Get(ctx, "", dir.ID); getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
		_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
		return "", getErr
	} else if existing != nil {
		_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
		return "", fmt.Errorf("%w: memory %s already exists", store.ErrInvalidArgument, dir.ID)
	}
	scope := strings.TrimSpace(dir.Scope)
	if scope != "owner" && scope != "repo" && scope != "run" {
		scope = "run"
	}
	memRunID := runID
	if scope != "run" {
		memRunID = ""
	}
	_, err := d.store.Memories().Put(ctx, store.MemoryEntry{
		ID: dir.ID, Name: "Dispatcher directive", Description: dir.Text, Type: "rule", Scope: scope,
		RunID: memRunID, Source: dir.Text, Body: dir.Text, ProposedBy: "model",
	})
	if err != nil {
		_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
		return "", err
	}
	return fmt.Sprintf("recorded directive note %s", dir.ID), nil
}

func (d *Dispatcher) applyPlannerRequest(ctx context.Context, runID string, p PlannerRequestParams) (string, error) {
	reqID := d.nextID("planreq")

	text := strings.TrimSpace(p.Reason)
	if p.Urgent {
		text = "[urgent] " + text
	}
	dir := store.Directive{
		ID:     reqID,
		RunID:  runID,
		Kind:   store.DirectiveInstruction,
		Source: store.DirectiveSourceDispatcher,
		Ref:    plannerRefPrefix + cmp.Or(strings.TrimSpace(p.PlanRef), reqID),
		Text:   text,
		Status: store.DirectiveActive,
	}
	if err := d.store.Directives().Add(ctx, dir); err != nil {
		return "", err
	}
	if d.cfg.OnPlannerRequest != nil {
		if err := d.cfg.OnPlannerRequest(ctx, runID, p.Reason, p.PlanRef); err != nil {
			_ = d.store.Directives().UpdateStatus(context.WithoutCancel(ctx), runID, dir.ID, store.DirectiveRevoked)
			return "", err
		}
	}
	return fmt.Sprintf("requested planner turn (%s)", reqID), nil
}

func (d *Dispatcher) applyReadLaneReport(ctx context.Context, runID string, view *StateView, p ReadLaneReportParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	termID := cmp.Or(strings.TrimSpace(p.TerminalID), ln.TerminalID)
	maxBytes := d.effectiveReadLimit(p.MaxBytes)

	if d.driver != nil && termID != "" {
		if rep, err := d.driver.ReadReport(ctx, runID, ln.ID, termID); err == nil && rep != nil {
			out := formatPhaseReport(*rep)
			return truncateText(out, maxBytes, nil), nil
		}
	}

	evs, err := d.store.Events().List(ctx, store.EventFilter{
		RunID:  runID,
		LaneID: ln.ID,
		Kinds:  []store.EventKind{store.EventLaneReport},
	})
	if err != nil {
		return "", err
	}
	if len(evs) == 0 {
		return fmt.Sprintf("lane %s: no phase report found", ln.ID), nil
	}
	latest := evs[len(evs)-1]
	out := fmt.Sprintf("lane=%s event=%s seq=%d summary=%s payload=%s",
		ln.ID, latest.ID, latest.Seq, latest.Summary, string(latest.Payload))
	return truncateText(out, maxBytes, nil), nil
}

func (d *Dispatcher) applyReadPR(_ context.Context, _ string, view *StateView, p ReadPRParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	maxBytes := d.effectiveReadLimit(p.MaxBytes)
	out := fmt.Sprintf("lane=%s status=%s phase=%s round=%d pr=#%d branch=%s base=%s head=%s approval=%s approval_stale=%t merge_sha=%s checks=%s",
		ln.ID,
		ln.Status,
		cmp.Or(ln.Phase, "-"),
		ln.Round,
		ln.PR,
		cmp.Or(ln.Branch, "-"),
		cmp.Or(ln.BaseBranch, "-"),
		cmp.Or(ln.PRHead, "-"),
		cmp.Or(ln.ApprovalSHA, "-"),
		ln.ApprovalStale(),
		cmp.Or(ln.MergeSHA, "-"),
		cmp.Or(ln.Checks, "-"),
	)
	return truncateText(out, maxBytes, nil), nil
}

func (d *Dispatcher) applyReadCI(ctx context.Context, runID string, view *StateView, p ReadCIParams) (string, error) {
	ln, ok := view.Lane(p.LaneID)
	if !ok {
		return "", fmt.Errorf("%w: lane %q", store.ErrNotFound, p.LaneID)
	}
	maxBytes := d.effectiveReadLimit(p.MaxBytes)

	evs, err := d.store.Events().List(ctx, store.EventFilter{
		RunID:  runID,
		LaneID: ln.ID,
		Kinds:  []store.EventKind{store.EventCI},
	})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "lane=%s pr=#%d head=%s checks=%s ci_events=%d",
		ln.ID, ln.PR, cmp.Or(ln.PRHead, "-"), cmp.Or(ln.Checks, "-"), len(evs))
	for i := max(len(evs)-3, 0); i < len(evs); i++ {
		ev := evs[i]
		fmt.Fprintf(&b, "\n- [%s seq=%d] %s %s", ev.ID, ev.Seq, ev.Summary, string(ev.Payload))
	}
	return truncateText(b.String(), maxBytes, nil), nil
}

func (d *Dispatcher) effectiveReadLimit(requested int) int {
	limit := d.cfg.MaxReadBytes
	if limit <= 0 {
		limit = DefaultMaxReadBytes
	}
	if requested > 0 && requested < limit {
		return requested
	}
	return limit
}

func formatPhaseReport(rep lane.PhaseReport) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("Outcome: %s", rep.Outcome))
	if rep.Phase != "" {
		parts = append(parts, fmt.Sprintf("Phase: %s (round %d)", rep.Phase, max(rep.Round, 1)))
	}
	if rep.HeadSHA != "" {
		parts = append(parts, fmt.Sprintf("HEAD: %s", rep.HeadSHA))
	}
	if rep.PR > 0 {
		parts = append(parts, fmt.Sprintf("PR: #%d", rep.PR))
	}
	if rep.Reason != "" {
		parts = append(parts, fmt.Sprintf("Reason: %s", rep.Reason))
	}
	if rep.Breaks != "" {
		parts = append(parts, fmt.Sprintf("Breaks: %s", rep.Breaks))
	}
	if rep.For != "" {
		parts = append(parts, fmt.Sprintf("For: %s", rep.For))
	}
	if rep.Ask != "" {
		parts = append(parts, fmt.Sprintf("Ask: %s", rep.Ask))
	}
	if rep.RiskiestStep != "" {
		parts = append(parts, fmt.Sprintf("Riskiest step: %s", rep.RiskiestStep))
	}
	if rep.Observed != "" {
		parts = append(parts, fmt.Sprintf("Observed: %s", rep.Observed))
	}
	if rep.Summary != "" {
		parts = append(parts, rep.Summary)
	} else if rep.Raw != "" {
		parts = append(parts, rep.Raw)
	}
	return strings.Join(parts, "\n")
}

// CompileRecipeOverride applies a deterministic natural-language override to
// base, producing a per-lane Recipe.
func CompileRecipeOverride(base recipe.Recipe, override string) recipe.Recipe {
	if len(base.Phases) == 0 {
		base = recipe.Default()
	}
	out := base.Clone()
	out.BaseRecipe = base.Name
	out.BaseVersion = base.Version
	trimmed := strings.TrimSpace(override)
	if trimmed == "" {
		return out
	}
	out.Version = 0
	if out.Source != "" {
		out.Source = out.Source + " | Override: " + trimmed
	} else {
		out.Source = trimmed
	}

	lower := strings.ToLower(trimmed)

	if containsOverridePhrase(lower, "no clean") || containsOverridePhrase(lower, "skip clean") || containsOverridePhrase(lower, "without clean") {
		out = removePhaseFromRecipe(out, recipe.PhaseClean)
	}

	if containsOverridePhrase(lower, "no code review") || containsOverridePhrase(lower, "no review") ||
		containsOverridePhrase(lower, "skip review") || containsOverridePhrase(lower, "without review") {
		out = removePhaseFromRecipe(out, recipe.PhaseReview)
		out.MergePolicy.RequireReviewPass = false
		for i := range out.Phases {
			out.Phases[i].Gates = slices.DeleteFunc(out.Phases[i].Gates, func(g recipe.Gate) bool {
				return g.Name == "review_approved" || g.Name == "review_head_pinned"
			})
		}
	}

	switch {
	case containsOverridePhrase(lower, "merge themselves") || containsOverridePhrase(lower, "self merge") ||
		containsOverridePhrase(lower, "self-merge") || containsOverridePhrase(lower, "merge_mode=self"):
		out.MergePolicy.Mode = recipe.MergeModeSelf
	case containsOverridePhrase(lower, "manual merge") || containsOverridePhrase(lower, "merge_mode=manual"):
		out.MergePolicy.Mode = recipe.MergeModeManual
	case containsOverridePhrase(lower, "analysis only") || containsOverridePhrase(lower, "no merge") ||
		containsOverridePhrase(lower, "skip merge") || containsOverridePhrase(lower, "without merge") ||
		containsOverridePhrase(lower, "merge_mode=none"):
		out = removePhaseFromRecipe(out, recipe.PhaseIntegrate)
		out.MergePolicy.Mode = recipe.MergeModeNone
		out.MergePolicy.RequireReviewPass = false
		out.MergePolicy.RequireCIGreen = false
	}

	return out
}

func containsOverridePhrase(s, phrase string) bool {
	offset := 0
	for offset <= len(s)-len(phrase) {
		idx := strings.Index(s[offset:], phrase)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(phrase)
		leftOK := start == 0 || !isOverrideWordChar(s[start-1])
		if leftOK && isOverrideClauseEnd(s[end:]) {
			return true
		}
		offset = start + 1
	}
	return false
}

func isOverrideWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

func isOverrideClauseEnd(rest string) bool {
	if rest == "" {
		return true
	}
	if isOverrideWordChar(rest[0]) {
		return false
	}
	trimmed := strings.TrimLeft(rest, " \t")
	for _, suffix := range []string{"phase", "step"} {
		if strings.HasPrefix(trimmed, suffix) {
			after := trimmed[len(suffix):]
			if after == "" || !isOverrideWordChar(after[0]) {
				trimmed = strings.TrimLeft(after, " \t")
				break
			}
		}
	}
	if trimmed == "" {
		return true
	}
	switch trimmed[0] {
	case ',', ';', '.', '|', '\n', '\r', ')', ':':
		return true
	default:
		return false
	}
}

func removePhaseFromRecipe(r recipe.Recipe, phaseToRemove string) recipe.Recipe {
	if len(r.Phases) <= 1 {
		return r
	}
	var nextPhase string
	for _, tr := range r.Transitions {
		if tr.From == phaseToRemove && tr.On.Verdict == recipe.OutcomeDone {
			nextPhase = tr.To
			break
		}
	}
	if nextPhase == "" {
		nextPhase = recipe.TargetComplete
	}

	r.Phases = slices.DeleteFunc(r.Phases, func(p recipe.Phase) bool {
		return p.Name == phaseToRemove
	})

	var updatedTransitions []recipe.Transition
	for _, tr := range r.Transitions {
		if tr.From == phaseToRemove {
			continue
		}
		if tr.To == phaseToRemove {
			tr.To = nextPhase
		}
		updatedTransitions = append(updatedTransitions, tr)
	}
	r.Transitions = updatedTransitions
	return r
}
